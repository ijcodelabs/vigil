// Package otlp is a local endpoint for applications instrumented with
// OpenTelemetry: OTLP over HTTP, protobuf encoding: metrics, logs and traces. What
// arrives is turned into the agent's own samples and log entries, so it is
// labelled, queued and shipped like everything else.
package otlp

import (
	"compress/gzip"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"maps"
	"math"
	"net"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	collogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"

	"github.com/ijcodelabs/vigil/internal/model"
)

const maxBody = 16 << 20

// resourceLabels are the resource attributes kept as labels. The rest
// (process ids, SDK versions, host details) would only multiply series.
var resourceLabels = map[string]string{
	"service.name":                "service",
	"service.namespace":           "service_namespace",
	"service.instance.id":         "service_instance",
	"deployment.environment":      "deployment_environment",
	"deployment.environment.name": "deployment_environment",
}

// Receiver serves /v1/metrics, /v1/logs and /v1/traces.
type Receiver struct {
	Listen  string
	Metrics func([]model.Sample)
	Logs    func([]model.LogEntry) error
	// Spans receives the spans that are kept, as an OTLP trace request
	// (protobuf); nil switches traces off. The metrics counted from all
	// spans go to Metrics.
	Spans  func(otlpRequest []byte) error
	Traces TraceOptions
	Log    *slog.Logger
	// DropLabels are metric labels (as stored) left out of every point.
	DropLabels map[string]bool
	// Redact, if set, masks secrets in the attributes and events of the
	// spans that are passed on. Log entries are masked by the caller.
	Redact func(string) string

	// Sums and histograms sent as differences since the last report are
	// added up here, because the metric store wants running totals.
	mu     sync.Mutex
	totals map[uint64]*running
	trace  *traceState
}

type running struct {
	sum, count float64
	buckets    []float64
	seen       time.Time
	// last are the samples written from this series' latest report. A
	// sender of differences reports only when something changed, so they
	// are written again on a clock (repeatTotals); a counter with a single
	// sample has no rate.
	last []model.Sample
}

// repeatFor is how long the running total of a series that stopped
// reporting is still written.
const repeatFor = time.Hour

// Run serves until ctx is cancelled.
func (r *Receiver) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", r.Listen)
	if err != nil {
		return fmt.Errorf("OTLP endpoint: %w", err)
	}
	srv := &http.Server{Handler: r.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		srv.Shutdown(shut)
	}()
	// Running totals (span counters, and sums sent as differences) are
	// written on a clock, like a scrape, whether or not anything arrived
	// in between.
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				samples := r.repeatTotals(now)
				if r.Spans != nil {
					samples = append(samples, r.traceMetrics(now)...)
				}
				if len(samples) > 0 {
					r.Metrics(samples)
				}
			}
		}
	}()
	r.Log.Info("OTLP endpoint on", "addr", ln.Addr().String(), "traces", r.Spans != nil)
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (r *Receiver) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/metrics", func(w http.ResponseWriter, req *http.Request) {
		in := &colmetrics.ExportMetricsServiceRequest{}
		if !read(w, req, in) {
			return
		}
		if samples := r.convertMetrics(in); len(samples) > 0 {
			r.Metrics(samples)
		}
		reply(w, &colmetrics.ExportMetricsServiceResponse{})
	})
	mux.HandleFunc("POST /v1/logs", func(w http.ResponseWriter, req *http.Request) {
		in := &collogs.ExportLogsServiceRequest{}
		if !read(w, req, in) {
			return
		}
		if entries := convertLogs(in); len(entries) > 0 {
			if err := r.Logs(entries); err != nil {
				http.Error(w, "could not queue the logs", http.StatusServiceUnavailable) // the SDK retries
				return
			}
		}
		reply(w, &collogs.ExportLogsServiceResponse{})
	})
	mux.HandleFunc("POST /v1/traces", func(w http.ResponseWriter, req *http.Request) {
		if r.Spans == nil {
			// Traces are switched off here. Accepting and discarding them
			// keeps SDKs that send all three signals from logging an error
			// every few seconds.
			io.Copy(io.Discard, http.MaxBytesReader(w, req.Body, maxBody))
			w.Header().Set("Content-Type", "application/x-protobuf")
			return
		}
		in := &coltrace.ExportTraceServiceRequest{}
		if !read(w, req, in) {
			return
		}
		if kept := r.convertTraces(in, time.Now()); kept != nil {
			payload, err := proto.Marshal(kept)
			if err == nil {
				err = r.Spans(payload)
			}
			if err != nil {
				http.Error(w, "could not queue the spans", http.StatusServiceUnavailable) // the SDK retries
				return
			}
		}
		reply(w, &coltrace.ExportTraceServiceResponse{})
	})
	return mux
}

func read(w http.ResponseWriter, req *http.Request, into proto.Message) bool {
	if ct := req.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/x-protobuf") {
		http.Error(w, "this endpoint takes OTLP as protobuf; set OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf", http.StatusUnsupportedMediaType)
		return false
	}
	var body io.Reader = http.MaxBytesReader(w, req.Body, maxBody)
	if req.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(body)
		if err != nil {
			http.Error(w, "bad gzip body", http.StatusBadRequest)
			return false
		}
		defer zr.Close()
		body = io.LimitReader(zr, 4*maxBody)
	}
	b, err := io.ReadAll(body)
	if err == nil {
		err = proto.Unmarshal(b, into)
	}
	if err != nil {
		http.Error(w, "unreadable OTLP request: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

func reply(w http.ResponseWriter, m proto.Message) {
	b, _ := proto.Marshal(m)
	w.Header().Set("Content-Type", "application/x-protobuf")
	w.Write(b)
}

// sanitize makes a name the metric store accepts: http.server.duration → http_server_duration.
func sanitize(name string) string {
	var b strings.Builder
	for i, c := range name {
		ok := c == '_' || c == ':' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9' && i > 0)
		if ok {
			b.WriteRune(c)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

func anyString(v *commonpb.AnyValue) string {
	switch x := v.GetValue().(type) {
	case nil:
		return ""
	case *commonpb.AnyValue_StringValue:
		return x.StringValue
	case *commonpb.AnyValue_BoolValue:
		return strconv.FormatBool(x.BoolValue)
	case *commonpb.AnyValue_IntValue:
		return strconv.FormatInt(x.IntValue, 10)
	case *commonpb.AnyValue_DoubleValue:
		return strconv.FormatFloat(x.DoubleValue, 'g', -1, 64)
	case *commonpb.AnyValue_BytesValue:
		return hex.EncodeToString(x.BytesValue)
	}
	// Lists and maps: as JSON, which is what a person would expect to read.
	b, _ := json.Marshal(anyNative(v))
	return string(b)
}

func anyNative(v *commonpb.AnyValue) any {
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_ArrayValue:
		out := []any{}
		for _, e := range x.ArrayValue.GetValues() {
			out = append(out, anyNative(e))
		}
		return out
	case *commonpb.AnyValue_KvlistValue:
		out := map[string]any{}
		for _, kv := range x.KvlistValue.GetValues() {
			out[kv.Key] = anyNative(kv.Value)
		}
		return out
	}
	return anyString(v)
}

func resourceOf(res *resourcepb.Resource) map[string]string {
	out := map[string]string{}
	for _, kv := range res.GetAttributes() {
		if name, keep := resourceLabels[kv.Key]; keep {
			if v := anyString(kv.Value); v != "" {
				out[name] = v
			}
		}
	}
	return out
}

func labelsOf(base map[string]string, attrs []*commonpb.KeyValue) map[string]string {
	out := make(map[string]string, len(base)+len(attrs)+1)
	for k, v := range base {
		out[k] = v
	}
	for _, kv := range attrs {
		if v := anyString(kv.Value); v != "" {
			out[sanitize(kv.Key)] = v
		}
	}
	return out
}

// metricLabels are the labels of one point, without the dropped ones.
func (r *Receiver) metricLabels(base map[string]string, attrs []*commonpb.KeyValue) map[string]string {
	out := labelsOf(base, attrs)
	for k, drop := range r.DropLabels {
		if drop {
			delete(out, k)
		}
	}
	return out
}

// repeatTotals returns the latest samples of every series sent as
// differences that reported within repeatFor, stamped now.
func (r *Receiver) repeatTotals(now time.Time) []model.Sample {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []model.Sample
	for _, t := range r.totals {
		if now.Sub(t.seen) > repeatFor {
			continue
		}
		for _, s := range t.last {
			s.Labels = maps.Clone(s.Labels)
			s.TS = now.UnixMilli()
			out = append(out, s)
		}
	}
	return out
}

func seriesKey(name string, labels map[string]string) uint64 {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := fnv.New64a()
	h.Write([]byte(name))
	for _, k := range keys {
		h.Write([]byte{0})
		h.Write([]byte(k))
		h.Write([]byte{0})
		h.Write([]byte(labels[k]))
	}
	return h.Sum64()
}

// total returns the running state of a series reported as differences.
func (r *Receiver) total(name string, labels map[string]string, now time.Time) *running {
	if r.totals == nil {
		r.totals = map[uint64]*running{}
	}
	if len(r.totals) > 200000 { // forget series that stopped reporting
		for k, t := range r.totals {
			if now.Sub(t.seen) > time.Hour {
				delete(r.totals, k)
			}
		}
	}
	key := seriesKey(name, labels)
	t := r.totals[key]
	if t == nil {
		t = &running{}
		r.totals[key] = t
	}
	t.seen = now
	return t
}

func millis(unixNano uint64, now time.Time) int64 {
	if unixNano == 0 {
		return now.UnixMilli()
	}
	return int64(unixNano / 1e6)
}

func number(p *metricspb.NumberDataPoint) float64 {
	switch v := p.GetValue().(type) {
	case *metricspb.NumberDataPoint_AsInt:
		return float64(v.AsInt)
	case *metricspb.NumberDataPoint_AsDouble:
		return v.AsDouble
	}
	return math.NaN()
}

// convertMetrics maps OTLP metrics onto Prometheus-style samples:
// gauges as they are, monotonic sums as counters (name_total), histograms
// as name_bucket{le}, name_sum and name_count.
func (r *Receiver) convertMetrics(in *colmetrics.ExportMetricsServiceRequest) []model.Sample {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	var out []model.Sample
	add := func(name string, labels map[string]string, v float64, ts int64) {
		if !math.IsNaN(v) {
			out = append(out, model.Sample{Name: name, Labels: labels, Value: v, TS: ts})
		}
	}
	for _, rm := range in.GetResourceMetrics() {
		base := resourceOf(rm.GetResource())
		if svc := base["service"]; svc != "" {
			base["job"] = svc
		}
		for _, sm := range rm.GetScopeMetrics() {
			for _, m := range sm.GetMetrics() {
				name := sanitize(m.GetName())
				switch d := m.GetData().(type) {
				case *metricspb.Metric_Gauge:
					for _, p := range d.Gauge.GetDataPoints() {
						add(name, r.metricLabels(base, p.GetAttributes()), number(p), millis(p.GetTimeUnixNano(), now))
					}
				case *metricspb.Metric_Sum:
					delta := d.Sum.GetAggregationTemporality() == metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA
					if d.Sum.GetIsMonotonic() && !strings.HasSuffix(name, "_total") {
						name += "_total"
					}
					for _, p := range d.Sum.GetDataPoints() {
						labels, v := r.metricLabels(base, p.GetAttributes()), number(p)
						if delta && !math.IsNaN(v) {
							t := r.total(name, labels, now)
							ts := millis(p.GetTimeUnixNano(), now)
							if t.last == nil {
								// A new counter starts at zero, at the start of
								// the first difference; without that sample the
								// store cannot tell how fast it rose.
								start := millis(p.GetStartTimeUnixNano(), now)
								if start >= ts || p.GetStartTimeUnixNano() == 0 {
									start = ts - 1000
								}
								add(name, labels, 0, start)
							}
							t.sum += v
							v = t.sum
							from := len(out)
							add(name, labels, v, ts)
							t.last = slices.Clone(out[from:])
							continue
						}
						add(name, labels, v, millis(p.GetTimeUnixNano(), now))
					}
				case *metricspb.Metric_Histogram:
					delta := d.Histogram.GetAggregationTemporality() == metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA
					for _, p := range d.Histogram.GetDataPoints() {
						labels, ts := r.metricLabels(base, p.GetAttributes()), millis(p.GetTimeUnixNano(), now)
						counts := make([]float64, len(p.GetBucketCounts()))
						for i, c := range p.GetBucketCounts() {
							counts[i] = float64(c)
						}
						sum, count := p.GetSum(), float64(p.GetCount())
						var t *running
						from := len(out)
						if delta {
							t = r.total(name, labels, now)
							if len(t.buckets) != len(counts) {
								t.buckets = make([]float64, len(counts))
							}
							t.sum += sum
							t.count += count
							for i := range counts {
								t.buckets[i] += counts[i]
								counts[i] = t.buckets[i]
							}
							sum, count = t.sum, t.count
						}
						cumulative := 0.0
						for i, c := range counts {
							cumulative += c
							le := "+Inf"
							if i < len(p.GetExplicitBounds()) {
								le = strconv.FormatFloat(p.GetExplicitBounds()[i], 'g', -1, 64)
							}
							bl := labelsOf(labels, nil)
							bl["le"] = le
							add(name+"_bucket", bl, cumulative, ts)
						}
						if p.Sum != nil || delta {
							add(name+"_sum", labelsOf(labels, nil), sum, ts)
						}
						add(name+"_count", labelsOf(labels, nil), count, ts)
						if t != nil {
							t.last = slices.Clone(out[from:])
						}
					}
				case *metricspb.Metric_Summary:
					for _, p := range d.Summary.GetDataPoints() {
						labels, ts := r.metricLabels(base, p.GetAttributes()), millis(p.GetTimeUnixNano(), now)
						for _, q := range p.GetQuantileValues() {
							ql := labelsOf(labels, nil)
							ql["quantile"] = strconv.FormatFloat(q.GetQuantile(), 'g', -1, 64)
							add(name, ql, q.GetValue(), ts)
						}
						add(name+"_sum", labelsOf(labels, nil), p.GetSum(), ts)
						add(name+"_count", labelsOf(labels, nil), float64(p.GetCount()), ts)
					}
				case *metricspb.Metric_ExponentialHistogram:
					// The buckets have no fixed bounds to translate; the count
					// and sum still give rate and average.
					for _, p := range d.ExponentialHistogram.GetDataPoints() {
						if d.ExponentialHistogram.GetAggregationTemporality() == metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA {
							continue
						}
						labels, ts := r.metricLabels(base, p.GetAttributes()), millis(p.GetTimeUnixNano(), now)
						add(name+"_sum", labels, p.GetSum(), ts)
						add(name+"_count", labelsOf(labels, nil), float64(p.GetCount()), ts)
					}
				}
			}
		}
	}
	return out
}

var severities = []string{"", "trace", "debug", "info", "warn", "error", "fatal"}

// convertLogs maps OTLP log records onto log entries. The trace and span
// ids are kept as fields, which is what later links a line to its trace.
func convertLogs(in *collogs.ExportLogsServiceRequest) []model.LogEntry {
	now := time.Now()
	var out []model.LogEntry
	for _, rl := range in.GetResourceLogs() {
		stream := resourceOf(rl.GetResource())
		stream["source"] = "otlp"
		for _, sl := range rl.GetScopeLogs() {
			for _, rec := range sl.GetLogRecords() {
				ts := rec.GetTimeUnixNano()
				if ts == 0 {
					ts = rec.GetObservedTimeUnixNano()
				}
				if ts == 0 {
					ts = uint64(now.UnixNano())
				}
				e := model.LogEntry{Time: int64(ts), Msg: anyString(rec.GetBody()), Stream: stream, Fields: map[string]string{}}
				for _, kv := range rec.GetAttributes() {
					if v := anyString(kv.Value); v != "" {
						e.Fields[sanitize(kv.Key)] = v
					}
				}
				level := strings.ToLower(rec.GetSeverityText())
				if n := int(rec.GetSeverityNumber()); level == "" && n > 0 {
					level = severities[min((n-1)/4+1, len(severities)-1)]
				}
				if level != "" {
					e.Fields["level"] = level
				}
				if id := rec.GetTraceId(); len(id) > 0 {
					e.Fields["trace_id"] = hex.EncodeToString(id)
				}
				if id := rec.GetSpanId(); len(id) > 0 {
					e.Fields["span_id"] = hex.EncodeToString(id)
				}
				if name := sl.GetScope().GetName(); name != "" {
					e.Fields["logger"] = name
				}
				if e.Msg == "" {
					continue
				}
				out = append(out, e)
			}
		}
	}
	return out
}

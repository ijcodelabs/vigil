package otlp

import (
	"bytes"
	"compress/gzip"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	collogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"

	"github.com/ijcodelabs/vigil/internal/model"
)

func str(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

var resource = &resourcepb.Resource{Attributes: []*commonpb.KeyValue{str("service.name", "checkout"),
	str("deployment.environment", "prod"), str("process.pid", "4211"), str("telemetry.sdk.version", "1.40")}}

func dbl(v float64, attrs ...*commonpb.KeyValue) *metricspb.NumberDataPoint {
	return &metricspb.NumberDataPoint{TimeUnixNano: 1_700_000_000_000_000_000, Attributes: attrs, Value: &metricspb.NumberDataPoint_AsDouble{AsDouble: v}}
}

func post(t *testing.T, h http.Handler, path string, m proto.Message, zip bool) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := proto.Marshal(m)
	req := httptest.NewRequest("POST", path, nil)
	if zip {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		zw.Write(b)
		zw.Close()
		b = buf.Bytes()
		req.Header.Set("Content-Encoding", "gzip")
	}
	req.Body = io.NopCloser(bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/x-protobuf")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestMetrics(t *testing.T) {
	var got []model.Sample
	r := &Receiver{Metrics: func(s []model.Sample) { got = append(got, s...) }, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	cumulative, delta := metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA
	sum := 12.5
	req := &colmetrics.ExportMetricsServiceRequest{ResourceMetrics: []*metricspb.ResourceMetrics{{Resource: resource,
		ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: []*metricspb.Metric{
			{Name: "jvm.memory.used", Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{DataPoints: []*metricspb.NumberDataPoint{dbl(1024, str("jvm.memory.pool.name", "eden"))}}}},
			{Name: "http.server.requests", Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{IsMonotonic: true, AggregationTemporality: cumulative,
				DataPoints: []*metricspb.NumberDataPoint{dbl(40, str("http.route", "/pay"))}}}},
			{Name: "orders.placed", Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{IsMonotonic: true, AggregationTemporality: delta,
				DataPoints: []*metricspb.NumberDataPoint{dbl(3)}}}},
			{Name: "queue.depth", Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{IsMonotonic: false, AggregationTemporality: cumulative,
				DataPoints: []*metricspb.NumberDataPoint{dbl(7)}}}},
			{Name: "http.server.duration", Data: &metricspb.Metric_Histogram{Histogram: &metricspb.Histogram{AggregationTemporality: cumulative,
				DataPoints: []*metricspb.HistogramDataPoint{{TimeUnixNano: 1_700_000_000_000_000_000, Count: 6, Sum: &sum,
					ExplicitBounds: []float64{0.1, 1}, BucketCounts: []uint64{2, 3, 1}}}}}},
		}}}}}}
	if w := post(t, r.Handler(), "/v1/metrics", req, true); w.Code != 200 || w.Header().Get("Content-Type") != "application/x-protobuf" {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	post(t, r.Handler(), "/v1/metrics", req, false) // the delta counter reports 3 more

	last := map[string]float64{}
	var labels map[string]string
	for _, s := range got {
		key := s.Name
		if le := s.Labels["le"]; le != "" {
			key += "{le=" + le + "}"
		}
		last[key] = s.Value
		if s.Name == "http_server_requests_total" {
			labels = s.Labels
		}
	}
	for name, want := range map[string]float64{
		"jvm_memory_used": 1024, "http_server_requests_total": 40, "orders_placed_total": 6, "queue_depth": 7,
		"http_server_duration_bucket{le=0.1}": 2, "http_server_duration_bucket{le=1}": 5, "http_server_duration_bucket{le=+Inf}": 6,
		"http_server_duration_sum": 12.5, "http_server_duration_count": 6,
	} {
		if v, ok := last[name]; !ok || v != want {
			t.Errorf("%s = %v (present %v), want %v", name, v, ok, want)
		}
	}
	if labels["service"] != "checkout" || labels["job"] != "checkout" || labels["deployment_environment"] != "prod" ||
		labels["http_route"] != "/pay" || labels["process_pid"] != "" || got[0].TS != 1_700_000_000_000 {
		t.Errorf("labels %v, ts %d", labels, got[0].TS)
	}

	// JSON and garbage are refused with a reason an SDK log will show.
	req2 := httptest.NewRequest("POST", "/v1/metrics", strings.NewReader("{}"))
	req2.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.Handler().ServeHTTP(w, req2)
	if w.Code != http.StatusUnsupportedMediaType || !strings.Contains(w.Body.String(), "http/protobuf") {
		t.Errorf("json: %d %s", w.Code, w.Body)
	}
	// Traces are accepted and dropped, so an SDK sending all signals stays quiet.
	if w := post(t, r.Handler(), "/v1/traces", &colmetrics.ExportMetricsServiceRequest{}, false); w.Code != 200 {
		t.Errorf("traces: %d", w.Code)
	}
}

func TestLogs(t *testing.T) {
	var got []model.LogEntry
	r := &Receiver{Logs: func(e []model.LogEntry) error { got = append(got, e...); return nil }, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	req := &collogs.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{Resource: resource, ScopeLogs: []*logspb.ScopeLogs{{
		Scope: &commonpb.InstrumentationScope{Name: "com.shop.Checkout"},
		LogRecords: []*logspb.LogRecord{
			{TimeUnixNano: 1_700_000_000_123_000_000, SeverityNumber: logspb.SeverityNumber_SEVERITY_NUMBER_ERROR, SeverityText: "ERROR",
				Body:       &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "payment declined"}},
				Attributes: []*commonpb.KeyValue{str("order.id", "42")},
				TraceId:    []byte{0x4b, 0xf9, 0x2f, 0x35, 0x77, 0xb3, 0x4d, 0xa6, 0xa3, 0xce, 0x92, 0x9d, 0x0e, 0x0e, 0x47, 0x36},
				SpanId:     []byte{0x00, 0xf0, 0x67, 0xaa, 0x0b, 0xa9, 0x02, 0xb7}},
			{ObservedTimeUnixNano: 1_700_000_001_000_000_000, SeverityNumber: logspb.SeverityNumber_SEVERITY_NUMBER_WARN,
				Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "retrying"}}},
			{Body: &commonpb.AnyValue{}}, // nothing to say: skipped
		}}}}}}
	if w := post(t, r.Handler(), "/v1/logs", req, false); w.Code != 200 || len(got) != 2 {
		t.Fatalf("status %d, %d entries", w.Code, len(got))
	}
	e := got[0]
	if e.Msg != "payment declined" || e.Time != 1_700_000_000_123_000_000 || e.Stream["service"] != "checkout" || e.Stream["source"] != "otlp" ||
		e.Fields["level"] != "error" || e.Fields["order_id"] != "42" || e.Fields["trace_id"] != "4bf92f3577b34da6a3ce929d0e0e4736" ||
		e.Fields["span_id"] != "00f067aa0ba902b7" || e.Fields["logger"] != "com.shop.Checkout" {
		t.Errorf("entry: %+v", e)
	}
	if got[1].Fields["level"] != "warn" || got[1].Time != 1_700_000_001_000_000_000 {
		t.Errorf("second entry: %+v", got[1])
	}
}

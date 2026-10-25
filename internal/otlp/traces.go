package otlp

import (
	"encoding/binary"
	"maps"
	"sort"
	"strconv"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/ijcodelabs/vigil/internal/model"
	"github.com/ijcodelabs/vigil/internal/otlpwire"
)

// Traces are handled in two ways at once.
//
// Every span is counted and timed here, on the host, into a few metrics
// per service and operation (request rate, errors, duration). Those are
// cheap, exact, and are what the Services pages and alert rules read.
//
// The spans themselves are passed on as they came (OTLP), for the server to
// hand to the trace store. Which spans are kept is decided per trace, from
// its id, so every service keeps or drops the same traces; a span that
// failed or was slow is kept regardless.

// TraceOptions say which spans are kept.
type TraceOptions struct {
	Sample     float64       // share of traces kept, 0..1
	SlowerThan time.Duration // spans at least this slow are always kept; 0: no such rule
	// MaxNames bounds the distinct operation names per service in the
	// metrics; names beyond it are counted as "(other)".
	MaxNames int
}

const otherName = "(other)"

var (
	spanKinds = map[tracepb.Span_SpanKind]string{
		tracepb.Span_SPAN_KIND_SERVER: "server", tracepb.Span_SPAN_KIND_CLIENT: "client",
		tracepb.Span_SPAN_KIND_PRODUCER: "producer", tracepb.Span_SPAN_KIND_CONSUMER: "consumer",
		tracepb.Span_SPAN_KIND_INTERNAL: "internal",
	}
	// durationBuckets are the histogram bounds in seconds.
	durationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}
	// peerAttributes name what a client span talks to, most specific first.
	peerAttributes = []string{"peer.service", "db.system.name", "db.system", "messaging.system", "rpc.service",
		"server.address", "net.peer.name", "http.host", "url.domain"}
)

// spanSeries is the running count and duration histogram of one kind of span.
type spanSeries struct {
	labels  map[string]string
	count   float64
	sum     float64
	buckets []float64
	seen    time.Time
}

type traceState struct {
	spans map[uint64]*spanSeries // by service, operation, kind, status
	calls map[uint64]*spanSeries // by service, peer, status
	names map[string]map[string]bool
	// Calls to language models, from spans that follow OpenTelemetry's
	// conventions for generative AI.
	genai  map[uint64]*spanSeries // by service, provider, model, operation, status
	tokens map[uint64]*spanSeries // by service, provider, model, type; count is the number of tokens
}

// tokenAttributes map a span attribute to the kind of token it counts.
// Instrumentation libraries have used several names over time.
var tokenAttributes = []struct{ attr, typ string }{
	{"gen_ai.usage.input_tokens", "input"}, {"gen_ai.usage.prompt_tokens", "input"}, {"llm.usage.prompt_tokens", "input"},
	{"gen_ai.usage.output_tokens", "output"}, {"gen_ai.usage.completion_tokens", "output"}, {"llm.usage.completion_tokens", "output"},
	{"gen_ai.usage.cache_read.input_tokens", "cache_read"}, {"gen_ai.usage.cache_read_input_tokens", "cache_read"}, {"gen_ai.usage.cached_tokens", "cache_read"},
	{"gen_ai.usage.cache_creation.input_tokens", "cache_write"}, {"gen_ai.usage.cache_creation_input_tokens", "cache_write"},
	{"gen_ai.usage.reasoning_tokens", "reasoning"}, {"gen_ai.usage.reasoning.output_tokens", "reasoning"},
}

// selfReporting are services that report their own model usage as
// metrics, more completely than their spans could: Claude Code sends
// claude_code_token_usage and claude_code_cost_usage, and its spans carry
// gen_ai.* names without gen_ai.usage.* tokens. Counting the spans too
// would show its calls twice, once without tokens.
var selfReporting = map[string]bool{"claude-code": true, "claude-code-desktop": true}

// redactSpan masks secrets in a span's attributes and in its events (a
// tool's output arrives as an event).
func redactSpan(sp *tracepb.Span, mask func(string) string) {
	redactKVs(sp.GetAttributes(), mask)
	for _, e := range sp.GetEvents() {
		redactKVs(e.GetAttributes(), mask)
	}
}

func redactKVs(kvs []*commonpb.KeyValue, mask func(string) string) {
	for _, kv := range kvs {
		redactValue(kv.GetValue(), mask)
	}
}

func redactValue(v *commonpb.AnyValue, mask func(string) string) {
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		x.StringValue = mask(x.StringValue)
	case *commonpb.AnyValue_ArrayValue:
		for _, e := range x.ArrayValue.GetValues() {
			redactValue(e, mask)
		}
	case *commonpb.AnyValue_KvlistValue:
		redactKVs(x.KvlistValue.GetValues(), mask)
	}
}

// observeGenAI counts a call to a language model: one request, and the
// tokens it used by kind. Called with r.mu held.
func (r *Receiver) observeGenAI(service, status string, seconds float64, attrs []*commonpb.KeyValue, now time.Time) {
	provider := attr(attrs, "gen_ai.provider.name")
	if provider == "" {
		provider = attr(attrs, "gen_ai.system")
	}
	model := attr(attrs, "gen_ai.response.model")
	if model == "" {
		model = attr(attrs, "gen_ai.request.model")
	}
	if provider == "" && model == "" {
		return
	}
	st := r.trace
	if len(st.tokens) > 5000 { // model names are few; this many means something writes ids into them
		return
	}
	provider, model = truncate(provider, 60), truncate(model, 120)
	op := attr(attrs, "gen_ai.operation.name")
	st.series(st.genai, now, "service", service, "provider", provider, "model", model, "operation", op, "status", status).add(seconds)
	seen := map[string]bool{}
	for _, t := range tokenAttributes {
		if seen[t.typ] {
			continue // the same count under an older name
		}
		if v, err := strconv.ParseFloat(attr(attrs, t.attr), 64); err == nil && v > 0 {
			seen[t.typ] = true
			st.series(st.tokens, now, "service", service, "provider", provider, "model", model, "type", t.typ).count += v
		}
	}
}

func attr(attrs []*commonpb.KeyValue, key string) string {
	for _, kv := range attrs {
		if kv.Key == key {
			return anyString(kv.Value)
		}
	}
	return ""
}

// keepTrace decides from the trace id alone, so every service that sees a
// part of the trace decides the same.
func keepTrace(id []byte, share float64) bool {
	if share >= 1 {
		return true
	}
	if share <= 0 || len(id) < 8 {
		return false
	}
	return float64(binary.BigEndian.Uint64(id[len(id)-8:])>>11)/float64(uint64(1)<<53) < share
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// convertTraces counts every span and returns the request cut down to the
// spans that are kept, ready to be passed on; nil if none are.
func (r *Receiver) convertTraces(in *otlpwire.TraceRequest, now time.Time) *otlpwire.TraceRequest {
	opts := r.Traces
	out := &otlpwire.TraceRequest{}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.trace == nil {
		r.trace = newTraceState()
	}
	for _, rs := range in.GetResourceSpans() {
		service := resourceOf(rs.GetResource())["service"]
		if service == "" {
			service = "unknown"
		}
		keptScopes := make([]*tracepb.ScopeSpans, 0, len(rs.GetScopeSpans()))
		for _, ss := range rs.GetScopeSpans() {
			kept := make([]*tracepb.Span, 0, len(ss.GetSpans()))
			for _, sp := range ss.GetSpans() {
				start, end := sp.GetStartTimeUnixNano(), sp.GetEndTimeUnixNano()
				seconds := 0.0
				if end > start && start > 0 {
					seconds = float64(end-start) / 1e9
				}
				kind := spanKinds[sp.GetKind()]
				if kind == "" {
					kind = "internal"
				}
				status := "ok"
				if sp.GetStatus().GetCode() == tracepb.Status_STATUS_CODE_ERROR {
					status = "error"
				}
				r.observe(service, sp.GetName(), kind, status, seconds, sp.GetAttributes(), now, opts.MaxNames)
				if !selfReporting[service] {
					r.observeGenAI(service, status, seconds, sp.GetAttributes(), now)
				}

				slow := opts.SlowerThan > 0 && seconds >= opts.SlowerThan.Seconds()
				if keepTrace(sp.GetTraceId(), opts.Sample) || status == "error" || slow {
					if r.Redact != nil {
						redactSpan(sp, r.Redact)
					}
					kept = append(kept, sp)
				}
			}
			if len(kept) > 0 {
				keptScopes = append(keptScopes, &tracepb.ScopeSpans{Scope: ss.GetScope(), Spans: kept, SchemaUrl: ss.GetSchemaUrl()})
			}
		}
		if len(keptScopes) > 0 {
			out.ResourceSpans = append(out.ResourceSpans, &tracepb.ResourceSpans{Resource: rs.GetResource(), ScopeSpans: keptScopes, SchemaUrl: rs.GetSchemaUrl()})
		}
	}
	if len(out.ResourceSpans) == 0 {
		return nil
	}
	return out
}

func newTraceState() *traceState {
	return &traceState{spans: map[uint64]*spanSeries{}, calls: map[uint64]*spanSeries{}, names: map[string]map[string]bool{},
		genai: map[uint64]*spanSeries{}, tokens: map[uint64]*spanSeries{}}
}

func (st *traceState) series(m map[uint64]*spanSeries, now time.Time, kv ...string) *spanSeries {
	labels := make(map[string]string, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		labels[kv[i]] = kv[i+1]
	}
	key := seriesKey("", labels)
	s := m[key]
	if s == nil {
		s = &spanSeries{labels: labels, buckets: make([]float64, len(durationBuckets))}
		m[key] = s
	}
	s.seen = now
	return s
}

func (s *spanSeries) add(seconds float64) {
	s.count++
	s.sum += seconds
	for i, le := range durationBuckets {
		if seconds <= le {
			s.buckets[i]++
		}
	}
}

// observe counts one span. Called with r.mu held.
func (r *Receiver) observe(service, name, kind, status string, seconds float64, attrs []*commonpb.KeyValue, now time.Time, maxNames int) {
	st := r.trace
	if kind != "internal" {
		// Operation names are meant to be few ("GET /orders/{id}"); an
		// application that puts ids into them would otherwise make a series
		// per request.
		names := st.names[service]
		if names == nil {
			names = map[string]bool{}
			st.names[service] = names
		}
		if maxNames <= 0 {
			maxNames = 200
		}
		if !names[name] {
			if len(names) >= maxNames {
				name = otherName
			} else {
				names[name] = true
			}
		}
		st.series(st.spans, now, "service", service, "span_name", name, "kind", kind, "status", status).add(seconds)
	}
	if kind == "client" || kind == "producer" {
		peer := ""
		for _, key := range peerAttributes {
			if peer = attr(attrs, key); peer != "" {
				break
			}
		}
		if peer != "" {
			st.series(st.calls, now, "service", service, "peer", truncate(peer, 120), "status", status).add(seconds)
		}
	}
}

// traceMetrics returns the current totals as samples, and forgets series
// nothing has been counted on for an hour.
func (r *Receiver) traceMetrics(now time.Time) []model.Sample {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.trace == nil {
		return nil
	}
	ts := now.UnixMilli()
	var out []model.Sample
	emit := func(m map[uint64]*spanSeries, total, duration string, histogram bool) {
		keys := make([]uint64, 0, len(m))
		for k, s := range m {
			if now.Sub(s.seen) > time.Hour {
				delete(m, k)
				continue
			}
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
		for _, k := range keys {
			s := m[k]
			with := func(extra ...string) map[string]string {
				l := make(map[string]string, len(s.labels)+1)
				for k, v := range s.labels {
					l[k] = v
				}
				for i := 0; i+1 < len(extra); i += 2 {
					l[extra[i]] = extra[i+1]
				}
				return l
			}
			out = append(out, model.Sample{Name: total, Labels: with(), Value: s.count, TS: ts},
				model.Sample{Name: duration + "_sum", Labels: with(), Value: s.sum, TS: ts},
				model.Sample{Name: duration + "_count", Labels: with(), Value: s.count, TS: ts})
			if histogram {
				for i, le := range durationBuckets {
					out = append(out, model.Sample{Name: duration + "_bucket", Labels: with("le", strconv.FormatFloat(le, 'g', -1, 64)), Value: s.buckets[i], TS: ts})
				}
				out = append(out, model.Sample{Name: duration + "_bucket", Labels: with("le", "+Inf"), Value: s.count, TS: ts})
			}
		}
	}
	emit(r.trace.spans, "traces_spans_total", "traces_span_duration_seconds", true)
	emit(r.trace.calls, "traces_service_calls_total", "traces_service_call_duration_seconds", false)
	emit(r.trace.genai, "genai_requests_total", "genai_request_duration_seconds", false)
	// Token counters carry their count only.
	for k, t := range r.trace.tokens {
		if now.Sub(t.seen) > time.Hour {
			delete(r.trace.tokens, k)
			continue
		}
		out = append(out, model.Sample{Name: "genai_tokens_total", Labels: maps.Clone(t.labels), Value: t.count, TS: ts})
	}
	return out
}

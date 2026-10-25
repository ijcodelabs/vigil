package otlp

import (
	"bytes"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	coltrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/ijcodelabs/vigil/internal/model"
)

// kept is a span as it was passed on, with the service it belongs to.
type kept struct {
	service string
	span    *tracepb.Span
}

func TestTraces(t *testing.T) {
	var mu sync.Mutex
	var spans []kept
	var samples []model.Sample
	r := &Receiver{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Traces: TraceOptions{Sample: 1},
		Metrics: func(b []model.Sample) { mu.Lock(); samples = append(samples, b...); mu.Unlock() },
		Spans: func(payload []byte) error {
			req := &coltrace.ExportTraceServiceRequest{}
			if err := proto.Unmarshal(payload, req); err != nil {
				t.Errorf("what was passed on is not an OTLP request: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			for _, rs := range req.GetResourceSpans() {
				service := resourceOf(rs.GetResource())["service"]
				if len(rs.GetResource().GetAttributes()) < 3 {
					t.Errorf("the resource lost its attributes: %v", rs.GetResource())
				}
				for _, ss := range rs.GetScopeSpans() {
					for _, sp := range ss.GetSpans() {
						spans = append(spans, kept{service, sp})
					}
				}
			}
			return nil
		}}
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()

	send := func(n int) {
		t.Helper()
		for i := 0; i < n; i++ {
			for _, req := range DemoTrace(time.Now().Add(-5 * time.Second)) {
				body, _ := req.Marshal()
				resp, err := http.Post(srv.URL+"/v1/traces", "application/x-protobuf", bytes.NewReader(body))
				if err != nil || resp.StatusCode != 200 {
					t.Fatalf("post: %v %v", resp, err)
				}
				resp.Body.Close()
			}
		}
	}
	send(300)

	// With everything kept, every span is passed on as it came.
	byTrace := map[string][]kept{}
	services, failed := map[string]bool{}, 0
	for _, k := range spans {
		id := hex.EncodeToString(k.span.GetTraceId())
		byTrace[id] = append(byTrace[id], k)
		services[k.service] = true
		if k.span.GetStatus().GetCode() == tracepb.Status_STATUS_CODE_ERROR {
			failed++
		}
	}
	if len(byTrace) != 300 || !services["frontend"] || !services["catalog"] || !services["payments"] || !services["orders"] || failed == 0 {
		t.Fatalf("%d traces, services %v, %d failed spans", len(byTrace), services, failed)
	}
	for id, ss := range byTrace {
		ids, roots := map[string]bool{}, 0
		for _, k := range ss {
			ids[string(k.span.GetSpanId())] = true
		}
		for _, k := range ss {
			if p := k.span.GetParentSpanId(); len(p) == 0 {
				roots++
			} else if !ids[string(p)] {
				t.Fatalf("trace %s: span %q has a parent that is not in the trace", id, k.span.GetName())
			}
		}
		if roots != 1 {
			t.Fatalf("trace %s has %d roots", id, roots)
		}
	}

	// The metrics count every span, by service, operation, kind and outcome.
	get := func(all []model.Sample, name string, want map[string]string) (float64, bool) {
		for _, s := range all {
			if s.Name != name {
				continue
			}
			ok := true
			for k, v := range want {
				ok = ok && s.Labels[k] == v
			}
			if ok {
				return s.Value, true
			}
		}
		return 0, false
	}
	m := r.traceMetrics(time.Now())
	total := 0.0
	for _, s := range m {
		if s.Name == "traces_spans_total" && s.Labels["service"] == "frontend" && s.Labels["kind"] == "server" {
			total += s.Value
		}
		if s.Labels["kind"] == "internal" {
			t.Fatalf("internal spans should not make metric series: %+v", s)
		}
	}
	if total != 300 {
		t.Errorf("frontend server spans counted: %v, want 300", total)
	}
	if v, ok := get(m, "traces_spans_total", map[string]string{"service": "frontend", "span_name": "POST /checkout", "status": "error"}); !ok || v == 0 {
		t.Errorf("failed checkouts: %v %v", v, ok)
	}
	inf, _ := get(m, "traces_span_duration_seconds_bucket", map[string]string{"service": "catalog", "span_name": "GET /internal/items", "le": "+Inf"})
	fast, _ := get(m, "traces_span_duration_seconds_bucket", map[string]string{"service": "catalog", "span_name": "GET /internal/items", "le": "0.5"})
	if inf != 300 || fast >= inf || fast < 200 {
		t.Errorf("catalog duration histogram: %v of %v under half a second (some are slow on purpose)", fast, inf)
	}
	// Who calls whom, from the client spans.
	for _, edge := range [][2]string{{"frontend", "catalog"}, {"frontend", "payments"}, {"catalog", "postgresql"}, {"payments", "api.bank.example"}, {"payments", "kafka"}} {
		if v, ok := get(m, "traces_service_calls_total", map[string]string{"service": edge[0], "peer": edge[1]}); !ok || v == 0 {
			t.Errorf("no calls counted from %s to %s", edge[0], edge[1])
		}
	}

	// Calls to language models are counted with their tokens, by service and model.
	req, ok := get(m, "genai_requests_total", map[string]string{"service": "assistant", "provider": "anthropic", "operation": "chat"})
	in, ok2 := get(m, "genai_tokens_total", map[string]string{"service": "assistant", "provider": "anthropic", "type": "input"})
	outTok, _ := get(m, "genai_tokens_total", map[string]string{"service": "assistant", "provider": "anthropic", "type": "output"})
	if !ok || !ok2 || req == 0 || in < 400*req/3 || outTok < 60 {
		t.Errorf("model calls: %v requests, %v input and %v output tokens", req, in, outTok)
	}
	cached := 0.0
	for _, s := range m {
		if s.Name == "genai_tokens_total" && s.Labels["type"] == "cache_read" {
			cached += s.Value
		}
		if s.Name == "genai_tokens_total" && (s.Labels["model"] == "" || s.Labels["service"] != "assistant") {
			t.Errorf("token series with odd labels: %v", s.Labels)
		}
	}
	if cached == 0 {
		t.Error("tokens read from the cache were not counted")
	}
	if v, ok := get(m, "traces_service_calls_total", map[string]string{"service": "assistant", "peer": "api.anthropic.example"}); !ok || v == 0 {
		t.Error("the model provider should show as something the assistant depends on")
	}

	// Sampling keeps whole traces or none, and always what failed or was slow.
	spans = nil
	r.Traces = TraceOptions{Sample: 0.2, SlowerThan: 800 * time.Millisecond}
	send(400)
	count := map[string]int{}
	extra := 0
	for _, k := range spans {
		id := k.span.GetTraceId()
		count[hex.EncodeToString(id)]++
		if !keepTrace(id, 0.2) {
			extra++
			slow := time.Duration(k.span.GetEndTimeUnixNano()-k.span.GetStartTimeUnixNano()) >= 800*time.Millisecond
			if k.span.GetStatus().GetCode() != tracepb.Status_STATUS_CODE_ERROR && !slow {
				t.Fatalf("a span that is neither sampled, failed nor slow was kept: %q", k.span.GetName())
			}
		}
	}
	whole := 0
	for id, n := range count {
		raw, _ := hex.DecodeString(id)
		if keepTrace(raw, 0.2) {
			whole++
			if n < 4 {
				t.Errorf("sampled trace %s kept only %d spans", id, n)
			}
		}
	}
	if whole < 40 || whole > 130 {
		t.Errorf("kept %d of 400 traces at a share of 0.2", whole)
	}
	if extra == 0 {
		t.Error("failed and slow spans of unsampled traces should have been kept")
	}
	// The counters do not sample: they still saw everything.
	total = 0
	for _, s := range r.traceMetrics(time.Now()) {
		if s.Name == "traces_spans_total" && s.Labels["service"] == "frontend" && s.Labels["kind"] == "server" {
			total += s.Value
		}
	}
	if total != 700 {
		t.Errorf("after sampling the counters say %v, want 700", total)
	}

	// Operation names with ids in them do not become a series each.
	r2 := &Receiver{Traces: TraceOptions{Sample: 0, MaxNames: 5}, trace: nil}
	r2.mu.Lock()
	r2.trace = newTraceState()
	for i := 0; i < 50; i++ {
		r2.observe("api", "GET /user/"+string(rune('a'+i%26))+string(rune('a'+i/26)), "server", "ok", 0.01, nil, time.Now(), 5)
	}
	r2.mu.Unlock()
	names := map[string]bool{}
	for _, s := range r2.traceMetrics(time.Now()) {
		names[s.Labels["span_name"]] = true
	}
	if len(names) != 6 || !names[otherName] {
		t.Errorf("operation names: %v", names)
	}
}

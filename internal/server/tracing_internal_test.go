package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// A request's span names its route, not its path; continues the caller's
// trace; carries no address, agent or query; and agents' pushes make none.
func TestTraced(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	old := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	defer otel.SetTracerProvider(old)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/incidents/{id}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	mux.HandleFunc("POST /api/v1/write", func(http.ResponseWriter, *http.Request) {})
	mux.HandleFunc("GET /api/v1/query", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) })
	h := traced(mux)

	parent := "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	req := httptest.NewRequest("GET", "/api/v1/incidents/42", nil)
	req.Header.Set("traceparent", parent)
	req.Header.Set("User-Agent", "boop-chat/1.0")
	h.ServeHTTP(httptest.NewRecorder(), req)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/api/v1/write", nil))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/v1/query?query=up{owner=%22ada%22}", nil))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/static/app.js", nil))

	spans := rec.Ended()
	if len(spans) != 2 {
		t.Fatalf("%d spans, want 2 (no push, no file)", len(spans))
	}
	s := spans[0]
	if s.Name() != "GET /api/v1/incidents/{id}" || s.SpanContext().TraceID().String() != "0af7651916cd43dd8448eb211c80319c" ||
		s.Parent().SpanID().String() != "b7ad6b7169203331" {
		t.Errorf("first span: %q trace %s parent %s", s.Name(), s.SpanContext().TraceID(), s.Parent().SpanID())
	}
	want := map[string]bool{"http.request.method": true, "http.route": true, "http.response.status_code": true}
	for _, sp := range spans {
		for _, a := range sp.Attributes() {
			if !want[string(a.Key)] {
				t.Errorf("%s carries %s=%s", sp.Name(), a.Key, a.Value.Emit())
			}
		}
	}
	if spans[1].Status().Code.String() != "Error" {
		t.Errorf("a 502 is not an error: %v", spans[1].Status())
	}
}

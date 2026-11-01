package server

import (
	"net/http"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// traced makes a server span of each API request, a child of the caller's
// span when the request carries W3C trace context (a Boop service calling
// with a grant, say). The tracer is the global one: a no-op unless the
// command set one up from the OTEL_* variables (servercli).
//
// The span is written here rather than by otelhttp so that exactly these
// attributes go out: the method, the route pattern and the status. No
// address, user agent, raw path or query: a path holds ids and a query
// holds whatever someone asked, and a caller's address is a person's.
// Agents' pushes, health checks and the interface's files make no spans;
// they are many and say nothing about a request someone made.
func traced(next http.Handler) http.Handler {
	tracer := otel.Tracer("github.com/ijcodelabs/vigil/internal/server")
	prop := propagation.TraceContext{}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if !(strings.HasPrefix(p, "/api/") || strings.HasPrefix(p, "/alertmanager/") || strings.HasPrefix(p, "/select/")) ||
			(r.Method == http.MethodPost && agentPushes[p]) {
			next.ServeHTTP(w, r)
			return
		}
		ctx := prop.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		ctx, span := tracer.Start(ctx, r.Method, trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(attribute.String("http.request.method", r.Method)))
		defer span.End()
		sw := &statusWriter{ResponseWriter: w, code: http.StatusOK}
		r = r.WithContext(ctx)
		next.ServeHTTP(sw, r)
		// The mux has filled in the pattern that matched by now.
		if route := routeOf(r.Pattern); route != "" {
			span.SetName(r.Method + " " + route)
			span.SetAttributes(attribute.String("http.route", route))
		}
		span.SetAttributes(attribute.Int("http.response.status_code", sw.code))
		if sw.code >= 500 {
			span.SetStatus(codes.Error, http.StatusText(sw.code))
		}
	})
}

// agentPushes are the paths agents send to.
var agentPushes = map[string]bool{"/api/v1/write": true, "/api/v1/logs": true, "/api/v1/traces": true,
	"/api/v1/events": true, "/api/v1/captures": true, "/api/v1/agent/config": true}

// routeOf takes the method off a mux pattern: "GET /api/v1/x/{id}" is
// the route "/api/v1/x/{id}".
func routeOf(pattern string) string {
	if _, route, ok := strings.Cut(pattern, " "); ok {
		return route
	}
	return pattern
}

type statusWriter struct {
	http.ResponseWriter
	code    int
	written bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.written {
		w.code, w.written = code, true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	w.written = true
	return w.ResponseWriter.Write(b)
}

// Flush keeps streaming answers (the live log tail) streaming.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the connection beneath.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

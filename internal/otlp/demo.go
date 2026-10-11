package otlp

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	mrand "math/rand/v2"
	"net/http"
	"strings"
	"time"

	coltrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// Demo traces: a small shop made of four services, for trying the trace
// pages without instrumenting anything. `vigil tracegen` sends them.

func kvs(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

func num(k string, v int64) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: v}}}
}

type demoSpan struct {
	service string
	span    *tracepb.Span
}

func id(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

// DemoTrace builds one request through the shop, starting at start. About
// one in twelve fails in the payment service and one in fifteen is slow in
// the database.
func DemoTrace(start time.Time) []*coltrace.ExportTraceServiceRequest {
	trace := id(16)
	var spans []demoSpan
	ms := func(lo, hi int) time.Duration { return time.Duration(lo+mrand.IntN(hi-lo+1)) * time.Millisecond }
	add := func(service, name string, kind tracepb.Span_SpanKind, parent []byte, at time.Time, d time.Duration, attrs ...*commonpb.KeyValue) *tracepb.Span {
		sp := &tracepb.Span{TraceId: trace, SpanId: id(8), ParentSpanId: parent, Name: name, Kind: kind,
			StartTimeUnixNano: uint64(at.UnixNano()), EndTimeUnixNano: uint64(at.Add(d).UnixNano()), Attributes: attrs}
		spans = append(spans, demoSpan{service, sp})
		return sp
	}
	fail := func(sp *tracepb.Span, typ, msg string) {
		sp.Status = &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR, Message: msg}
		sp.Events = append(sp.Events, &tracepb.Span_Event{Name: "exception", TimeUnixNano: sp.EndTimeUnixNano, Attributes: []*commonpb.KeyValue{
			kvs("exception.type", typ), kvs("exception.message", msg),
			kvs("exception.stacktrace", typ+": "+msg+"\n    at PaymentClient.charge(PaymentClient.java:88)\n    at CheckoutService.pay(CheckoutService.java:41)")}})
	}
	routes := []struct{ method, route string }{{"GET", "/products"}, {"GET", "/products/{id}"}, {"POST", "/cart/items"}, {"POST", "/checkout"}}
	r := routes[mrand.IntN(len(routes))]
	slowDB, failing := mrand.IntN(15) == 0, r.route == "/checkout" && mrand.IntN(3) == 0

	t := start
	dbTime := ms(2, 12)
	if slowDB {
		dbTime = ms(900, 2400)
	}
	root := add("frontend", r.method+" "+r.route, tracepb.Span_SPAN_KIND_SERVER, nil, t, 0,
		kvs("http.request.method", r.method), kvs("http.route", r.route), kvs("url.path", strings.ReplaceAll(r.route, "{id}", fmt.Sprint(1000+mrand.IntN(50)))),
		kvs("client.address", fmt.Sprintf("203.0.113.%d", 1+mrand.IntN(200))), kvs("user_agent.original", "Mozilla/5.0 (demo)"))
	t = t.Add(ms(1, 3))
	callCat := add("frontend", "GET catalog", tracepb.Span_SPAN_KIND_CLIENT, root.SpanId, t, 0, kvs("peer.service", "catalog"), kvs("http.request.method", "GET"))
	cat := add("catalog", "GET /internal/items", tracepb.Span_SPAN_KIND_SERVER, callCat.SpanId, t.Add(ms(1, 2)), 0, kvs("http.route", "/internal/items"))
	db := add("catalog", "SELECT shop.items", tracepb.Span_SPAN_KIND_CLIENT, cat.SpanId, t.Add(ms(2, 4)), dbTime,
		kvs("db.system", "postgresql"), kvs("db.namespace", "shop"), kvs("db.query.text", "SELECT id, name, price FROM items WHERE category = $1"))
	cat.EndTimeUnixNano = db.EndTimeUnixNano + uint64(ms(1, 3))
	callCat.EndTimeUnixNano = cat.EndTimeUnixNano + uint64(ms(1, 2))
	t = time.Unix(0, int64(callCat.EndTimeUnixNano))
	add("frontend", "render", tracepb.Span_SPAN_KIND_INTERNAL, root.SpanId, t, ms(1, 6))
	t = t.Add(ms(2, 8))
	status := int64(200)
	if r.route == "/checkout" {
		callPay := add("frontend", "POST payments", tracepb.Span_SPAN_KIND_CLIENT, root.SpanId, t, 0, kvs("peer.service", "payments"), kvs("http.request.method", "POST"))
		pay := add("payments", "POST /charge", tracepb.Span_SPAN_KIND_SERVER, callPay.SpanId, t.Add(ms(1, 2)), 0, kvs("http.route", "/charge"))
		bank := add("payments", "POST api.bank.example", tracepb.Span_SPAN_KIND_CLIENT, pay.SpanId, t.Add(ms(2, 4)), ms(40, 180),
			kvs("server.address", "api.bank.example"), kvs("http.request.method", "POST"))
		queue := add("payments", "orders publish", tracepb.Span_SPAN_KIND_PRODUCER, pay.SpanId, time.Unix(0, int64(bank.EndTimeUnixNano)), ms(1, 4),
			kvs("messaging.system", "kafka"), kvs("messaging.destination.name", "orders"))
		pay.EndTimeUnixNano = queue.EndTimeUnixNano + uint64(ms(1, 2))
		if failing {
			bank.Attributes = append(bank.Attributes, num("http.response.status_code", 503))
			fail(bank, "BankUnavailable", "the bank answered 503")
			fail(pay, "PaymentFailed", "charge was not accepted: the bank answered 503")
			fail(callPay, "PaymentFailed", "payments answered 502")
			fail(root, "CheckoutFailed", "payment did not go through")
			status = 502
		} else {
			add("orders", "orders process", tracepb.Span_SPAN_KIND_CONSUMER, queue.SpanId, time.Unix(0, int64(queue.EndTimeUnixNano)).Add(ms(5, 60)), ms(4, 30),
				kvs("messaging.system", "kafka"), kvs("messaging.destination.name", "orders"))
		}
		callPay.EndTimeUnixNano = pay.EndTimeUnixNano + uint64(ms(1, 2))
		t = time.Unix(0, int64(callPay.EndTimeUnixNano))
	}
	// One request in four asks the shop's assistant, which calls a language model.
	if mrand.IntN(4) == 0 {
		models := []struct{ provider, model string }{{"anthropic", "claude-sonnet-demo"}, {"anthropic", "claude-haiku-demo"}, {"openai", "gpt-demo-mini"}}
		m := models[mrand.IntN(len(models))]
		callAI := add("frontend", "POST assistant", tracepb.Span_SPAN_KIND_CLIENT, root.SpanId, t, 0, kvs("peer.service", "assistant"))
		ai := add("assistant", "POST /ask", tracepb.Span_SPAN_KIND_SERVER, callAI.SpanId, t.Add(ms(1, 2)), 0, kvs("http.route", "/ask"))
		in, out := int64(400+mrand.IntN(3000)), int64(60+mrand.IntN(900))
		llm := add("assistant", "chat "+m.model, tracepb.Span_SPAN_KIND_CLIENT, ai.SpanId, t.Add(ms(2, 5)), ms(400, 2600),
			kvs("gen_ai.operation.name", "chat"), kvs("gen_ai.provider.name", m.provider), kvs("gen_ai.request.model", m.model),
			kvs("gen_ai.response.model", m.model), num("gen_ai.usage.input_tokens", in), num("gen_ai.usage.output_tokens", out),
			kvs("server.address", "api."+m.provider+".example"))
		if mrand.IntN(3) == 0 {
			llm.Attributes = append(llm.Attributes, num("gen_ai.usage.cache_read.input_tokens", in*3))
		}
		ai.EndTimeUnixNano = llm.EndTimeUnixNano + uint64(ms(1, 3))
		callAI.EndTimeUnixNano = ai.EndTimeUnixNano + uint64(ms(1, 2))
		t = time.Unix(0, int64(callAI.EndTimeUnixNano))
	}
	root.Attributes = append(root.Attributes, num("http.response.status_code", status))
	root.EndTimeUnixNano = uint64(t.Add(ms(1, 4)).UnixNano())

	// One request per service, as each service's own SDK would send it.
	by := map[string][]*tracepb.Span{}
	var order []string
	for _, s := range spans {
		if by[s.service] == nil {
			order = append(order, s.service)
		}
		by[s.service] = append(by[s.service], s.span)
	}
	var out []*coltrace.ExportTraceServiceRequest
	for _, service := range order {
		out = append(out, &coltrace.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{kvs("service.name", service), kvs("service.namespace", "shop"),
				kvs("deployment.environment", "demo"), kvs("telemetry.sdk.language", "demo")}},
			ScopeSpans: []*tracepb.ScopeSpans{{Scope: &commonpb.InstrumentationScope{Name: "vigil.tracegen"}, Spans: by[service]}},
		}}})
	}
	return out
}

// SendDemo sends demo traces to an OTLP endpoint at about rate a second
// until ctx ends, and returns how many traces went out.
func SendDemo(ctx context.Context, endpoint string, rate float64) (int, error) {
	if rate <= 0 {
		rate = 2
	}
	hc := &http.Client{Timeout: 10 * time.Second}
	t := time.NewTicker(time.Duration(float64(time.Second) / rate))
	defer t.Stop()
	sent := 0
	for {
		// Dated a little back, so that every span has ended when it is sent.
		for _, req := range DemoTrace(time.Now().Add(-4 * time.Second)) {
			body, _ := proto.Marshal(req)
			r, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(endpoint, "/")+"/v1/traces", bytes.NewReader(body))
			if err != nil {
				return sent, err
			}
			r.Header.Set("Content-Type", "application/x-protobuf")
			resp, err := hc.Do(r)
			if err != nil {
				if ctx.Err() != nil {
					return sent, nil
				}
				return sent, err
			}
			resp.Body.Close()
			if resp.StatusCode/100 != 2 {
				return sent, fmt.Errorf("the endpoint answered HTTP %d", resp.StatusCode)
			}
		}
		sent++
		select {
		case <-ctx.Done():
			return sent, nil
		case <-t.C:
		}
	}
}

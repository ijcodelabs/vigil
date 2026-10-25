package otlpwire

import (
	"bytes"
	"testing"

	collogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func res(service string) *resourcepb.Resource {
	return &resourcepb.Resource{Attributes: []*commonpb.KeyValue{{Key: "service.name",
		Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: service}}}}}
}

// The bytes are the generated types' bytes, both ways, for all three.
func TestSameBytesAsTheGeneratedTypes(t *testing.T) {
	spans := []*tracepb.ResourceSpans{
		{Resource: res("shop-api"), ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{{Name: "GET /cart", TraceId: bytes.Repeat([]byte{1}, 16), SpanId: bytes.Repeat([]byte{2}, 8)}}}}},
		{Resource: res("payments")},
	}
	logs := []*logspb.ResourceLogs{{Resource: res("shop-api"), ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{
		Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "hello"}}}}}}}}
	metrics := []*metricspb.ResourceMetrics{{Resource: res("shop-api"), ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: []*metricspb.Metric{{
		Name: "requests", Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{DataPoints: []*metricspb.NumberDataPoint{{Value: &metricspb.NumberDataPoint_AsInt{AsInt: 3}}}}}}}}}}}

	for _, c := range []struct {
		name      string
		generated proto.Message
		ours      interface {
			Marshal() ([]byte, error)
			Unmarshal([]byte) error
		}
		back func(b []byte) proto.Message
	}{
		{"trace", &coltrace.ExportTraceServiceRequest{ResourceSpans: spans}, &TraceRequest{ResourceSpans: spans},
			func(b []byte) proto.Message {
				var r TraceRequest
				r.Unmarshal(b)
				return &coltrace.ExportTraceServiceRequest{ResourceSpans: r.ResourceSpans}
			}},
		{"logs", &collogs.ExportLogsServiceRequest{ResourceLogs: logs}, &LogsRequest{ResourceLogs: logs},
			func(b []byte) proto.Message {
				var r LogsRequest
				r.Unmarshal(b)
				return &collogs.ExportLogsServiceRequest{ResourceLogs: r.ResourceLogs}
			}},
		{"metrics", &colmetrics.ExportMetricsServiceRequest{ResourceMetrics: metrics}, &MetricsRequest{ResourceMetrics: metrics},
			func(b []byte) proto.Message {
				var r MetricsRequest
				r.Unmarshal(b)
				return &colmetrics.ExportMetricsServiceRequest{ResourceMetrics: r.ResourceMetrics}
			}},
	} {
		want, _ := proto.Marshal(c.generated)
		got, err := c.ours.Marshal()
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s: our bytes differ from the generated type's (%v)", c.name, err)
		}
		if !proto.Equal(c.back(want), c.generated) {
			t.Errorf("%s: reading the generated type's bytes gave something else", c.name)
		}
	}
}

// A field a newer OTLP might add is skipped; broken input is an error.
func TestUnknownFieldsAndBrokenInput(t *testing.T) {
	b, _ := (&TraceRequest{ResourceSpans: []*tracepb.ResourceSpans{{Resource: res("a")}}}).Marshal()
	b = protowire.AppendTag(b, 7, protowire.VarintType)
	b = protowire.AppendVarint(b, 42)
	var r TraceRequest
	if err := r.Unmarshal(b); err != nil || len(r.ResourceSpans) != 1 {
		t.Errorf("with an unknown field: %v %d", err, len(r.ResourceSpans))
	}
	if err := r.Unmarshal([]byte{0x0a, 0x05, 0x01}); err == nil {
		t.Error("a truncated message was accepted")
	}
	var empty TraceRequest
	if err := empty.Unmarshal(nil); err != nil || empty.GetResourceSpans() != nil {
		t.Errorf("empty: %v", err)
	}
}

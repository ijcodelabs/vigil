// Package otlpwire reads and writes OTLP's three export requests without the
// packages generated for its gRPC services. Those carry a gRPC client and
// server, and with them grpc and grpc-gateway, into every binary that only
// wants the messages. On the wire each request is nothing but its list of
// resources in field 1, so these types give exactly the bytes the generated
// ones would (a test compares them).
package otlpwire

import (
	"fmt"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// MetricsRequest is ExportMetricsServiceRequest.
type MetricsRequest struct{ ResourceMetrics []*metricspb.ResourceMetrics }

// LogsRequest is ExportLogsServiceRequest.
type LogsRequest struct{ ResourceLogs []*logspb.ResourceLogs }

// TraceRequest is ExportTraceServiceRequest.
type TraceRequest struct{ ResourceSpans []*tracepb.ResourceSpans }

func (r *MetricsRequest) GetResourceMetrics() []*metricspb.ResourceMetrics {
	if r == nil {
		return nil
	}
	return r.ResourceMetrics
}

func (r *LogsRequest) GetResourceLogs() []*logspb.ResourceLogs {
	if r == nil {
		return nil
	}
	return r.ResourceLogs
}

func (r *TraceRequest) GetResourceSpans() []*tracepb.ResourceSpans {
	if r == nil {
		return nil
	}
	return r.ResourceSpans
}

func (r *MetricsRequest) Unmarshal(b []byte) (err error) {
	r.ResourceMetrics, err = unmarshal(b, func() *metricspb.ResourceMetrics { return &metricspb.ResourceMetrics{} })
	return err
}

func (r *LogsRequest) Unmarshal(b []byte) (err error) {
	r.ResourceLogs, err = unmarshal(b, func() *logspb.ResourceLogs { return &logspb.ResourceLogs{} })
	return err
}

func (r *TraceRequest) Unmarshal(b []byte) (err error) {
	r.ResourceSpans, err = unmarshal(b, func() *tracepb.ResourceSpans { return &tracepb.ResourceSpans{} })
	return err
}

func (r *MetricsRequest) Marshal() ([]byte, error) { return marshal(r.ResourceMetrics) }
func (r *LogsRequest) Marshal() ([]byte, error)    { return marshal(r.ResourceLogs) }
func (r *TraceRequest) Marshal() ([]byte, error)   { return marshal(r.ResourceSpans) }

// The resources are field 1 of every export request.
const resources = protowire.Number(1)

// unmarshal reads the repeated field 1; other fields, which a newer OTLP
// might add, are skipped as protobuf allows.
func unmarshal[T proto.Message](b []byte, fresh func() T) ([]T, error) {
	var out []T
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return nil, fmt.Errorf("bad field tag: %w", protowire.ParseError(n))
		}
		b = b[n:]
		if num != resources || typ != protowire.BytesType {
			if n = protowire.ConsumeFieldValue(num, typ, b); n < 0 {
				return nil, fmt.Errorf("bad field %d: %w", num, protowire.ParseError(n))
			}
			b = b[n:]
			continue
		}
		v, n := protowire.ConsumeBytes(b)
		if n < 0 {
			return nil, fmt.Errorf("bad resource: %w", protowire.ParseError(n))
		}
		b = b[n:]
		m := fresh()
		if err := proto.Unmarshal(v, m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

func marshal[T proto.Message](items []T) ([]byte, error) {
	var b []byte
	for _, m := range items {
		v, err := proto.Marshal(m)
		if err != nil {
			return nil, err
		}
		b = protowire.AppendTag(b, resources, protowire.BytesType)
		b = protowire.AppendBytes(b, v)
	}
	return b, nil
}

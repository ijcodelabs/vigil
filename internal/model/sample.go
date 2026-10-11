// Package model holds the types shared by every signal pipeline.
package model

// Sample is one metric data point. The metric name lives in Name, not in Labels.
type Sample struct {
	Name   string
	Labels map[string]string
	Value  float64
	TS     int64 // unix milliseconds
}

// LogEntry is one log record on the agent→server wire. Stream labels
// identify where it came from; Fields are whatever a template extracted.
type LogEntry struct {
	Time   int64             `json:"t"` // unix nanoseconds
	Msg    string            `json:"m"`
	Stream map[string]string `json:"s"`
	Fields map[string]string `json:"f,omitempty"`
}

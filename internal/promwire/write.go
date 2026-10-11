// Package promwire encodes and decodes the Prometheus remote-write v1 body
// (snappy-compressed protobuf) without pulling in the Prometheus module.
//
//	WriteRequest { repeated TimeSeries timeseries = 1; }
//	TimeSeries   { repeated Label labels = 1; repeated Sample samples = 2; }
//	Label        { string name = 1; string value = 2; }
//	Sample       { double value = 1; int64 timestamp = 2; }
package promwire

import (
	"errors"
	"math"
	"sort"

	"github.com/golang/snappy"
	"google.golang.org/protobuf/encoding/protowire"

	"github.com/ijcodelabs/vigil/internal/model"
)

const nameLabel = "__name__"

// Encode returns the snappy-compressed WriteRequest for samples.
func Encode(samples []model.Sample) []byte {
	var req, ts, msg []byte
	for _, s := range samples {
		ts = ts[:0]
		names := make([]string, 0, len(s.Labels)+1)
		for k := range s.Labels {
			if k != nameLabel {
				names = append(names, k)
			}
		}
		names = append(names, nameLabel)
		sort.Strings(names) // remote write requires sorted label names
		for _, k := range names {
			v := s.Labels[k]
			if k == nameLabel {
				v = s.Name
			}
			msg = protowire.AppendString(protowire.AppendTag(msg[:0], 1, protowire.BytesType), k)
			msg = protowire.AppendString(protowire.AppendTag(msg, 2, protowire.BytesType), v)
			ts = protowire.AppendBytes(protowire.AppendTag(ts, 1, protowire.BytesType), msg)
		}
		msg = protowire.AppendFixed64(protowire.AppendTag(msg[:0], 1, protowire.Fixed64Type), math.Float64bits(s.Value))
		msg = protowire.AppendVarint(protowire.AppendTag(msg, 2, protowire.VarintType), uint64(s.TS))
		ts = protowire.AppendBytes(protowire.AppendTag(ts, 2, protowire.BytesType), msg)
		req = protowire.AppendBytes(protowire.AppendTag(req, 1, protowire.BytesType), ts)
	}
	return snappy.Encode(nil, req)
}

var errMalformed = errors.New("promwire: malformed write request")

// fields calls fn for every length-delimited, fixed64 or varint field in b.
func fields(b []byte, fn func(num protowire.Number, typ protowire.Type, raw []byte, v uint64) error) error {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return errMalformed
		}
		b = b[n:]
		var raw []byte
		var v uint64
		switch typ {
		case protowire.BytesType:
			raw, n = protowire.ConsumeBytes(b)
		case protowire.Fixed64Type:
			v, n = protowire.ConsumeFixed64(b)
		case protowire.VarintType:
			v, n = protowire.ConsumeVarint(b)
		default:
			n = protowire.ConsumeFieldValue(num, typ, b)
		}
		if n < 0 {
			return errMalformed
		}
		b = b[n:]
		if err := fn(num, typ, raw, v); err != nil {
			return err
		}
	}
	return nil
}

// CountSamples says how many samples a write request carries, without
// building them: the server counts what every agent sends.
func CountSamples(body []byte) (int, error) {
	req, err := snappy.Decode(nil, body)
	if err != nil {
		return 0, err
	}
	n := 0
	err = fields(req, func(num protowire.Number, typ protowire.Type, ts []byte, _ uint64) error {
		if num != 1 || typ != protowire.BytesType {
			return nil
		}
		return fields(ts, func(num protowire.Number, typ protowire.Type, _ []byte, _ uint64) error {
			if num == 2 && typ == protowire.BytesType {
				n++
			}
			return nil
		})
	})
	return n, err
}

// Decode is the inverse of Encode.
func Decode(body []byte) ([]model.Sample, error) {
	req, err := snappy.Decode(nil, body)
	if err != nil {
		return nil, err
	}
	var out []model.Sample
	err = fields(req, func(num protowire.Number, _ protowire.Type, ts []byte, _ uint64) error {
		if num != 1 {
			return nil
		}
		labels := map[string]string{}
		var name string
		var points []model.Sample
		err := fields(ts, func(num protowire.Number, _ protowire.Type, raw []byte, _ uint64) error {
			switch num {
			case 1:
				var k, v string
				if err := fields(raw, func(n protowire.Number, _ protowire.Type, r []byte, _ uint64) error {
					if n == 1 {
						k = string(r)
					} else if n == 2 {
						v = string(r)
					}
					return nil
				}); err != nil {
					return err
				}
				if k == nameLabel {
					name = v
				} else {
					labels[k] = v
				}
			case 2:
				var p model.Sample
				if err := fields(raw, func(n protowire.Number, _ protowire.Type, _ []byte, v uint64) error {
					if n == 1 {
						p.Value = math.Float64frombits(v)
					} else if n == 2 {
						p.TS = int64(v)
					}
					return nil
				}); err != nil {
					return err
				}
				points = append(points, p)
			}
			return nil
		})
		if err != nil {
			return err
		}
		for _, p := range points {
			p.Name, p.Labels = name, labels
			out = append(out, p)
		}
		return nil
	})
	return out, err
}

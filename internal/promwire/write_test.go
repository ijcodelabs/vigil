package promwire

import (
	"math"
	"reflect"
	"testing"

	"github.com/ijcodelabs/vigil/internal/model"
)

func TestRoundTrip(t *testing.T) {
	in := []model.Sample{
		{Name: "up", Labels: map[string]string{"job": "node", "instance": "a:9100"}, Value: 1, TS: 1700000000123},
		{Name: "temp", Labels: map[string]string{}, Value: -3.5, TS: 1},
		{Name: "inf", Labels: map[string]string{"le": "+Inf"}, Value: math.Inf(1), TS: 2},
	}
	out, err := Decode(Encode(in))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("got %+v\nwant %+v", out, in)
	}
}

func TestDecodeGarbage(t *testing.T) {
	if _, err := Decode([]byte("not snappy")); err == nil {
		t.Fatal("expected error")
	}
}

package project

import (
	"bytes"
	"encoding/json"
	"math"
	"math/rand/v2"
	"reflect"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// TestNormalizeMatchesJSON: converting plain JSON directly gives what a JSON round trip gives.
func TestNormalizeMatchesJSON(t *testing.T) {
	values := []any{
		nil, true, "", "a <&>", 0.0, math.Copysign(0, -1), 1.0, -2.5, 0.1, 1e-6, 1e-7, 123456789e-15,
		1e20, 1e21, 1.5e300, -1e-300, 9007199254740993.0, math.MaxFloat64, math.SmallestNonzeroFloat64,
		map[string]any(nil), []any(nil), map[string]any{}, []any{},
		map[string]any{"a": []any{1.0, "x", nil, map[string]any{"b": false}}, "c": map[string]any(nil)},
	}
	rnd := rand.New(rand.NewPCG(1, 2))
	for range 10000 {
		values = append(values, math.Float64frombits(rnd.Uint64()&^(0x7ff<<52)|uint64(rnd.IntN(0x7ff))<<52))
	}
	for _, v := range values {
		got, ok := plainDoc(v)
		if !ok {
			t.Fatalf("%#v: not plain", v)
		}
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		want, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%#v: got %#v, want %#v", v, got, want)
		}
	}
	for _, v := range []any{math.NaN(), math.Inf(1), "\xff", map[string]any{"\xff": 1.0}, 1, []string{"a"}, json.Number("1.0")} {
		if _, ok := plainDoc(v); ok {
			t.Errorf("%#v: want the JSON round trip", v)
		}
	}
}

package sidmesh

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"testing"
)

func TestIdempotencyKeyMatchesSpecFormula(t *testing.T) {
	// sha256 of: acme \n vendor.place_order@1 \n "o-1" \n "A&B<>" \n 3 \n 1.5 (computed with shasum).
	got, err := IdempotencyKey("acme", "vendor.place_order", 1, []any{"o-1", "A&B<>", 3, 1.5})
	if err != nil {
		t.Fatal(err)
	}
	const want = "3802755f35c5d2d4e8182bf14df08f175d2697aebd2be099569af3f3d3fab736"
	if got != want {
		t.Fatalf("key = %s, want %s", got, want)
	}
}

// TestIdempotencyLineSeparatorsAreRaw: JCS writes U+2028/U+2029 as raw characters, while
// encoding/json escapes them. A literal backslash-u text must stay escaped.
func TestIdempotencyLineSeparatorsAreRaw(t *testing.T) {
	got, err := IdempotencyKey("acme", "vendor.place_order", 1, []any{"a\u2028b\u2029c", `x\u2028y`})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("acme\nvendor.place_order@1\n\"a\u2028b\u2029c\"\n\"x\\\\u2028y\""))
	if want := hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("key = %s, want %s", got, want)
	}
}

// TestIdempotencyConformanceVectors runs the vectors shared with the TS runtime.
func TestIdempotencyConformanceVectors(t *testing.T) {
	raw, err := os.ReadFile("../spec/conformance/idempotency.json")
	if errors.Is(err, fs.ErrNotExist) {
		t.Skip("spec/conformance/idempotency.json not present")
	}
	if err != nil {
		t.Fatal(err)
	}
	var vectors []struct {
		Tenant     string `json:"tenant"`
		Capability string `json:"capability"`
		Version    int    `json:"version"`
		Values     []any  `json:"values"`
		Key        string `json:"key"`
	}
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatalf("idempotency.json: %v", err)
	}
	if len(vectors) == 0 {
		t.Fatal("idempotency.json has no vectors")
	}
	for i, v := range vectors {
		got, err := IdempotencyKey(v.Tenant, v.Capability, v.Version, v.Values)
		if err != nil {
			t.Fatalf("vector %d: %v", i, err)
		}
		if got != v.Key {
			t.Errorf("vector %d (%s %s@%d %v): key %s, want %s", i, v.Tenant, v.Capability, v.Version, v.Values, got, v.Key)
		}
	}
}

// TestDocumentHashVectors runs spec/conformance/vectors/document-hash.json.
func TestDocumentHashVectors(t *testing.T) {
	raw, err := os.ReadFile("../spec/conformance/vectors/document-hash.json")
	if errors.Is(err, fs.ErrNotExist) {
		t.Skip("document-hash.json not present")
	}
	if err != nil {
		t.Fatal(err)
	}
	var vectors []struct{ Note, Text, JCS, Hash string }
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatal(err)
	}
	if len(vectors) == 0 {
		t.Fatal("no vectors")
	}
	for _, v := range vectors {
		var doc any
		if err := json.Unmarshal([]byte(v.Text), &doc); err != nil {
			t.Fatalf("%s: %v", v.Note, err)
		}
		enc, err := JCS(doc)
		if err != nil {
			t.Fatalf("%s: %v", v.Note, err)
		}
		if string(enc) != v.JCS {
			t.Errorf("%s: JCS\n got %s\nwant %s", v.Note, enc, v.JCS)
		}
		if h, err := DocHash([]byte(v.Text)); err != nil || h != v.Hash {
			t.Errorf("%s: hash %s, want %s (%v)", v.Note, h, v.Hash, err)
		}
	}
}

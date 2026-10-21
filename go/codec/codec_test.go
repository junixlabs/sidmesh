package codec

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"testing"

	commonpb "go.temporal.io/api/common/v1"
	"go.temporal.io/sdk/converter"
	"google.golang.org/protobuf/proto"
)

type wirePayload struct {
	Metadata map[string]string `json:"metadata"`
	Data     string            `json:"data"`
}

func (w wirePayload) payload(t *testing.T) *commonpb.Payload {
	t.Helper()
	p := &commonpb.Payload{Metadata: map[string][]byte{}}
	for k, v := range w.Metadata {
		p.Metadata[k] = b64(t, v)
	}
	if d := b64(t, w.Data); len(d) > 0 {
		p.Data = d
	}
	return p
}

func b64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestConformanceVectors runs spec/conformance/codec.json, shared with the TS runtime.
func TestConformanceVectors(t *testing.T) {
	raw, err := os.ReadFile("../../spec/conformance/codec.json")
	if errors.Is(err, fs.ErrNotExist) {
		t.Skip("spec/conformance/codec.json not present")
	}
	if err != nil {
		t.Fatal(err)
	}
	var vectors []struct {
		Note      string       `json:"note"`
		Keys      string       `json:"keys"`
		Nonce     string       `json:"nonce"`
		Payload   *wirePayload `json:"payload"`
		Protobuf  string       `json:"protobuf"`
		Encrypted wirePayload  `json:"encrypted"`
		Expect    string       `json:"expect"`
	}
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatal(err)
	}
	for _, v := range vectors {
		t.Run(v.Note, func(t *testing.T) {
			keys, err := ParseKeys(v.Keys)
			if err != nil {
				t.Fatal(err)
			}
			c, err := New(keys)
			if err != nil {
				t.Fatal(err)
			}
			got, err := c.Decode([]*commonpb.Payload{v.Encrypted.payload(t)})
			if v.Expect == "error" {
				if err == nil {
					t.Fatal("decode succeeded, want an error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := v.Payload.payload(t)
			if !proto.Equal(got[0], want) {
				t.Fatalf("decoded %v, want %v", got[0], want)
			}
			if v.Nonce == "" || v.Protobuf == "" {
				return
			}
			c.nonce = bytes.NewReader(b64(t, v.Nonce))
			enc, err := c.Encode([]*commonpb.Payload{want})
			if err != nil {
				t.Fatal(err)
			}
			if !proto.Equal(enc[0], v.Encrypted.payload(t)) {
				t.Fatalf("encoded %x, want %x", enc[0].Data, b64(t, v.Encrypted.Data))
			}
		})
	}
}

func TestRotationAndPassThrough(t *testing.T) {
	old, _ := ParseKeys("k1:" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32)))
	both, _ := ParseKeys("k2:" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32)) +
		",k1:" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32)))
	c1, _ := New(old)
	c2, _ := New(both)
	dc := converter.GetDefaultDataConverter()
	p, err := dc.ToPayload(map[string]any{"order_ref": "2001"})
	if err != nil {
		t.Fatal(err)
	}
	enc, err := c1.Encode([]*commonpb.Payload{p})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(enc[0].Data, []byte("2001")) {
		t.Fatal("ciphertext contains plaintext")
	}
	dec, err := c2.Decode(enc) // the new key set still reads payloads written with k1
	if err != nil || !proto.Equal(dec[0], p) {
		t.Fatalf("rotation decode: %v", err)
	}
	enc2, _ := c2.Encode([]*commonpb.Payload{p})
	if string(enc2[0].Metadata[MetaKeyID]) != "k2" {
		t.Fatal("the first key must encrypt")
	}
	if _, err := c1.Decode(enc2); err == nil {
		t.Fatal("decoding with a key set that lacks k2 must fail")
	}
	plain, err := c1.Decode([]*commonpb.Payload{p})
	if err != nil || plain[0] != p {
		t.Fatal("unencrypted payloads pass through")
	}
}

func TestParseKeysRejectsBadInput(t *testing.T) {
	for _, spec := range []string{"", "nokey", "k1:!!", "k1:" + base64.StdEncoding.EncodeToString([]byte("short"))} {
		if _, err := ParseKeys(spec); err == nil {
			t.Errorf("ParseKeys(%q) succeeded", spec)
		}
	}
}

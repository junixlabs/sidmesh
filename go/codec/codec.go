// Package codec is the payload codec of SPEC §7: AES-256-GCM over the serialised Temporal
// Payload, keys from SIDMESH_CODEC_KEYS.
package codec

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	commonpb "go.temporal.io/api/common/v1"
	"go.temporal.io/sdk/converter"
	"google.golang.org/protobuf/proto"
)

const (
	// EnvKeys names the environment variable holding "<id>:<base64 32 bytes>,...".
	EnvKeys = "SIDMESH_CODEC_KEYS"
	// Encoding marks an encrypted payload.
	Encoding = "binary/encrypted"
	// MetaKeyID names the key a payload was encrypted with.
	MetaKeyID = "encryption-key-id"
)

// Key is one AES-256 key and its id.
type Key struct {
	ID  string
	Raw []byte
}

// Codec encrypts with the first key and decrypts with any key it holds.
type Codec struct {
	keys  []Key
	aeads map[string]cipher.AEAD
	nonce io.Reader // crypto/rand; tests may fix it
}

var _ converter.PayloadCodec = (*Codec)(nil)

// ParseKeys reads the SIDMESH_CODEC_KEYS format.
func ParseKeys(spec string) ([]Key, error) {
	var keys []Key
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, b64, ok := strings.Cut(part, ":")
		if !ok || id == "" {
			return nil, fmt.Errorf("%s: entry %q is not <id>:<base64 key>", EnvKeys, part)
		}
		raw, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return nil, fmt.Errorf("%s: key %q: %w", EnvKeys, id, err)
		}
		if len(raw) != 32 {
			return nil, fmt.Errorf("%s: key %q is %d bytes, want 32", EnvKeys, id, len(raw))
		}
		keys = append(keys, Key{ID: id, Raw: raw})
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("%s has no keys", EnvKeys)
	}
	return keys, nil
}

// New builds a codec from keys; the first one encrypts.
func New(keys []Key) (*Codec, error) {
	c := &Codec{keys: keys, aeads: map[string]cipher.AEAD{}, nonce: rand.Reader}
	for _, k := range keys {
		block, err := aes.NewCipher(k.Raw)
		if err != nil {
			return nil, err
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, err
		}
		c.aeads[k.ID] = aead
	}
	return c, nil
}

// FromEnv returns the codec configured by SIDMESH_CODEC_KEYS, or nil when it is unset.
func FromEnv() (*Codec, error) {
	spec := os.Getenv(EnvKeys)
	if strings.TrimSpace(spec) == "" {
		return nil, nil
	}
	keys, err := ParseKeys(spec)
	if err != nil {
		return nil, err
	}
	return New(keys)
}

// DataConverter returns the default converter, wrapped with the codec when
// SIDMESH_CODEC_KEYS is set. Use it for every client and worker of a deployment.
func DataConverter() (converter.DataConverter, error) {
	c, err := FromEnv()
	if err != nil || c == nil {
		return converter.GetDefaultDataConverter(), err
	}
	return converter.NewCodecDataConverter(converter.GetDefaultDataConverter(), c), nil
}

// Encode encrypts each payload with the first key.
func (c *Codec) Encode(payloads []*commonpb.Payload) ([]*commonpb.Payload, error) {
	key := c.keys[0]
	aead := c.aeads[key.ID]
	out := make([]*commonpb.Payload, len(payloads))
	for i, p := range payloads {
		plain, err := proto.MarshalOptions{Deterministic: true}.Marshal(p) // sorted metadata keys
		if err != nil {
			return nil, err
		}
		nonce := make([]byte, aead.NonceSize())
		if _, err := io.ReadFull(c.nonce, nonce); err != nil {
			return nil, err
		}
		out[i] = &commonpb.Payload{
			Metadata: map[string][]byte{
				converter.MetadataEncoding: []byte(Encoding),
				MetaKeyID:                  []byte(key.ID),
			},
			Data: aead.Seal(nonce, nonce, plain, nil), // nonce || ciphertext || tag
		}
	}
	return out, nil
}

// Decode restores encrypted payloads; others pass through unchanged.
func (c *Codec) Decode(payloads []*commonpb.Payload) ([]*commonpb.Payload, error) {
	out := make([]*commonpb.Payload, len(payloads))
	for i, p := range payloads {
		if string(p.GetMetadata()[converter.MetadataEncoding]) != Encoding {
			out[i] = p
			continue
		}
		id := string(p.GetMetadata()[MetaKeyID])
		aead, ok := c.aeads[id]
		if !ok {
			return nil, fmt.Errorf("payload encrypted with unknown key %q", id)
		}
		data := p.GetData()
		if len(data) < aead.NonceSize()+aead.Overhead() {
			return nil, errors.New("encrypted payload is too short")
		}
		plain, err := aead.Open(nil, data[:aead.NonceSize()], data[aead.NonceSize():], nil)
		if err != nil {
			return nil, fmt.Errorf("decrypt payload with key %q: %w", id, err)
		}
		var orig commonpb.Payload
		if err := proto.Unmarshal(plain, &orig); err != nil {
			return nil, err
		}
		out[i] = &orig
	}
	return out, nil
}

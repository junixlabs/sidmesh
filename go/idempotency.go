package sidmesh

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// IdempotencyKey computes the key of SPEC §4: lowercase hex SHA-256 of
// "<tenant>\n<capability>@<version>\n<value1>\n<value2>..." with each value in JCS form.
func IdempotencyKey(tenant, capability string, version int, values []any) (string, error) {
	var b strings.Builder
	b.WriteString(tenant)
	b.WriteString("\n")
	b.WriteString(Key(capability, version))
	for _, v := range values {
		enc, err := JCS(v)
		if err != nil {
			return "", err
		}
		b.WriteString("\n")
		b.Write(enc)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:]), nil
}

// DocHash is a document's pin (SPEC §5 "Version pinning"): "sha256:" and the hex SHA-256 of
// the document's JCS form.
func DocHash(raw []byte) (string, error) {
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", err
	}
	enc, err := JCS(doc)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(enc)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

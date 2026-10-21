package project

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"sync"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// The spec's meta-schemas, copied from ../../spec by go generate. TestMetaSchemasMatchSpec
// fails if the copies drift.
//
//go:generate sh -c "cp ../../spec/*.schema.json schemas/"
//go:embed schemas/*.schema.json
var metaFS embed.FS

var (
	metaOnce    sync.Once
	metaSchemas map[string]*jsonschema.Schema
	metaErr     error
)

// metaSchema returns the compiled spec schema "node", "contract" or "definition".
func metaSchema(name string) (*jsonschema.Schema, error) {
	metaOnce.Do(func() {
		c := jsonschema.NewCompiler()
		c.DefaultDraft(jsonschema.Draft2020)
		ids := map[string]string{}
		for _, n := range []string{"node", "contract", "definition"} {
			raw, err := metaFS.ReadFile("schemas/" + n + ".schema.json")
			if err != nil {
				metaErr = err
				return
			}
			doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
			if err != nil {
				metaErr = fmt.Errorf("%s.schema.json: %w", n, err)
				return
			}
			// Each schema is registered under its own $id, so relative references between
			// them resolve whatever spec version the copies carry.
			id, _ := doc.(map[string]any)["$id"].(string)
			if id == "" {
				metaErr = fmt.Errorf("%s.schema.json has no $id", n)
				return
			}
			if err := c.AddResource(id, doc); err != nil {
				metaErr = err
				return
			}
			ids[n] = id
		}
		metaSchemas = map[string]*jsonschema.Schema{}
		for _, n := range []string{"node", "contract", "definition"} {
			s, err := c.Compile(ids[n])
			if err != nil {
				metaErr = err
				return
			}
			metaSchemas[n] = s
		}
	})
	if metaErr != nil {
		return nil, metaErr
	}
	return metaSchemas[name], nil
}

// Compile compiles a payload schema taken from a contract or definition. name only labels
// error messages (the schema's URL is mem:///<name>). It does not cache: callers keep what
// they compile.
func Compile(name string, schema map[string]any) (*jsonschema.Schema, error) {
	raw, err := json.Marshal(schema)
	if err != nil {
		return nil, err
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.AssertFormat() // SPEC §2: format is asserted on payloads
	url := "mem:///" + name
	if err := c.AddResource(url, doc); err != nil {
		return nil, err
	}
	return c.Compile(url)
}

// Validate checks v against s. v may be any JSON-shaped Go value.
func Validate(s *jsonschema.Schema, v any) error {
	doc, err := normalize(v)
	if err != nil {
		return err
	}
	return s.Validate(doc)
}

// normalize re-reads v the way the schema library expects (numbers as json.Number), as
// jsonschema.UnmarshalJSON(json.Marshal(v)) would. Plain decoded JSON (maps, slices, finite
// float64s, UTF-8 strings, bools, nil) is converted directly; anything else goes through JSON.
func normalize(v any) (any, error) {
	if doc, ok := plainDoc(v); ok {
		return doc, nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return jsonschema.UnmarshalJSON(bytes.NewReader(raw))
}

// plainDoc converts a plain JSON value to the schema library's form; ok is false when v holds
// anything json.Marshal could render differently.
func plainDoc(v any) (any, bool) {
	switch x := v.(type) {
	case nil, bool:
		return x, true
	case string:
		return x, utf8.ValidString(x)
	case float64:
		if math.IsInf(x, 0) || math.IsNaN(x) {
			return nil, false
		}
		return json.Number(jsonFloat(x)), true
	case map[string]any:
		if x == nil {
			return nil, true // json.Marshal writes null
		}
		out := make(map[string]any, len(x))
		for k, e := range x {
			d, ok := plainDoc(e)
			if !ok || !utf8.ValidString(k) {
				return nil, false
			}
			out[k] = d
		}
		return out, true
	case []any:
		if x == nil {
			return nil, true
		}
		out := make([]any, len(x))
		for i, e := range x {
			d, ok := plainDoc(e)
			if !ok {
				return nil, false
			}
			out[i] = d
		}
		return out, true
	}
	return nil, false
}

// jsonFloat formats f exactly as encoding/json does (ES6 number to string).
func jsonFloat(f float64) string {
	format := byte('f')
	if abs := math.Abs(f); abs != 0 && (abs < 1e-6 || abs >= 1e21) {
		format = 'e'
	}
	b := strconv.AppendFloat(nil, f, format, -1, 64)
	if n := len(b); format == 'e' && n >= 4 && b[n-4] == 'e' && b[n-3] == '-' && b[n-2] == '0' {
		b[n-2] = b[n-1] // e-09 to e-9
		b = b[:n-1]
	}
	return string(b)
}

package project

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadTestProject(t *testing.T) {
	p, err := Load("../testdata/project")
	if err != nil {
		t.Fatal(err)
	}
	if got := len(p.Nodes); got != 3 {
		t.Errorf("nodes = %d, want 3", got)
	}
	if got := len(p.Contracts); got != 6 {
		t.Errorf("contracts = %d, want 6", got)
	}
	if got := len(p.Definitions); got != 7 {
		t.Errorf("definitions = %d, want 7", got)
	}
	if q := p.Manifest.Queues; q.Workflow != "test-workflows" || q.Go != "test-go" || q.TS != "test-ts" {
		t.Errorf("default queues = %+v", q)
	}
	c := p.Contracts["vendor.place_order@1"]
	if c == nil || c.Runtime != "go" || c.Node != "vendor" || c.Lookup.Capability != "vendor.find_order" {
		t.Errorf("place_order contract = %+v", c)
	}
	gate := p.Definitions["item@1"].Steps["suggest"].Gate
	if gate == nil || gate.Threshold.Path != "params.threshold" {
		t.Errorf("gate = %+v", gate)
	}
}

func TestLoadReportsEveryShapeError(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string) {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("sidmesh.json", `{"name": "Bad Name"}`)
	write("nodes/a/node.json", `{"id": "a", "responsibility": "x", "runtime": "python"}`)
	write("nodes/b/node.json", `{"id": "c", "responsibility": "x", "runtime": "go"}`)
	write("definitions/d.v1.json", `{"id": "d", "version": 1}`)

	_, err := Load(dir)
	if err == nil {
		t.Fatal("want errors")
	}
	msg := err.Error()
	for _, want := range []string{"sidmesh.json: name", "nodes/a/node.json", `differs from directory "b"`, "definitions/d.v1.json"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error does not mention %q:\n%s", want, msg)
		}
	}
}

func TestSpecSupported(t *testing.T) {
	for _, c := range []struct {
		declared, implemented string
		want                  bool
	}{
		{"0.3", "0.3", true},
		{"0.2", "0.3", false},
		{"0.4", "0.3", false},
		{"1.0", "0.3", false},
		{"1.0", "1.2", true},
		{"1.2", "1.2", true},
		{"1.3", "1.2", false},
		{"0.9", "1.2", false},
		{"2.0", "1.2", false},
	} {
		if got := SpecSupported(c.declared, c.implemented); got != c.want {
			t.Errorf("SpecSupported(%q, %q) = %v, want %v", c.declared, c.implemented, got, c.want)
		}
	}
}

func TestLoadSpec(t *testing.T) {
	for _, c := range []struct {
		spec, want string // want "" means no error
	}{
		{`"` + SpecVersion + `"`, ""},
		{`"0.2"`, "sidmesh.json: project declares spec 0.2; this toolchain implements " + SpecVersion + ", see the upgrade notes in CHANGELOG.md"},
		{`"v0.3"`, "sidmesh.json: spec must be a string MAJOR.MINOR"},
		{`0.3`, "sidmesh.json: spec must be a string MAJOR.MINOR"},
	} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "sidmesh.json"), []byte(`{"name": "demo", "spec": `+c.spec+`}`), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := Load(dir)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("spec %s: %v", c.spec, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("spec %s: error %v, want %q", c.spec, err, c.want)
		}
	}
}

func TestMetaSchemasMatchSpec(t *testing.T) {
	for _, n := range []string{"node", "contract", "definition"} {
		spec, err := os.ReadFile("../../spec/" + n + ".schema.json")
		if err != nil {
			t.Skipf("spec not present next to go/: %v", err)
		}
		embedded, _ := metaFS.ReadFile("schemas/" + n + ".schema.json")
		if !bytes.Equal(spec, embedded) {
			t.Errorf("%s.schema.json differs from spec; run go generate ./project", n)
		}
	}
}

func TestBindingRoundTrip(t *testing.T) {
	for _, in := range []string{`"input.a"`, `{"const":null}`, `{"const":{"x":1}}`, `{"expr":"input.a + 1"}`} {
		var b Binding
		if err := b.UnmarshalJSON([]byte(in)); err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		out, err := b.MarshalJSON()
		if err != nil {
			t.Fatal(err)
		}
		if string(out) != in {
			t.Errorf("round trip %s -> %s", in, out)
		}
	}
}

package project

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	sidmesh "gitlab.com/sidcorp-it/sidmesh/go"
)

// Project is a loaded project directory.
type Project struct {
	Dir         string
	Manifest    Manifest
	Nodes       map[string]*Node       // by node id
	Contracts   map[string]*Contract   // by capability@version
	Definitions map[string]*Definition // by id@version
	Documents   map[string]Document    // contracts and definitions by pin key
}

// Document is a node, contract or definition as published to the registry: its pin key
// ("node:<id>", "definition:<id>@<v>", "contract:<capability>@<v>") and the key's parts,
// content hash and source bytes.
type Document struct {
	Key     string          `json:"key"`
	Kind    string          `json:"-"` // KindNode, KindContract or KindDefinition
	Name    string          `json:"-"` // node id, capability or definition id
	Version int             `json:"-"` // 0 for a node
	Hash    string          `json:"hash"`
	Doc     json.RawMessage `json:"doc"`
}

// Document kinds.
const (
	KindDefinition = "definition"
	KindContract   = "contract"
	KindNode       = "node"
)

// Pin key prefixes, one per document kind.
const (
	DefinitionPin = KindDefinition + ":"
	ContractPin   = KindContract + ":"
	NodePin       = KindNode + ":"
)

// PinKey is the pin key of a document: "node:<id>" (version 0), "<kind>:<name>@<version>"
// otherwise.
func PinKey(kind, name string, version int) string {
	if kind == KindNode {
		return NodePin + name
	}
	return kind + ":" + sidmesh.Key(name, version)
}

// ParsePinKey splits a pin key into its document kind, name and version (0 for a node).
func ParsePinKey(key string) (kind, name string, version int, err error) {
	kind, name, _ = strings.Cut(key, ":")
	switch kind {
	case KindNode:
		if name != "" && !strings.Contains(name, "@") {
			return kind, name, 0, nil
		}
	case KindDefinition, KindContract:
		var v string
		name, v, _ = strings.Cut(name, "@")
		if version, err = strconv.Atoi(v); err == nil && name != "" && version > 0 {
			return kind, name, version, nil
		}
	}
	return "", "", 0, fmt.Errorf("pin key %q: want node:<id>, contract:<capability>@<v> or definition:<id>@<v>", key)
}

var (
	projectName = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	specPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+$`)
)

// SpecVersion is the SidMesh spec version this toolchain implements (SPEC.md), as MAJOR.MINOR.
const SpecVersion = "0.3"

// SpecSupported reports whether a project declaring spec declared can run on a toolchain
// implementing implemented (both MAJOR.MINOR). Before 1.0 they must be equal; from 1.0 the
// major must match and the declared minor must not be newer.
func SpecSupported(declared, implemented string) bool {
	dMajor, dMinor := splitSpec(declared)
	iMajor, iMinor := splitSpec(implemented)
	if iMajor == 0 {
		return dMajor == 0 && dMinor == iMinor
	}
	return dMajor == iMajor && dMinor <= iMinor
}

func splitSpec(v string) (major, minor int) {
	a, b, _ := strings.Cut(v, ".")
	major, _ = strconv.Atoi(a)
	minor, _ = strconv.Atoi(b)
	return major, minor
}

// Load reads sidmesh.json, nodes/*/node.json, nodes/*/contracts/*.json and
// definitions/*.json, checking each against its spec schema. All problems are returned
// together.
func Load(dir string) (*Project, error) {
	p := &Project{
		Dir:         dir,
		Nodes:       map[string]*Node{},
		Contracts:   map[string]*Contract{},
		Definitions: map[string]*Definition{},
		Documents:   map[string]Document{},
	}
	var errs []error
	if err := p.loadManifest(); err != nil {
		errs = append(errs, err)
	}

	nodeFiles, _ := filepath.Glob(filepath.Join(dir, "nodes", "*", "node.json"))
	sort.Strings(nodeFiles)
	for _, file := range nodeFiles {
		var n Node
		raw, err := readDocument(file, "node", &n)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if dirName := filepath.Base(filepath.Dir(file)); dirName != n.ID {
			errs = append(errs, fmt.Errorf("%s: node id %q differs from directory %q", file, n.ID, dirName))
			continue
		}
		p.Nodes[n.ID] = &n
		if err := p.addDocument(KindNode, n.ID, 0, raw); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", file, err))
		}
		errs = append(errs, p.loadContracts(&n, filepath.Join(filepath.Dir(file), "contracts"))...)
	}

	defFiles, _ := filepath.Glob(filepath.Join(dir, "definitions", "*.json"))
	sort.Strings(defFiles)
	for _, file := range defFiles {
		var d Definition
		raw, err := readDocument(file, "definition", &d)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		key := sidmesh.Key(d.ID, d.Version)
		if _, dup := p.Definitions[key]; dup {
			errs = append(errs, fmt.Errorf("%s: definition %s declared twice", file, key))
			continue
		}
		p.Definitions[key] = &d
		if err := p.addDocument(KindDefinition, d.ID, d.Version, raw); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", file, err))
		}
	}
	return p, errors.Join(errs...)
}

func (p *Project) loadManifest() error {
	file := filepath.Join(p.Dir, "sidmesh.json")
	raw, err := os.ReadFile(file)
	if err != nil {
		return fmt.Errorf("sidmesh.json: %w", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return fmt.Errorf("sidmesh.json: %w", err)
	}
	if rawSpec, ok := fields["spec"]; ok {
		var spec string
		if json.Unmarshal(rawSpec, &spec) != nil || !specPattern.MatchString(spec) {
			return fmt.Errorf(`sidmesh.json: spec must be a string MAJOR.MINOR, e.g. "%s"`, SpecVersion)
		}
		if !SpecSupported(spec, SpecVersion) {
			return fmt.Errorf("sidmesh.json: project declares spec %s; this toolchain implements %s, see the upgrade notes in CHANGELOG.md", spec, SpecVersion)
		}
	}
	if err := json.Unmarshal(raw, &p.Manifest); err != nil {
		return fmt.Errorf("sidmesh.json: %w", err)
	}
	if !projectName.MatchString(p.Manifest.Name) {
		return fmt.Errorf("sidmesh.json: name %q must match %s", p.Manifest.Name, projectName)
	}
	q := &p.Manifest.Queues
	if q.Workflow == "" {
		q.Workflow = p.Manifest.Name + "-workflows"
	}
	if q.Go == "" {
		q.Go = p.Manifest.Name + "-go"
	}
	if q.TS == "" {
		q.TS = p.Manifest.Name + "-ts"
	}
	return nil
}

func (p *Project) loadContracts(n *Node, dir string) []error {
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	sort.Strings(files)
	var errs []error
	for _, file := range files {
		var c Contract
		raw, err := readDocument(file, "contract", &c)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		c.Node, c.Runtime = n.ID, n.Runtime
		key := sidmesh.Key(c.Capability, c.Version)
		if _, dup := p.Contracts[key]; dup {
			errs = append(errs, fmt.Errorf("%s: capability %s declared twice", file, key))
			continue
		}
		p.Contracts[key] = &c
		if err := p.addDocument(KindContract, c.Capability, c.Version, raw); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", file, err))
		}
	}
	return errs
}

func (p *Project) addDocument(kind, name string, version int, raw []byte) error {
	hash, err := sidmesh.DocHash(raw)
	if err != nil {
		return err
	}
	key := PinKey(kind, name, version)
	p.Documents[key] = Document{Key: key, Kind: kind, Name: name, Version: version, Hash: hash, Doc: raw}
	return nil
}

// readDocument validates file against the named spec schema, decodes it into out and
// returns its bytes.
func readDocument(file, schema string, out any) ([]byte, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	s, err := metaSchema(schema)
	if err != nil {
		return nil, err
	}
	if err := s.Validate(doc); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	return raw, nil
}

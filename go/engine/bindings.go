package engine

import (
	"fmt"
	"math"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"cel.dev/cel-go/cel"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/junixlabs/sidmesh/go/project"
)

// Binding roots of SPEC §5. Every root is a CEL variable of type dyn.
var bindingRoots = []string{"input", "params", "vars", "steps", "signals", "item", "workflow"}

// walk follows a dotted path through nested maps. A path that does not resolve yields nil.
func walk(root map[string]any, path string) any {
	var cur any = root
	for _, part := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[part]
	}
	return cur
}

// exprs compiles CEL expressions once per process; runs share it. Evaluation is pure, so it
// is safe in workflow code.
type exprs struct {
	env   *cel.Env
	progs *cache[cel.Program] // expression source -> program
}

// maxPrograms bounds the compiled CEL programs a process keeps.
const maxPrograms = 4096

// cache is a concurrency-safe map of at most max entries: adding to a full cache drops an
// arbitrary entry. Values must be pure functions of their keys, so a miss only costs time.
type cache[V any] struct {
	mu    sync.Mutex
	max   int
	items map[string]V
}

func newCache[V any](max int) *cache[V] { return &cache[V]{max: max, items: map[string]V{}} }

func (c *cache[V]) get(key string) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.items[key]
	return v, ok
}

func (c *cache[V]) put(key string, v V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.items[key]; !ok && len(c.items) >= c.max {
		for k := range c.items {
			delete(c.items, k)
			break
		}
	}
	c.items[key] = v
}

var sharedExprs = sync.OnceValues(func() (*exprs, error) {
	opts := []cel.EnvOption{cel.CrossTypeNumericComparisons(true)}
	for _, root := range bindingRoots {
		opts = append(opts, cel.Variable(root, cel.DynType))
	}
	env, err := cel.NewEnv(opts...)
	if err != nil {
		return nil, err
	}
	return &exprs{env: env, progs: newCache[cel.Program](maxPrograms)}, nil
})

func (e *exprs) program(src string) (cel.Program, error) {
	if p, ok := e.progs.get(src); ok {
		return p, nil
	}
	ast, iss := e.env.Compile(src)
	if iss != nil && iss.Err() != nil {
		return nil, fmt.Errorf("expression %q: %w", src, iss.Err())
	}
	p, err := e.env.Program(ast)
	if err != nil {
		return nil, fmt.Errorf("expression %q: %w", src, err)
	}
	e.progs.put(src, p)
	return p, nil
}

// value evaluates src and returns the result as plain JSON-shaped Go values.
func (e *exprs) value(src string, vars map[string]any) (any, error) {
	p, err := e.program(src)
	if err != nil {
		return nil, err
	}
	out, _, err := p.Eval(vars)
	if err != nil {
		return nil, fmt.Errorf("expression %q: %w", src, err)
	}
	native, err := out.ConvertToNative(reflect.TypeOf(&structpb.Value{}))
	if err != nil {
		return nil, fmt.Errorf("expression %q: result is not JSON: %w", src, err)
	}
	return native.(*structpb.Value).AsInterface(), nil
}

func (e *exprs) bool(src string, vars map[string]any) (bool, error) {
	v, err := e.value(src, vars)
	if err != nil {
		return false, err
	}
	b, ok := v.(bool)
	if !ok {
		return false, fmt.Errorf("expression %q: want bool, got %T", src, v)
	}
	return b, nil
}

// resolve evaluates one binding against the scope.
func (e *exprs) resolve(b project.Binding, scope map[string]any) (any, error) {
	switch {
	case b.IsConst:
		return typed(b.Const, nil), nil
	case b.Expr != "":
		return e.value(b.Expr, scope)
	default:
		return walk(scope, b.Path), nil
	}
}

// resolveMap evaluates a set of bindings into an object. A nil set gives an empty object.
func (e *exprs) resolveMap(bs map[string]project.Binding, scope map[string]any) (map[string]any, error) {
	out := make(map[string]any, len(bs))
	for k, b := range bs {
		v, err := e.resolve(b, scope)
		if err != nil {
			return nil, fmt.Errorf("binding %s: %w", k, err)
		}
		out[k] = v
	}
	return out, nil
}

var durationPattern = regexp.MustCompile(`^([1-9][0-9]*)(ms|s|m|h|d)$`)

// parseDuration reads the spec's duration form: 500ms, 30s, 10m, 4h, 21d.
func parseDuration(s string) (time.Duration, error) {
	m := durationPattern.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("duration %q: want <n>(ms|s|m|h|d)", s)
	}
	n, _ := strconv.ParseInt(m[1], 10, 64)
	unit := map[string]time.Duration{
		"ms": time.Millisecond, "s": time.Second, "m": time.Minute, "h": time.Hour, "d": 24 * time.Hour,
	}[m[2]]
	return time.Duration(n) * unit, nil
}

// typed gives JSON numbers the CEL type their schema declares (SPEC §5 "Numbers in CEL"):
// integer becomes int64, number stays float64, and a number with no schema type becomes
// int64 when it has no fractional part. Maps and arrays are copied, other values returned.
func typed(v any, schema map[string]any) any {
	switch x := v.(type) {
	case map[string]any:
		props, _ := schema["properties"].(map[string]any)
		extra, _ := schema["additionalProperties"].(map[string]any)
		out := make(map[string]any, len(x))
		for k, e := range x {
			sub, ok := props[k].(map[string]any)
			if !ok {
				sub = extra
			}
			out[k] = typed(e, sub)
		}
		return out
	case []any:
		items, _ := schema["items"].(map[string]any)
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = typed(e, items)
		}
		return out
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1<<53 && !declaresNumber(schema) {
			return int64(x)
		}
		return x
	}
	return v
}

// declaresNumber reports whether the schema's type includes "number".
func declaresNumber(schema map[string]any) bool {
	switch t := schema["type"].(type) {
	case string:
		return t == "number"
	case []any:
		return slices.Contains(t, any("number"))
	}
	return false
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}

// toText renders a scalar for ids and search attributes; whole numbers lose the ".0".
func toText(v any) string {
	if f, ok := toFloat(v); ok {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

func asMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

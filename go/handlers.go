package sidmesh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

// Handler is the untyped form every handler is stored in. Input and config have already
// been validated against the contract when it runs.
type Handler func(ctx context.Context, input, config map[string]any, meta Meta) (Result, error)

// Registry maps capability@version to its handler.
type Registry struct {
	mu       sync.RWMutex
	handlers map[string]Handler
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{handlers: map[string]Handler{}}
}

// Default is the registry node packages register into from their init functions.
var Default = NewRegistry()

// Add registers h for capability@version. Registering the same key twice panics: it is a
// wiring mistake that should fail at start-up, not at the first call.
func (r *Registry) Add(capability string, version int, h Handler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := Key(capability, version)
	if _, dup := r.handlers[key]; dup {
		panic(fmt.Sprintf("sidmesh: handler %s registered twice", key))
	}
	r.handlers[key] = h
}

// Lookup returns the handler for key, if any.
func (r *Registry) Lookup(key string) (Handler, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	h, ok := r.handlers[key]
	return h, ok
}

// Register adds a typed handler to the Default registry. I and C are the generated input and
// config types of the contract.
func Register[I, C any](capability string, version int, fn func(context.Context, I, C, Meta) (Result, error)) {
	Default.Add(capability, version, Wrap(fn))
}

// Wrap turns a typed handler into a Handler by decoding input and config through JSON.
// A decoding failure means the generated types and the contract disagree; it is fatal.
func Wrap[I, C any](fn func(context.Context, I, C, Meta) (Result, error)) Handler {
	return func(ctx context.Context, input, config map[string]any, meta Meta) (Result, error) {
		var in I
		var cfg C
		if err := convert(input, &in); err != nil {
			return Result{}, Fatal(fmt.Errorf("decode input: %w", err))
		}
		if err := convert(config, &cfg); err != nil {
			return Result{}, Fatal(fmt.Errorf("decode config: %w", err))
		}
		return fn(ctx, in, cfg, meta)
	}
}

func convert(from any, to any) error {
	raw, err := json.Marshal(from)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, to)
}

type fatalError struct{ err error }

func (e fatalError) Error() string { return e.err.Error() }
func (e fatalError) Unwrap() error { return e.err }

// Fatal marks err as non-retryable: the step fails at once instead of being retried.
func Fatal(err error) error {
	if err == nil {
		return nil
	}
	return fatalError{err: err}
}

// IsFatal reports whether err was marked with Fatal.
func IsFatal(err error) bool {
	var f fatalError
	return errors.As(err, &f)
}

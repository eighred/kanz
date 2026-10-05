// Package replay records the resolved inputs of one risk evaluation and runs
// historical calculations without consulting live providers.
package replay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

const maxInputBytes = 32 << 20

type inputKey struct {
	Kind   string
	ID     string
	AsOf   time.Time
	Window int
}

type inputs struct {
	mu     sync.Mutex
	replay bool
	values map[string]json.RawMessage
	size   int
	err    error
}

type contextKey struct{}

func withInputs(ctx context.Context, in *inputs) context.Context {
	return context.WithValue(ctx, contextKey{}, in)
}

func failInput(ctx context.Context, err error) {
	if in, ok := ctx.Value(contextKey{}).(*inputs); ok {
		in.fail(err)
	}
}

// resolve memoizes a provider answer, including an explicit unavailable answer.
// A missing replay entry is an error, never permission to call the live source.
func resolve[T any](ctx context.Context, key inputKey, live func() T) T {
	var zero T
	in, _ := ctx.Value(contextKey{}).(*inputs)
	if in == nil {
		return live()
	}
	key.AsOf = key.AsOf.UTC()
	k, err := json.Marshal(key)
	if err != nil {
		in.fail(err)
		return zero
	}
	name := string(k)
	in.mu.Lock()
	data, found := in.values[name]
	if found {
		in.mu.Unlock()
		var out T
		if err := json.Unmarshal(data, &out); err != nil {
			in.fail(err)
			return zero
		}
		return out
	}
	if in.replay {
		in.mu.Unlock()
		in.fail(fmt.Errorf("risk replay input missing: %s", key.Kind))
		return zero
	}
	in.mu.Unlock()
	value := live()
	data, err = json.Marshal(value)
	if err != nil {
		in.fail(err)
		return zero
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	if previous, ok := in.values[name]; ok {
		if string(previous) != string(data) {
			in.err = errors.New("risk inputs changed during evaluation")
		}
		return value
	}
	if len(in.values) >= 100000 || len(data)+len(name) > maxInputBytes-in.size {
		in.err = errors.New("risk evaluation input budget exceeded")
		return zero
	}
	in.values[name] = data
	in.size += len(data) + len(name)
	return value
}

func (in *inputs) fail(err error) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.err == nil {
		in.err = err
	}
}

func (in *inputs) failure() error {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.err
}

// Release the evaluation's retained inputs on every exit, including refusals.
func (in *inputs) release() {
	in.mu.Lock()
	defer in.mu.Unlock()
	clear(in.values)
}

type answer[T any] struct {
	Value  T
	Known  bool
	Error  string
	Status int
}

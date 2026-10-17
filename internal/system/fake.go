package system

import (
	"context"
	"strings"
	"sync"
)

// FakeRunner records commands instead of running them. Handler, if set,
// decides each command's output and error. It's for tests.
type FakeRunner struct {
	mu      sync.Mutex
	Calls   []string
	Inputs  map[string]string // cmdline -> stdin, for RunInput calls
	Handler func(cmdline string) ([]byte, error)
}

// RunInput implements InputRunner.
func (f *FakeRunner) RunInput(ctx context.Context, input string, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	if f.Inputs == nil {
		f.Inputs = map[string]string{}
	}
	f.Inputs[strings.Join(append([]string{name}, args...), " ")] = input
	f.mu.Unlock()
	return f.Run(ctx, name, args...)
}

// Run implements Runner.
func (f *FakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	cmdline := strings.Join(append([]string{name}, args...), " ")
	f.mu.Lock()
	f.Calls = append(f.Calls, cmdline)
	f.mu.Unlock()
	if f.Handler != nil {
		return f.Handler(cmdline)
	}
	return nil, nil
}

// Commands returns a copy of the recorded command lines.
func (f *FakeRunner) Commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.Calls...)
}

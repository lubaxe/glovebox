// Package system runs external commands (losetup, mount, fsck, ...).
package system

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
)

// Runner runs a command and returns its combined output.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// InputRunner can also feed a command's standard input.
type InputRunner interface {
	Runner
	RunInput(ctx context.Context, input string, name string, args ...string) ([]byte, error)
}

// CmdError is returned when a command exits with a non-zero status.
type CmdError struct {
	Cmd      string
	ExitCode int
	Output   string
}

func (e *CmdError) Error() string {
	out := strings.TrimSpace(e.Output)
	if out == "" {
		return fmt.Sprintf("%s: exit status %d", e.Cmd, e.ExitCode)
	}
	return fmt.Sprintf("%s: exit status %d: %s", e.Cmd, e.ExitCode, out)
}

// ExitCode returns the exit code of a failed command, or -1 if err isn't a
// CmdError.
func ExitCode(err error) int {
	var ce *CmdError
	if errors.As(err, &ce) {
		return ce.ExitCode
	}
	return -1
}

// Exec runs commands on the real system.
type Exec struct {
	Log *slog.Logger
}

// Run implements Runner.
func (e Exec) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return e.RunInput(ctx, "", name, args...)
}

// RunInput implements InputRunner.
func (e Exec) RunInput(ctx context.Context, input string, name string, args ...string) ([]byte, error) {
	cmdline := strings.Join(append([]string{name}, args...), " ")
	if e.Log != nil {
		e.Log.Debug("run", "cmd", cmdline)
	}
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	if input != "" {
		cmd.Stdin = strings.NewReader(input)
	}
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return out.Bytes(), &CmdError{Cmd: cmdline, ExitCode: ee.ExitCode(), Output: out.String()}
	}
	if err != nil {
		return out.Bytes(), fmt.Errorf("%s: %w", cmdline, err)
	}
	return out.Bytes(), nil
}

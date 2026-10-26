package bootfs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lubaxe/glovebox/internal/system"
)

const (
	bootMount    = "/boot/firmware"
	queryCommand = "findmnt --json --list --output TARGET,FSTYPE,OPTIONS"
	rwCommand    = "mount -o remount,rw /boot/firmware"
	roCommand    = "mount -o remount,ro /boot/firmware"
	syncCommand  = "sync -f /boot/firmware"
)

type runnerFunc func(context.Context, string, ...string) ([]byte, error)

func (runner runnerFunc) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return runner(ctx, name, args...)
}

type writerFunc func(context.Context, func() error) error

func (writer writerFunc) WithWrite(ctx context.Context, fn func() error) error {
	return writer(ctx, fn)
}

func mountJSON(target, filesystem, options string) []byte {
	output, err := json.Marshal(map[string]any{"filesystems": []any{
		map[string]string{"target": target, "fstype": filesystem, "options": options},
	}})
	if err != nil {
		panic(err)
	}
	return output
}

func fakeMount(options string) *system.FakeRunner {
	return &system.FakeRunner{Handler: func(command string) ([]byte, error) {
		if command == queryCommand {
			return mountJSON(bootMount, "vfat", options), nil
		}
		return nil, nil
	}}
}

func assertCommands(t *testing.T, runner *system.FakeRunner, want ...string) {
	t.Helper()
	if got := runner.Commands(); !reflect.DeepEqual(got, want) {
		t.Fatalf("commands = %q, want %q", got, want)
	}
}

func TestWithWriteHelper(t *testing.T) {
	callbackErr := errors.New("callback")
	called := 0
	callback := func() error { called++; return callbackErr }
	if err := WithWrite(context.Background(), nil, callback); !errors.Is(err, callbackErr) || called != 1 {
		t.Fatalf("nil writer: err = %v, calls = %d", err, called)
	}
	delegated := 0
	writer := writerFunc(func(ctx context.Context, fn func() error) error {
		delegated++
		return fn()
	})
	if err := WithWrite(context.Background(), writer, callback); !errors.Is(err, callbackErr) || delegated != 1 || called != 2 {
		t.Fatalf("delegation: err = %v, delegated = %d, calls = %d", err, delegated, called)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, candidate := range []Writer{nil, writer} {
		if err := WithWrite(ctx, candidate, callback); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled helper: %v", err)
		}
	}
	if delegated != 1 || called != 2 {
		t.Fatal("canceled helper executed work")
	}
}

func TestWriteStatesAndFailures(t *testing.T) {
	callbackErr := errors.New("callback")
	flushErr := errors.New("flush")
	writableErr := errors.New("writable remount")
	restoreErr := errors.New("restore remount")
	for _, test := range []struct {
		name                                           string
		options                                        string
		callbackErr, flushErr, writableErr, restoreErr error
	}{
		{name: "original ro", options: "ro,relatime"},
		{name: "original rw", options: "rw,relatime"},
		{name: "callback failure", options: "ro", callbackErr: callbackErr},
		{name: "flush failure", options: "ro", flushErr: flushErr},
		{name: "writable failure", options: "ro", writableErr: writableErr},
		{name: "writable failure joins cleanup failures", options: "ro", writableErr: writableErr, flushErr: flushErr, restoreErr: restoreErr},
		{name: "restore failure", options: "ro", restoreErr: restoreErr},
		{name: "joined failures", options: "ro", callbackErr: callbackErr, flushErr: flushErr, restoreErr: restoreErr},
		{name: "rw failures preserve rw", options: "rw", callbackErr: callbackErr, flushErr: flushErr},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := fakeMount(test.options)
			query := runner.Handler
			runner.Handler = func(command string) ([]byte, error) {
				switch command {
				case rwCommand:
					return nil, test.writableErr
				case syncCommand:
					return nil, test.flushErr
				case roCommand:
					return nil, test.restoreErr
				default:
					return query(command)
				}
			}
			called := false
			err := New(bootMount, runner).WithWrite(context.Background(), func() error {
				called = true
				_, _ = runner.Run(context.Background(), "callback")
				return test.callbackErr
			})
			want := []string{queryCommand}
			if strings.HasPrefix(test.options, "ro") {
				want = append(want, rwCommand)
			}
			if test.writableErr == nil {
				want = append(want, "callback")
			}
			want = append(want, syncCommand)
			if strings.HasPrefix(test.options, "ro") {
				want = append(want, roCommand)
			}
			assertCommands(t, runner, want...)
			if called != (test.writableErr == nil) {
				t.Fatalf("callback called = %v", called)
			}
			wantError := false
			for _, expected := range []error{test.callbackErr, test.flushErr, test.writableErr, test.restoreErr} {
				if expected != nil {
					wantError = true
					if !errors.Is(err, expected) {
						t.Errorf("error %v missing %v", err, expected)
					}
				}
			}
			if (err != nil) != wantError {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestProtect(t *testing.T) {
	flushErr, restoreErr := errors.New("flush"), errors.New("restore")
	for _, test := range []struct {
		name, options        string
		flushErr, restoreErr error
	}{
		{name: "already ro", options: "ro"},
		{name: "rw", options: "rw"},
		{name: "flush failure still restores", options: "rw", flushErr: flushErr},
		{name: "restore failure", options: "rw", restoreErr: restoreErr},
		{name: "joined failures", options: "rw", flushErr: flushErr, restoreErr: restoreErr},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := fakeMount(test.options)
			query := runner.Handler
			runner.Handler = func(command string) ([]byte, error) {
				switch command {
				case syncCommand:
					return nil, test.flushErr
				case roCommand:
					return nil, test.restoreErr
				default:
					return query(command)
				}
			}
			err := New(bootMount, runner).Protect(context.Background())
			want := []string{queryCommand}
			if test.options == "rw" {
				want = append(want, syncCommand, roCommand)
			}
			assertCommands(t, runner, want...)
			if test.flushErr != nil && !errors.Is(err, test.flushErr) {
				t.Fatalf("lost flush error: %v", err)
			}
			if test.restoreErr != nil && !errors.Is(err, test.restoreErr) {
				t.Fatalf("lost restore error: %v", err)
			}
			if test.flushErr == nil && test.restoreErr == nil && err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMountSafety(t *testing.T) {
	permissionErr := errors.New("permission denied")
	for _, test := range []struct {
		name, target string
		output       []byte
		queryErr     error
		wantError    bool
	}{
		{name: "empty listing", target: bootMount, output: []byte("{\"filesystems\":[]}")},
		{name: "root containing directory is unmanaged", target: bootMount, output: mountJSON("/", "vfat", "rw")},
		{name: "parent boot mount is unmanaged", target: bootMount, output: mountJSON("/boot", "vfat", "rw")},
		{name: "child mount is unmanaged", target: bootMount, output: mountJSON(bootMount+"/child", "vfat", "rw")},
		{name: "non vfat exact mount is unmanaged", target: bootMount, output: mountJSON(bootMount, "ext4", "rw")},
		{name: "root rejected", target: "/", wantError: true},
		{name: "relative rejected", target: "boot/firmware", wantError: true},
		{name: "empty rejected", target: "", wantError: true},
		{name: "traversal rejected", target: "/boot/..", wantError: true},
		{name: "trailing slash rejected", target: bootMount + "/", wantError: true},
		{name: "windows separator rejected", target: "/boot\\firmware", wantError: true},
		{name: "nul rejected", target: bootMount + "\x00", wantError: true},
		{name: "noncanonical returned target", target: bootMount, output: mountJSON(bootMount+"/", "vfat", "rw"), wantError: true},
		{name: "malformed JSON", target: bootMount, output: []byte("not json"), wantError: true},
		{name: "empty output", target: bootMount, wantError: true},
		{name: "missing array", target: bootMount, output: []byte("{}"), wantError: true},
		{name: "null array", target: bootMount, output: []byte("{\"filesystems\":null}"), wantError: true},
		{name: "missing fields", target: bootMount, output: mountJSON(bootMount, "", "rw"), wantError: true},
		{name: "unexpected relative target", target: bootMount, output: mountJSON("boot", "vfat", "rw"), wantError: true},
		{name: "conflicting modes", target: bootMount, output: mountJSON(bootMount, "vfat", "rw,ro"), wantError: true},
		{name: "missing mode", target: bootMount, output: mountJSON(bootMount, "vfat", "relatime"), wantError: true},
		{name: "mode substring", target: bootMount, output: mountJSON(bootMount, "vfat", "errors=remount-ro"), wantError: true},
		{name: "permission error", target: bootMount, output: []byte("{\"filesystems\":[]}"), queryErr: permissionErr, wantError: true},
		{name: "exit one is not missing", target: bootMount, queryErr: &system.CmdError{ExitCode: 1}, wantError: true},
		{name: "duplicate exact target", target: bootMount, output: []byte(fmt.Sprintf("{\"filesystems\":[%s,%s]}",
			"{\"target\":\"/boot/firmware\",\"fstype\":\"vfat\",\"options\":\"ro\"}",
			"{\"target\":\"/boot/firmware\",\"fstype\":\"vfat\",\"options\":\"rw\"}")), wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, protect := range []bool{false, true} {
				runner := &system.FakeRunner{Handler: func(string) ([]byte, error) { return test.output, test.queryErr }}
				manager := New(test.target, runner)
				called := false
				var err error
				if protect {
					err = manager.Protect(context.Background())
				} else {
					err = manager.WithWrite(context.Background(), func() error { called = true; return nil })
				}
				if (err != nil) != test.wantError {
					t.Fatalf("protect=%v: error = %v", protect, err)
				}
				if called != (!protect && !test.wantError) {
					t.Fatalf("protect=%v: called=%v", protect, called)
				}
				if test.queryErr != nil && !errors.Is(err, test.queryErr) {
					t.Fatalf("lost query error: %v", err)
				}
				for _, command := range runner.Commands() {
					if command != queryCommand {
						t.Fatalf("unsafe command %q", command)
					}
				}
			}
		})
	}
}

func TestMountpointWithSpaces(t *testing.T) {
	target := "/boot/firmware with spaces"
	var calls [][]string
	runner := runnerFunc(func(ctx context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		if name == "findmnt" {
			return mountJSON(target, "vfat", "ro"), nil
		}
		return nil, nil
	})
	if err := New(target, runner).WithWrite(context.Background(), func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	for _, command := range calls[1:] {
		if command[len(command)-1] != target {
			t.Fatalf("split mountpoint: %q", command)
		}
	}
}

func TestCanceledAcquire(t *testing.T) {
	for _, protect := range []bool{false, true} {
		runner := fakeMount("ro")
		manager := New(bootMount, runner)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		var err error
		if protect {
			err = manager.Protect(ctx)
		} else {
			err = manager.WithWrite(ctx, func() error { t.Fatal("callback after cancellation"); return nil })
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled acquire: %v", err)
		}
		assertCommands(t, runner)
	}
}

func TestContendedAcquire(t *testing.T) {
	runner := fakeMount("ro")
	manager := New(bootMount, runner)
	entered, release := make(chan struct{}), make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		finished <- manager.WithWrite(context.Background(), func() error { close(entered); <-release; return nil })
	}()
	<-entered
	defer func() {
		close(release)
		if err := <-finished; err != nil {
			t.Error(err)
		}
	}()
	for _, protect := range []bool{false, true} {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		var err error
		if protect {
			err = manager.Protect(ctx)
		} else {
			err = manager.WithWrite(ctx, func() error { t.Error("contended callback ran"); return nil })
		}
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("contended acquire: %v", err)
		}
		assertCommands(t, runner, queryCommand, rwCommand)
	}
}

func TestConcurrentScopesAndProtect(t *testing.T) {
	mode := "ro"
	runner := &system.FakeRunner{Handler: func(command string) ([]byte, error) {
		switch command {
		case queryCommand:
			return mountJSON(bootMount, "vfat", mode), nil
		case rwCommand:
			mode = "rw"
		case roCommand:
			mode = "ro"
		}
		return nil, nil
	}}
	manager := New(bootMount, runner)
	start := make(chan struct{})
	results := make(chan error, 60)
	var workers sync.WaitGroup
	for index := 0; index < 60; index++ {
		workers.Add(1)
		go func(protect bool) {
			defer workers.Done()
			<-start
			if protect {
				results <- manager.Protect(context.Background())
				return
			}
			results <- manager.WithWrite(context.Background(), func() error {
				if mode != "rw" {
					return fmt.Errorf("callback mount is %s", mode)
				}
				_, _ = runner.Run(context.Background(), "callback")
				return nil
			})
		}(index%3 == 0)
	}
	close(start)
	workers.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Error(err)
		}
	}
	commands := runner.Commands()
	writes := 0
	for len(commands) > 0 {
		if commands[0] != queryCommand {
			t.Fatalf("interleaved commands: %q", commands)
		}
		commands = commands[1:]
		if len(commands) > 0 && commands[0] == rwCommand {
			want := []string{rwCommand, "callback", syncCommand, roCommand}
			if len(commands) < len(want) || !reflect.DeepEqual(commands[:len(want)], want) {
				t.Fatalf("interleaved write scope: %q", commands)
			}
			commands = commands[len(want):]
			writes++
		}
	}
	if writes != 40 || mode != "ro" {
		t.Fatalf("writes=%d, final mode=%s", writes, mode)
	}
}

func TestCanceledCleanup(t *testing.T) {
	type contextKey struct{}
	for _, protect := range []bool{false, true} {
		t.Run(fmt.Sprintf("protect=%v", protect), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.WithValue(context.Background(), contextKey{}, "preserved"))
			defer cancel()
			var calls []string
			callbackErr := errors.New("callback")
			runner := runnerFunc(func(commandCtx context.Context, name string, args ...string) ([]byte, error) {
				command := strings.Join(append([]string{name}, args...), " ")
				calls = append(calls, command)
				if name == "findmnt" {
					if protect {
						cancel()
						return mountJSON(bootMount, "vfat", "rw"), nil
					}
					return mountJSON(bootMount, "vfat", "ro"), nil
				}
				if command == syncCommand || command == roCommand {
					if err := commandCtx.Err(); err != nil {
						t.Errorf("cleanup inherited cancellation: %v", err)
					}
					deadline, ok := commandCtx.Deadline()
					if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > cleanupTimeout {
						t.Error("cleanup lacks bounded deadline")
					}
					if commandCtx.Value(contextKey{}) != "preserved" {
						t.Error("cleanup lost context values")
					}
				}
				return nil, nil
			})
			manager := New(bootMount, runner)
			var err error
			want := []string{queryCommand, syncCommand, roCommand}
			if protect {
				err = manager.Protect(ctx)
			} else {
				err = manager.WithWrite(ctx, func() error { cancel(); return callbackErr })
				want = []string{queryCommand, rwCommand, syncCommand, roCommand}
				if !errors.Is(err, callbackErr) {
					t.Fatalf("lost callback error: %v", err)
				}
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("lost cancellation: %v", err)
			}
			if !reflect.DeepEqual(calls, want) {
				t.Fatalf("calls=%q, want %q", calls, want)
			}
		})
	}
}

func TestCancellationAfterRemountSkipsCallback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runner := fakeMount("ro")
	query := runner.Handler
	runner.Handler = func(command string) ([]byte, error) {
		if command == rwCommand {
			cancel()
		}
		return query(command)
	}
	err := New(bootMount, runner).WithWrite(ctx, func() error { t.Error("canceled callback ran"); return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
	assertCommands(t, runner, queryCommand, rwCommand, syncCommand, roCommand)
}

func TestPanicCleanupAndUnlock(t *testing.T) {
	runner := fakeMount("ro")
	manager := New(bootMount, runner)
	panicValue := errors.New("panic")
	func() {
		defer func() {
			if got := recover(); got != panicValue {
				t.Fatalf("panic=%v", got)
			}
		}()
		_ = manager.WithWrite(context.Background(), func() error { panic(panicValue) })
	}()
	assertCommands(t, runner, queryCommand, rwCommand, syncCommand, roCommand)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := manager.WithWrite(ctx, func() error { return nil }); err != nil {
		t.Fatalf("manager locked after panic: %v", err)
	}
}

func TestNilRunnerFailsClosed(t *testing.T) {
	manager := New(bootMount, nil)
	if err := manager.WithWrite(context.Background(), func() error { t.Fatal("nil runner callback"); return nil }); err == nil {
		t.Fatal("nil runner succeeded")
	}
	if err := manager.Protect(context.Background()); err == nil {
		t.Fatal("nil runner protection succeeded")
	}
}

func TestUncertainWritableRemountFailure(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprintf("canceled=%v", canceled), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			mode := "ro"
			remountErr := errors.New("rw applied but command failed")
			flushErr, restoreErr := errors.New("flush failed"), errors.New("restore reported failure")
			var calls []string
			runner := runnerFunc(func(commandCtx context.Context, name string, args ...string) ([]byte, error) {
				command := strings.Join(append([]string{name}, args...), " ")
				calls = append(calls, command)
				switch command {
				case queryCommand:
					return mountJSON(bootMount, "vfat", mode), nil
				case rwCommand:
					mode = "rw"
					if canceled {
						cancel()
					}
					return nil, remountErr
				case syncCommand, roCommand:
					if err := commandCtx.Err(); err != nil {
						t.Errorf("cleanup canceled: %v", err)
					}
					deadline, ok := commandCtx.Deadline()
					if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > cleanupTimeout {
						t.Error("cleanup deadline missing")
					}
					if command == syncCommand {
						return nil, flushErr
					}
					mode = "ro"
					return nil, restoreErr
				}
				return nil, nil
			})
			err := New(bootMount, runner).WithWrite(ctx, func() error { t.Error("callback after uncertain rw failure"); return nil })
			for _, expected := range []error{remountErr, flushErr, restoreErr} {
				if !errors.Is(err, expected) {
					t.Errorf("error %v missing %v", err, expected)
				}
			}
			if errors.Is(err, context.Canceled) != canceled {
				t.Errorf("cancellation mismatch: %v", err)
			}
			if mode != "ro" {
				t.Fatalf("mount left %s", mode)
			}
			want := []string{queryCommand, rwCommand, syncCommand, roCommand}
			if !reflect.DeepEqual(calls, want) {
				t.Fatalf("calls=%q, want=%q", calls, want)
			}
		})
	}
}

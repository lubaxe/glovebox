package keepawake

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lubaxe/glovebox/internal/system"
)

const testVIN = "5YJ3E1EA0PF000001"

// newCar returns a Car in a temp dir, with testVIN set and paired.
func newCar(t *testing.T, r *system.FakeRunner) *Car {
	t.Helper()
	c := &Car{Command: "tesla-control", Dir: filepath.Join(t.TempDir(), "ble"), Runner: r}
	if err := c.SetVIN(testVIN); err != nil {
		t.Fatal(err)
	}
	if err := c.CheckPairing(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.Calls = nil
	return c
}

func newKeepAwake(t *testing.T, r *system.FakeRunner, busy func() bool) *KeepAwake {
	return &KeepAwake{
		Car: newCar(t, r), Interval: time.Hour, Busy: busy,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestPoke(t *testing.T) {
	r := &system.FakeRunner{}
	k := newKeepAwake(t, r, nil)
	if err := k.Poke(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := "tesla-control -ble -vin " + testVIN + " -key-file " + k.Car.KeyFile() + " wake"
	if cmds := r.Commands(); len(cmds) != 2 || cmds[0] != want || !strings.HasSuffix(cmds[1], " body-controller-state") {
		t.Errorf("commands = %v", cmds)
	}
	r.Handler = func(string) ([]byte, error) { return nil, errors.New("car not found") }
	if k.Poke(context.Background()) == nil || k.Status().LastError == "" {
		t.Error("want the failure reported")
	}
}

func TestRunWakesOnlyWhileBusy(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for the 5 s check")
	}
	r := &system.FakeRunner{}
	var busy atomic.Bool
	k := newKeepAwake(t, r, busy.Load)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go k.Run(ctx)

	time.Sleep(6 * time.Second)
	if n := len(r.Commands()); n != 0 {
		t.Fatalf("idle, but woke the car %d times", n)
	}
	busy.Store(true)
	time.Sleep(6 * time.Second)
	// once at the start of the busy spell (wake and a state check), then
	// not again within Interval
	if cmds := r.Commands(); len(cmds) != 2 || !strings.HasSuffix(cmds[0], " wake") {
		t.Errorf("commands = %v", cmds)
	}
	if !k.Status().Active {
		t.Error("status should say it's keeping the car awake")
	}
}

func TestRunStopsAfterMax(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for the 5 s check")
	}
	r := &system.FakeRunner{}
	k := newKeepAwake(t, r, func() bool { return true })
	k.Interval, k.Max, k.Cooldown = time.Millisecond, 7*time.Second, time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go k.Run(ctx)
	time.Sleep(21 * time.Second) // checks at 5, 10, 15, 20 s; Max reached at the 15 s check
	if n := len(r.Commands()); n != 4 {
		t.Errorf("ran %d commands, want 4 (wake and a state check at 5 s and 10 s)", n)
	}
	if k.Status().RestingUntil.IsZero() {
		t.Error("status should say it's resting")
	}
}

func TestNothingHappensUntilPaired(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for the 5 s check")
	}
	r := &system.FakeRunner{}
	k := newKeepAwake(t, r, func() bool { return true })
	if err := k.Car.Forget(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go k.Run(ctx)
	time.Sleep(6 * time.Second)
	if n := len(r.Commands()); n != 0 {
		t.Errorf("not paired, but ran %v", r.Commands())
	}
}

func TestPairingFlow(t *testing.T) {
	r := &system.FakeRunner{}
	c := &Car{Command: "tesla-control", Dir: filepath.Join(t.TempDir(), "ble"), Runner: r}
	ctx := context.Background()
	if c.RequestPairing(ctx) == nil || c.CheckPairing(ctx) == nil {
		t.Error("pairing needs a VIN")
	}
	if c.SetVIN("not a vin") == nil {
		t.Error("want an invalid VIN refused")
	}
	if err := c.SetVIN(" 5yj3e1ea0pf000001 "); err != nil || c.VIN() != testVIN {
		t.Fatalf("vin=%q err=%v", c.VIN(), err)
	}
	if err := c.RequestPairing(ctx); err != nil {
		t.Fatal(err)
	}
	want := "tesla-control -ble -vin " + testVIN + " add-key-request " + filepath.Join(c.Dir, "public.pem") + " charging_manager cloud_key"
	if cmds := r.Commands(); len(cmds) != 1 || cmds[0] != want {
		t.Errorf("commands = %v", cmds)
	}
	if _, err := os.Stat(c.KeyFile()); err != nil {
		t.Error("the key should have been created")
	}

	// the car hasn't accepted the key yet
	r.Handler = func(string) ([]byte, error) {
		return nil, &system.CmdError{Cmd: "tesla-control", ExitCode: 1, Output: "connecting...\nFailed to execute command: key not paired\n"}
	}
	if err := c.CheckPairing(ctx); err == nil || err.Error() != "Failed to execute command: key not paired" {
		t.Errorf("err = %v", err)
	}
	if _, ok := c.Paired(); ok {
		t.Error("not paired yet")
	}
	r.Handler = nil
	if err := c.CheckPairing(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Paired(); !ok {
		t.Error("should be paired")
	}
	// another car: the pairing was for the old one
	if err := c.SetVIN("5YJ3E1EA0PF000002"); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Paired(); ok {
		t.Error("a new VIN needs pairing again")
	}
}

func TestSleepState(t *testing.T) {
	r := &system.FakeRunner{Handler: func(cmd string) ([]byte, error) {
		return []byte(`{
	"vehicleSleepStatus": "VEHICLE_SLEEP_STATUS_AWAKE",
}`), nil
	}}
	if got, err := newCar(t, r).SleepState(context.Background()); got != "AWAKE" || err != nil {
		t.Errorf("got %q, %v", got, err)
	}
}

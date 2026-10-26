package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/lubaxe/glovebox/internal/packages"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestKeepPackagesRetriesThenRestarts(t *testing.T) {
	warn := &notices{}
	warn.set("boot", "boot warning")
	attempts, restarts := 0, 0
	ensure := func(context.Context) ([]string, error) {
		attempts++
		if attempts < 3 {
			if got := warn.list(); attempts == 2 && (len(got) != 2 || !strings.Contains(got[1], "offline")) {
				t.Errorf("warnings while retrying = %q", got)
			}
			return nil, fmt.Errorf("updating the package lists: offline")
		}
		return []string{"rclone"}, nil
	}
	keepPackages(context.Background(), ensure, warn, func() { restarts++ }, quiet, time.Millisecond)
	if attempts != 3 || restarts != 1 {
		t.Errorf("attempts=%d restarts=%d", attempts, restarts)
	}
	if got := warn.list(); len(got) != 1 || got[0] != "boot warning" {
		t.Errorf("warnings after installing = %q", got)
	}
}

func TestKeepPackagesNothingToDo(t *testing.T) {
	for _, err := range []error{nil, packages.ErrUnsupported} {
		warn, restarts := &notices{}, 0
		keepPackages(context.Background(), func(context.Context) ([]string, error) { return nil, err }, warn,
			func() { restarts++ }, quiet, time.Millisecond)
		if restarts != 0 || len(warn.list()) != 0 {
			t.Errorf("%v: restarts=%d warnings=%q", err, restarts, warn.list())
		}
	}
}

func TestKeepPackagesStopsOnShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		keepPackages(ctx, func(context.Context) ([]string, error) { return nil, errors.New("offline") },
			&notices{}, func() { t.Error("restarted") }, quiet, time.Hour)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("still retrying after shutdown")
	}
}

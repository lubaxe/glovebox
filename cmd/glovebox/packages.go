package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/lubaxe/glovebox/internal/packages"
	"github.com/lubaxe/glovebox/internal/system"
)

// notices are the warnings the web UI shows, by what they're about. Some
// clear up while Glovebox runs, like missing packages once installed.
type notices struct {
	mu   sync.Mutex
	byID map[string][]string
	keys []string
}

func (n *notices) set(id string, warnings ...string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.byID == nil {
		n.byID = map[string][]string{}
	}
	if _, ok := n.byID[id]; !ok {
		n.keys = append(n.keys, id)
	}
	n.byID[id] = warnings
}

func (n *notices) list() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	var all []string
	for _, id := range n.keys {
		all = append(all, n.byID[id]...)
	}
	return all
}

// prepareUpdate is glovebox -prepare-update: the updater runs the new
// version that way before switching to it, so it installs what it needs
// while the old version keeps running.
func prepareUpdate() error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	installed, err := packages.New(system.Exec{}).Ensure(ctx)
	switch {
	case errors.Is(err, packages.ErrUnsupported):
		fmt.Println("no dpkg here; system packages are up to you")
		return nil
	case err != nil:
		return err
	case len(installed) > 0:
		fmt.Println("installed", strings.Join(installed, " "))
	}
	return nil
}

// packageRetry is how long keepPackages waits before trying again.
const packageRetry = 15 * time.Minute

// keepPackages installs the packages this version needs, should they be
// missing: on a Pi updated by a version whose updater didn't run
// -prepare-update, or after the power was cut halfway through. Until it
// succeeds, a warning says what's missing; it tries again every
// packageRetry, as the Pi may be away from home Wi-Fi. Once it has installed
// something, restart starts Glovebox afresh with it.
func keepPackages(ctx context.Context, ensure func(context.Context) ([]string, error), warn *notices, restart func(), log *slog.Logger, retry time.Duration) {
	for {
		installed, err := ensure(ctx)
		switch {
		case errors.Is(err, packages.ErrUnsupported) || ctx.Err() != nil:
			return
		case err == nil:
			warn.set("packages")
			if len(installed) > 0 {
				log.Info("installed the system packages this version needs; restarting", "packages", installed)
				restart()
			}
			return
		}
		log.Warn("couldn't install the system packages this version needs; trying again later", "err", err, "retry", retry)
		warn.set("packages", "Some system packages this version needs couldn't be installed, so some features may not work. Glovebox tries again every "+
			retry.String()+" while it's online: "+err.Error())
		select {
		case <-ctx.Done():
			return
		case <-time.After(retry):
		}
	}
}

// installedHere reports whether this is the installed Glovebox, the one
// that looks after the Pi (rather than a test copy, run with -no-usb or
// from elsewhere).
func installedHere(noUSB bool) bool {
	executable, _ := os.Executable()
	return executable == "/usr/local/bin/glovebox" && !noUSB
}

// Package archive copies the car's clips to a NAS whenever the car is home.
//
// "Home" means the archive target answers on the network. Once it has for
// HomeDelay, the archiver takes a fresh snapshot and uploads everything not
// yet archived: Sentry events first, then Saved, then (optionally) Recent,
// newest first, so the most important footage gets out before the car goes
// to sleep and cuts the Pi's power. While home it checks again every
// RescanInterval, so Sentry events recorded in the garage get archived too.
package archive

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/lubaxe/glovebox/internal/clips"
)

// Library is what the archiver needs from the clip library.
type Library interface {
	Refresh() (map[string]clips.Snapshot, error)
	Events(kind string) []clips.Summary
	Event(kind, id string, encrypted bool) (clips.Event, bool)
	Recent() []clips.Clip
	Open(snapshot, rel string) (*os.File, error)
}

// Snapshotter takes a snapshot and waits for it.
type Snapshotter interface {
	SnapshotNow(ctx context.Context) error
}

// Archiver states.
const (
	Away      = "away"      // target unreachable
	Home      = "home"      // reachable, nothing to do
	Archiving = "archiving" // uploading
	Syncing   = "syncing"   // updating the extra drives
)

// Result summarizes one archiving run.
type Result struct {
	Started  time.Time     `json:"started"`
	Duration time.Duration `json:"-"`
	Seconds  float64       `json:"seconds"`
	Uploaded int           `json:"uploaded"`
	Bytes    int64         `json:"bytes"`
	Failed   int           `json:"failed"`
	Pending  int           `json:"pending"` // left for next time
	Events   []string      `json:"events,omitempty"`
	Err      string        `json:"error,omitempty"`
}

// Status is what the API reports.
type Status struct {
	Target   string `json:"target"`
	State    string `json:"state"`
	Current  string `json:"current,omitempty"`
	Done     int    `json:"done"`
	Total    int    `json:"total"`
	Archived int    `json:"archived_files"`
	// CatchingUp is set during the first run after the car gets home, which
	// uploads what it recorded while away. Later runs only pick up what it
	// recorded while parked, which isn't worth keeping it awake for: an
	// awake car keeps recording, faster than the Wi-Fi can upload.
	CatchingUp bool    `json:"catching_up"`
	Last       *Result `json:"last,omitempty"`
}

// Archiver runs the archive loop.
type Archiver struct {
	Target    Target
	Library   Library
	Snapshots Snapshotter
	State     *State
	Log       *slog.Logger

	Sentry, Saved, Recent bool

	HomeDelay      time.Duration
	CheckInterval  time.Duration
	RescanInterval time.Duration

	// OnResult, if set, is called after each run that had something to do.
	OnResult func(Result)
	// SyncDrives, if set, updates the extra drives from the target. It runs
	// after the first archiving run once the car is home, and after runs
	// asked for with Trigger.
	SyncDrives func(ctx context.Context)

	trigger chan struct{}
	mu      sync.Mutex
	status  Status
}

// New returns an Archiver with upstream's timing defaults.
func New(t Target, lib Library, snaps Snapshotter, st *State, log *slog.Logger) *Archiver {
	return &Archiver{
		Target: t, Library: lib, Snapshots: snaps, State: st, Log: log,
		Sentry: true, Saved: true,
		HomeDelay:      20 * time.Second,
		CheckInterval:  30 * time.Second,
		RescanInterval: 10 * time.Minute,
		trigger:        make(chan struct{}, 1),
		status:         Status{Target: t.String(), State: Away},
	}
}

// Trigger asks for a run as soon as the target is reachable.
func (a *Archiver) Trigger() {
	select {
	case a.trigger <- struct{}{}:
	default:
	}
}

// Status returns the current status.
func (a *Archiver) Status() Status {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := a.status
	st.Archived = a.State.Count()
	return st
}

// Run loops until ctx is done.
func (a *Archiver) Run(ctx context.Context) {
	home := false
	catchUp := false
	syncDrives := false
	var lastRun time.Time
	for {
		triggered := false
		if err := a.Target.Reachable(ctx); err != nil {
			if home {
				a.Log.Info("archive target unreachable, car left home", "target", a.Target, "err", err)
			}
			home = false
			a.setState(Away)
		} else {
			if !home {
				a.Log.Info("archive target reachable, car is home", "target", a.Target)
				home = true
				a.setState(Home)
				// let the car settle (and the Wi-Fi connection stabilize)
				if !sleep(ctx, a.HomeDelay) {
					return
				}
				lastRun = time.Time{}
				catchUp = true
				syncDrives = true
			}
			if time.Since(lastRun) >= a.RescanInterval {
				a.setCatchingUp(catchUp)
				a.run(ctx)
				a.setCatchingUp(false)
				catchUp = false
				if syncDrives && a.SyncDrives != nil && ctx.Err() == nil {
					a.setState(Syncing)
					a.SyncDrives(ctx)
				}
				syncDrives = false
				lastRun = time.Now()
				a.setState(Home)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-a.trigger:
			triggered = true
		case <-time.After(a.CheckInterval):
		}
		if triggered {
			lastRun = time.Time{}
			syncDrives = true
		}
	}
}

// item is one file to upload.
type item struct {
	snapshot, path, remote string
	size                   int64
	event                  string // kind/id, for the summary
}

func (a *Archiver) run(ctx context.Context) {
	res := Result{Started: time.Now()}
	defer func() {
		res.Duration = time.Since(res.Started)
		res.Seconds = res.Duration.Seconds()
		a.mu.Lock()
		a.status.Current = ""
		if res.Uploaded > 0 || res.Failed > 0 || res.Err != "" {
			last := res
			a.status.Last = &last
		}
		a.mu.Unlock()
		if a.OnResult != nil && (res.Uploaded > 0 || res.Failed > 0) {
			a.OnResult(res)
		}
	}()

	// pick up whatever the car recorded since the last snapshot
	if err := a.Snapshots.SnapshotNow(ctx); err != nil {
		a.Log.Warn("couldn't take a fresh snapshot, archiving from existing ones", "err", err)
	}
	if _, err := a.Library.Refresh(); err != nil {
		res.Err = err.Error()
		a.Log.Error("listing snapshots", "err", err)
		return
	}
	all := a.queue()
	keep := map[string]bool{}
	var todo []item
	for _, it := range all {
		keep[it.path] = true
		if !a.State.Done(it.path, it.size) {
			todo = append(todo, it)
		}
	}
	a.State.Prune(keep)
	if len(todo) == 0 {
		return
	}

	a.setState(Archiving)
	a.setProgress("", 0, len(todo))
	a.Log.Info("archiving", "files", len(todo), "target", a.Target)
	sess, err := a.Target.Connect(ctx)
	if err != nil {
		res.Err = err.Error()
		res.Pending = len(todo)
		a.Log.Error("connecting to archive", "err", err)
		return
	}
	defer sess.Close()

	events := map[string]bool{}
	for i, it := range todo {
		a.setProgress(it.remote, i, len(todo))
		err := a.upload(ctx, sess, it)
		if err == nil {
			a.State.Mark(it.path, it.size)
			if serr := a.State.Save(); serr != nil {
				a.Log.Error("saving archive state", "err", serr)
			}
			res.Uploaded++
			res.Bytes += it.size
			if !events[it.event] && it.event != "" {
				events[it.event] = true
				res.Events = append(res.Events, it.event)
			}
			continue
		}
		res.Failed++
		a.Log.Error("archiving file", "file", it.remote, "err", err)
		if ctx.Err() != nil || isConnErr(err) {
			// the car probably left or went to sleep; try again next time
			res.Err = err.Error()
			res.Pending = len(todo) - i - 1
			break
		}
	}
	a.Log.Info("archiving finished", "uploaded", res.Uploaded, "failed", res.Failed, "pending", res.Pending,
		"mb", res.Bytes>>20, "seconds", int(time.Since(res.Started).Seconds()))
}

func (a *Archiver) upload(ctx context.Context, sess Session, it item) error {
	// already there in full, e.g. archived before the state was lost
	if size, ok, err := sess.Size(ctx, it.remote); err != nil {
		return err
	} else if ok && size == it.size {
		return nil
	}
	f, err := a.Library.Open(it.snapshot, it.path)
	if err != nil {
		return err
	}
	defer f.Close()
	return sess.Upload(ctx, it.remote, f, it.size)
}

// queue lists every archivable file in priority order: Sentry, Saved,
// Recent; newest events first; within an event, metadata then clips.
func (a *Archiver) queue() []item {
	var out []item
	for _, kind := range []string{clips.Sentry, clips.Saved} {
		if (kind == clips.Sentry && !a.Sentry) || (kind == clips.Saved && !a.Saved) {
			continue
		}
		for _, sum := range a.Library.Events(kind) { // newest first
			ev, ok := a.Library.Event(kind, sum.ID, sum.Encrypted)
			if !ok {
				continue
			}
			tag := kind + "/" + ev.ID
			if ev.Thumb != nil {
				out = append(out, newItem(*ev.Thumb, tag))
			}
			for _, c := range ev.Clips {
				for _, cam := range clips.SortCameras(cameraSet(c)) {
					out = append(out, newItem(c.Cameras[cam], tag))
				}
			}
			if ev.Meta != nil {
				out = append(out, newItem(*ev.Meta, tag))
			}
		}
	}
	if a.Recent {
		for _, c := range a.Library.Recent() {
			for _, cam := range clips.SortCameras(cameraSet(c)) {
				out = append(out, newItem(c.Cameras[cam], ""))
			}
		}
	}
	return out
}

func newItem(f clips.File, event string) item {
	return item{snapshot: f.Snapshot, path: f.Path, remote: strings.TrimPrefix(f.Path, "TeslaCam/"), size: f.Size, event: event}
}

func cameraSet(c clips.Clip) map[string]bool {
	set := map[string]bool{}
	for cam := range c.Cameras {
		set[cam] = true
	}
	return set
}

func (a *Archiver) setCatchingUp(v bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.status.CatchingUp = v
}

func (a *Archiver) setState(s string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.status.State = s
	if s != Archiving {
		a.status.Done, a.status.Total = 0, 0
	}
}

func (a *Archiver) setProgress(current string, done, total int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.status.Current, a.status.Done, a.status.Total = current, done, total
}

// isConnErr reports whether err means the connection is gone, as opposed
// to a problem with one file.
func isConnErr(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	msg := err.Error()
	for _, s := range []string{"connection reset", "broken pipe", "i/o timeout", "use of closed", "EOF", "no route to host", "network is unreachable",
		"input/output error", "stale file handle", "host is down"} { // the last ones: NFS
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

func sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// Summary describes a result in one line, for logs and notifications.
func (r Result) Summary() string {
	s := fmt.Sprintf("Archived %d files (%.1f GB) from %d events in %s", r.Uploaded, float64(r.Bytes)/(1<<30), len(r.Events), r.Duration.Round(time.Second))
	if r.Failed > 0 || r.Pending > 0 {
		s += fmt.Sprintf("; %d failed, %d left for next time", r.Failed, r.Pending)
	}
	return s
}

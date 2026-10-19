// Package archive copies the car's clips to a NAS whenever the car is home.
//
// "Home" means the archive target answers on the network. Once it has for
// HomeDelay, the archiver takes a fresh snapshot and uploads everything not
// yet archived: Sentry events first, then Saved, then (optionally) Recent,
// newest first, so the most important footage gets out before the car goes
// to sleep and cuts the Pi's power. While home it checks again every
// RescanInterval, so Sentry events recorded in the garage get archived too.
// Recent clips recorded at home aren't: an awake car records the garage all
// the time, and each upload would leave more of it for the next.
package archive

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
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
	// Locate finds the current copy of a file whose snapshot has gone.
	Locate(rel string) (clips.File, bool)
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
	// RecentAtHome also archives Recent clips recorded while home.
	RecentAtHome bool

	HomeDelay      time.Duration
	CheckInterval  time.Duration
	RescanInterval time.Duration
	// FinalPassAfter is how long after arriving the catch-up takes its
	// final pass (see finishCatchUp).
	FinalPassAfter time.Duration

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
		FinalPassAfter: 150 * time.Second,
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
	var lastRun, awaySince, arrived time.Time
	for {
		triggered := false
		if err := a.Target.Reachable(ctx); err != nil {
			if home {
				a.Log.Info("archive target unreachable, car left home", "target", a.Target, "err", err)
			}
			home = false
			a.setState(Away)
			if awaySince.IsZero() {
				awaySince = time.Now()
			}
			// gone for a while, not a Wi-Fi blip: next time home is a new arrival
			if time.Since(awaySince) >= leftHomeAfter && a.State.HomeCutoff() != "" {
				a.State.SetHomeCutoff("")
				if err := a.State.Save(); err != nil {
					a.Log.Error("saving archive state", "err", err)
				}
			}
		} else {
			awaySince = time.Time{}
			if !home {
				a.Log.Info("archive target reachable, car is home", "target", a.Target)
				home = true
				arrived = time.Now()
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
				res := a.run(ctx)
				if catchUp {
					a.finishCatchUp(ctx, arrived, res)
				}
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

// leftHomeAfter is how long the target must be unreachable before the car
// counts as having left home.
const leftHomeAfter = 2 * time.Minute

// homeMargin is added to the newest Recent clip when the car gets home: the
// clip being recorded then, and pulling in, still count as the drive.
const homeMargin = time.Minute

// clipTime is clips.Clip.Start's layout.
const clipTime = "2006-01-02T15:04:05"

// setHomeCutoff records, the first time the car is home after being away,
// the newest Recent clip on the drive; clips after it are recorded at home.
// It goes by the clips' own timestamps, so the car's clock and time zone
// don't matter.
func (a *Archiver) setHomeCutoff() {
	if a.State.HomeCutoff() != "" {
		return
	}
	cutoff := "0" // no Recent clips: everything from now on is at home
	for _, c := range a.Library.Recent() {
		if t, err := time.Parse(clipTime, c.Start); err == nil {
			if s := t.Add(homeMargin).Format(clipTime); s > cutoff {
				cutoff = s
			}
		}
	}
	a.State.SetHomeCutoff(cutoff)
	if err := a.State.Save(); err != nil {
		a.Log.Error("saving archive state", "err", err)
	}
	a.Log.Info("car is home; Recent clips recorded after this aren't archived", "after", cutoff)
}

// finalPasses bounds the catch-up's final passes.
const finalPasses = 3

// finishCatchUp backs up what the car was still writing as it got home:
// the minute being recorded on arrival, and the cameras of the minute
// before, only reach the drive in full a minute or two later. Once the
// catch-up ends keep-awake lets the car sleep, cutting the Pi's power
// before the next rescan, so the catch-up waits until FinalPassAfter has
// passed since arriving and goes again, with a fresh snapshot, for as long
// as that finds something (at most finalPasses times). The home cutoff
// still keeps out what was recorded parked.
func (a *Archiver) finishCatchUp(ctx context.Context, arrived time.Time, res Result) {
	for i := 0; i < finalPasses && res.Uploaded > 0 && res.Err == "" && ctx.Err() == nil; i++ {
		if wait := a.FinalPassAfter - time.Since(arrived); wait > 0 && !sleep(ctx, wait) {
			return
		}
		res = a.run(ctx)
	}
}

// run takes a fresh snapshot and uploads what isn't archived yet.
func (a *Archiver) run(ctx context.Context) (res Result) {
	res = Result{Started: time.Now()}
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
	if !a.RecentAtHome {
		a.setHomeCutoff()
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
	a.removePartials(ctx, sess)

	events := map[string]bool{}
	for i, it := range todo {
		a.setProgress(it.remote, i, len(todo))
		size, err := a.upload(ctx, sess, it)
		if err == nil {
			a.State.Mark(it.path, size)
			if serr := a.State.Save(); serr != nil {
				a.Log.Error("saving archive state", "err", serr)
			}
			res.Uploaded++
			res.Bytes += size
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
	return res
}

// upload copies one file and returns the size it archived. The snapshot the
// queue found it in may have been released since (to free space, or once
// its clips were kept in the clip store); then the current copy is used.
func (a *Archiver) upload(ctx context.Context, sess Session, it item) (int64, error) {
	f, err := a.Library.Open(it.snapshot, it.path)
	if errors.Is(err, clips.ErrNotFound) {
		if cur, ok := a.Library.Locate(it.path); ok && cur.Snapshot != it.snapshot {
			it.snapshot, it.size = cur.Snapshot, cur.Size
			f, err = a.Library.Open(it.snapshot, it.path)
		}
	}
	if err != nil {
		return 0, err
	}
	defer f.Close()
	// already there in full, e.g. archived before the state was lost
	if size, ok, err := sess.Size(ctx, it.remote); err != nil {
		return 0, err
	} else if ok && size == it.size {
		return it.size, nil
	}
	tu, ok := sess.(TempUploader)
	if !ok {
		return it.size, sess.Upload(ctx, it.remote, f, it.size)
	}
	// Record the temporary file before writing it: if the power is cut
	// mid-upload, the next run deletes it.
	tmp := TempName(it.remote)
	a.State.AddPartial(tmp)
	if err := a.State.Save(); err != nil {
		a.Log.Warn("saving archive state", "err", err)
	}
	err = tu.UploadVia(ctx, it.remote, tmp, f, it.size)
	if err == nil {
		a.State.DropPartial(tmp) // renamed into place; saved with the file's Mark
	}
	return it.size, err
}

// maxPartialTries is how many runs try to delete a temporary file before
// giving up on it. A server can keep a dead connection's file open, and
// undeletable, for hours.
const maxPartialTries = 10

// removePartials deletes temporary files that earlier uploads left behind.
func (a *Archiver) removePartials(ctx context.Context, sess Session) {
	partials := a.State.Partials()
	if len(partials) == 0 {
		return
	}
	for _, p := range partials {
		err := sess.Remove(ctx, p)
		switch {
		case err == nil:
			a.Log.Info("deleted an unfinished upload", "file", p)
			a.State.DropPartial(p)
		case errors.Is(err, fs.ErrNotExist):
			a.State.DropPartial(p)
		case a.State.FailedPartial(p) >= maxPartialTries:
			a.Log.Warn("giving up on deleting an unfinished upload", "file", p, "err", err)
			a.State.DropPartial(p)
		default:
			a.Log.Debug("can't delete an unfinished upload yet", "file", p, "err", err)
		}
	}
	if err := a.State.Save(); err != nil {
		a.Log.Error("saving archive state", "err", err)
	}
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
		cutoff := a.State.HomeCutoff()
		for _, c := range a.Library.Recent() {
			if !a.RecentAtHome && cutoff != "" && c.Start > cutoff {
				continue // recorded at home
			}
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

// Outstanding counts the files on the drive that aren't archived yet: what
// erasing the drive now would lose. Recent clips recorded at home don't
// count, as they're never archived (unless RecentAtHome).
func (a *Archiver) Outstanding() (int, error) {
	if _, err := a.Library.Refresh(); err != nil {
		return 0, err
	}
	n := 0
	for _, it := range a.queue() {
		if !a.State.Done(it.path, it.size) {
			n++
		}
	}
	return n, nil
}

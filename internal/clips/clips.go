// Package clips indexes the car's recordings across all snapshots.
//
// The car's drive looks like this:
//
//	TeslaCam/RecentClips/2026-10-15_00-39-00-front.mp4        rolling last hour
//	TeslaCam/RecentClips/2026-10-15/2026-10-15_00-39-00-...    (newer firmware)
//	TeslaCam/SavedClips/2026-10-15_00-40-00/<clips>, event.json, thumb.png
//	TeslaCam/SentryClips/2026-10-15_00-40-00/<clips>, event.json, thumb.png
//	TeslaCam/EncryptedClips/<same folders>   when the car encrypts recordings
//
// Encrypted clips can only be decrypted by the car or dashcam.tesla.com, so
// they're listed (with Encrypted set) for archiving and downloading, but
// can't be played here.
//
// Each clip file covers about a minute from one camera; files with the same
// timestamp form one Clip.
package clips

import (
	"encoding/json"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Kinds of recordings.
const (
	Recent = "recent"
	Saved  = "saved"
	Sentry = "sentry"
)

var (
	// 2026-10-15_00-39-00-front.mp4
	clipName = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2})_(\d{2})-(\d{2})-(\d{2})-([a-z_]+)\.mp4$`)
	// 2026-10-15_00-40-00
	eventName = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2})_(\d{2})-(\d{2})-(\d{2})$`)
)

// minClipSize skips files too short to play, e.g. ones the car had just
// started when the snapshot was taken.
const minClipSize = 100 << 10

// File is one file in one snapshot.
type File struct {
	Snapshot string `json:"snapshot"`
	Path     string `json:"path"` // slash-separated, relative to the drive root
	Size     int64  `json:"size"`
}

// Clip is about a minute of video from all cameras.
type Clip struct {
	// Start is the car's local time, e.g. "2026-10-15T00:39:00".
	Start   string          `json:"start"`
	Cameras map[string]File `json:"cameras"`
	// Encrypted clips can't be played here; see the package comment.
	Encrypted bool `json:"encrypted,omitempty"`
}

// Event is a Saved or Sentry recording.
type Event struct {
	Kind  string            `json:"kind"`
	ID    string            `json:"id"`   // folder name
	Time  string            `json:"time"` // e.g. "2026-10-15T00:40:00"
	Info  map[string]string `json:"info,omitempty"`
	Thumb *File             `json:"thumb,omitempty"`
	Meta  *File             `json:"meta,omitempty"` // event.json
	Clips []Clip            `json:"clips"`
	// Encrypted events are under TeslaCam/EncryptedClips.
	Encrypted bool `json:"encrypted,omitempty"`
}

// Summary is an Event without its clips, for listings.
type Summary struct {
	Kind      string            `json:"kind"`
	ID        string            `json:"id"`
	Time      string            `json:"time"`
	Info      map[string]string `json:"info,omitempty"`
	Thumb     *File             `json:"thumb,omitempty"`
	Clips     int               `json:"clips"`
	Cameras   []string          `json:"cameras"`
	Encrypted bool              `json:"encrypted,omitempty"`
}

// Snapshot is what the index needs to know about a snapshot.
type Snapshot struct {
	ID    string
	Mount string
	// Version changes when the files under Mount do; snapshots never change,
	// the clip store does.
	Version int64
}

// Index merges the clips of all snapshots. Each is scanned again only when
// its Version changes.
type Index struct {
	mu     sync.Mutex
	scans  map[string]*scan
	best   map[string]File   // path -> most complete copy
	events map[string]*Event // key: kind/id
	recent []Clip            // newest first
	// hide, if set, leaves Recent clips out of the index, e.g. the clip
	// store's parked minutes, which aren't drives
	hide func(p string, size int64) bool
}

// New returns an empty index.
func New() *Index { return &Index{scans: map[string]*scan{}, events: map[string]*Event{}} }

// HideRecent leaves out the Recent clips hide reports, from the next
// rebuild on.
func (x *Index) HideRecent(hide func(p string, size int64) bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.hide = hide
}

// Refresh brings the index up to date with the given snapshots: new ones
// are scanned, deleted ones dropped.
func (x *Index) Refresh(snaps []Snapshot) {
	x.mu.Lock()
	defer x.mu.Unlock()
	changed := false
	current := map[string]bool{}
	for _, s := range snaps {
		current[s.ID] = true
		if sc, ok := x.scans[s.ID]; !ok || sc.version != s.Version {
			x.scans[s.ID] = scanSnapshot(s)
			changed = true
		}
	}
	for id := range x.scans {
		if !current[id] {
			delete(x.scans, id)
			changed = true
		}
	}
	if changed {
		x.rebuild()
	}
}

// Events returns summaries of events of the given kind, newest first.
func (x *Index) Events(kind string) []Summary {
	x.mu.Lock()
	defer x.mu.Unlock()
	out := []Summary{}
	for _, e := range x.events {
		if e.Kind != kind {
			continue
		}
		out = append(out, Summary{Kind: e.Kind, ID: e.ID, Time: e.Time, Info: e.Info, Thumb: e.Thumb,
			Clips: len(e.Clips), Cameras: cameraNames(e.Clips), Encrypted: e.Encrypted})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time > out[j].Time })
	return out
}

// Event returns one event, or false if there's no such event.
func (x *Index) Event(kind, id string, encrypted bool) (Event, bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	e, ok := x.events[eventKey(kind, id, encrypted)]
	if !ok {
		return Event{}, false
	}
	return *e, true
}

// Recent returns the recent clips, newest first.
func (x *Index) Recent() []Clip {
	x.mu.Lock()
	defer x.mu.Unlock()
	return append([]Clip{}, x.recent...)
}

// File returns the best copy of the drive file at p (slash-separated), and
// whether it's known. It's how the server checks that a requested file is
// part of the index before serving it.
func (x *Index) File(snapshot, p string) (File, bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	s, ok := x.scans[snapshot]
	if !ok {
		return File{}, false
	}
	f, ok := s.files[p]
	return f, ok
}

// scan is what one snapshot contains.
type scan struct {
	version int64
	files   map[string]File              // path -> file
	info    map[string]map[string]string // event folder path -> event.json
}

func scanSnapshot(s Snapshot) *scan {
	sc := &scan{version: s.Version, files: map[string]File{}, info: map[string]map[string]string{}}
	root := filepath.Join(s.Mount, "TeslaCam")
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil // unreadable parts of a damaged snapshot are skipped
		}
		rel, err := filepath.Rel(s.Mount, p)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		name := d.Name()
		switch {
		case name == "event.json":
			if info := readEventInfo(p); info != nil {
				sc.info[path.Dir(rel)] = info
			}
			fallthrough
		case name == "thumb.png" || clipName.MatchString(name):
			if fi, err := d.Info(); err == nil {
				sc.files[rel] = File{Snapshot: s.ID, Path: rel, Size: fi.Size()}
			}
		}
		return nil
	})
	return sc
}

// readEventInfo reads event.json, whose values are all strings, e.g.
// {"timestamp":"2026-10-15T00:40:00","city":"Springfield","reason":"sentry_aware_object_detection"}.
func readEventInfo(p string) map[string]string {
	b, err := os.ReadFile(p)
	if err != nil || len(b) > 64<<10 {
		return nil
	}
	var raw map[string]any
	if json.Unmarshal(b, &raw) != nil {
		return nil
	}
	info := map[string]string{}
	for k, v := range raw {
		if s, ok := v.(string); ok {
			info[k] = s
		}
	}
	return info
}

// rebuild merges all scans. For each file the most complete copy wins
// (largest size, then newest snapshot), so a clip cut short in one snapshot
// is served whole from a later one.
func (x *Index) rebuild() {
	best := map[string]File{}
	info := map[string]map[string]string{}
	ids := make([]string, 0, len(x.scans))
	for id := range x.scans {
		ids = append(ids, id)
	}
	sort.Strings(ids) // oldest first, so newer snapshots win ties
	for _, id := range ids {
		sc := x.scans[id]
		for p, f := range sc.files {
			if cur, ok := best[p]; !ok || f.Size >= cur.Size {
				best[p] = f
			}
		}
		for dir, i := range sc.info {
			info[dir] = i
		}
	}

	events := map[string]*Event{}
	eventClips := map[string]map[string]*Clip{} // event key -> start -> clip
	recent := map[string]*Clip{}                // key: start, plus "/enc" for encrypted
	for p, f := range best {
		parts := strings.Split(p, "/") // TeslaCam/[EncryptedClips/]<folder>/...
		encrypted := len(parts) > 1 && parts[1] == "EncryptedClips"
		if encrypted {
			parts = parts[1:] // the same layout one level down
		}
		if len(parts) < 3 {
			continue
		}
		name := parts[len(parts)-1]
		switch kind := kindOf(parts[1]); kind {
		case Recent:
			if m := clipName.FindStringSubmatch(name); m != nil && f.Size >= minClipSize && (x.hide == nil || !x.hide(p, f.Size)) {
				addToClip(recent, m, f, encrypted)
			}
		case Saved, Sentry:
			if len(parts) != 4 || !eventName.MatchString(parts[2]) {
				continue
			}
			key := eventKey(kind, parts[2], encrypted)
			e := events[key]
			if e == nil {
				e = &Event{Kind: kind, ID: parts[2], Time: isoTime(eventName.FindStringSubmatch(parts[2])),
					Info: info[path.Dir(p)], Encrypted: encrypted}
				events[key] = e
				eventClips[key] = map[string]*Clip{}
			}
			if name == "thumb.png" {
				thumb := f
				e.Thumb = &thumb
			} else if name == "event.json" {
				meta := f
				e.Meta = &meta
			} else if m := clipName.FindStringSubmatch(name); m != nil && f.Size >= minClipSize {
				addToClip(eventClips[key], m, f, encrypted)
			}
		}
	}
	for key, e := range events {
		e.Clips = flatten(eventClips[key], false)
	}
	x.events = events
	x.recent = flatten(recent, true)
	x.best = best
}

// eventKey keys an event in Index.events. Encrypted events get their own
// keys, so one can't hide an unencrypted event with the same folder name.
func eventKey(kind, id string, encrypted bool) string {
	if encrypted {
		return kind + "/" + id + "/enc"
	}
	return kind + "/" + id
}

// kindOf maps a TeslaCam folder name to a kind, or "" if it isn't one.
func kindOf(folder string) string {
	switch folder {
	case "RecentClips":
		return Recent
	case "SavedClips":
		return Saved
	case "SentryClips":
		return Sentry
	}
	return ""
}

func addToClip(clips map[string]*Clip, m []string, f File, encrypted bool) {
	start := isoTime(m[:5])
	key := start
	if encrypted {
		key += "/enc"
	}
	c := clips[key]
	if c == nil {
		c = &Clip{Start: start, Cameras: map[string]File{}, Encrypted: encrypted}
		clips[key] = c
	}
	c.Cameras[m[5]] = f
}

func flatten(clips map[string]*Clip, newestFirst bool) []Clip {
	out := make([]Clip, 0, len(clips))
	for _, c := range clips {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool {
		if newestFirst {
			return out[i].Start > out[j].Start
		}
		return out[i].Start < out[j].Start
	})
	return out
}

// isoTime turns regexp groups (date, hh, mm, ss) into "2026-10-15T00:39:00".
func isoTime(m []string) string {
	return m[1] + "T" + m[2] + ":" + m[3] + ":" + m[4]
}

func cameraNames(clips []Clip) []string {
	seen := map[string]bool{}
	for _, c := range clips {
		for cam := range c.Cameras {
			seen[cam] = true
		}
	}
	return SortCameras(seen)
}

// cameraOrder is how cameras are laid out: front first.
var cameraOrder = []string{"front", "back", "left_repeater", "right_repeater", "left_pillar", "right_pillar"}

// SortCameras returns camera names in display order; unknown ones go last.
func SortCameras(set map[string]bool) []string {
	var out []string
	for _, c := range cameraOrder {
		if set[c] {
			out = append(out, c)
		}
	}
	var rest []string
	for c := range set {
		if !contains(cameraOrder, c) {
			rest = append(rest, c)
		}
	}
	sort.Strings(rest)
	return append(out, rest...)
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// Locate returns the most complete copy of the drive file at p in any
// snapshot, for a caller holding a copy whose snapshot has since gone.
func (x *Index) Locate(p string) (File, bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	f, ok := x.best[p]
	return f, ok
}

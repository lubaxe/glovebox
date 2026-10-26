// Package store keeps the car's Recent clips from drives after the car and
// the snapshots have let them go.
//
// The car keeps only its last hour of Recent clips, and a snapshot of the
// whole drive is an expensive way to keep more: it holds on to every block
// the car later writes over, including blocks of free space and hours of
// footage recorded parked. Instead, after each snapshot the store clones
// each new clip, alone, out of the snapshot image (a reflink of its byte
// range, so it costs nothing until the car overwrites it), skips minutes
// recorded parked, and the snapshot can then go.
//
// Files are kept under Dir with the drive's own layout
// (Dir/TeslaCam/RecentClips/...), so the clip index reads the store like
// one more snapshot.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lubaxe/glovebox/internal/exfat"
	"github.com/lubaxe/glovebox/internal/storage"
)

// ID is the store's snapshot ID in the clip index.
const ID = "store"

const stateName = ".store.json"

// minClipSize matches the clip index: smaller files are clips the car had
// only just started.
const minClipSize = 100 << 10

var (
	// 2026-10-15_00-39-00-front.mp4
	clipName = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2}_\d{2}-\d{2}-\d{2})-([a-z_]+)\.mp4$`)
	// newer firmware: RecentClips/2026-10-15/
	dayName = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
)

// recentDirs are where the car writes Recent clips.
var recentDirs = []string{"TeslaCam/RecentClips", "TeslaCam/EncryptedClips/RecentClips"}

// Motion is what a clip's telemetry says about the car.
type Motion int

const (
	// NoTelemetry: the clip has none. The car records it only while it's on,
	// so once it has been seen in any clip, a clip without it was recorded
	// parked (Sentry, or sitting in the garage awake).
	NoTelemetry Motion = iota
	Parked             // in Park and still the whole time
	Moving

	unreadable Motion = -1 // couldn't be read: kept
)

// Store is the clip store.
type Store struct {
	Dir string
	Log *slog.Logger
	// Motion reads how the car moved during a clip. nil, or an error, keeps
	// the clip.
	Motion func(ctx context.Context, r io.ReaderAt, size int64) (Motion, error)
	// HomeCutoff, if set, returns when the car got home, in the car's clock
	// ("2006-01-02T15:04:05"), or "" while it's away. Minutes recorded after
	// it are left out unless the car moved in them: an awake car at home
	// records its garage (preconditioning, say), without telemetry, and
	// those minutes are never archived, so the store would keep them for
	// good. Moving minutes are kept whatever it says, as it's only cleared
	// a while after the car has left: the start of a drive can come after
	// it. Set it unless Recent clips are archived at home too.
	HomeCutoff func() string

	// hooks for tests
	cloneRange func(dst, src *os.File, srcOff, length, dstOff int64) error
	now        func() time.Time

	mu    sync.Mutex
	state state
}

type state struct {
	// Captured are the snapshots whose clips are in the store.
	Captured []string `json:"captured"`
	// Parked are clips left out as recorded parked, path -> size, so they
	// aren't read again by every snapshot that still has them.
	Parked map[string]int64 `json:"parked"`
	// Dropped counts minutes of driving deleted for space before they were
	// archived, the last time that happened.
	Dropped   int       `json:"dropped,omitempty"`
	DroppedAt time.Time `json:"dropped_at,omitzero"`
	Version   int64     `json:"version"`
	// Telemetry is set once a clip has had telemetry: from then on clips
	// without it count as parked.
	Telemetry bool `json:"telemetry,omitempty"`
}

// Status is what the API reports about the store.
type Status struct {
	Files   int   `json:"files"`
	Bytes   int64 `json:"bytes"`
	Minutes int   `json:"minutes"`
	// Dropped minutes of driving that had to go before they were archived.
	Dropped   int       `json:"dropped,omitempty"`
	DroppedAt time.Time `json:"dropped_at,omitzero"`
}

// Open opens the store in dir, creating it if needed, and removes clones
// that were interrupted.
func Open(dir string, log *slog.Logger) (*Store, error) {
	s := &Store{Dir: dir, Log: log, cloneRange: storage.CloneRange, now: time.Now}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(dir, stateName))
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(b, &s.state); err != nil {
			log.Warn("clip store state unreadable, starting over", "err", err)
			s.state = state{}
		}
	}
	if s.state.Parked == nil {
		s.state.Parked = map[string]int64{}
	}
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".part") {
			os.Remove(p)
		}
		return nil
	})
	return s, nil
}

// Version changes whenever files are added or removed.
func (s *Store) Version() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.Version
}

// Captured reports whether a snapshot's clips are in the store.
func (s *Store) Captured(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.state.Captured {
		if c == id {
			return true
		}
	}
	return false
}

// Forget drops a released snapshot from the captured list.
func (s *Store) Forget(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Captured = slicesDelete(s.state.Captured, id)
	if err := s.save(); err != nil {
		s.Log.Error("saving clip store state", "err", err)
	}
}

func slicesDelete(list []string, v string) []string {
	out := list[:0]
	for _, x := range list {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}

// Capture adds the Recent clips of a snapshot that the store doesn't have
// in full yet, skipping minutes recorded parked. image is the snapshot's
// disk image and mount where it's mounted (read to classify clips).
func (s *Store) Capture(ctx context.Context, id, image, mount string) error {
	img, err := os.Open(image)
	if err != nil {
		return err
	}
	defer img.Close()
	off, err := exfat.PartitionOffset(img)
	if err != nil {
		return err
	}
	fsys, err := exfat.Open(img, off)
	if err != nil {
		return err
	}

	// minute key -> the clip files of that minute
	minutes := map[string][]clipFile{}
	for _, dir := range recentDirs {
		list, err := fsys.ReadDir(dir)
		if errors.Is(err, exfat.ErrNotFound) {
			continue
		}
		if err != nil {
			return fmt.Errorf("listing %s: %w", dir, err)
		}
		for _, e := range list {
			switch {
			case e.Dir && dayName.MatchString(e.Name):
				sub, err := fsys.ReadDir(dir + "/" + e.Name)
				if err != nil {
					return fmt.Errorf("listing %s/%s: %w", dir, e.Name, err)
				}
				for _, se := range sub {
					addClip(minutes, dir+"/"+e.Name, se)
				}
			default:
				addClip(minutes, dir, e)
			}
		}
	}

	s.mu.Lock()
	parked := maps.Clone(s.state.Parked)
	s.mu.Unlock()
	cutoff := ""
	if s.HomeCutoff != nil {
		cutoff = s.HomeCutoff()
	}
	seen := map[string]int64{}
	added, skipped := 0, 0
	keys := make([]string, 0, len(minutes))
	for k := range minutes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	// First what each new minute is: whether a clip has telemetry decides
	// what the clips without it are, so all of them are read before any is
	// kept.
	type candidate struct {
		files, todo []clipFile
		motion      Motion
		home        bool
	}
	var candidates []candidate
	for _, k := range keys {
		if err := ctx.Err(); err != nil {
			return err
		}
		files := minutes[k]
		var todo []clipFile
		for _, f := range files {
			seen[f.rel] = f.entry.Size
			if parked[f.rel] == f.entry.Size {
				continue
			}
			if fi, err := os.Stat(s.path(f.rel)); err == nil && fi.Size() >= f.entry.Size {
				continue
			}
			todo = append(todo, f)
		}
		if len(todo) == 0 {
			continue
		}
		candidates = append(candidates, candidate{files: files, todo: todo,
			motion: s.motion(ctx, mount, files), home: recordedAfter(k, cutoff)})
	}
	s.mu.Lock()
	for _, c := range candidates {
		if c.motion == Moving || c.motion == Parked {
			s.state.Telemetry = true
		}
	}
	telemetry := s.state.Telemetry
	s.mu.Unlock()
	for _, c := range candidates {
		if err := ctx.Err(); err != nil {
			return err
		}
		atHome := c.home && (c.motion == Parked || c.motion == NoTelemetry)
		if atHome || c.motion == Parked || c.motion == NoTelemetry && telemetry {
			for _, f := range c.files {
				parked[f.rel] = f.entry.Size
			}
			skipped++
			continue
		}
		for _, f := range c.todo {
			if err := s.clone(fsys, img, f); err != nil {
				s.Log.Warn("couldn't keep clip", "file", f.rel, "err", err)
				continue
			}
			added++
		}
	}
	dropped, err := s.dropAfter(ctx, cutoff, parked, seen)
	if err != nil {
		s.Log.Warn("couldn't drop clips recorded at home", "err", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// forget parked clips the car no longer has
	for p, size := range parked {
		if seen[p] != size {
			delete(parked, p)
		}
	}
	s.state.Parked = parked
	if added > 0 || dropped > 0 {
		s.state.Version++
	}
	s.state.Captured = append(slicesDelete(s.state.Captured, id), id)
	if err := s.save(); err != nil {
		return err
	}
	s.Log.Info("kept clips from snapshot", "id", id, "clips", added, "parked_minutes_skipped", skipped)
	if dropped > 0 {
		s.Log.Info("dropped clips recorded at home", "clips", dropped, "home_since", cutoff)
	}
	return nil
}

// DropRecordedAtHome deletes the stored minutes recorded after the car got
// home in which it didn't move, as Capture does for each new snapshot: for
// the ones kept before there was one, or before this rule.
func (s *Store) DropRecordedAtHome(ctx context.Context) (int, error) {
	if s.HomeCutoff == nil {
		return 0, nil
	}
	cutoff := s.HomeCutoff()
	s.mu.Lock()
	parked := maps.Clone(s.state.Parked)
	s.mu.Unlock()
	n, err := s.dropAfter(ctx, cutoff, parked, nil)
	if n == 0 {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Parked = parked
	s.state.Version++
	s.Log.Info("dropped clips recorded at home", "clips", n, "home_since", cutoff)
	return n, errors.Join(err, s.save())
}

// recordedAfter reports whether the minute key (dir/2006-01-02_15-04-05)
// was recorded after cutoff, a clip time; "" is never, "0" always.
func recordedAfter(key, cutoff string) bool {
	if cutoff == "" {
		return false
	}
	t, err := time.Parse("2006-01-02_15-04-05", path.Base(key))
	if err != nil {
		return false
	}
	return t.Format("2006-01-02T15:04:05") > cutoff
}

// dropAfter deletes stored minutes recorded after cutoff in which the car
// didn't move: kept before the store knew the car was home. They become
// parked, so a snapshot that still has them doesn't bring them back.
func (s *Store) dropAfter(ctx context.Context, cutoff string, parked, onDrive map[string]int64) (int, error) {
	if cutoff == "" {
		return 0, nil
	}
	mins, err := s.minutes()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, m := range mins {
		start, dir, _ := strings.Cut(m.key, " ")
		if !recordedAfter(dir+"/"+start, cutoff) {
			continue
		}
		files := make([]clipFile, 0, len(m.files))
		for _, f := range m.files {
			if c := clipName.FindStringSubmatch(path.Base(f.rel)); c != nil {
				files = append(files, clipFile{rel: f.rel, camera: c[2]})
			}
		}
		if len(files) == 0 {
			continue
		}
		if motion := s.motion(ctx, s.Dir, files); motion != Parked && motion != NoTelemetry {
			continue
		}
		for _, f := range m.files {
			if err := os.Remove(s.path(f.rel)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return n, err
			}
			if size, ok := onDrive[f.rel]; ok {
				parked[f.rel] = size
			} else if onDrive == nil {
				parked[f.rel] = f.size
			}
			n++
		}
	}
	return n, nil
}

type clipFile struct {
	rel    string // slash-separated, from the drive root
	camera string
	entry  exfat.Entry
}

func addClip(minutes map[string][]clipFile, dir string, e exfat.Entry) {
	if e.Dir || e.Size < minClipSize {
		return
	}
	m := clipName.FindStringSubmatch(e.Name)
	if m == nil {
		return
	}
	key := dir + "/" + m[1]
	minutes[key] = append(minutes[key], clipFile{rel: dir + "/" + e.Name, camera: m[2], entry: e})
}

// motion classifies a minute by one of its clips: the front camera if it's
// there. Encrypted clips can't be read, nor can anything without Motion:
// those are unreadable, and kept.
func (s *Store) motion(ctx context.Context, mount string, files []clipFile) Motion {
	if s.Motion == nil || strings.Contains(files[0].rel, "/EncryptedClips/") {
		return unreadable
	}
	pick := files[0]
	for _, f := range files {
		if f.camera == "front" {
			pick = f
		}
	}
	f, err := os.Open(filepath.Join(mount, filepath.FromSlash(pick.rel)))
	if err != nil {
		return unreadable
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return unreadable
	}
	m, err := s.Motion(ctx, f, fi.Size())
	if err != nil {
		return unreadable
	}
	return m
}

func (s *Store) path(rel string) string { return filepath.Join(s.Dir, filepath.FromSlash(rel)) }

// cloneAlign is the block size clones must be aligned to (XFS).
const cloneAlign = 4096

// clone reflinks one clip out of the image into the store.
func (s *Store) clone(fsys *exfat.FS, img *os.File, f clipFile) error {
	ext, err := fsys.Extents(f.entry)
	if err != nil {
		return err
	}
	dst := s.path(f.rel)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp := dst + ".part"
	if err := s.cloneTo(tmp, img, ext, f.entry.Size); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

func (s *Store) cloneTo(tmp string, img *os.File, ext []exfat.Extent, size int64) (err error) {
	out, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
	}()
	var at int64
	for _, x := range ext {
		if x.Off%cloneAlign != 0 || x.Len%cloneAlign != 0 {
			return fmt.Errorf("clip at %d+%d isn't block aligned", x.Off, x.Len)
		}
		if err := s.cloneRange(out, img, x.Off, x.Len, at); err != nil {
			return err
		}
		at += x.Len
	}
	if err := out.Truncate(size); err != nil {
		return err
	}
	// a clip starts with an ftyp box; anything else means the extents are wrong
	var hdr [8]byte
	if _, err := out.ReadAt(hdr[:], 0); err != nil {
		return err
	}
	if string(hdr[4:8]) != "ftyp" {
		return errors.New("cloned data isn't an MP4 file")
	}
	return nil
}

// Prune deletes clips while low reports that space is short: archived ones
// first, then the oldest of the rest, a minute at a time. It returns how many
// minutes not yet archived it deleted.
func (s *Store) Prune(low func() bool, archived func(rel string, size int64) bool) (int, error) {
	if !low() {
		return 0, nil
	}
	minutes, err := s.minutes()
	if err != nil {
		return 0, err
	}
	removed := false
	defer func() {
		if removed {
			s.mu.Lock()
			s.state.Version++
			if err := s.save(); err != nil {
				s.Log.Error("saving clip store state", "err", err)
			}
			s.mu.Unlock()
		}
	}()
	dropped := 0
	for pass := 0; pass < 2; pass++ {
		for _, m := range minutes {
			if m.gone {
				continue
			}
			if !low() {
				return dropped, s.noteDropped(dropped)
			}
			done := archived != nil
			for _, f := range m.files {
				if done && !archived(f.rel, f.size) {
					done = false
				}
			}
			if pass == 0 && !done {
				continue
			}
			for _, f := range m.files {
				if err := os.Remove(s.path(f.rel)); err != nil && !errors.Is(err, fs.ErrNotExist) {
					return dropped, err
				}
			}
			m.gone, removed = true, true
			if !done {
				dropped++
				s.Log.Warn("out of space: deleted a minute of driving that wasn't archived yet", "minute", m.key)
			}
		}
	}
	return dropped, s.noteDropped(dropped)
}

func (s *Store) noteDropped(n int) error {
	if n == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.now().Sub(s.state.DroppedAt) < 24*time.Hour {
		s.state.Dropped += n
	} else {
		s.state.Dropped = n
	}
	s.state.DroppedAt = s.now()
	return s.save()
}

type storedFile struct {
	rel  string
	size int64
}

type minute struct {
	key   string // time, then folder, so they sort oldest first
	files []storedFile
	gone  bool
}

// minutes lists the stored clips by minute, oldest first.
func (s *Store) minutes() ([]*minute, error) {
	byKey := map[string]*minute{}
	err := filepath.WalkDir(s.Dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		m := clipName.FindStringSubmatch(d.Name())
		if m == nil {
			return nil
		}
		rel, err := filepath.Rel(s.Dir, p)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		key := m[1] + " " + path.Dir(rel)
		if byKey[key] == nil {
			byKey[key] = &minute{key: key}
		}
		byKey[key].files = append(byKey[key].files, storedFile{rel, fi.Size()})
		return nil
	})
	out := make([]*minute, 0, len(byKey))
	for _, m := range byKey {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key < out[j].key })
	return out, err
}

// Status counts what the store holds.
func (s *Store) Status() Status {
	minutes, _ := s.minutes()
	st := Status{Minutes: len(minutes)}
	for _, m := range minutes {
		for _, f := range m.files {
			st.Files++
			st.Bytes += f.size
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st.Dropped, st.DroppedAt = s.state.Dropped, s.state.DroppedAt
	return st
}

func (s *Store) save() error {
	b, err := json.Marshal(s.state)
	if err != nil {
		return err
	}
	return storage.WriteFileAtomic(filepath.Join(s.Dir, stateName), b, 0o644)
}

// Usage is what the store holds, split by whether it's archived.
type Usage struct {
	Bytes           int64 `json:"bytes"`
	Minutes         int   `json:"minutes"`
	ArchivedBytes   int64 `json:"archived_bytes"`
	ArchivedMinutes int   `json:"archived_minutes"`
}

// Usage counts the kept clips; a minute counts as archived once all its
// clips are.
func (s *Store) Usage(archived func(rel string, size int64) bool) Usage {
	minutes, _ := s.minutes()
	var u Usage
	for _, m := range minutes {
		var b int64
		done := archived != nil
		for _, f := range m.files {
			b += f.size
			if done && !archived(f.rel, f.size) {
				done = false
			}
		}
		u.Bytes += b
		u.Minutes++
		if done {
			u.ArchivedBytes += b
			u.ArchivedMinutes++
		}
	}
	return u
}

// IsParked reports whether the Recent clip at rel, at this size, was left
// out as recorded parked.
func (s *Store) IsParked(rel string, size int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	got, ok := s.state.Parked[rel]
	return ok && got == size
}

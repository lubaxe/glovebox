package clips

import (
	"errors"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// ErrNotFound means a file isn't in the index.
var ErrNotFound = errors.New("no such clip file")

// Library keeps an Index in step with the current snapshots and opens the
// files it lists.
type Library struct {
	*Index
	// Snapshots returns the mounted snapshots.
	Snapshots func() ([]Snapshot, error)
}

// NewLibrary returns a Library over the snapshots that list returns.
func NewLibrary(list func() ([]Snapshot, error)) *Library {
	return &Library{Index: New(), Snapshots: list}
}

// Refresh updates the index from the current snapshots and returns them
// by id.
func (l *Library) Refresh() (map[string]Snapshot, error) {
	snaps, err := l.Snapshots()
	if err != nil {
		return nil, err
	}
	byID := make(map[string]Snapshot, len(snaps))
	for _, s := range snaps {
		byID[s.ID] = s
	}
	l.Index.Refresh(snaps)
	return byID, nil
}

// Open opens a file from the index. rel is the slash-separated path on the
// drive; anything that isn't an indexed clip, thumbnail or event.json is
// refused, so callers can't reach the rest of the filesystem.
func (l *Library) Open(snapshot, rel string) (*os.File, error) {
	if rel != path.Clean(rel) || strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, "../") {
		return nil, ErrNotFound
	}
	snaps, err := l.Refresh()
	if err != nil {
		return nil, err
	}
	if _, ok := l.File(snapshot, rel); !ok {
		return nil, ErrNotFound
	}
	snap, ok := snaps[snapshot]
	if !ok {
		return nil, ErrNotFound
	}
	return os.Open(filepath.Join(snap.Mount, filepath.FromSlash(rel)))
}

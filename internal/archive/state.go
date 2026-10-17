package archive

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// State records which drive files have been archived and at what size, so
// nothing is uploaded twice and a file that grew (a snapshot first caught it
// mid-recording) is uploaded again.
type State struct {
	path  string
	mu    sync.Mutex
	files map[string]int64
	// target is where the files went (see config.ArchiveConfig.Destination)
	target string
}

type stateFile struct {
	Target string           `json:"target,omitempty"`
	Files  map[string]int64 `json:"files"`
}

// ErrStateReset is returned with an empty State when the state file was
// unreadable. It's moved aside to <path>.corrupt. Archiving can carry on:
// files already on the share at full size aren't uploaded again.
var ErrStateReset = errors.New("archive state was unreadable; starting over")

// LoadState reads the state file at path; a missing file is an empty state.
func LoadState(path string) (*State, error) {
	s := &State{path: path, files: map[string]int64{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var f stateFile
	if err := json.Unmarshal(b, &f); err != nil {
		_ = os.Rename(path, path+".corrupt")
		return s, fmt.Errorf("%w (%v)", ErrStateReset, err)
	}
	if f.Files != nil {
		s.files = f.Files
	}
	s.target = f.Target
	return s, nil
}

// Done reports whether rel was archived at size or bigger.
func (s *State) Done(rel string, size int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	got, ok := s.files[rel]
	return ok && got >= size
}

// Mark records that rel was archived at size.
func (s *State) Mark(rel string, size int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files[rel] = size
}

// Prune forgets files that aren't in keep (they're gone from every
// snapshot), so the state doesn't grow forever.
func (s *State) Prune(keep map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for rel := range s.files {
		if !keep[rel] {
			delete(s.files, rel)
		}
	}
}

// Count returns how many files are recorded.
func (s *State) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.files)
}

// Save writes the state atomically.
func (s *State) Save() error {
	s.mu.Lock()
	b, err := json.Marshal(stateFile{Target: s.target, Files: s.files})
	s.mu.Unlock()
	if err != nil {
		return err
	}
	// The Pi loses power whenever the car sleeps. Flush the new file before
	// renaming it over the old one, or XFS may keep the rename but not the
	// data, leaving an empty file.
	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	// and make the rename itself durable
	if d, err := os.Open(filepath.Dir(s.path)); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}

// UseTarget says where files are archived to. If the state is for another
// target, it starts over: nothing is on the new one yet, so nothing may be
// skipped or deleted from the car's drive as archived. It reports whether
// it started over. A state from before targets were recorded is kept.
func (s *State) UseTarget(target string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	reset := s.target != "" && s.target != target && len(s.files) > 0
	if reset {
		s.files = map[string]int64{}
	}
	s.target = target
	return reset
}

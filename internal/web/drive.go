package web

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/lubaxe/glovebox/internal/drive"
	"github.com/lubaxe/glovebox/internal/snapshot"
)

// Drive is what the API needs from the drive.
type Drive interface {
	Status() (drive.Status, error)
	TakeSnapshot()
}

// SnapshotStore is what the API needs from the snapshot manager.
type SnapshotStore interface {
	List() ([]snapshot.Snapshot, error)
	Release(ctx context.Context, id string) error
}

// maxFiles caps how many files /api/snapshots/{id}/files returns.
const maxFiles = 20000

type fileEntry struct {
	Path     string    `json:"path"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

func (s *Server) driveRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/drive", s.needDrive(s.handleDrive))
	mux.HandleFunc("GET /api/snapshots", s.needDrive(s.handleSnapshots))
	mux.HandleFunc("POST /api/snapshots", s.needDrive(s.handleTakeSnapshot))
	mux.HandleFunc("DELETE /api/snapshots/{id}", s.needDrive(s.handleReleaseSnapshot))
	mux.HandleFunc("GET /api/snapshots/{id}/files", s.needDrive(s.handleSnapshotFiles))
}

func (s *Server) needDrive(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Drive == nil || s.Snapshots == nil {
			http.Error(w, "the drive is not enabled (drive.enabled in the config)", http.StatusServiceUnavailable)
			return
		}
		h(w, r)
	}
}

func (s *Server) handleDrive(w http.ResponseWriter, r *http.Request) {
	st, err := s.Drive.Status()
	if err != nil {
		s.fail(w, "reading drive status", err)
		return
	}
	writeJSON(w, st)
}

func (s *Server) handleSnapshots(w http.ResponseWriter, r *http.Request) {
	snaps, err := s.Snapshots.List()
	if err != nil {
		s.fail(w, "listing snapshots", err)
		return
	}
	if snaps == nil {
		snaps = []snapshot.Snapshot{}
	}
	writeJSON(w, snaps)
}

func (s *Server) handleTakeSnapshot(w http.ResponseWriter, r *http.Request) {
	s.Drive.TakeSnapshot()
	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, map[string]string{"status": "snapshot requested"})
}

func (s *Server) handleReleaseSnapshot(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.findSnapshot(w, r.PathValue("id")); !ok {
		return
	}
	if err := s.Snapshots.Release(r.Context(), r.PathValue("id")); err != nil {
		s.fail(w, "deleting snapshot", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleSnapshotFiles(w http.ResponseWriter, r *http.Request) {
	snap, ok := s.findSnapshot(w, r.PathValue("id"))
	if !ok {
		return
	}
	files := []fileEntry{}
	truncated := false
	err := filepath.WalkDir(snap.Mount, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if len(files) == maxFiles {
			truncated = true
			return fs.SkipAll
		}
		info, err := d.Info()
		if err != nil {
			return nil // vanished or unreadable entry; skip it
		}
		rel, _ := filepath.Rel(snap.Mount, path)
		files = append(files, fileEntry{Path: filepath.ToSlash(rel), Size: info.Size(), Modified: info.ModTime()})
		return nil
	})
	if err != nil {
		s.fail(w, "listing snapshot files", err)
		return
	}
	writeJSON(w, map[string]any{"snapshot": snap.ID, "files": files, "truncated": truncated})
}

// findSnapshot looks up id and writes a 404 if it doesn't exist.
func (s *Server) findSnapshot(w http.ResponseWriter, id string) (snapshot.Snapshot, bool) {
	snaps, err := s.Snapshots.List()
	if err != nil {
		s.fail(w, "listing snapshots", err)
		return snapshot.Snapshot{}, false
	}
	for _, snap := range snaps {
		if snap.ID == id {
			if _, err := os.Stat(snap.Mount); errors.Is(err, fs.ErrNotExist) {
				http.Error(w, "snapshot is not mounted", http.StatusConflict)
				return snapshot.Snapshot{}, false
			}
			return snap, true
		}
	}
	http.Error(w, "no such snapshot", http.StatusNotFound)
	return snapshot.Snapshot{}, false
}

func (s *Server) fail(w http.ResponseWriter, what string, err error) {
	s.Log.Error(what, "err", err)
	http.Error(w, what+" failed", http.StatusInternalServerError)
}

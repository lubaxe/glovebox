package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/lubaxe/glovebox/internal/archive"
	"github.com/lubaxe/glovebox/internal/drive"
)

// Sizing is what the storage section of Settings needs: how the card is
// used, and changing the size of the car's drive.
type Sizing interface {
	Storage() (drive.Storage, error)
	// Resize asks for the car's drive to be recreated at size on the next
	// start, and saves the size in the config.
	Resize(ctx context.Context, size int64) error
	CancelResize() error
}

type storageResponse struct {
	drive.Storage
	// Archived: the last backup finished with nothing left over; resizing
	// erases the car's drive. Backups says whether they're set up at all.
	Archived bool `json:"archived"`
	Backups  bool `json:"backups"`
}

func (s *Server) storageRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/storage", s.needSizing(s.handleStorage))
	mux.HandleFunc("POST /api/storage/resize", s.needSizing(s.handleResize))
	mux.HandleFunc("DELETE /api/storage/resize", s.needSizing(s.handleCancelResize))
}

// needSizing also refuses changes that don't come as JSON (see needSetup).
func (s *Server) needSizing(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Sizing == nil {
			http.Error(w, "the drive is not enabled (drive.enabled in the config)", http.StatusServiceUnavailable)
			return
		}
		if r.Method == http.MethodPost && r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "send JSON", http.StatusUnsupportedMediaType)
			return
		}
		h(w, r)
	}
}

// outstander counts what isn't archived yet (archive.Archiver does).
type outstander interface {
	Outstanding() (int, error)
}

// archivedAll reports whether everything on the drive is archived, and
// whether archiving is set up at all.
func (s *Server) archivedAll() (archived, backups bool) {
	if s.Archive == nil {
		return false, false
	}
	a := s.Archive.Status()
	if a.State == archive.Archiving {
		return false, true
	}
	if o, ok := s.Archive.(outstander); ok {
		n, err := o.Outstanding()
		return err == nil && n == 0, true
	}
	// without a count, go by the last run
	return a.Last != nil && a.Last.Err == "" && a.Last.Failed == 0 && a.Last.Pending == 0, true
}

func (s *Server) handleStorage(w http.ResponseWriter, r *http.Request) {
	st, err := s.Sizing.Storage()
	if err != nil {
		s.fail(w, "reading storage", err)
		return
	}
	archived, backups := s.archivedAll()
	writeJSON(w, storageResponse{Storage: st, Archived: archived, Backups: backups})
}

func (s *Server) handleResize(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Size    int64  `json:"size"`
		Confirm string `json:"confirm"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if body.Confirm != eraseWord {
		userError(w, errors.New("type "+eraseWord+" to confirm"))
		return
	}
	// Without backups set up there's nothing to wait for; the page says
	// what's lost.
	if archived, backups := s.archivedAll(); backups && !archived {
		userError(w, errors.New("wait until everything is backed up to the NAS: this erases the car's drive"))
		return
	}
	if err := s.Sizing.Resize(r.Context(), body.Size); err != nil {
		if errors.Is(err, drive.ErrBadSize) {
			userError(w, err)
			return
		}
		s.fail(w, "resizing the car's drive", err)
		return
	}
	s.Log.Warn("recreating the car's drive at a new size was asked for; restarting to do it", "size", body.Size)
	s.handleStorage(w, r)
	if s.Restart != nil {
		s.Restart()
	}
}

func (s *Server) handleCancelResize(w http.ResponseWriter, r *http.Request) {
	if err := s.Sizing.CancelResize(); err != nil {
		s.fail(w, "cancelling the resize", err)
		return
	}
	s.handleStorage(w, r)
}

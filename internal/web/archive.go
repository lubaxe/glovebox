package web

import (
	"net/http"

	"github.com/lubaxe/glovebox/internal/archive"
)

// Archive is what the API needs from the archiver.
type Archive interface {
	Status() archive.Status
	Trigger()
}

func (s *Server) archiveRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/archive", s.needArchive(s.handleArchive))
	mux.HandleFunc("POST /api/archive/run", s.needArchive(s.handleArchiveRun))
}

func (s *Server) needArchive(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Archive == nil {
			http.Error(w, "archiving is not enabled (archive.enabled in the config)", http.StatusServiceUnavailable)
			return
		}
		h(w, r)
	}
}

func (s *Server) handleArchive(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.Archive.Status())
}

func (s *Server) handleArchiveRun(w http.ResponseWriter, r *http.Request) {
	s.Archive.Trigger()
	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, map[string]string{"status": "archive run requested"})
}

package web

import (
	"context"
	"net/http"

	"github.com/lubaxe/glovebox/internal/update"
)

// Updater is what the update section of the setup page needs (see
// package update).
type Updater interface {
	Status(ctx context.Context, check bool) update.Status
	Check(ctx context.Context) update.Status
	Install() error
}

func (s *Server) updateRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/update", s.needUpdater(s.handleUpdate))
	mux.HandleFunc("POST /api/update/check", s.needUpdater(s.handleCheckUpdate))
	mux.HandleFunc("POST /api/update/install", s.needUpdater(s.handleInstallUpdate))
}

// needUpdater also refuses changes that don't come as JSON, which a page on
// another site can't make a browser send.
func (s *Server) needUpdater(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Update == nil {
			http.Error(w, "updates aren't available", http.StatusServiceUnavailable)
			return
		}
		if r.Method != http.MethodGet && r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "send JSON", http.StatusUnsupportedMediaType)
			return
		}
		h(w, r)
	}
}

// handleUpdate returns what's known; ?check=1 checks first if the last
// check is old.
func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.Update.Status(r.Context(), r.URL.Query().Get("check") != ""))
}

func (s *Server) handleCheckUpdate(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.Update.Check(r.Context()))
}

func (s *Server) handleInstallUpdate(w http.ResponseWriter, r *http.Request) {
	if err := s.Update.Install(); err != nil {
		userError(w, err)
		return
	}
	s.Log.Info("installing an update")
	writeJSON(w, s.Update.Status(r.Context(), false))
}

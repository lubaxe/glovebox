package web

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/lubaxe/glovebox/internal/archive"
	"github.com/lubaxe/glovebox/internal/setup"
)

// Setup is what the setup page needs (see package setup).
type Setup interface {
	Get() (setup.Settings, error)
	Test(ctx context.Context, in setup.Settings) []archive.CheckStep
	Save(in setup.Settings) error
	SSHKey(ctx context.Context) (string, error)
	SetRcloneConfig(text string) ([]string, error)
}

func (s *Server) setupRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/setup", s.needSetup(s.handleSetup))
	mux.HandleFunc("PUT /api/setup", s.needSetup(s.handleSaveSetup))
	mux.HandleFunc("POST /api/setup/test", s.needSetup(s.handleTestSetup))
	mux.HandleFunc("POST /api/setup/ssh-key", s.needSetup(s.handleSSHKey))
	mux.HandleFunc("PUT /api/setup/rclone-config", s.needSetup(s.handleRcloneConfig))
}

// needSetup also refuses changes that don't come as JSON, which a page on
// another site can't make a browser send.
func (s *Server) needSetup(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Setup == nil {
			http.Error(w, "setup isn't available", http.StatusServiceUnavailable)
			return
		}
		if r.Method != http.MethodGet && r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "send JSON", http.StatusUnsupportedMediaType)
			return
		}
		h(w, r)
	}
}

// userError reports a problem the user can fix, as {"error": ...}.
func userError(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnprocessableEntity)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func readSettings(w http.ResponseWriter, r *http.Request) (setup.Settings, bool) {
	var in setup.Settings
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return in, false
	}
	return in, true
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	st, err := s.Setup.Get()
	if err != nil {
		s.fail(w, "reading the settings", err)
		return
	}
	writeJSON(w, st)
}

func (s *Server) handleTestSetup(w http.ResponseWriter, r *http.Request) {
	in, ok := readSettings(w, r)
	if !ok {
		return
	}
	writeJSON(w, map[string]any{"steps": s.Setup.Test(r.Context(), in)})
}

func (s *Server) handleSaveSetup(w http.ResponseWriter, r *http.Request) {
	in, ok := readSettings(w, r)
	if !ok {
		return
	}
	if err := s.Setup.Save(in); err != nil {
		userError(w, err)
		return
	}
	s.Log.Info("settings saved on the setup page", "archive", in.Archive.Enabled, "type", in.Archive.Type)
	writeJSON(w, map[string]bool{"restarting": s.Restart != nil})
	if s.Restart != nil {
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		s.Restart()
	}
}

func (s *Server) handleSSHKey(w http.ResponseWriter, r *http.Request) {
	pub, err := s.Setup.SSHKey(r.Context())
	if err != nil {
		userError(w, err)
		return
	}
	writeJSON(w, map[string]string{"public_key": pub})
}

func (s *Server) handleRcloneConfig(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Config string `json:"config"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	remotes, err := s.Setup.SetRcloneConfig(body.Config)
	if err != nil {
		userError(w, err)
		return
	}
	writeJSON(w, map[string]any{"remotes": remotes})
}

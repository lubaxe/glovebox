// Package web serves glovebox's HTTP API and web UI.
package web

import (
	"embed"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"

	"github.com/lubaxe/glovebox/internal/clips"
	"github.com/lubaxe/glovebox/internal/sysinfo"
)

//go:embed ui
var uiFiles embed.FS

// StatusSource provides the system status shown by /api/status.
type StatusSource interface {
	Read() (sysinfo.Info, error)
}

// Server holds what the HTTP handlers need.
type Server struct {
	Version string
	Status  StatusSource
	Log     *slog.Logger
	// Drive, Snapshots and Clips are nil when the drive is disabled.
	Drive     Drive
	Snapshots SnapshotStore
	Clips     *clips.Library
	// Archive is nil when archiving is disabled.
	Archive Archive
	// Car and KeepAwake are nil when keep-awake is off.
	Car       Car
	KeepAwake KeepAwake
	// Setup is the setup page; Restart, if set, restarts Glovebox after
	// the settings are saved.
	Setup   Setup
	Restart func()
	// WiFi is nil where Glovebox doesn't manage Wi-Fi.
	WiFi WiFi
	// Update installs new releases.
	Update Updater
	// Encryption is nil when the drive is disabled.
	Encryption Encryption
	// Sizing is nil when the drive is disabled.
	Sizing Sizing
}

// Handler returns the HTTP handler for all routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", s.handleStatus)
	s.driveRoutes(mux)
	s.clipRoutes(mux)
	s.zipRoutes(mux)
	s.archiveRoutes(mux)
	s.carRoutes(mux)
	s.setupRoutes(mux)
	s.wifiRoutes(mux)
	s.updateRoutes(mux)
	s.encryptionRoutes(mux)
	s.storageRoutes(mux)
	ui, err := fs.Sub(uiFiles, "ui")
	if err != nil {
		panic(err) // the embedded directory is always there
	}
	files := http.FileServerFS(ui)
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		// The embedded files have no modification time, so without this a
		// browser may keep showing the UI from before an update.
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	})
	return mux
}

type statusResponse struct {
	Version string       `json:"version"`
	System  sysinfo.Info `json:"system"`
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	info, err := s.Status.Read()
	if err != nil {
		s.Log.Error("reading system status", "err", err)
		http.Error(w, "couldn't read system status", http.StatusInternalServerError)
		return
	}
	writeJSON(w, statusResponse{Version: s.Version, System: info})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

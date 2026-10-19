package web

import (
	"errors"
	"github.com/lubaxe/glovebox/internal/clips"
	"github.com/lubaxe/glovebox/internal/telemetry"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// Bound concurrent scans on a Pi. Telemetry never reads video pixels or shells out.
var telemetrySlots = make(chan struct{}, 2)

func (s *Server) handleTelemetry(w http.ResponseWriter, r *http.Request) {
	rel := r.PathValue("path")
	if strings.ToLower(path.Ext(rel)) != ".mp4" || strings.Contains(rel, "/EncryptedClips/") {
		http.Error(w, "telemetry requires a playable MP4", http.StatusBadRequest)
		return
	}
	file, err := s.Clips.Open(r.PathValue("snapshot"), rel)
	if errors.Is(err, clips.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
		http.Error(w, "no such recording", http.StatusNotFound)
		return
	}
	if err != nil {
		s.fail(w, "opening telemetry recording", err)
		return
	}
	defer file.Close()
	select {
	case telemetrySlots <- struct{}{}:
		defer func() { <-telemetrySlots }()
	case <-r.Context().Done():
		return
	}
	info, err := file.Stat()
	if err != nil {
		s.fail(w, "reading telemetry recording", err)
		return
	}
	result, err := telemetry.Extract(r.Context(), file, info.Size())
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		http.Error(w, "telemetry could not be read from this recording", http.StatusUnprocessableEntity)
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=86400, immutable")
	writeJSON(w, result)
}

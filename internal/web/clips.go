package web

import (
	"errors"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"

	"github.com/lubaxe/glovebox/internal/clips"
	"github.com/lubaxe/glovebox/internal/mp4fix"
)

func (s *Server) clipRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/telemetry/{snapshot}/{path...}", s.needClips(s.handleTelemetry))
	mux.HandleFunc("GET /api/events", s.needClips(s.handleEvents))
	mux.HandleFunc("GET /api/events/{kind}/{id}", s.needClips(s.handleEvent))
	mux.HandleFunc("GET /api/recent", s.needClips(s.handleRecent))
	mux.HandleFunc("GET /media/{snapshot}/{path...}", s.needClips(s.handleMedia))
}

// needClips makes sure the index is current before the handler runs.
func (s *Server) needClips(h http.HandlerFunc) http.HandlerFunc {
	return s.needDrive(func(w http.ResponseWriter, r *http.Request) {
		if s.Clips == nil {
			http.Error(w, "clip index unavailable", http.StatusServiceUnavailable)
			return
		}
		if _, err := s.Clips.Refresh(); err != nil {
			s.fail(w, "listing snapshots", err)
			return
		}
		h(w, r)
	})
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	kind := r.URL.Query().Get("kind")
	if kind == "" {
		kind = clips.Sentry
	}
	if kind != clips.Sentry && kind != clips.Saved {
		http.Error(w, "kind must be sentry or saved", http.StatusBadRequest)
		return
	}
	writeJSON(w, s.Clips.Events(kind))
}

func (s *Server) handleEvent(w http.ResponseWriter, r *http.Request) {
	encrypted := r.URL.Query().Get("encrypted") == "1"
	ev, ok := s.Clips.Event(r.PathValue("kind"), r.PathValue("id"), encrypted)
	if !ok {
		http.Error(w, "no such event", http.StatusNotFound)
		return
	}
	writeJSON(w, ev)
}

func (s *Server) handleRecent(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.Clips.Recent())
}

// handleMedia serves a clip, thumbnail or event.json from a snapshot; with
// ?keyframes, a clip's keyframes only. Only
// files in the index can be served, so the rest of the drive (and the Pi)
// isn't reachable through this route.
func (s *Server) handleMedia(w http.ResponseWriter, r *http.Request) {
	rel := r.PathValue("path")
	f, err := s.Clips.Open(r.PathValue("snapshot"), rel)
	if errors.Is(err, clips.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
		http.Error(w, "no such file", http.StatusNotFound)
		return
	}
	if err != nil {
		s.fail(w, "opening file", err)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		s.fail(w, "reading file", err)
		return
	}

	name := path.Base(rel)
	if r.URL.Query().Has("download") {
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	}
	// Files in a snapshot never change.
	w.Header().Set("Cache-Control", "private, max-age=86400, immutable")

	var content io.ReadSeeker = f
	if strings.HasSuffix(name, ".mp4") && r.URL.Query().Has("keyframes") {
		// low-bandwidth playback: 2 frames a second
		keys, err := mp4fix.Keyframes(f, fi.Size())
		if errors.Is(err, mp4fix.ErrNoKeyframes) {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		if err != nil {
			s.fail(w, "reading mp4", err)
			return
		}
		content = keys
	} else if strings.HasSuffix(name, ".mp4") {
		fixed, err := mp4fix.NewFile(f, fi.Size())
		if err != nil {
			s.fail(w, "reading mp4", err)
			return
		}
		content = fixed
	}
	http.ServeContent(w, r, name, fi.ModTime(), content)
}

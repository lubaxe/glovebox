package web

import (
	"archive/zip"
	"io"
	"mime"
	"net/http"
	"path"
	"sort"
	"strings"

	"github.com/lubaxe/glovebox/internal/clips"
)

// Zip downloads bundle every camera of an event or a stretch of recent
// footage into one file, mostly for encrypted clips, which have to be
// handed to dashcam.tesla.com by hand.

func (s *Server) zipRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/events/{kind}/{id}/zip", s.needClips(s.handleEventZip))
	mux.HandleFunc("GET /api/recent/zip", s.needClips(s.handleRecentZip))
}

func (s *Server) handleEventZip(w http.ResponseWriter, r *http.Request) {
	ev, ok := s.Clips.Event(r.PathValue("kind"), r.PathValue("id"), r.URL.Query().Get("encrypted") == "1")
	if !ok {
		http.Error(w, "no such event", http.StatusNotFound)
		return
	}
	files := clipFiles(ev.Clips)
	if ev.Meta != nil {
		files = append(files, *ev.Meta)
	}
	if ev.Thumb != nil {
		files = append(files, *ev.Thumb)
	}
	s.writeZip(w, r, ev.Kind+"-"+ev.ID, files)
}

// handleRecentZip bundles the recent clips that start between from and to
// (the car's local time, e.g. 2026-10-16T20:54:26), inclusive.
func (s *Server) handleRecentZip(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	from, to := q.Get("from"), q.Get("to")
	if from == "" || to == "" {
		http.Error(w, "from and to are required", http.StatusBadRequest)
		return
	}
	encrypted := q.Get("encrypted") == "1"
	var picked []clips.Clip
	for _, c := range s.Clips.Recent() {
		if c.Encrypted == encrypted && c.Start >= from && c.Start <= to {
			picked = append(picked, c)
		}
	}
	if len(picked) == 0 {
		http.Error(w, "no clips in that range", http.StatusNotFound)
		return
	}
	s.writeZip(w, r, "drive-"+strings.NewReplacer(":", "-", "T", "_").Replace(from), clipFiles(picked))
}

func clipFiles(cs []clips.Clip) []clips.File {
	var files []clips.File
	for _, c := range cs {
		for _, f := range c.Cameras {
			files = append(files, f)
		}
	}
	sort.Slice(files, func(i, j int) bool { return path.Base(files[i].Path) < path.Base(files[j].Path) })
	return files
}

// writeZip streams files into a zip named name.zip, with each file at
// name/<file name>. Videos are already compressed, so they're stored as is.
func (s *Server) writeZip(w http.ResponseWriter, r *http.Request, name string, files []clips.File) {
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name + ".zip"}))
	zw := zip.NewWriter(w)
	for _, f := range files {
		if err := r.Context().Err(); err != nil {
			return // the download was cancelled
		}
		src, err := s.Clips.Open(f.Snapshot, f.Path)
		if err != nil {
			s.Log.Warn("zip: skipping a file", "file", f.Path, "err", err)
			continue
		}
		fi, err := src.Stat()
		if err != nil {
			src.Close()
			continue
		}
		hdr := &zip.FileHeader{Name: name + "/" + path.Base(f.Path), Method: zip.Store, Modified: fi.ModTime()}
		hdr.SetMode(0o644)
		dst, err := zw.CreateHeader(hdr)
		if err == nil {
			_, err = io.Copy(dst, src)
		}
		src.Close()
		if err != nil {
			s.Log.Warn("zip: download stopped", "err", err)
			return
		}
	}
	_ = zw.Close()
}

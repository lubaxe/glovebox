package web

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/lubaxe/glovebox/internal/wifi"
)

// WiFi is what the first-run wizard needs from the Wi-Fi manager.
type WiFi interface {
	Status(ctx context.Context) wifi.Status
	Scan(ctx context.Context) ([]wifi.Network, error)
	Join(ctx context.Context, ssid, password, country string) wifi.JoinResult
	FinishSetup() error
}

func (s *Server) wifiRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/wifi", s.needWiFi(s.handleWiFi))
	mux.HandleFunc("GET /api/wifi/networks", s.needWiFi(s.handleNetworks))
	mux.HandleFunc("POST /api/wifi/join", s.needWiFi(s.handleJoin))
	mux.HandleFunc("POST /api/setup/finish", s.needWiFi(s.handleFinishSetup))
}

// needWiFi also refuses changes that don't come as JSON.
func (s *Server) needWiFi(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.WiFi == nil {
			http.Error(w, "Wi-Fi isn't managed here", http.StatusServiceUnavailable)
			return
		}
		if r.Method != http.MethodGet && r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "send JSON", http.StatusUnsupportedMediaType)
			return
		}
		h(w, r)
	}
}

func (s *Server) handleWiFi(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.WiFi.Status(r.Context()))
}

func (s *Server) handleNetworks(w http.ResponseWriter, r *http.Request) {
	nets, err := s.WiFi.Scan(r.Context())
	if err != nil {
		userError(w, err)
		return
	}
	writeJSON(w, nets)
}

func (s *Server) handleJoin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SSID     string `json:"ssid"`
		Password string `json:"password"`
		Country  string `json:"country"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	// The phone may lose this connection while the radio changes channel;
	// the page then asks for the result with GET /api/wifi.
	res := s.WiFi.Join(context.WithoutCancel(r.Context()), body.SSID, body.Password, body.Country)
	writeJSON(w, res)
}

func (s *Server) handleFinishSetup(w http.ResponseWriter, r *http.Request) {
	if err := s.WiFi.FinishSetup(); err != nil {
		s.fail(w, "finishing setup", err)
		return
	}
	s.Log.Info("first-run setup finished")
	writeJSON(w, s.WiFi.Status(r.Context()))
}

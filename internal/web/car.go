package web

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/lubaxe/glovebox/internal/keepawake"
)

// Car is what the API needs to pair with the car.
type Car interface {
	VIN() string
	SetVIN(vin string) error
	Paired() (time.Time, bool)
	Role() string
	RequestPairing(ctx context.Context) error
	CheckPairing(ctx context.Context) error
	Forget() error
	SleepState(ctx context.Context) (string, error)
}

// KeepAwake is what the API shows about keep-awake, and the Car page's
// button to keep the car awake by hand.
type KeepAwake interface {
	Status() keepawake.Status
	Hold(d time.Duration)
}

type carResponse struct {
	VIN       string            `json:"vin"`
	Paired    bool              `json:"paired"`
	PairedAt  *time.Time        `json:"paired_at,omitempty"`
	Role      string            `json:"role"` // the key's, see keepawake.Driver
	KeepAwake *keepawake.Status `json:"keep_awake,omitempty"`
}

func (s *Server) carRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/car", s.needCar(s.handleCar))
	mux.HandleFunc("PUT /api/car/vin", s.needCar(s.handleSetVIN))
	mux.HandleFunc("POST /api/car/pair", s.needCar(s.handlePair))
	mux.HandleFunc("POST /api/car/check", s.needCar(s.handleCheckPairing))
	mux.HandleFunc("POST /api/car/forget", s.needCar(s.handleForget))
	mux.HandleFunc("GET /api/car/state", s.needCar(s.handleCarState))
	mux.HandleFunc("POST /api/car/keep-awake", s.needCar(s.handleHold))
}

// needCar also refuses changes that don't come as JSON: a web page on
// another site can make a browser send a form here, but not JSON.
func (s *Server) needCar(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Car == nil {
			http.Error(w, "keep-awake is off (keep_awake.enabled in the config) or the drive isn't running", http.StatusServiceUnavailable)
			return
		}
		if r.Method != http.MethodGet && r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "send JSON", http.StatusUnsupportedMediaType)
			return
		}
		h(w, r)
	}
}

func (s *Server) handleCar(w http.ResponseWriter, r *http.Request) {
	resp := carResponse{VIN: s.Car.VIN(), Role: s.Car.Role()}
	if t, ok := s.Car.Paired(); ok {
		resp.Paired = true
		if !t.IsZero() {
			resp.PairedAt = &t
		}
	}
	if s.KeepAwake != nil {
		st := s.KeepAwake.Status()
		resp.KeepAwake = &st
	}
	writeJSON(w, resp)
}

// carError reports a failure the user can act on, with the car's own words.
func carError(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnprocessableEntity)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func (s *Server) handleSetVIN(w http.ResponseWriter, r *http.Request) {
	var body struct {
		VIN string `json:"vin"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.Car.SetVIN(body.VIN); err != nil {
		carError(w, err)
		return
	}
	s.handleCar(w, r)
}

func (s *Server) handlePair(w http.ResponseWriter, r *http.Request) {
	if err := s.Car.RequestPairing(r.Context()); err != nil {
		s.Log.Warn("pairing request failed", "err", err)
		carError(w, err)
		return
	}
	s.Log.Info("sent the car a pairing request", "vin", s.Car.VIN())
	s.handleCar(w, r)
}

func (s *Server) handleCheckPairing(w http.ResponseWriter, r *http.Request) {
	if err := s.Car.CheckPairing(r.Context()); err != nil {
		carError(w, err)
		return
	}
	s.Log.Info("the car accepted the key", "vin", s.Car.VIN())
	s.handleCar(w, r)
}

func (s *Server) handleForget(w http.ResponseWriter, r *http.Request) {
	if err := s.Car.Forget(); err != nil {
		s.fail(w, "forgetting the car", err)
		return
	}
	s.handleCar(w, r)
}

// maxHold is the longest the Car page can keep the car awake for.
const maxHold = 2 * time.Hour

// handleHold keeps the car awake for the given minutes; 0 stops.
func (s *Server) handleHold(w http.ResponseWriter, r *http.Request) {
	if s.KeepAwake == nil {
		http.Error(w, "keep-awake needs archiving set up", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Minutes int `json:"minutes"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&body); err != nil || body.Minutes < 0 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	d := min(time.Duration(body.Minutes)*time.Minute, maxHold)
	s.KeepAwake.Hold(d)
	if d > 0 {
		s.Log.Info("keeping the car awake, as asked on the Car page", "for", d)
	} else {
		s.Log.Info("stopped keeping the car awake, as asked on the Car page")
	}
	s.handleCar(w, r)
}

func (s *Server) handleCarState(w http.ResponseWriter, r *http.Request) {
	state, err := s.Car.SleepState(r.Context())
	if err != nil {
		carError(w, err)
		return
	}
	writeJSON(w, map[string]string{"state": state})
}

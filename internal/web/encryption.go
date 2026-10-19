package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/lubaxe/glovebox/internal/crypt"
)

// Encryption is what the API needs for the encrypted drive.
type Encryption interface {
	Status() crypt.Status
	SetRecipient(ctx context.Context, recipient string) error
	UnlockWithKey(ctx context.Context, identity string) error
	RequestEncrypt() error
}

// encryptionResponse is the status plus what "Encrypt the drive" needs.
type encryptionResponse struct {
	crypt.Status
	// Ready lists what encrypting needs, and whether it's there.
	Ready readiness `json:"ready"`
}

type readiness struct {
	Key bool `json:"key"` // the user's public key is set
	Car bool `json:"car"` // a car is paired, to unlock the drive
	// Archived: the last NAS backup finished with nothing left over, since
	// encrypting erases the drive.
	Archived bool `json:"archived"`
}

// eraseWord must be typed to encrypt the drive.
const eraseWord = "ERASE"

func (s *Server) encryptionRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/encryption", s.needEncryption(s.handleEncryption))
	mux.HandleFunc("PUT /api/encryption/recipient", s.needEncryption(s.handleSetRecipient))
	mux.HandleFunc("POST /api/encryption/unlock", s.needEncryption(s.handleUnlock))
	mux.HandleFunc("POST /api/encryption/encrypt", s.needEncryption(s.handleEncrypt))
}

// needEncryption also refuses changes that don't come as JSON (see needCar).
func (s *Server) needEncryption(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Encryption == nil {
			http.Error(w, "the drive is off (drive.enabled in the config)", http.StatusServiceUnavailable)
			return
		}
		if r.Method != http.MethodGet && r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "send JSON", http.StatusUnsupportedMediaType)
			return
		}
		h(w, r)
	}
}

func (s *Server) readiness(st crypt.Status) readiness {
	rd := readiness{Key: st.Recipient != ""}
	if s.Car != nil {
		_, rd.Car = s.Car.Paired()
	}
	if s.Archive != nil {
		a := s.Archive.Status()
		rd.Archived = a.Last != nil && a.Last.Err == "" && a.Last.Failed == 0 && a.Last.Pending == 0
	}
	return rd
}

func (s *Server) handleEncryption(w http.ResponseWriter, r *http.Request) {
	st := s.Encryption.Status()
	writeJSON(w, encryptionResponse{Status: st, Ready: s.readiness(st)})
}

func (s *Server) handleSetRecipient(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Recipient string `json:"recipient"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.Encryption.SetRecipient(r.Context(), body.Recipient); err != nil {
		carError(w, err)
		return
	}
	s.Log.Info("set the key that recovers the encrypted drive")
	s.handleEncryption(w, r)
}

func (s *Server) handleUnlock(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Identity string `json:"identity"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.Encryption.UnlockWithKey(r.Context(), body.Identity); err != nil {
		if !errors.Is(err, crypt.ErrWrongKey) {
			s.Log.Warn("unlocking with the user's key failed", "err", err)
		}
		carError(w, err)
		return
	}
	s.handleEncryption(w, r)
}

func (s *Server) handleEncrypt(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Confirm string `json:"confirm"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	st := s.Encryption.Status()
	rd := s.readiness(st)
	switch {
	case body.Confirm != eraseWord:
		carError(w, errors.New("type "+eraseWord+" to confirm"))
		return
	case !rd.Car:
		carError(w, errors.New("pair the car first (Car page): the drive opens when Glovebox sees it"))
		return
	case !rd.Archived:
		carError(w, errors.New("wait until everything is backed up to the NAS: encrypting erases the drive"))
		return
	}
	if err := s.Encryption.RequestEncrypt(); err != nil {
		carError(w, err)
		return
	}
	s.Log.Warn("encrypting the drive was asked for; restarting to do it")
	s.handleEncryption(w, r)
	if s.Restart != nil {
		s.Restart()
	}
}

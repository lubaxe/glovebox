package crypt

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/lubaxe/glovebox/internal/bootfs"
)

// Car check modes.
const (
	// Enforce opens the volume only once the car has been seen.
	Enforce = "enforce"
	// Audit checks for the car and logs the result, but opens the volume
	// either way: for trying the check in the car without risking the
	// dashcam.
	Audit = "audit"
)

// Unlock states.
const (
	Off      = "off"      // not encrypted
	Waiting  = "waiting"  // checking for the car
	Unlocked = "unlocked" // open until the power is cut
)

// How the volume was opened.
const (
	ByCar     = "car"     // the car check passed
	ByAudit   = "audit"   // audit mode, whatever the check said
	ByKey     = "key"     // the user's key
	ByRestart = "restart" // already open: Glovebox restarted during this wake
	ByCreate  = "create"  // just encrypted
)

// errOpen wraps cryptsetup failing to open the volume.
var errOpen = errors.New("opening the drive")

// ErrNoCar means there's no paired car to check for.
var ErrNoCar = errors.New("no car is paired; pair it on the Car page, or unlock with your key")

// Status is what the API shows.
type Status struct {
	Encrypted  bool       `json:"encrypted"`
	State      string     `json:"state"`
	CarCheck   string     `json:"car_check"`
	Attempts   int        `json:"attempts,omitempty"`
	LastError  string     `json:"last_error,omitempty"`
	LastTry    *time.Time `json:"last_try,omitempty"`
	UnlockedAt *time.Time `json:"unlocked_at,omitempty"`
	UnlockedBy string     `json:"unlocked_by,omitempty"`
	// CheckSeconds is how long the car check took when it passed.
	CheckSeconds float64 `json:"check_seconds,omitempty"`
	// Recipient is the user's age public key, "" if not set.
	Recipient string `json:"recipient,omitempty"`
	// EncryptRequested is set while encrypting waits for the restart.
	EncryptRequested bool `json:"encrypt_requested,omitempty"`
	// EncryptError is why the last attempt to encrypt failed.
	EncryptError string `json:"encrypt_error,omitempty"`
}

// Service decides when the volume opens: at startup it checks for the car
// until it's seen (Run), or the user unlocks with their key. It also holds
// the user's public key and the request to encrypt the drive.
type Service struct {
	Volume   *Volume
	Recovery Recovery
	// Check checks that the paired car is in Bluetooth range (see
	// keepawake.Car.Verify). Nil means there's no car to check for.
	Check func(ctx context.Context) error
	// Mode is Enforce or Audit.
	Mode string
	// RequestFile, when present, asks for the drive to be encrypted at the
	// next start (see Conversion); ErrorFile keeps why that last failed.
	RequestFile, ErrorFile string
	Log                    *slog.Logger
	// Backoff is the wait after each failed check; the last one repeats.
	Backoff []time.Duration
	Now     func() time.Time

	opMu     sync.Mutex // cryptsetup open, one at a time
	mu       sync.Mutex
	st       Status
	unlocked chan struct{}
	once     sync.Once
}

var defaultBackoff = []time.Duration{2 * time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second, 20 * time.Second, 30 * time.Second}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) init() {
	s.once.Do(func() {
		s.unlocked = make(chan struct{})
		s.st.State = Off
		if s.Mode == "" {
			s.Mode = Enforce
		}
	})
}

// Encrypted reports whether there is an encrypted volume. It also sets up
// the status, so call it at startup.
func (s *Service) Encrypted(ctx context.Context) (bool, error) {
	s.init()
	_, err := s.Volume.Find(ctx)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	s.mu.Lock()
	s.st.Encrypted = true
	if s.st.State == Off {
		s.st.State = Waiting
	}
	s.mu.Unlock()
	return true, nil
}

// Unlocked is closed once the volume is open.
func (s *Service) Unlocked() <-chan struct{} {
	s.init()
	return s.unlocked
}

// Status returns the current status.
func (s *Service) Status() Status {
	s.init()
	s.mu.Lock()
	st := s.st
	s.mu.Unlock()
	st.CarCheck = s.Mode
	st.Recipient, _ = s.Recovery.Recipient()
	if s.RequestFile != "" {
		if _, err := os.Stat(s.RequestFile); err == nil {
			st.EncryptRequested = true
		}
	}
	if s.ErrorFile != "" {
		if b, err := os.ReadFile(s.ErrorFile); err == nil {
			st.EncryptError = strings.TrimSpace(string(b))
		}
	}
	return st
}

// Run opens the volume: at once if it's already open (Glovebox restarted
// during this wake), otherwise once the car check passes, retrying for as
// long as it takes. It returns when the volume is open, or with ctx's
// error.
func (s *Service) Run(ctx context.Context) error {
	s.init()
	select {
	case <-s.unlocked:
		return nil // just encrypted (Created)
	default:
	}
	if open, err := s.Volume.IsOpen(ctx); err != nil {
		s.Log.Warn("can't tell whether the encrypted drive is open", "err", err)
	} else if open {
		s.Log.Info("the encrypted drive is already open (restarted during this wake)")
		s.markUnlocked(ByRestart, 0)
		return nil
	}
	for i := 0; ; i++ {
		select {
		case <-s.unlocked:
			return nil // unlocked with the user's key meanwhile
		default:
		}
		start := s.now()
		err := s.check(ctx)
		took := s.now().Sub(start)
		switch {
		case err == nil:
			s.Log.Info("car check passed", "took", took.Round(10*time.Millisecond), "attempt", i+1)
			oerr := s.open(ctx, ByCar, took, func() error { return s.Volume.Open(ctx) })
			if oerr == nil {
				return nil
			}
			err = fmt.Errorf("%w: %w", errOpen, oerr)
		case s.Mode == Audit:
			s.Log.Warn("car check failed; opening the drive anyway (car_check = audit)", "err", err,
				"took", took.Round(10*time.Millisecond), "attempt", i+1)
			oerr := s.open(ctx, ByAudit, 0, func() error { return s.Volume.Open(ctx) })
			if oerr == nil {
				return nil
			}
			err = fmt.Errorf("%w: %w", errOpen, oerr)
		default:
			s.Log.Info("car check failed; trying again", "err", err, "took", took.Round(10*time.Millisecond), "attempt", i+1)
		}
		if errors.Is(err, errOpen) {
			s.Log.Error("couldn't open the encrypted drive; trying again", "err", err)
		}
		s.recordFailure(err)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		backoff := s.Backoff
		if len(backoff) == 0 {
			backoff = defaultBackoff
		}
		wait := backoff[min(i, len(backoff)-1)]
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.unlocked:
			return nil
		case <-time.After(wait):
		}
	}
}

func (s *Service) check(ctx context.Context) error {
	if s.Check == nil {
		return ErrNoCar
	}
	return s.Check(ctx)
}

// UnlockWithKey opens the volume with the user's private key.
func (s *Service) UnlockWithKey(ctx context.Context, identity string) error {
	s.init()
	if !s.Status().Encrypted {
		return errors.New("the drive isn't encrypted")
	}
	key, err := s.Recovery.VolumeKey(identity)
	if err != nil {
		return err
	}
	defer clear(key)
	if err := s.open(ctx, ByKey, 0, func() error { return s.Volume.OpenWithVolumeKey(ctx, key) }); err != nil {
		return err
	}
	s.Log.Info("encrypted drive unlocked with the user's key")
	return nil
}

// open runs openFn unless the volume is already open, and records how.
func (s *Service) open(ctx context.Context, by string, took time.Duration, openFn func() error) error {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	select {
	case <-s.unlocked:
		return nil
	default:
	}
	if err := openFn(); err != nil {
		return err
	}
	s.markUnlocked(by, took)
	return nil
}

func (s *Service) markUnlocked(by string, took time.Duration) {
	s.mu.Lock()
	t := s.now()
	s.st.Encrypted = true
	s.st.State = Unlocked
	s.st.UnlockedAt = &t
	s.st.UnlockedBy = by
	s.st.CheckSeconds = took.Seconds()
	s.mu.Unlock()
	select {
	case <-s.unlocked:
	default:
		close(s.unlocked)
	}
}

func (s *Service) recordFailure(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.now()
	s.st.Attempts++
	s.st.LastTry = &t
	s.st.LastError = err.Error()
}

// SetRecipient sets the user's public key. On an encrypted drive the volume
// key is sealed to the new key, which needs the drive unlocked: otherwise
// anyone reaching the web page could swap in their own key and unlock.
func (s *Service) SetRecipient(ctx context.Context, recipient string) error {
	s.init()
	recipient, err := ParseRecipient(recipient)
	if err != nil {
		return err
	}
	st := s.Status()
	return bootfs.WithWrite(ctx, s.Recovery.BootWriter, func() error {
		if st.Encrypted {
			if st.State != Unlocked {
				return errors.New("the drive is locked; it has to be unlocked to change the key")
			}
			key, err := s.Volume.VolumeKey(ctx)
			if err != nil {
				return err
			}
			defer clear(key)
			if err := s.Recovery.saveVolumeKey(recipient, key); err != nil {
				return err
			}
		}
		return s.Recovery.setRecipient(recipient)
	})
}

// RequestEncrypt asks for the drive to be encrypted at the next start; the
// caller restarts Glovebox.
func (s *Service) RequestEncrypt() error {
	st := s.Status()
	switch {
	case st.Encrypted:
		return errors.New("the drive is already encrypted")
	case st.Recipient == "":
		return errors.New("set your key first: it's the only way to recover the footage without the Pi")
	}
	if s.ErrorFile != "" {
		if err := os.Remove(s.ErrorFile); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return os.WriteFile(s.RequestFile, []byte(s.now().UTC().Format(time.RFC3339)+"\n"), 0o600)
}

// TakeRequest reports whether encrypting was requested, and clears the
// request so a failure can't repeat at every start.
func (s *Service) TakeRequest() bool {
	if s.RequestFile == "" {
		return false
	}
	if err := os.Remove(s.RequestFile); err != nil {
		return false
	}
	return true
}

// RecordEncryptError keeps why encrypting failed, for the web page.
func (s *Service) RecordEncryptError(err error) {
	if s.ErrorFile != "" {
		_ = os.WriteFile(s.ErrorFile, []byte(err.Error()+"\n"), 0o600)
	}
}

// Created records that the volume was just made and is open.
func (s *Service) Created() {
	s.init()
	s.markUnlocked(ByCreate, 0)
}

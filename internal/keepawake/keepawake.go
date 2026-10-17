// Package keepawake stops the car from sleeping while Glovebox needs it awake.
//
// When the car sleeps it cuts power to its USB ports, which would stop an
// upload partway through. While Busy reports true, Glovebox tells the car to
// wake every Interval over Bluetooth, using Tesla's tesla-control and a key
// paired with the car. The key only needs the Charging Manager role, which
// can wake the car but can't unlock or drive it.
//
// An awake car may keep recording, so a busy spell is cut off after Max,
// and then nothing is woken for Cooldown, to protect the battery whatever
// Busy says.
package keepawake

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// KeepAwake wakes the car while Busy reports true.
type KeepAwake struct {
	Car      *Car
	Interval time.Duration
	Max      time.Duration // longest busy spell to keep the car awake for
	Cooldown time.Duration // after hitting Max, how long to leave it alone
	// Busy reports whether the car needs to stay awake.
	Busy func() bool
	Log  *slog.Logger

	mu     sync.Mutex
	status Status
}

// Status is what the web UI shows about keep-awake.
type Status struct {
	Active    bool      `json:"active"` // keeping the car awake right now
	LastWake  time.Time `json:"last_wake,omitzero"`
	CarState  string    `json:"car_state,omitempty"` // as the car reported after the last wake
	LastError string    `json:"last_error,omitempty"`
	// RestingUntil is set after a spell hit Max.
	RestingUntil time.Time `json:"resting_until,omitzero"`
}

// Status returns the current status.
func (k *KeepAwake) Status() Status {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.status
}

func (k *KeepAwake) update(f func(*Status)) {
	k.mu.Lock()
	defer k.mu.Unlock()
	f(&k.status)
}

// Run checks Busy every few seconds and wakes the car every Interval while
// it reports true, until ctx is done. Nothing happens until the car is
// paired.
func (k *KeepAwake) Run(ctx context.Context) {
	check := time.NewTicker(5 * time.Second)
	defer check.Stop()
	var (
		last       time.Time // last poke; zero while idle, so a busy spell starts with one
		spellStart time.Time // start of the current busy spell
		restUntil  time.Time // after hitting Max
	)
	for {
		select {
		case <-ctx.Done():
			return
		case <-check.C:
		}
		now := time.Now()
		_, paired := k.Car.Paired()
		if !paired || !k.Busy() {
			last, spellStart = time.Time{}, time.Time{}
			k.update(func(s *Status) { s.Active = false })
			continue
		}
		if now.Before(restUntil) {
			continue
		}
		if spellStart.IsZero() {
			spellStart = now
		}
		if k.Max > 0 && now.Sub(spellStart) >= k.Max {
			k.Log.Warn("kept the car awake as long as allowed; letting it sleep", "max", k.Max, "until", now.Add(k.Cooldown))
			restUntil, spellStart = now.Add(k.Cooldown), time.Time{}
			k.update(func(s *Status) { s.Active, s.RestingUntil = false, restUntil })
			continue
		}
		k.update(func(s *Status) { s.Active = true })
		if !last.IsZero() && now.Sub(last) < k.Interval {
			continue
		}
		last = now
		k.Poke(ctx)
	}
}

// Poke tells the car to wake (or stay awake) once.
func (k *KeepAwake) Poke(ctx context.Context) error {
	k.mu.Lock()
	failing := k.status.LastError != ""
	k.mu.Unlock()
	err := k.Car.Wake(ctx)
	if err != nil {
		if !failing {
			k.Log.Warn("couldn't keep the car awake", "err", err)
		}
		k.update(func(s *Status) { s.LastError = err.Error() })
		return err
	}
	state, _ := k.Car.SleepState(ctx)
	if failing {
		k.Log.Info("keeping the car awake again")
	}
	k.Log.Info("woke the car", "car", state)
	k.update(func(s *Status) {
		s.LastWake, s.CarState, s.LastError = time.Now(), state, ""
	})
	return nil
}

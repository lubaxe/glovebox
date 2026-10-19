// Package keepawake stops the car from sleeping while Glovebox needs it awake.
//
// When the car sleeps it cuts power to its USB ports, which would stop an
// upload partway through. While Busy reports true (or someone asked for it
// with Hold), Glovebox wakes the car every Interval over Bluetooth and nudges
// it to stay awake: it asks it to close the charge port and re-sets the
// charging current to what it already is. Waking alone isn't enough; the car
// cuts the USB power about 12 minutes after it woke anyway. It uses Tesla's
// tesla-control and a key paired with the car as Driver (see
// Car.RequestPairing). An older Charging Manager key can't close the charge
// port, and the charging current alone doesn't keep the car awake.
//
// An awake car may keep recording, so a busy spell is cut off after Max,
// and then nothing is woken for Cooldown, to protect the battery whatever
// Busy says.
package keepawake

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
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

	mu        sync.Mutex
	status    Status
	holdUntil time.Time
}

// Status is what the web UI shows about keep-awake.
type Status struct {
	Active bool      `json:"active"` // keeping the car awake right now
	Since  time.Time `json:"since,omitzero"`
	// HeldUntil is set while someone asked for it with Hold.
	HeldUntil time.Time `json:"held_until,omitzero"`
	LastWake  time.Time `json:"last_wake,omitzero"`
	NextWake  time.Time `json:"next_wake,omitzero"`
	Nudging   bool      `json:"nudging"`             // a wake and nudge is under way
	CarState  string    `json:"car_state,omitempty"` // "AWAKE" once the car answered the last wake
	// Nudge says how the car took the last nudges.
	Nudge     string `json:"nudge,omitempty"`
	LastError string `json:"last_error,omitempty"`
	// RestingUntil is set after a spell hit Max.
	RestingUntil time.Time `json:"resting_until,omitzero"`
}

// Status returns the current status.
func (k *KeepAwake) Status() Status {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.status
}

// Hold keeps the car awake for d whatever Busy says, ignoring Max and
// Cooldown; d <= 0 stops a hold.
func (k *KeepAwake) Hold(d time.Duration) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.holdUntil = time.Time{}
	if d > 0 {
		k.holdUntil = time.Now().Add(d)
	}
	k.status.HeldUntil = k.holdUntil
}

func (k *KeepAwake) held(now time.Time) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	if !k.holdUntil.IsZero() && !now.Before(k.holdUntil) {
		k.holdUntil, k.status.HeldUntil = time.Time{}, time.Time{}
	}
	return !k.holdUntil.IsZero()
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
		held := k.held(now)
		if !paired || !(held || k.Busy()) {
			last, spellStart = time.Time{}, time.Time{}
			k.update(func(s *Status) { s.Active, s.Since, s.NextWake = false, time.Time{}, time.Time{} })
			continue
		}
		if !held && now.Before(restUntil) {
			continue
		}
		if spellStart.IsZero() {
			spellStart = now
			k.Log.Info("keeping the car awake", "asked", held)
		}
		if !held && k.Max > 0 && now.Sub(spellStart) >= k.Max {
			k.Log.Warn("kept the car awake as long as allowed; letting it sleep", "max", k.Max, "until", now.Add(k.Cooldown))
			restUntil, spellStart = now.Add(k.Cooldown), time.Time{}
			k.update(func(s *Status) {
				s.Active, s.Since, s.NextWake, s.RestingUntil = false, time.Time{}, time.Time{}, restUntil
			})
			continue
		}
		k.update(func(s *Status) { s.Active, s.Since = true, spellStart })
		if !last.IsZero() && now.Sub(last) < k.Interval {
			continue
		}
		last = now
		if k.Poke(ctx) != nil && k.Interval > retryAfter {
			// Bluetooth is flaky: try again soon rather than a whole
			// Interval later, when the car may have cut the power
			last = now.Add(retryAfter - k.Interval)
		}
		k.update(func(s *Status) { s.NextWake = last.Add(k.Interval) })
	}
}

// retryAfter is how soon Run tries again after a failed poke.
const retryAfter = 20 * time.Second

// Poke wakes the car and nudges it to stay awake, once. It fails if the car
// can't be woken or takes neither nudge.
func (k *KeepAwake) Poke(ctx context.Context) error {
	k.mu.Lock()
	failing := k.status.LastError != ""
	k.status.Nudging = true
	k.mu.Unlock()
	defer k.update(func(s *Status) { s.Nudging = false })
	err := k.Car.Wake(ctx)
	var nudge string
	if err == nil {
		port, portErr := k.Car.ClosePort(ctx)
		amps, ampsErr := k.Car.ResetAmps(ctx)
		var said []string
		switch {
		case portErr == nil:
			said = append(said, "charge port nudged ("+port+")")
		case errors.Is(portErr, ErrNotAllowed):
			said = append(said, "charge port: not allowed for this key")
		default:
			said = append(said, "charge port: "+portErr.Error())
		}
		if ampsErr == nil {
			said = append(said, fmt.Sprintf("charging current re-set to %d A", amps))
		} else {
			said = append(said, "charging current: "+ampsErr.Error())
		}
		nudge = strings.Join(said, "; ")
		if portErr != nil && ampsErr != nil {
			err = errors.New("the car took neither nudge (" + nudge + ")")
		}
	}
	if err != nil {
		if !failing {
			k.Log.Warn("couldn't keep the car awake", "err", err)
		}
		k.update(func(s *Status) { s.LastError = err.Error() })
		return err
	}
	if failing {
		k.Log.Info("keeping the car awake again")
	}
	k.Log.Info("woke and nudged the car", "nudge", nudge)
	k.update(func(s *Status) {
		s.LastWake, s.CarState, s.Nudge, s.LastError = time.Now(), "AWAKE", nudge, ""
	})
	return nil
}

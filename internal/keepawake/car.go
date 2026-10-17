package keepawake

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/lubaxe/glovebox/internal/system"
)

// VINs use digits and capital letters other than I, O and Q.
var vinPattern = regexp.MustCompile(`^[A-HJ-NPR-Z0-9]{17}$`)

// ValidVIN reports whether vin looks like a VIN.
func ValidVIN(vin string) bool { return vinPattern.MatchString(vin) }

// Car talks to the car over Bluetooth with Tesla's tesla-control. It keeps
// the key, the VIN and whether pairing was confirmed in Dir:
//
//	private.pem, public.pem   the key (created on first use)
//	vin                       the VIN, if set from the web UI
//	paired                    "<VIN> <time>" once the car accepted the key
//
// Only one Bluetooth conversation runs at a time.
type Car struct {
	Command string // tesla-control
	Dir     string // e.g. /etc/glovebox/ble
	// ConfigVIN is used when no VIN was set from the web UI.
	ConfigVIN string
	Runner    system.Runner
	Now       func() time.Time

	mu sync.Mutex
}

// KeyFile is the private key's path.
func (c *Car) KeyFile() string { return filepath.Join(c.Dir, "private.pem") }

// VIN returns the car's VIN, or "" if it isn't set.
func (c *Car) VIN() string {
	if b, err := os.ReadFile(filepath.Join(c.Dir, "vin")); err == nil {
		if vin := strings.TrimSpace(string(b)); vin != "" {
			return vin
		}
	}
	return c.ConfigVIN
}

// SetVIN stores the VIN. Changing it forgets the pairing, which was for
// another car.
func (c *Car) SetVIN(vin string) error {
	vin = strings.ToUpper(strings.TrimSpace(vin))
	if !ValidVIN(vin) {
		return fmt.Errorf("%q isn't a VIN: it should be 17 letters and digits (no I, O or Q)", vin)
	}
	if err := os.MkdirAll(c.Dir, 0o700); err != nil {
		return err
	}
	if vin != c.VIN() {
		if err := os.Remove(filepath.Join(c.Dir, "paired")); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return os.WriteFile(filepath.Join(c.Dir, "vin"), []byte(vin+"\n"), 0o600)
}

// Paired reports whether the car accepted the key for the current VIN, and
// when that was confirmed.
func (c *Car) Paired() (time.Time, bool) {
	b, err := os.ReadFile(filepath.Join(c.Dir, "paired"))
	if err != nil {
		return time.Time{}, false
	}
	f := strings.Fields(string(b))
	if len(f) != 2 || f[0] != c.VIN() || f[0] == "" {
		return time.Time{}, false
	}
	t, _ := time.Parse(time.RFC3339, f[1])
	return t, true
}

// RequestPairing sends the car a request to add Glovebox's key with the
// Charging Manager role, creating the key first if needed. Someone then has
// to lay a key card on the car's card reader and confirm on its screen.
func (c *Car) RequestPairing(ctx context.Context) error {
	vin := c.VIN()
	if vin == "" {
		return errors.New("set the VIN first")
	}
	if _, err := EnsureKey(c.KeyFile()); err != nil {
		return fmt.Errorf("creating the key: %w", err)
	}
	_, err := c.run(ctx, "-ble", "-vin", vin, "add-key-request",
		filepath.Join(c.Dir, "public.pem"), "charging_manager", "cloud_key")
	return err
}

// CheckPairing asks the car for its charge state with the key, which only
// works once the car has accepted it, and records the pairing if it does.
func (c *Car) CheckPairing(ctx context.Context) error {
	vin := c.VIN()
	if vin == "" {
		return errors.New("set the VIN first")
	}
	if _, err := c.run(ctx, "-ble", "-vin", vin, "-key-file", c.KeyFile(), "state", "charge"); err != nil {
		return err
	}
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	line := vin + " " + now().UTC().Format(time.RFC3339) + "\n"
	return os.WriteFile(filepath.Join(c.Dir, "paired"), []byte(line), 0o600)
}

// Forget clears the VIN set from the web UI and the pairing. The key stays
// in the car until it's removed there (Controls > Locks > Keys).
func (c *Car) Forget() error {
	for _, name := range []string{"paired", "vin"} {
		if err := os.Remove(filepath.Join(c.Dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// Wake wakes the car, or keeps it awake.
func (c *Car) Wake(ctx context.Context) error {
	_, err := c.run(ctx, "-ble", "-vin", c.VIN(), "-key-file", c.KeyFile(), "wake")
	return err
}

var sleepStatus = regexp.MustCompile(`"vehicleSleepStatus":\s*"VEHICLE_SLEEP_STATUS_(\w+)"`)

// SleepState asks the car whether it's awake ("AWAKE" or "ASLEEP"). It
// needs no key, so it also shows whether the car is in Bluetooth range.
func (c *Car) SleepState(ctx context.Context) (string, error) {
	vin := c.VIN()
	if vin == "" {
		return "", errors.New("set the VIN first")
	}
	out, err := c.run(ctx, "-ble", "-vin", vin, "body-controller-state")
	if err != nil {
		return "", err
	}
	if m := sleepStatus.FindSubmatch(out); m != nil {
		return string(m[1]), nil
	}
	return "UNKNOWN", nil
}

func (c *Car) run(ctx context.Context, args ...string) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	out, err := c.Runner.Run(ctx, c.Command, args...)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return out, errors.New("the car didn't answer over Bluetooth; is it nearby and awake?")
		}
		return out, friendly(err)
	}
	return out, nil
}

// friendly shortens tesla-control's errors to their last line, which says
// what went wrong.
func friendly(err error) error {
	var ce *system.CmdError
	if errors.As(err, &ce) {
		lines := strings.Split(strings.TrimSpace(ce.Output), "\n")
		if last := strings.TrimSpace(lines[len(lines)-1]); last != "" {
			return errors.New(last)
		}
	}
	return err
}

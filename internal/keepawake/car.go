package keepawake

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
//	role                      the role it was paired with (see Roles)
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
	// Log, if set, gets each Bluetooth command at debug level.
	Log *slog.Logger

	mu sync.Mutex
	// portRefused is set once the car refused to close the charge port for
	// this key, so it isn't asked again
	portRefused atomic.Bool
}

// Key roles.
const (
	// Driver is what Glovebox pairs with: the car only lets a Driver (or
	// Owner) key close the charge port, which is what keeps it awake. The
	// key could also unlock and drive the car if it were taken from the Pi;
	// PIN to Drive stops the driving.
	Driver = "driver"
	// ChargingManager is what Glovebox paired with before 0.5. It can't
	// unlock or drive the car, but nothing it may do keeps the car awake
	// more than about 12 minutes, so such a key is replaced when paired
	// again.
	ChargingManager = "charging_manager"
)

var roles = []string{Driver, ChargingManager}

// Role returns the role the key was paired with.
func (c *Car) Role() string {
	if b, err := os.ReadFile(filepath.Join(c.Dir, "role")); err == nil {
		if r := strings.TrimSpace(string(b)); slices.Contains(roles, r) {
			return r
		}
	}
	return ChargingManager // before roles were recorded
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
// Driver role, creating the key first if needed. Someone then has to lay a
// key card on the car's card reader and confirm on its screen. A key paired
// with another role (Charging Manager, before 0.5) is replaced with a new
// one rather than asked to change roles; the old one should be removed in
// the car.
func (c *Car) RequestPairing(ctx context.Context) error {
	const role = Driver
	vin := c.VIN()
	if vin == "" {
		return errors.New("set the VIN first")
	}
	if _, err := os.Stat(c.KeyFile()); err == nil && c.Role() != role {
		if err := c.retireKey(); err != nil {
			return fmt.Errorf("replacing the key: %w", err)
		}
	}
	if _, err := EnsureKey(c.KeyFile()); err != nil {
		return fmt.Errorf("creating the key: %w", err)
	}
	if err := os.WriteFile(filepath.Join(c.Dir, "role"), []byte(role+"\n"), 0o600); err != nil {
		return err
	}
	c.portRefused.Store(false)
	_, err := c.run(ctx, "-ble", "-vin", vin, "add-key-request",
		filepath.Join(c.Dir, "public.pem"), role, "cloud_key")
	return err
}

// retireKey moves the key aside as <name>.<role>.old and forgets the
// pairing.
func (c *Car) retireKey() error {
	old := c.Role()
	for _, name := range []string{"private.pem", "public.pem"} {
		p := filepath.Join(c.Dir, name)
		if err := os.Rename(p, p+"."+old+".old"); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	if err := os.Remove(filepath.Join(c.Dir, "paired")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
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

// Wake wakes the car. It doesn't keep it awake: about 12 minutes after
// waking, the car cuts the USB power however often it's woken. The nudges
// below are for that.
func (c *Car) Wake(ctx context.Context) error {
	_, err := c.run(ctx, "-ble", "-vin", c.VIN(), "-key-file", c.KeyFile(), "wake")
	return err
}

// ErrNotAllowed means the car refused a command for the key's role.
var ErrNotAllowed = errors.New("not allowed for this key")

// ClosePort asks the car to close the charge port, which teslausb and
// Sentry-USB use to keep it awake, and says how the car took it. A port
// that's already closed, or a plugged-in cable, still counts. The car only
// accepts it from a Driver or Owner key; for a Charging Manager key it
// returns ErrNotAllowed, then and from then on without asking.
func (c *Car) ClosePort(ctx context.Context) (string, error) {
	if c.portRefused.Load() {
		return "", ErrNotAllowed
	}
	out, err := c.run(ctx, "-ble", "-vin", c.VIN(), "-key-file", c.KeyFile(), "charge-port-close")
	if err == nil {
		return "closed", nil
	}
	msg := strings.ToLower(string(out) + " " + err.Error())
	switch {
	case strings.Contains(msg, "already closed"):
		return "already closed", nil
	case strings.Contains(msg, "cable connected"):
		return "cable connected", nil
	case strings.Contains(msg, "insufficient_privileges"):
		c.portRefused.Store(true)
		return "", ErrNotAllowed
	}
	return "", err
}

// ResetAmps sets the charging current to what it already is, as another
// nudge: it reads the setting first, so nothing changes. It returns the
// current in amps.
func (c *Car) ResetAmps(ctx context.Context) (int, error) {
	out, err := c.run(ctx, "-ble", "-vin", c.VIN(), "-key-file", c.KeyFile(), "state", "charge")
	if err != nil {
		return 0, err
	}
	m := chargingAmps.FindSubmatch(out)
	if m == nil {
		return 0, errors.New("the car didn't say what charging current it's set to")
	}
	amps, _ := strconv.Atoi(string(m[1]))
	if _, err := c.run(ctx, "-ble", "-vin", c.VIN(), "-key-file", c.KeyFile(), "charging-set-amps", string(m[1])); err != nil {
		return 0, err
	}
	return amps, nil
}

var chargingAmps = regexp.MustCompile(`"chargingAmps":\s*(\d+)`)

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
	start := time.Now()
	out, err := c.Runner.Run(ctx, c.Command, args...)
	if c.Log != nil {
		c.Log.Debug("bluetooth command", "args", strings.Join(args[3:], " "), // after the VIN
			"took", time.Since(start).Round(10*time.Millisecond), "err", err)
	}
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

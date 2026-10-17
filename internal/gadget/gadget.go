// Package gadget presents disk images to the car as a USB mass storage
// device, using the kernel's configfs USB gadget interface.
package gadget

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/lubaxe/glovebox/internal/system"
)

const (
	lang   = "0x409" // US English
	config = "c.1"
	msFunc = "mass_storage.0"
	// acmFunc is a USB serial port, so the Pi keeps a login console on the
	// same cable (shows up as ttyGS0 on the Pi).
	acmFunc = "acm.usb0"
)

// LUN is one drive the host sees.
type LUN struct {
	File    string // disk image
	Product string // drive name shown to the host, max 16 characters
}

// inquiry builds the SCSI inquiry string: an 8-character vendor, a
// 16-character product and a 4-character revision, space padded.
func inquiry(product string) string {
	return fmt.Sprintf("%-8.8s%-16.16s%-4.4s", "glovebox", product, "1.0")
}

// Gadget manages one configfs USB gadget.
type Gadget struct {
	Name          string // directory name under usb_gadget/, e.g. "glovebox"
	ConfigFS      string // usually /sys/kernel/config/usb_gadget
	UDCDir        string // usually /sys/class/udc
	MachineIDFile string // usually /etc/machine-id, used for the serial number
	MaxPowerMA    int
	SerialConsole bool
	Runner        system.Runner
}

// New returns a Gadget with the standard system paths.
func New(r system.Runner) *Gadget {
	return &Gadget{
		Name:          "glovebox",
		ConfigFS:      "/sys/kernel/config/usb_gadget",
		UDCDir:        "/sys/class/udc",
		MachineIDFile: "/etc/machine-id",
		MaxPowerMA:    200,
		Runner:        r,
	}
}

func (g *Gadget) root() string { return filepath.Join(g.ConfigFS, g.Name) }

// State describes the gadget and the USB host's view of it.
type State struct {
	Enabled bool   `json:"enabled"`
	UDC     string `json:"udc,omitempty"`
	// HostState is the controller state: "configured" means a host (the
	// car) has enumerated the drive; "not attached" means no host.
	HostState string   `json:"host_state,omitempty"`
	Files     []string `json:"files,omitempty"`
}

// Connected reports whether a host is using the drive.
func (s State) Connected() bool { return s.HostState == "configured" }

// Enable creates the gadget with the given drives and binds it to the USB
// controller, which makes the drives appear on the host.
func (g *Gadget) Enable(ctx context.Context, luns []LUN) error {
	if len(luns) == 0 {
		return errors.New("no drives to present")
	}
	if st, err := g.State(); err != nil {
		return err
	} else if st.Enabled {
		return errors.New("gadget is already enabled")
	}
	udc, err := g.firstUDC()
	if err != nil {
		return err
	}
	if _, err := g.Runner.Run(ctx, "modprobe", "libcomposite"); err != nil {
		return err
	}

	root := g.root()
	serial, err := g.serialNumber()
	if err != nil {
		return err
	}
	w := writer{}
	w.mkdir(root)
	w.write(filepath.Join(root, "idVendor"), "0x1d6b")  // Linux Foundation
	w.write(filepath.Join(root, "idProduct"), "0x0104") // Multifunction Composite Gadget
	w.write(filepath.Join(root, "bcdDevice"), "0x0100")
	w.write(filepath.Join(root, "bcdUSB"), "0x0200")
	strs := filepath.Join(root, "strings", lang)
	w.mkdir(strs)
	w.write(filepath.Join(strs, "serialnumber"), serial)
	w.write(filepath.Join(strs, "manufacturer"), "glovebox")
	w.write(filepath.Join(strs, "product"), "Glovebox drive")
	cfg := filepath.Join(root, "configs", config)
	w.mkdir(cfg)
	w.mkdir(filepath.Join(cfg, "strings", lang))
	w.write(filepath.Join(cfg, "strings", lang, "configuration"), "glovebox")
	w.write(filepath.Join(cfg, "MaxPower"), fmt.Sprint(g.MaxPowerMA))

	ms := filepath.Join(root, "functions", msFunc)
	w.mkdir(ms)
	for i, lun := range luns {
		dir := filepath.Join(ms, fmt.Sprintf("lun.%d", i))
		w.mkdir(dir) // lun.0 already exists in configfs; mkdir tolerates that
		w.write(filepath.Join(dir, "removable"), "1")
		w.write(filepath.Join(dir, "ro"), "0")
		w.write(filepath.Join(dir, "inquiry_string"), inquiry(lun.Product))
		w.write(filepath.Join(dir, "file"), lun.File)
	}
	w.symlink(ms, filepath.Join(cfg, msFunc))
	if g.SerialConsole {
		acm := filepath.Join(root, "functions", acmFunc)
		w.mkdir(acm)
		w.symlink(acm, filepath.Join(cfg, acmFunc))
	}
	w.write(filepath.Join(root, "UDC"), udc)
	if w.err != nil {
		cleanupErr := g.Disable(ctx)
		return errors.Join(fmt.Errorf("enabling USB gadget: %w", w.err), cleanupErr)
	}

	if g.SerialConsole {
		// the console's getty waits for ttyGS0, which only exists now
		_, _ = g.Runner.Run(ctx, "systemctl", "--no-block", "restart", "serial-getty@ttyGS0.service")
	}
	return nil
}

// Disable unbinds the gadget and removes it, which disconnects the drives
// from the host. It's safe to call when the gadget doesn't exist.
func (g *Gadget) Disable(ctx context.Context) error {
	root := g.root()
	if _, err := os.Stat(root); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	var errs []error
	keep := func(err error) {
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	keep(os.WriteFile(filepath.Join(root, "UDC"), []byte("\n"), 0o644))
	cfg := filepath.Join(root, "configs", config)
	keep(os.Remove(filepath.Join(cfg, msFunc)))
	keep(os.Remove(filepath.Join(cfg, acmFunc)))
	keep(os.Remove(filepath.Join(cfg, "strings", lang)))
	keep(os.Remove(cfg))
	ms := filepath.Join(root, "functions", msFunc)
	if luns, err := filepath.Glob(filepath.Join(ms, "lun.*")); err == nil {
		for _, l := range luns {
			if filepath.Base(l) != "lun.0" { // lun.0 goes away with the function
				keep(os.Remove(l))
			}
		}
	}
	keep(os.Remove(ms))
	keep(os.Remove(filepath.Join(root, "functions", acmFunc)))
	keep(os.Remove(filepath.Join(root, "strings", lang)))
	keep(os.Remove(root))
	if len(errs) > 0 {
		return fmt.Errorf("disabling USB gadget: %w", errors.Join(errs...))
	}
	return nil
}

// State reports whether the gadget exists, which controller it's bound to,
// and whether a host has enumerated it.
func (g *Gadget) State() (State, error) {
	var st State
	root := g.root()
	if _, err := os.Stat(root); errors.Is(err, fs.ErrNotExist) {
		return st, nil
	} else if err != nil {
		return st, err
	}
	udc, err := readTrim(filepath.Join(root, "UDC"))
	if err != nil {
		return st, err
	}
	st.UDC = udc
	st.Enabled = udc != ""
	luns, _ := filepath.Glob(filepath.Join(root, "functions", msFunc, "lun.*", "file"))
	for _, l := range luns {
		if f, err := readTrim(l); err == nil && f != "" {
			st.Files = append(st.Files, f)
		}
	}
	if st.Enabled {
		st.HostState, _ = readTrim(filepath.Join(g.UDCDir, udc, "state"))
	}
	return st, nil
}

func (g *Gadget) firstUDC() (string, error) {
	entries, err := os.ReadDir(g.UDCDir)
	if err != nil {
		return "", fmt.Errorf("finding USB device controller: %w", err)
	}
	if len(entries) == 0 {
		return "", errors.New("no USB device controller; is dtoverlay=dwc2 set in config.txt?")
	}
	return entries[0].Name(), nil
}

func (g *Gadget) serialNumber() (string, error) {
	id, err := os.ReadFile(g.MachineIDFile)
	if err != nil {
		return "", fmt.Errorf("reading machine id: %w", err)
	}
	sum := sha256.Sum256(id)
	return fmt.Sprintf("glovebox-%x", sum[:8]), nil
}

func readTrim(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// writer does a series of configfs operations and keeps the first error.
type writer struct{ err error }

func (w *writer) mkdir(path string) {
	if w.err == nil {
		if err := os.MkdirAll(path, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			w.err = err
		}
	}
}

func (w *writer) write(path, value string) {
	if w.err == nil {
		w.err = os.WriteFile(path, []byte(value+"\n"), 0o644)
	}
}

func (w *writer) symlink(target, link string) {
	if w.err == nil {
		w.err = os.Symlink(target, link)
	}
}

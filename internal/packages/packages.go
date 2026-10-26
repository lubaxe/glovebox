// Package packages makes sure the Debian packages Glovebox needs are
// installed.
//
// Required is the whole list this version needs, not what's new in it, so a
// Pi that skipped versions catches up in one go. The updater has the new
// version install them before it replaces the old one (glovebox
// -prepare-update), and every start checks again, for Pis that were updated
// by a version too old to do that.
package packages

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/lubaxe/glovebox/internal/system"
)

// Required are the packages Glovebox uses. scripts/install.sh and the
// image's 00-packages-nr install them too (a test checks); the installer
// also installs curl, for itself.
var Required = []string{
	"xfsprogs", "cryptsetup-bin", "exfatprogs", "dosfstools", "ca-certificates",
	"rsync", "rclone", "nfs-common", "openssh-client", "dnsmasq-base", "avahi-daemon",
}

// bootPackages write the kernel, firmware or initramfs to the boot
// partition, which is read-only. Installing a Glovebox dependency must never
// pull them in: that's an OS update, done by hand with
// glovebox-boot-maintenance.
var bootPackages = []string{
	"linux-image-", "raspi-firmware", "raspberrypi-kernel", "raspberrypi-bootloader",
	"initramfs-tools", "rpi-eeprom",
}

// ErrUnsupported means this system has no dpkg, so packages are left to
// whoever installed Glovebox.
var ErrUnsupported = errors.New("not a Debian system")

// Installer installs missing packages.
type Installer struct {
	Runner   system.Runner
	Required []string
	// Step, if set, is told what's happening, for the update page.
	Step func(string)
	// Scratch, if set, is where apt keeps the package lists, its cache and
	// the downloads while installing, removed afterwards. Its parent must be
	// a mount point. The image's root filesystem is far too small for them
	// (about 110 MB), so on a Pi it's on backingfiles.
	Scratch string
	// scratch is Scratch while Ensure installs, and arch dpkg's architecture
	scratch, arch string
}

// DefaultScratch is the Installer's Scratch on a Pi.
const DefaultScratch = "/backingfiles/.glovebox-apt"

// New returns an Installer for Required.
func New(r system.Runner) *Installer {
	return &Installer{Runner: r, Required: Required, Scratch: DefaultScratch}
}

func (in *Installer) step(s string) {
	if in.Step != nil {
		in.Step(s)
	}
}

// Missing returns the required packages that aren't installed.
func (in *Installer) Missing(ctx context.Context) ([]string, error) {
	if _, err := in.Runner.Run(ctx, "dpkg-query", "--version"); err != nil && system.ExitCode(err) == -1 {
		return nil, ErrUnsupported
	}
	args := append([]string{"-W", "-f=${Package} ${db:Status-Abbrev}\n"}, in.Required...)
	// exits 1 when some package is unknown, but still lists the others
	out, err := in.Runner.Run(ctx, "dpkg-query", args...)
	if err != nil && system.ExitCode(err) != 1 {
		return nil, err
	}
	installed := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		if name, status, ok := strings.Cut(strings.TrimSpace(line), " "); ok && strings.HasPrefix(status, "ii") {
			name, _, _ = strings.Cut(name, ":") // dpkg-query may add ":arm64"
			installed[name] = true
		}
	}
	var missing []string
	for _, p := range in.Required {
		if !installed[p] {
			missing = append(missing, p)
		}
	}
	return missing, nil
}

// Ensure finishes anything dpkg was in the middle of when the power was cut,
// then installs the missing packages. It needs the internet only when
// something is missing.
func (in *Installer) Ensure(ctx context.Context) (installed []string, err error) {
	if err := in.repair(ctx); err != nil {
		return nil, err
	}
	missing, err := in.Missing(ctx)
	if err != nil || len(missing) == 0 {
		return nil, err
	}
	if in.Scratch != "" {
		if _, err := in.Runner.Run(ctx, "mountpoint", "-q", filepath.Dir(in.Scratch)); err != nil {
			return nil, fmt.Errorf("installing %s needs %s for room, and it isn't mounted yet", strings.Join(missing, ", "), filepath.Dir(in.Scratch))
		}
		out, err := in.Runner.Run(ctx, "dpkg", "--print-architecture")
		if err != nil {
			return nil, err
		}
		in.arch = strings.TrimSpace(string(out))
		if err := os.RemoveAll(in.Scratch); err != nil {
			return nil, err
		}
		for _, dir := range []string{"lists/partial", "cache/archives/partial"} {
			if err := os.MkdirAll(filepath.Join(in.Scratch, dir), 0o755); err != nil {
				return nil, err
			}
		}
		in.scratch = in.Scratch
		defer func() {
			in.scratch = ""
			if rmErr := os.RemoveAll(in.Scratch); rmErr != nil && err == nil {
				err = rmErr
			}
		}()
	}
	in.step("Updating the package lists")
	if _, err := in.apt(ctx, "update", "-q"); err != nil {
		return nil, fmt.Errorf("updating the package lists: %w", err)
	}
	// See what apt would do before doing it.
	plan, err := in.apt(ctx, append([]string{"install", "-s", "--no-install-recommends"}, missing...)...)
	if err != nil {
		return nil, fmt.Errorf("planning the install of %s: %w", strings.Join(missing, ", "), err)
	}
	if err := checkPlan(string(plan)); err != nil {
		return nil, err
	}
	in.step("Installing " + strings.Join(missing, ", "))
	if _, err := in.apt(ctx, append([]string{"install", "-y", "-q", "--no-install-recommends"}, missing...)...); err != nil {
		return nil, fmt.Errorf("installing %s: %w", strings.Join(missing, ", "), err)
	}
	if in.scratch == "" {
		_, _ = in.apt(ctx, "clean") // the downloaded packages, not needed any more
	}
	return missing, nil
}

// repair configures packages dpkg was installing when the power was cut.
func (in *Installer) repair(ctx context.Context) error {
	out, err := in.Runner.Run(ctx, "dpkg", "--audit")
	if err != nil && system.ExitCode(err) == -1 {
		return ErrUnsupported
	}
	if strings.TrimSpace(string(out)) == "" {
		return nil
	}
	in.step("Finishing an interrupted package install")
	if _, err := in.Runner.Run(ctx, "env", "DEBIAN_FRONTEND=noninteractive", "dpkg",
		"--force-confdef", "--force-confold", "--configure", "-a"); err != nil {
		return fmt.Errorf("finishing an interrupted package install: %w", err)
	}
	return nil
}

func (in *Installer) apt(ctx context.Context, args ...string) ([]byte, error) {
	options := []string{"DEBIAN_FRONTEND=noninteractive", "apt-get",
		"-o", "DPkg::Lock::Timeout=120",
		"-o", "Dpkg::Options::=--force-confdef", "-o", "Dpkg::Options::=--force-confold"}
	if in.scratch != "" {
		// Compressed lists, no translations, and only this Pi's
		// architecture keep the lists to about 20 MB.
		options = append(options,
			"-o", "Dir::State::Lists="+filepath.Join(in.scratch, "lists"),
			"-o", "Dir::Cache="+filepath.Join(in.scratch, "cache"),
			"-o", "Acquire::GzipIndexes=true", "-o", "Acquire::Languages=none",
			"-o", "APT::Architectures::="+in.arch)
	}
	return in.Runner.Run(ctx, "env", append(options, args...)...)
}

// checkPlan refuses an apt plan that removes packages or touches the boot
// partition. Its lines look like "Inst rsync (3.2.7-1 Debian:12/stable [arm64])".
func checkPlan(plan string) error {
	var removes, boot []string
	for _, line := range strings.Split(plan, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "Remv", "Purg":
			removes = append(removes, fields[1])
		case "Inst":
			for _, p := range bootPackages {
				if strings.HasPrefix(fields[1], p) {
					boot = append(boot, fields[1])
				}
			}
		}
	}
	switch {
	case len(removes) > 0:
		return fmt.Errorf("installing would remove %s; not done", strings.Join(removes, ", "))
	case len(boot) > 0:
		return fmt.Errorf("installing would update %s, which changes the boot partition; update the system with glovebox-boot-maintenance first", strings.Join(boot, ", "))
	}
	return nil
}

// Package provision sets up a freshly flashed Pi from the glovebox.toml on its
// boot partition, which can be edited on any computer before first boot.
//
// The file is glovebox's whole configuration plus a [system] section (Wi-Fi,
// hostname, SSH key). At boot, if it sets anything, it's applied: Wi-Fi
// becomes a NetworkManager connection, the NAS login moves to a root-only
// credentials file, the rest (without secrets) is stored in
// /etc/glovebox/glovebox.toml, and the file on the boot partition is replaced by
// a note. Dropping a new glovebox.toml on the card applies it again.
package provision

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/lubaxe/glovebox/internal/config"
	"github.com/lubaxe/glovebox/internal/system"
)

// Provisioner applies the boot partition's configuration.
type Provisioner struct {
	// Root is prepended to every system path; "/" on the Pi, a temp dir in
	// tests.
	Root string
	// LoginUser gets the SSH key.
	LoginUser string
	Runner    system.Runner
	Log       *slog.Logger
	Now       func() time.Time
	// lookupUser returns a user's uid, gid and home directory.
	lookupUser func(name string) (uid, gid int, home string, err error)
	chown      func(name string, uid, gid int) error
}

// New returns a Provisioner for the running system.
func New(r system.Runner, log *slog.Logger) *Provisioner {
	return &Provisioner{Root: "/", LoginUser: "glovebox", Runner: r, Log: log, Now: time.Now, lookupUser: lookupUser, chown: os.Chown}
}

func (p *Provisioner) path(abs string) string { return filepath.Join(p.Root, filepath.FromSlash(abs)) }

// Apply applies bootFile to the system and stores the resulting config in
// etcFile (both absolute paths). It reports whether anything was applied;
// a missing file or one with only comments does nothing.
func (p *Provisioner) Apply(ctx context.Context, bootFile, etcFile string) (bool, error) {
	cfg, keys, err := config.LoadFile(p.path(bootFile))
	if errors.Is(err, fs.ErrNotExist) || (err == nil && keys == 0) {
		return false, nil
	}
	if err != nil {
		// tell the user on the card itself; they'll look there
		_ = p.note(bootFile, "Glovebox couldn't use glovebox.toml:\n\n"+err.Error()+"\n\nFix the file and boot again.\n")
		return false, err
	}

	var done []string
	s := cfg.System
	if s.Hostname != "" {
		if err := p.setHostname(ctx, s.Hostname); err != nil {
			return false, err
		}
		done = append(done, "hostname "+s.Hostname)
	}
	if s.WiFiCountry != "" {
		if err := p.setWiFiCountry(ctx, s.WiFiCountry); err != nil {
			return false, err
		}
		done = append(done, "Wi-Fi country "+s.WiFiCountry)
	}
	if s.WiFiSSID != "" {
		if err := p.setWiFi(ctx, s.WiFiSSID, s.WiFiPassword); err != nil {
			return false, err
		}
		done = append(done, fmt.Sprintf("Wi-Fi network %q", s.WiFiSSID))
	}
	if strings.TrimSpace(s.SSHKey) != "" {
		if err := p.addSSHKey(s.SSHKey); err != nil {
			return false, err
		}
		done = append(done, "SSH key for "+p.LoginUser)
	}
	if cfg.Archive.Username != "" {
		creds := fmt.Sprintf("username=%s\npassword=%s\n", cfg.Archive.Username, cfg.Archive.Password)
		if err := writeFile(p.path(cfg.Archive.Credentials), []byte(creds), 0o600); err != nil {
			return false, err
		}
		done = append(done, "NAS login for "+cfg.Archive.Username)
	}

	// Keep everything but the secrets. The Wi-Fi network goes with its
	// password: NetworkManager has both, and a network without a password
	// wouldn't pass validation when the stored config is loaded.
	stored := cfg
	stored.System.WiFiSSID, stored.System.WiFiPassword = "", ""
	stored.Archive.Username, stored.Archive.Password = "", ""
	var buf bytes.Buffer
	buf.WriteString("# Applied from the boot partition's glovebox.toml on " + p.Now().Format(time.RFC1123) + ".\n")
	buf.WriteString("# Secrets were moved to root-only files.\n\n")
	if err := toml.NewEncoder(&buf).Encode(stored); err != nil {
		return false, err
	}
	if err := stored.Validate(); err != nil { // glovebox must be able to load it
		return false, fmt.Errorf("stored config wouldn't load: %w", err)
	}
	if err := writeFile(p.path(etcFile), buf.Bytes(), 0o600); err != nil {
		return false, err
	}
	if err := os.Remove(p.path(bootFile)); err != nil {
		return false, err
	}
	_ = p.note(bootFile, fmt.Sprintf("Glovebox applied glovebox.toml on %s:\n\n- %s\n\n"+
		"The settings are now in %s and secrets were moved to root-only files.\n"+
		"To change something, put a new glovebox.toml here and restart the Pi.\n",
		p.Now().Format(time.RFC1123), strings.Join(done, "\n- "), etcFile))
	p.Log.Info("applied boot partition config", "applied", strings.Join(done, ", "))
	return true, nil
}

// note writes a text file next to bootFile for the user to read on a PC.
func (p *Provisioner) note(bootFile, text string) error {
	name := filepath.Join(filepath.Dir(p.path(bootFile)), "glovebox-status.txt")
	return os.WriteFile(name, []byte(strings.ReplaceAll(text, "\n", "\r\n")), 0o644) // read on Windows
}

func (p *Provisioner) setHostname(ctx context.Context, name string) error {
	if err := writeFile(p.path("/etc/hostname"), []byte(name+"\n"), 0o644); err != nil {
		return err
	}
	hosts := p.path("/etc/hosts")
	b, err := os.ReadFile(hosts)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	line := "127.0.1.1\t" + name
	re := regexp.MustCompile(`(?m)^127\.0\.1\.1\s.*$`)
	if re.Match(b) {
		b = re.ReplaceAll(b, []byte(line))
	} else {
		b = append(b, []byte(line+"\n")...)
	}
	if err := writeFile(hosts, b, 0o644); err != nil {
		return err
	}
	_, _ = p.Runner.Run(ctx, "hostname", name) // takes effect now; the files make it stick
	return nil
}

// setWiFiCountry sets the regulatory domain for this boot and in
// cmdline.txt for later boots.
func (p *Provisioner) setWiFiCountry(ctx context.Context, cc string) error {
	cmdline := p.path("/boot/firmware/cmdline.txt")
	b, err := os.ReadFile(cmdline)
	if err != nil {
		return err
	}
	line := strings.TrimSpace(string(b))
	opt := "cfg80211.ieee80211_regdom=" + cc
	re := regexp.MustCompile(`cfg80211\.ieee80211_regdom=\S*`)
	if re.MatchString(line) {
		line = re.ReplaceAllString(line, opt)
	} else {
		line += " " + opt
	}
	if err := writeFile(cmdline, []byte(line+"\n"), 0o644); err != nil {
		return err
	}
	_, _ = p.Runner.Run(ctx, "iw", "reg", "set", cc)
	return nil
}

// setWiFi writes a NetworkManager connection, readable only by root.
func (p *Provisioner) setWiFi(ctx context.Context, ssid, password string) error {
	conn := strings.Join([]string{
		"[connection]",
		"id=glovebox-wifi",
		"type=wifi",
		"autoconnect=true",
		"",
		"[wifi]",
		"mode=infrastructure",
		"ssid=" + keyfileEscape(ssid),
		"",
		"[wifi-security]",
		"key-mgmt=wpa-psk",
		"psk=" + keyfileEscape(password),
		"",
		"[ipv4]",
		"method=auto",
		"",
		"[ipv6]",
		"method=auto",
		"",
	}, "\n")
	name := p.path("/etc/NetworkManager/system-connections/glovebox-wifi.nmconnection")
	if err := writeFile(name, []byte(conn), 0o600); err != nil {
		return err
	}
	// NetworkManager may not be running yet; it reads the file when it starts
	_, _ = p.Runner.Run(ctx, "nmcli", "connection", "reload")
	return nil
}

// keyfileEscape escapes a value for a GKeyFile (NetworkManager keyfile).
func keyfileEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, "\n", `\n`, "\t", `\t`, "\r", `\r`)
	s = r.Replace(s)
	if strings.HasPrefix(s, " ") {
		s = `\s` + s[1:]
	}
	return s
}

func (p *Provisioner) addSSHKey(key string) error {
	key = strings.TrimSpace(key)
	uid, gid, home, err := p.lookupUser(p.LoginUser)
	if err != nil {
		return fmt.Errorf("looking up %s: %w", p.LoginUser, err)
	}
	dir := filepath.Join(p.path(home), ".ssh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	file := filepath.Join(dir, "authorized_keys")
	b, err := os.ReadFile(file)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == key {
			return nil // already there
		}
	}
	if len(b) > 0 && !bytes.HasSuffix(b, []byte("\n")) {
		b = append(b, '\n')
	}
	b = append(b, []byte(key+"\n")...)
	if err := os.WriteFile(file, b, 0o600); err != nil {
		return err
	}
	for _, f := range []string{dir, file} {
		if err := p.chown(f, uid, gid); err != nil {
			return err
		}
	}
	return nil
}

// lookupUser finds the login user. On a Raspberry Pi OS that Glovebox was
// installed on (rather than flashed), there's no "glovebox" user, so the
// first user (uid 1000, the one Raspberry Pi Imager creates) gets the key.
func lookupUser(name string) (int, int, string, error) {
	u, err := user.Lookup(name)
	if err != nil {
		var ferr error
		if u, ferr = user.LookupId("1000"); ferr != nil {
			return 0, 0, "", err
		}
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return 0, 0, "", err
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return 0, 0, "", err
	}
	return uid, gid, u.HomeDir, nil
}

// writeFile writes a file atomically, creating its directory.
func writeFile(name string, b []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		return err
	}
	tmp := name + ".tmp"
	if err := os.WriteFile(tmp, b, perm); err != nil {
		return err
	}
	if err := os.Chmod(tmp, perm); err != nil { // WriteFile keeps an existing file's mode
		return err
	}
	return os.Rename(tmp, name)
}

// UnblockRadios clears the software rf-kill switch for Wi-Fi and Bluetooth,
// which images and earlier boots can leave on. Hardware switches are left
// alone (the Pi has none).
func UnblockRadios(root string) (unblocked []string) {
	dirs, _ := filepath.Glob(filepath.Join(root, "sys", "class", "rfkill", "rfkill*"))
	for _, d := range dirs {
		typ, err := os.ReadFile(filepath.Join(d, "type"))
		if err != nil {
			continue
		}
		t := strings.TrimSpace(string(typ))
		if t != "wlan" && t != "bluetooth" {
			continue
		}
		soft, err := os.ReadFile(filepath.Join(d, "soft"))
		if err != nil || strings.TrimSpace(string(soft)) == "0" {
			continue
		}
		if os.WriteFile(filepath.Join(d, "soft"), []byte("0"), 0o644) == nil {
			unblocked = append(unblocked, t)
		}
	}
	return unblocked
}

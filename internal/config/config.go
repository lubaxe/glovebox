// Package config loads glovebox's TOML configuration.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// DefaultPath is where the service looks for its config on the Pi.
const DefaultPath = "/etc/glovebox/glovebox.toml"

// Config is the whole glovebox configuration.
type Config struct {
	Log       LogConfig       `toml:"log"`
	Web       WebConfig       `toml:"web"`
	Drive     DriveConfig     `toml:"drive"`
	Snapshots SnapshotsConfig `toml:"snapshots"`
	Archive   ArchiveConfig   `toml:"archive"`
	KeepAwake KeepAwakeConfig `toml:"keep_awake"`
	System    SystemConfig    `toml:"system"`
}

// SystemConfig sets up the Pi itself. It's read from glovebox.toml on the
// boot partition at first boot (see package provision); secrets in it are
// moved to root-only files and removed from the stored config.
type SystemConfig struct {
	Hostname     string `toml:"hostname"`
	WiFiSSID     string `toml:"wifi_ssid"`
	WiFiPassword string `toml:"wifi_password"`
	// WiFiCountry is the two-letter country code for Wi-Fi channel rules.
	WiFiCountry string `toml:"wifi_country"`
	// SSHKey is a public key allowed to log in over SSH.
	SSHKey string `toml:"ssh_key"`
	// SetupNetwork offers an open Wi-Fi network, Glovebox-XXXX, whose
	// sign-in page runs the setup wizard, until the Pi has joined Wi-Fi and
	// the wizard is finished.
	SetupNetwork bool `toml:"setup_network"`
}

// DriveConfig controls the disk image presented to the car.
type DriveConfig struct {
	// Enabled turns on the drive, gadget and snapshots. It needs a
	// reflink-capable (XFS) filesystem mounted at Backingfiles.
	Enabled      bool   `toml:"enabled"`
	Backingfiles string `toml:"backingfiles"`
	// CamSize is the size of the car's drive; 0 means as large as fits
	// while leaving Snapshots.MinFree.
	CamSize ByteSize `toml:"cam_size"`
	// SerialConsole adds a USB serial port to the gadget, so the Pi has a
	// login console on the same cable.
	SerialConsole bool `toml:"serial_console"`
	MaxPowerMA    int  `toml:"max_power_ma"`
	// KeepFreePercent: at startup, before the car sees the drive, archived
	// Sentry and Saved events are deleted from it (oldest first) until this
	// much of it is free. Events that aren't archived are never deleted.
	KeepFreePercent int `toml:"keep_free_percent"`
	// Music, LightShow and Boombox are extra drives for the car. Each one is
	// off while its size is 0.
	Music     ExtraDriveConfig `toml:"music"`
	LightShow ExtraDriveConfig `toml:"lightshow"`
	Boombox   ExtraDriveConfig `toml:"boombox"`
	// Encryption controls the encrypted drive, once the drive has been
	// encrypted from the web UI (Settings).
	Encryption EncryptionConfig `toml:"encryption"`
}

// EncryptionConfig controls when the encrypted backingfiles volume opens.
// Whether the drive is encrypted isn't a setting: it is if the card has
// the encrypted partition.
type EncryptionConfig struct {
	// CarCheck is "enforce": the drive opens only once the paired car has
	// been seen over Bluetooth (or with the user's key); or "audit": the car
	// check runs and is logged, but the drive opens either way.
	CarCheck string `toml:"car_check"`
	// Device is the partition or image file holding the encrypted volume;
	// empty means the partition labelled glovebox-crypt. For testing.
	Device string `toml:"device"`
}

// Extras returns the extra drives by name: music, lightshow, boombox.
func (d DriveConfig) Extras() map[string]ExtraDriveConfig {
	return map[string]ExtraDriveConfig{"music": d.Music, "lightshow": d.LightShow, "boombox": d.Boombox}
}

// ExtraDriveConfig is an extra drive the car reads but doesn't record to.
// Its files mirror a folder on the archive target: whenever the car is
// home, Glovebox syncs it and, if anything changed, briefly ejects the
// drive to swap in the new version.
type ExtraDriveConfig struct {
	// Size turns the drive on. Changing it recreates the drive, which is
	// then filled again from Source.
	Size ByteSize `toml:"size"`
	// Source is the folder on the archive target to mirror: for SMB
	// "share/folder" (a share, optionally with a folder in it), for NFS an
	// export path, for rsync a path on the server, for rclone a path on the
	// archive remote or a whole "remote:path". Empty means nothing syncs.
	Source string `toml:"source"`
}

// KeepAwakeConfig keeps the car from sleeping, and cutting the Pi's power,
// while clips are being archived. It talks to the car over Bluetooth with
// Tesla's tesla-control and a key paired with the car as Driver. Nothing
// happens until the key is paired, which is done from the web UI's Car
// page.
type KeepAwakeConfig struct {
	Enabled bool `toml:"enabled"`
	// VIN is optional: it can also be set on the Car page.
	VIN string `toml:"vin"`
	// KeyDir holds the Bluetooth key and the pairing state.
	KeyDir string `toml:"key_dir"`
	// Command is the tesla-control program.
	Command string `toml:"command"`
	// Interval is how often to wake the car while archiving.
	Interval Duration `toml:"interval"`
	// Only what the car recorded while away keeps it awake, and for at most
	// MaxDuration; after that it's left alone for Cooldown.
	MaxDuration Duration `toml:"max_duration"`
	Cooldown    Duration `toml:"cooldown"`
}

// SnapshotsConfig controls snapshots of the cam disk.
type SnapshotsConfig struct {
	Interval Duration `toml:"interval"`
	// MinFree is how much space to keep free on the backingfiles
	// filesystem; the oldest snapshots are deleted to stay above it.
	MinFree ByteSize `toml:"min_free"`
}

// LogConfig controls logging.
type LogConfig struct {
	// Level is one of debug, info, warn, error.
	Level string `toml:"level"`
}

// WebConfig controls the HTTP server.
type WebConfig struct {
	// Listen is the address the HTTP server listens on, e.g. ":8080".
	Listen string `toml:"listen"`
}

// Default returns the configuration used for anything the file doesn't set.
func Default() Config {
	return Config{
		Log: LogConfig{Level: "info"},
		Web: WebConfig{Listen: ":8080"},
		Drive: DriveConfig{
			Enabled:         true,
			Backingfiles:    "/backingfiles",
			MaxPowerMA:      200,
			KeepFreePercent: 25,
			Encryption:      EncryptionConfig{CarCheck: "enforce"},
		},
		Snapshots: SnapshotsConfig{
			Interval: Duration(58 * time.Minute),
			MinFree:  4 << 30,
		},
		System: SystemConfig{Hostname: "glovebox", SetupNetwork: true},
		KeepAwake: KeepAwakeConfig{
			Enabled:     true,
			KeyDir:      "/etc/glovebox/ble",
			Command:     "tesla-control",
			Interval:    Duration(time.Minute),
			MaxDuration: Duration(time.Hour),
			Cooldown:    Duration(6 * time.Hour),
		},
		Archive: defaultArchive(),
	}
}

// Load reads the config file at path on top of the defaults. A missing file
// is not an error: the defaults are returned and missing is set.
func Load(path string) (cfg Config, missing bool, err error) {
	cfg, _, err = LoadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Default(), true, nil
	}
	return cfg, false, err
}

// LoadFile is Load for a file that must exist; it also returns how many
// settings the file sets (0 for a file that's all comments).
func LoadFile(path string) (cfg Config, keys int, err error) {
	cfg = Default()
	md, err := toml.DecodeFile(path, &cfg)
	if errors.Is(err, fs.ErrNotExist) {
		return cfg, 0, err
	}
	if err != nil {
		return cfg, 0, fmt.Errorf("reading %s: %w", path, err)
	}
	cfg.Archive.migrateLegacy(md)
	for _, k := range md.Keys() {
		if md.Type(k...) != "Hash" { // a setting, not a [section] header
			keys++
		}
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		unknown := make([]string, len(undecoded))
		for i, k := range undecoded {
			unknown[i] = k.String()
		}
		return cfg, keys, fmt.Errorf("%s: unknown settings: %s", path, strings.Join(unknown, ", "))
	}
	return cfg, keys, cfg.Validate()
}

// Validate checks that the values make sense.
func (c Config) Validate() error {
	if _, err := c.Log.SlogLevel(); err != nil {
		return err
	}
	if _, _, err := net.SplitHostPort(c.Web.Listen); err != nil {
		return fmt.Errorf("web.listen %q: %w", c.Web.Listen, err)
	}
	if c.Drive.Enabled {
		if !path.IsAbs(c.Drive.Backingfiles) {
			return fmt.Errorf("drive.backingfiles %q must be an absolute path", c.Drive.Backingfiles)
		}
		if c.Drive.CamSize != 0 && c.Drive.CamSize < 1<<30 {
			return fmt.Errorf("drive.cam_size must be at least 1G, or 0 for as large as fits")
		}
		if c.Drive.KeepFreePercent < 0 || c.Drive.KeepFreePercent > 100 {
			return fmt.Errorf("drive.keep_free_percent must be between 0 and 100")
		}
		if c.Drive.MaxPowerMA < 0 || c.Drive.MaxPowerMA > 500 {
			return fmt.Errorf("drive.max_power_ma must be between 0 and 500")
		}
		if time.Duration(c.Snapshots.Interval) < time.Minute {
			return fmt.Errorf("snapshots.interval must be at least 1m")
		}
		if cc := c.Drive.Encryption.CarCheck; cc != "enforce" && cc != "audit" {
			return fmt.Errorf("drive.encryption.car_check %q must be enforce or audit", cc)
		}
		if d := c.Drive.Encryption.Device; d != "" && !path.IsAbs(d) {
			return fmt.Errorf("drive.encryption.device %q must be an absolute path", d)
		}
		for name, x := range c.Drive.Extras() {
			if x.Size != 0 && x.Size < 32<<20 {
				return fmt.Errorf("drive.%s.size must be at least 32M, or 0 for no %s drive", name, name)
			}
		}
	}
	if err := c.System.Validate(); err != nil {
		return err
	}
	if c.KeepAwake.Enabled {
		if c.KeepAwake.VIN != "" && !vinPattern.MatchString(c.KeepAwake.VIN) {
			return fmt.Errorf("keep_awake.vin %q must be the car's 17-character VIN", c.KeepAwake.VIN)
		}
		if !path.IsAbs(c.KeepAwake.KeyDir) {
			return fmt.Errorf("keep_awake.key_dir %q must be an absolute path", c.KeepAwake.KeyDir)
		}
		if time.Duration(c.KeepAwake.Interval) < 10*time.Second {
			return fmt.Errorf("keep_awake.interval must be at least 10s")
		}
	}
	if c.Archive.Enabled && !c.Drive.Enabled {
		return fmt.Errorf("archive.enabled needs drive.enabled")
	}
	if err := c.Archive.Validate(); err != nil {
		return err
	}
	return nil
}

// VINs use digits and capital letters other than I, O and Q.
var vinPattern = regexp.MustCompile(`^[A-HJ-NPR-Z0-9]{17}$`)

// SlogLevel converts the configured level name to a slog.Level.
func (l LogConfig) SlogLevel() (slog.Level, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(l.Level)); err != nil {
		return 0, fmt.Errorf("log.level %q: must be debug, info, warn or error", l.Level)
	}
	return level, nil
}

var (
	hostnameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	countryRe  = regexp.MustCompile(`^[A-Z]{2}$`)
)

// Validate checks the [system] section.
func (s SystemConfig) Validate() error {
	if s.Hostname != "" && !hostnameRe.MatchString(s.Hostname) {
		return fmt.Errorf("system.hostname %q: use lowercase letters, digits and dashes", s.Hostname)
	}
	if (s.WiFiSSID == "") != (s.WiFiPassword == "") {
		return fmt.Errorf("system.wifi_ssid and system.wifi_password must be set together")
	}
	if n := len(s.WiFiPassword); n != 0 && (n < 8 || n > 63) {
		return fmt.Errorf("system.wifi_password must be 8 to 63 characters (WPA2)")
	}
	if s.WiFiCountry != "" && !countryRe.MatchString(s.WiFiCountry) {
		return fmt.Errorf("system.wifi_country %q: use a two-letter code like CA or US", s.WiFiCountry)
	}
	if k := strings.TrimSpace(s.SSHKey); k != "" && !strings.HasPrefix(k, "ssh-") && !strings.HasPrefix(k, "ecdsa-") && !strings.HasPrefix(k, "sk-") {
		return fmt.Errorf("system.ssh_key doesn't look like an SSH public key (it should start with ssh-ed25519, ssh-rsa or ecdsa-)")
	}
	return nil
}

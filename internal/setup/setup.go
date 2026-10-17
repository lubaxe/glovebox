// Package setup reads and changes Glovebox's configuration for the setup
// page, so nobody has to edit glovebox.toml by hand: where clips are
// archived (with a connection test) and the extra drives.
//
// It's kept apart from the web server so the same setup can be offered on
// other pages, like a sign-in page on the Pi's own Wi-Fi network.
package setup

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/lubaxe/glovebox/internal/archive"
	"github.com/lubaxe/glovebox/internal/config"
	"github.com/lubaxe/glovebox/internal/system"
)

// Setup edits the config file at ConfigPath.
type Setup struct {
	ConfigPath string
	// Target options for connection tests.
	Options archive.Options
	// Runner runs ssh-keygen.
	Runner system.Runner
	// Gateway returns the default gateway's address, suggested as the check
	// address for rclone; nil means none.
	Gateway func() string
	Now     func() time.Time

	mu sync.Mutex // one change at a time
}

// SMB is the SMB settings on the page. The password is never sent back:
// PasswordSet says whether one is stored, and an empty Password on saving
// keeps it.
type SMB struct {
	Server      string `json:"server"`
	Share       string `json:"share"`
	Folder      string `json:"folder"`
	Username    string `json:"username"`
	Password    string `json:"password,omitempty"`
	PasswordSet bool   `json:"password_set"`
	Version     string `json:"version"`
}

// NFS is the NFS settings on the page.
type NFS struct {
	Server string `json:"server"`
	Export string `json:"export"`
	Folder string `json:"folder"`
}

// Rsync is the rsync settings on the page.
type Rsync struct {
	Server string `json:"server"`
	Port   int    `json:"port"`
	User   string `json:"user"`
	Path   string `json:"path"`
	// PublicKey is Glovebox's SSH key for the server's authorized_keys,
	// once it exists.
	PublicKey string `json:"public_key,omitempty"`
}

// Rclone is the rclone settings on the page.
type Rclone struct {
	Remote string `json:"remote"`
	Flags  string `json:"flags"`
	// Remotes are the remotes defined in the stored rclone.conf.
	Remotes []string `json:"remotes"`
}

// Archive is the [archive] settings on the page.
type Archive struct {
	Enabled      bool   `json:"enabled"`
	Type         string `json:"type"`
	CheckAddress string `json:"check_address"`
	Recent       bool   `json:"recent"`
	SMB          SMB    `json:"smb"`
	NFS          NFS    `json:"nfs"`
	Rsync        Rsync  `json:"rsync"`
	Rclone       Rclone `json:"rclone"`
}

// Drive is an extra drive on the page.
type Drive struct {
	Size   string `json:"size"` // e.g. "8G"; "0" is off
	Source string `json:"source"`
}

// Drives are the extra drives on the page.
type Drives struct {
	Music     Drive `json:"music"`
	LightShow Drive `json:"lightshow"`
	Boombox   Drive `json:"boombox"`
}

// Settings is everything the page shows and saves.
type Settings struct {
	Archive Archive `json:"archive"`
	Drives  Drives  `json:"drives"`
	// Gateway is the network's router, a likely check address.
	Gateway string `json:"gateway,omitempty"`
}

func (s *Setup) load() (config.Config, error) {
	cfg, _, err := config.Load(s.ConfigPath)
	return cfg, err
}

// Get returns the current settings.
func (s *Setup) Get() (Settings, error) {
	cfg, err := s.load()
	if err != nil {
		return Settings{}, err
	}
	a := cfg.Archive
	out := Settings{Archive: Archive{
		Enabled: a.Enabled, Type: a.Type, CheckAddress: a.CheckAddress, Recent: a.Recent,
		SMB: SMB{Server: a.SMB.Server, Share: a.SMB.Share, Folder: a.SMB.Folder, Version: a.SMB.Version},
		NFS: NFS{Server: a.NFS.Server, Export: a.NFS.Export, Folder: a.NFS.Folder},
		Rsync: Rsync{Server: a.Rsync.Server, Port: a.Rsync.Port, User: a.Rsync.User, Path: a.Rsync.Path,
			PublicKey: readPublicKey(a.Rsync.Key)},
		Rclone: Rclone{Remote: a.Rclone.Remote, Flags: a.Rclone.Flags, Remotes: rcloneRemotes(a.Rclone.Config)},
	}}
	if user, _, _, err := archive.ReadCredentials(a.SMB.Credentials); err == nil {
		out.Archive.SMB.Username, out.Archive.SMB.PasswordSet = user, true
	}
	d := cfg.Drive
	out.Drives = Drives{Music: drive(d.Music), LightShow: drive(d.LightShow), Boombox: drive(d.Boombox)}
	if s.Gateway != nil {
		out.Gateway = s.Gateway()
	}
	return out, nil
}

func drive(x config.ExtraDriveConfig) Drive {
	b, _ := x.Size.MarshalText()
	return Drive{Size: string(b), Source: x.Source}
}

// apply puts the page's settings into cfg. It returns the SMB login to
// store, if one was given.
func apply(cfg *config.Config, in Settings) (user, password string, err error) {
	a := &cfg.Archive
	p := in.Archive
	a.Enabled, a.Type, a.CheckAddress, a.Recent = p.Enabled, p.Type, strings.TrimSpace(p.CheckAddress), p.Recent
	a.SMB.Server, a.SMB.Share, a.SMB.Folder, a.SMB.Version = trim(p.SMB.Server), trim(p.SMB.Share), strings.Trim(trim(p.SMB.Folder), "/"), p.SMB.Version
	a.NFS.Server, a.NFS.Export, a.NFS.Folder = trim(p.NFS.Server), trim(p.NFS.Export), strings.Trim(trim(p.NFS.Folder), "/")
	a.Rsync.Server, a.Rsync.User, a.Rsync.Path = trim(p.Rsync.Server), trim(p.Rsync.User), trim(p.Rsync.Path)
	if p.Rsync.Port != 0 {
		a.Rsync.Port = p.Rsync.Port
	}
	a.Rclone.Remote, a.Rclone.Flags = trim(p.Rclone.Remote), trim(p.Rclone.Flags)

	for _, x := range []struct {
		name string
		in   Drive
		out  *config.ExtraDriveConfig
	}{
		{"music", in.Drives.Music, &cfg.Drive.Music},
		{"lightshow", in.Drives.LightShow, &cfg.Drive.LightShow},
		{"boombox", in.Drives.Boombox, &cfg.Drive.Boombox},
	} {
		size := strings.TrimSpace(x.in.Size)
		if size == "" {
			size = "0"
		}
		if err := x.out.Size.UnmarshalText([]byte(size)); err != nil {
			return "", "", fmt.Errorf("%s drive: %w", x.name, err)
		}
		x.out.Source = strings.TrimSpace(x.in.Source)
	}
	return trim(p.SMB.Username), p.SMB.Password, nil
}

func trim(s string) string { return strings.TrimSpace(s) }

// Test checks the archive settings in, as they would be saved, without
// saving them. A missing SMB password means the stored one.
func (s *Setup) Test(ctx context.Context, in Settings) []archive.CheckStep {
	fail := func(name string, err error) []archive.CheckStep {
		return []archive.CheckStep{{Name: name, Detail: err.Error()}}
	}
	cfg, err := s.load()
	if err != nil {
		return fail("Read the settings", err)
	}
	user, password, err := apply(&cfg, in)
	if err != nil {
		return fail("Check the settings", err)
	}
	cfg.Archive.Enabled = true // test what archiving would use
	if user != "" && password == "" {
		if u, p, _, err := archive.ReadCredentials(cfg.Archive.SMB.Credentials); err == nil && u == user {
			password = p
		}
	}
	if cfg.Archive.Type == config.TypeSMB && (user == "" || password == "") {
		return fail("Check the settings", errors.New("enter the share's username and password"))
	}
	cfg.Archive.SMB.Username, cfg.Archive.SMB.Password = user, password
	if err := cfg.Archive.Validate(); err != nil {
		return fail("Check the settings", err)
	}
	t, err := archive.NewTarget(cfg.Archive, s.Options)
	if err != nil {
		return fail("Check the settings", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	return archive.Check(ctx, t)
}

// Save stores in: the SMB login in its credentials file, the rest in the
// config file. Glovebox has to restart to use them.
func (s *Setup) Save(in Settings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg, err := s.load()
	if err != nil {
		return err
	}
	user, password, err := apply(&cfg, in)
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	if cfg.Archive.Type == config.TypeSMB && user != "" {
		stored, _, _, err := archive.ReadCredentials(cfg.Archive.SMB.Credentials)
		if password == "" && (err != nil || stored != user) {
			return fmt.Errorf("enter the password for %s", user)
		}
		if password != "" {
			if err := writeCredentials(cfg.Archive.SMB.Credentials, user, password, ""); err != nil {
				return err
			}
		}
	}
	if cfg.Archive.Enabled && cfg.Archive.Type == config.TypeSMB {
		if _, _, _, err := archive.ReadCredentials(cfg.Archive.SMB.Credentials); err != nil {
			return errors.New("enter the share's username and password")
		}
	}
	return s.write(cfg)
}

// write stores cfg in the config file, atomically.
func (s *Setup) write(cfg config.Config) error {
	var buf bytes.Buffer
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	buf.WriteString("# Saved by Glovebox's setup page on " + now().Format(time.RFC1123) + ".\n")
	buf.WriteString("# Secrets are in root-only files. All settings: deploy/glovebox.toml in the repo.\n\n")
	if err := toml.NewEncoder(&buf).Encode(cfg); err != nil {
		return err
	}
	return writeFile(s.ConfigPath, buf.Bytes(), 0o600)
}

func writeCredentials(path, user, password, domain string) error {
	if strings.ContainsAny(user+password+domain, "\n\r") {
		return errors.New("the username and password can't contain line breaks")
	}
	creds := fmt.Sprintf("username=%s\npassword=%s\n", user, password)
	if domain != "" {
		creds += "domain=" + domain + "\n"
	}
	return writeFile(path, []byte(creds), 0o600)
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
	if err := os.Chmod(tmp, perm); err != nil {
		return err
	}
	return os.Rename(tmp, name)
}

// SSHKey returns Glovebox's public SSH key for rsync, creating the key pair
// if there's none yet.
func (s *Setup) SSHKey(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg, err := s.load()
	if err != nil {
		return "", err
	}
	key := cfg.Archive.Rsync.Key
	if pub := readPublicKey(key); pub != "" {
		return pub, nil
	}
	if err := os.MkdirAll(filepath.Dir(key), 0o700); err != nil {
		return "", err
	}
	host, _ := os.Hostname()
	if _, err := s.Runner.Run(ctx, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "glovebox@"+host, "-f", key); err != nil {
		return "", fmt.Errorf("creating an SSH key: %w", err)
	}
	if pub := readPublicKey(key); pub != "" {
		return pub, nil
	}
	return "", errors.New("ssh-keygen made no public key")
}

func readPublicKey(key string) string {
	b, err := os.ReadFile(key + ".pub")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// SetRcloneConfig stores text as the rclone.conf and returns its remotes.
func (s *Setup) SetRcloneConfig(text string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg, err := s.load()
	if err != nil {
		return nil, err
	}
	if strings.Contains(text, "RCLONE_ENCRYPT_V0:") {
		return nil, errors.New("encrypted rclone configs aren't supported; remove the password with rclone config")
	}
	remotes := parseRemotes(text)
	if len(remotes) == 0 {
		return nil, errors.New("no remotes in it: paste the whole rclone.conf, with [name] lines")
	}
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	if err := writeFile(cfg.Archive.Rclone.Config, []byte(text), 0o600); err != nil {
		return nil, err
	}
	return remotes, nil
}

func rcloneRemotes(path string) []string {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) || err != nil {
		return []string{}
	}
	return parseRemotes(string(b))
}

// parseRemotes returns the [section] names of an rclone.conf.
func parseRemotes(text string) []string {
	out := []string{}
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") && len(line) > 2 {
			out = append(out, line[1:len(line)-1])
		}
	}
	return out
}

// Gateway reads the default gateway from /proc/net/route.
func Gateway() string {
	b, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return ""
	}
	return parseGateway(string(b))
}

func parseGateway(routes string) string {
	for _, line := range strings.Split(routes, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || f[1] != "00000000" {
			continue
		}
		var ip [4]byte
		if _, err := fmt.Sscanf(f[2], "%02x%02x%02x%02x", &ip[3], &ip[2], &ip[1], &ip[0]); err != nil {
			continue
		}
		return fmt.Sprintf("%d.%d.%d.%d", ip[0], ip[1], ip[2], ip[3])
	}
	return ""
}

package setup

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/lubaxe/glovebox/internal/config"
	"github.com/lubaxe/glovebox/internal/system"
)

// newSetup returns a Setup whose config keeps its secrets in a temp dir.
func newSetup(t *testing.T) *Setup {
	t.Helper()
	// the config wants absolute paths; on Windows, ones on the current drive
	dir := t.TempDir()
	dir = strings.TrimPrefix(dir, filepath.VolumeName(dir))
	conf := filepath.Join(dir, "glovebox.toml")
	content := `[archive.smb]
credentials = "` + filepath.ToSlash(filepath.Join(dir, "smb-credentials")) + `"
[archive.rsync]
key = "` + filepath.ToSlash(filepath.Join(dir, "ssh", "id_ed25519")) + `"
known_hosts = "` + filepath.ToSlash(filepath.Join(dir, "ssh", "known_hosts")) + `"
[archive.rclone]
config = "` + filepath.ToSlash(filepath.Join(dir, "rclone.conf")) + `"
`
	if err := os.WriteFile(conf, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return &Setup{ConfigPath: conf, Runner: &system.FakeRunner{}}
}

func smbSettings() Settings {
	return Settings{
		Archive: Archive{Enabled: true, Type: "smb", SMB: SMB{Server: "192.168.1.10", Share: "TeslaCam", Username: "carcam", Password: "pw", Version: "2.1"}},
		Drives:  Drives{Music: Drive{Size: "8G", Source: "Music"}, LightShow: Drive{Size: "0"}},
	}
}

func TestSaveAndGet(t *testing.T) {
	s := newSetup(t)
	if err := s.Save(smbSettings()); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := config.Load(s.ConfigPath)
	if err != nil {
		t.Fatalf("saved config doesn't load: %v", err)
	}
	if !cfg.Archive.Enabled || cfg.Archive.SMB.Server != "192.168.1.10" || cfg.Drive.Music.Size != 8<<30 || cfg.Drive.Music.Source != "Music" {
		t.Errorf("cfg = %+v", cfg)
	}
	b, _ := os.ReadFile(s.ConfigPath)
	if strings.Contains(string(b), `"pw"`) {
		t.Errorf("the password is in the config:\n%s", b)
	}
	if creds, _ := os.ReadFile(cfg.Archive.SMB.Credentials); string(creds) != "username=carcam\npassword=pw\n" {
		t.Errorf("credentials = %q", creds)
	}

	got, err := s.Get()
	if err != nil {
		t.Fatal(err)
	}
	if got.Archive.SMB.Username != "carcam" || !got.Archive.SMB.PasswordSet || got.Archive.SMB.Password != "" {
		t.Errorf("smb = %+v", got.Archive.SMB)
	}
	if got.Drives.Music != (Drive{Size: "8G", Source: "Music"}) || got.Drives.Boombox.Size != "0" {
		t.Errorf("drives = %+v", got.Drives)
	}

	// saving again without the password keeps it
	in := smbSettings()
	in.Archive.SMB.Password = ""
	in.Archive.Recent = true
	if err := s.Save(in); err != nil {
		t.Fatal(err)
	}
	if creds, _ := os.ReadFile(cfg.Archive.SMB.Credentials); !strings.Contains(string(creds), "password=pw") {
		t.Errorf("credentials = %q", creds)
	}
	// but a new user needs one
	in.Archive.SMB.Username = "other"
	if err := s.Save(in); err == nil || !strings.Contains(err.Error(), "password for other") {
		t.Errorf("err = %v", err)
	}
}

func TestSaveRejectsBadSettings(t *testing.T) {
	s := newSetup(t)
	before, _ := os.ReadFile(s.ConfigPath)
	in := smbSettings()
	in.Archive.SMB.Server = ""
	if err := s.Save(in); err == nil || !strings.Contains(err.Error(), "archive.smb.server") {
		t.Errorf("err = %v", err)
	}
	in = smbSettings()
	in.Drives.Boombox.Size = "lots"
	if err := s.Save(in); err == nil || !strings.Contains(err.Error(), "boombox") {
		t.Errorf("err = %v", err)
	}
	if after, _ := os.ReadFile(s.ConfigPath); string(after) != string(before) {
		t.Error("a rejected save changed the config")
	}
}

func TestTestReportsBadSettings(t *testing.T) {
	s := newSetup(t)
	in := smbSettings()
	in.Archive.SMB.Password = ""
	steps := s.Test(context.Background(), in)
	if len(steps) != 1 || steps[0].OK || !strings.Contains(steps[0].Detail, "password") {
		t.Errorf("steps = %+v", steps)
	}
	in = Settings{Archive: Archive{Type: "rclone", Rclone: Rclone{Remote: "gdrive:TeslaCam"}}}
	steps = s.Test(context.Background(), in)
	if len(steps) != 1 || !strings.Contains(steps[0].Detail, "check_address") {
		t.Errorf("steps = %+v", steps)
	}
}

func TestSSHKey(t *testing.T) {
	s := newSetup(t)
	r := &system.FakeRunner{Handler: func(cmd string) ([]byte, error) {
		f := strings.Fields(cmd)
		key := f[len(f)-1]
		return nil, os.WriteFile(key+".pub", []byte("ssh-ed25519 AAAA glovebox@pi\n"), 0o644)
	}}
	s.Runner = r
	for i := 0; i < 2; i++ {
		pub, err := s.SSHKey(context.Background())
		if err != nil || pub != "ssh-ed25519 AAAA glovebox@pi" {
			t.Fatalf("pub = %q, err = %v", pub, err)
		}
	}
	if n := len(r.Commands()); n != 1 {
		t.Errorf("ssh-keygen ran %d times, want once", n)
	}
	got, _ := s.Get()
	if got.Archive.Rsync.PublicKey == "" {
		t.Error("Get doesn't show the public key")
	}
}

func TestRcloneConfig(t *testing.T) {
	s := newSetup(t)
	if _, err := s.SetRcloneConfig("type = drive\n"); err == nil {
		t.Error("a config without remotes: want an error")
	}
	remotes, err := s.SetRcloneConfig("[gdrive]\ntype = drive\n\n[nas]\ntype = sftp\nhost = 192.168.1.10")
	if err != nil || !slices.Equal(remotes, []string{"gdrive", "nas"}) {
		t.Fatalf("remotes = %v, err = %v", remotes, err)
	}
	got, _ := s.Get()
	if !slices.Equal(got.Archive.Rclone.Remotes, []string{"gdrive", "nas"}) {
		t.Errorf("Get remotes = %v", got.Archive.Rclone.Remotes)
	}
}

func TestParseGateway(t *testing.T) {
	routes := "Iface\tDestination\tGateway \tFlags\nwlan0\t0001A8C0\t00000000\t0001\nwlan0\t00000000\t0101A8C0\t0003\n"
	if got := parseGateway(routes); got != "192.168.1.1" {
		t.Errorf("gateway = %q", got)
	}
}

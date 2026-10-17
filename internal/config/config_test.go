package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "glovebox.toml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadMissingFileUsesDefaults(t *testing.T) {
	cfg, missing, err := Load(filepath.Join(t.TempDir(), "nope.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !missing {
		t.Error("missing = false, want true")
	}
	if cfg != Default() {
		t.Errorf("cfg = %+v, want defaults %+v", cfg, Default())
	}
}

func TestLoadOverridesDefaults(t *testing.T) {
	path := writeConfig(t, "[web]\nlisten = \"127.0.0.1:9000\"\n")
	cfg, missing, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if missing {
		t.Error("missing = true, want false")
	}
	if cfg.Web.Listen != "127.0.0.1:9000" {
		t.Errorf("web.listen = %q", cfg.Web.Listen)
	}
	if cfg.Log.Level != "info" {
		t.Errorf("log.level = %q, want default info", cfg.Log.Level)
	}
}

func TestLoadRejectsUnknownSettings(t *testing.T) {
	path := writeConfig(t, "[web]\nlisten = \":80\"\nlsten = \":81\"\n")
	_, _, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "web.lsten") {
		t.Fatalf("err = %v, want unknown setting web.lsten", err)
	}
}

func TestLoadRejectsBadValues(t *testing.T) {
	for name, content := range map[string]string{
		"level":  "[log]\nlevel = \"loud\"\n",
		"listen": "[web]\nlisten = \"8080\"\n",
		"syntax": "[web\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := Load(writeConfig(t, content)); err == nil {
				t.Error("err = nil, want an error")
			}
		})
	}
}

func TestSlogLevel(t *testing.T) {
	level, err := LogConfig{Level: "debug"}.SlogLevel()
	if err != nil || level != slog.LevelDebug {
		t.Errorf("SlogLevel(debug) = %v, %v", level, err)
	}
}

func TestLegacySMBSettingsMoveToTheirTable(t *testing.T) {
	cfg, _, err := Load(writeConfig(t, `[archive]
enabled = true
server = "192.168.1.10"
share = "Cars"
smb_version = ""
parallel = 2
`))
	if err != nil {
		t.Fatal(err)
	}
	a := cfg.Archive
	if a.Type != TypeSMB || a.SMB.Server != "192.168.1.10" || a.SMB.Share != "Cars" || a.SMB.Version != "" || a.SMB.Parallel != 2 {
		t.Errorf("smb = %+v", a.SMB)
	}
	if a.SMB.Credentials != "/etc/glovebox/smb-credentials" {
		t.Errorf("credentials = %q, want the default", a.SMB.Credentials)
	}
	if a.LegacyServer != "" || a.LegacyParallel != 0 {
		t.Error("legacy fields should be cleared")
	}
}

func TestArchiveTypes(t *testing.T) {
	for name, tc := range map[string]struct {
		content string
		bad     string // part of the expected error, "" for none
	}{
		"smb":             {"type = \"smb\"\n[archive.smb]\nserver = \"nas\"\n", ""},
		"smb no server":   {"type = \"smb\"\n", "archive.smb.server"},
		"nfs":             {"type = \"nfs\"\n[archive.nfs]\nserver = \"nas\"\nexport = \"/mnt/user/TeslaCam\"\n", ""},
		"nfs relative":    {"type = \"nfs\"\n[archive.nfs]\nserver = \"nas\"\nexport = \"TeslaCam\"\n", "archive.nfs.export"},
		"rsync":           {"type = \"rsync\"\n[archive.rsync]\nserver = \"nas\"\nuser = \"carcam\"\npath = \"/volume1/TeslaCam\"\n", ""},
		"rsync no user":   {"type = \"rsync\"\n[archive.rsync]\nserver = \"nas\"\npath = \"x\"\n", "archive.rsync.user"},
		"rclone":          {"type = \"rclone\"\ncheck_address = \"192.168.1.1:80\"\n[archive.rclone]\nremote = \"gdrive:TeslaCam\"\n", ""},
		"rclone no check": {"type = \"rclone\"\n[archive.rclone]\nremote = \"gdrive:TeslaCam\"\n", "check_address"},
		"rclone bad":      {"type = \"rclone\"\ncheck_address = \"192.168.1.1:80\"\n[archive.rclone]\nremote = \"gdrive\"\n", "remote:path"},
		"bad check":       {"type = \"smb\"\ncheck_address = \"router\"\n[archive.smb]\nserver = \"nas\"\n", "host:port"},
		"unknown":         {"type = \"ftp\"\n", "archive.type"},
	} {
		t.Run(name, func(t *testing.T) {
			// the type's table follows [archive]'s own keys
			content := "[archive]\nenabled = true\n" + tc.content
			_, _, err := Load(writeConfig(t, content))
			switch {
			case tc.bad == "" && err != nil:
				t.Errorf("err = %v", err)
			case tc.bad != "" && (err == nil || !strings.Contains(err.Error(), tc.bad)):
				t.Errorf("err = %v, want one about %s", err, tc.bad)
			}
		})
	}
}

func TestExtraDrives(t *testing.T) {
	cfg, _, err := Load(writeConfig(t, "[drive.music]\nsize = \"8G\"\nsource = \"Music\"\n[drive.boombox]\nsize = \"256M\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Drive.Music.Size != 8<<30 || cfg.Drive.Music.Source != "Music" || cfg.Drive.Boombox.Size != 256<<20 || cfg.Drive.LightShow.Size != 0 {
		t.Errorf("drive = %+v", cfg.Drive)
	}
	if _, _, err := Load(writeConfig(t, "[drive.lightshow]\nsize = \"1M\"\n")); err == nil || !strings.Contains(err.Error(), "drive.lightshow.size") {
		t.Errorf("tiny drive: err = %v", err)
	}
}

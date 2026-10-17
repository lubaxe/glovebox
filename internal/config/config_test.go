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

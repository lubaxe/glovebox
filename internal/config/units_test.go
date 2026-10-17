package config

import (
	"testing"
	"time"
)

func TestByteSize(t *testing.T) {
	for in, want := range map[string]ByteSize{
		"40G":     40 << 30,
		"40GB":    40 << 30,
		"40GiB":   40 << 30,
		"512m":    512 << 20,
		"1.5G":    3 << 29,
		"1T":      1 << 40,
		"1048576": 1 << 20,
	} {
		var b ByteSize
		if err := b.UnmarshalText([]byte(in)); err != nil || b != want {
			t.Errorf("%q = %d, %v; want %d", in, b, err, want)
		}
	}
	for _, bad := range []string{"", "G", "-1G", "lots"} {
		var b ByteSize
		if err := b.UnmarshalText([]byte(bad)); err == nil {
			t.Errorf("%q: want an error", bad)
		}
	}
	if out, _ := ByteSize(40 << 30).MarshalText(); string(out) != "40G" {
		t.Errorf("MarshalText = %s", out)
	}
}

func TestDuration(t *testing.T) {
	var d Duration
	if err := d.UnmarshalText([]byte("1h30m")); err != nil || time.Duration(d) != 90*time.Minute {
		t.Errorf("1h30m = %v, %v", time.Duration(d), err)
	}
	if err := d.UnmarshalText([]byte("soon")); err == nil {
		t.Error("want an error")
	}
}

func TestLoadDriveSettings(t *testing.T) {
	path := writeConfig(t, `
[drive]
enabled = true
cam_size = "8G"
serial_console = true

[snapshots]
interval = "10m"
min_free = "2G"
`)
	cfg, _, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Drive.Enabled || cfg.Drive.CamSize != 8<<30 || !cfg.Drive.SerialConsole || cfg.Drive.Backingfiles != "/backingfiles" {
		t.Errorf("drive = %+v", cfg.Drive)
	}
	if time.Duration(cfg.Snapshots.Interval) != 10*time.Minute || cfg.Snapshots.MinFree != 2<<30 {
		t.Errorf("snapshots = %+v", cfg.Snapshots)
	}
}

func TestLoadRejectsTinyCamDisk(t *testing.T) {
	path := writeConfig(t, "[drive]\nenabled = true\ncam_size = \"100M\"\n")
	if _, _, err := Load(path); err == nil {
		t.Error("want an error")
	}
}

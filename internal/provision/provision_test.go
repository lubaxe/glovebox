package provision

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/lubaxe/glovebox/internal/config"
	"github.com/lubaxe/glovebox/internal/system"
)

func newProvisioner(t *testing.T) (*Provisioner, *system.FakeRunner) {
	t.Helper()
	root := t.TempDir()
	write(t, root, "/boot/firmware/cmdline.txt", "console=tty1 root=PARTUUID=abc-02 rootwait cfg80211.ieee80211_regdom=US\n")
	write(t, root, "/etc/hosts", "127.0.0.1\tlocalhost\n127.0.1.1\traspberrypi\n")
	r := &system.FakeRunner{}
	p := &Provisioner{
		Root: root, LoginUser: "glovebox", Runner: r,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now: func() time.Time { return time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC) },
		lookupUser: func(string) (int, int, string, error) {
			return 1000, 1000, "/home/glovebox", nil
		},
		chown: func(string, int, int) error { return nil }, // tests don't run as root
	}
	return p, r
}

func write(t *testing.T, root, name, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, root, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func exists(root, name string) bool {
	_, err := os.Stat(filepath.Join(root, filepath.FromSlash(name)))
	return err == nil
}

const (
	bootFile = "/boot/firmware/glovebox.toml"
	etcFile  = "/etc/glovebox/glovebox.toml"
)

const fullConfig = `
[system]
hostname = "glovebox"
wifi_ssid = "My Home; Wi-Fi"
wifi_password = "correct horse battery"
wifi_country = "CA"
ssh_key = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIJ3I you@pc"

[drive]
enabled = true
cam_size = "32G"

[archive]
enabled = true
server = "192.168.1.10"
username = "carcam"
password = "s3cret pass"
`

func TestApplyNothing(t *testing.T) {
	p, _ := newProvisioner(t)
	if applied, err := p.Apply(context.Background(), bootFile, etcFile); applied || err != nil {
		t.Errorf("no file: applied=%v err=%v", applied, err)
	}
	write(t, p.Root, bootFile, "# [system]\n# wifi_ssid = \"x\"\n")
	if applied, err := p.Apply(context.Background(), bootFile, etcFile); applied || err != nil {
		t.Errorf("only comments: applied=%v err=%v", applied, err)
	}
	if !exists(p.Root, bootFile) {
		t.Error("the template should stay for the user to fill in")
	}
}

func TestApply(t *testing.T) {
	p, r := newProvisioner(t)
	write(t, p.Root, bootFile, fullConfig)
	applied, err := p.Apply(context.Background(), bootFile, etcFile)
	if err != nil || !applied {
		t.Fatalf("applied=%v err=%v", applied, err)
	}

	if got := read(t, p.Root, "/etc/hostname"); got != "glovebox\n" {
		t.Errorf("hostname = %q", got)
	}
	if hosts := read(t, p.Root, "/etc/hosts"); !strings.Contains(hosts, "127.0.1.1\tglovebox") || strings.Contains(hosts, "raspberrypi") {
		t.Errorf("hosts = %q", hosts)
	}
	if cmdline := read(t, p.Root, "/boot/firmware/cmdline.txt"); !strings.Contains(cmdline, "regdom=CA") || strings.Contains(cmdline, "regdom=US") {
		t.Errorf("cmdline = %q", cmdline)
	}
	wifi := read(t, p.Root, "/etc/NetworkManager/system-connections/glovebox-wifi.nmconnection")
	if !strings.Contains(wifi, "ssid=My Home; Wi-Fi\n") || !strings.Contains(wifi, "psk=correct horse battery\n") {
		t.Errorf("wifi connection:\n%s", wifi)
	}
	if keys := read(t, p.Root, "/home/glovebox/.ssh/authorized_keys"); keys != "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIJ3I you@pc\n" {
		t.Errorf("authorized_keys = %q", keys)
	}
	if creds := read(t, p.Root, "/etc/glovebox/smb-credentials"); creds != "username=carcam\npassword=s3cret pass\n" {
		t.Errorf("credentials = %q", creds)
	}

	stored := read(t, p.Root, etcFile)
	for _, secret := range []string{"correct horse", "s3cret"} {
		if strings.Contains(stored, secret) {
			t.Errorf("stored config contains a secret:\n%s", stored)
		}
	}
	if !strings.Contains(stored, `cam_size = "32G"`) || !strings.Contains(stored, `server = "192.168.1.10"`) {
		t.Errorf("stored config lost settings:\n%s", stored)
	}
	// glovebox reads the stored config right after applying it
	loaded, _, err := config.Load(filepath.Join(p.Root, filepath.FromSlash(etcFile)))
	if err != nil {
		t.Fatalf("stored config doesn't load: %v", err)
	}
	if loaded.Drive.CamSize != 32<<30 || !loaded.Archive.Enabled || loaded.System.Hostname != "glovebox" {
		t.Errorf("loaded = %+v", loaded)
	}
	if exists(p.Root, bootFile) {
		t.Error("boot partition copy (with secrets) should be gone")
	}
	if note := read(t, p.Root, "/boot/firmware/glovebox-status.txt"); !strings.Contains(note, "Wi-Fi network \"My Home; Wi-Fi\"") || strings.Contains(note, "s3cret") {
		t.Errorf("note = %q", note)
	}
	if runtime.GOOS != "windows" {
		for _, f := range []string{"/etc/NetworkManager/system-connections/glovebox-wifi.nmconnection", "/etc/glovebox/smb-credentials", etcFile} {
			if fi, err := os.Stat(filepath.Join(p.Root, f)); err != nil || fi.Mode().Perm() != 0o600 {
				t.Errorf("%s mode = %v, want 0600", f, fi.Mode())
			}
		}
	}
	cmds := strings.Join(r.Commands(), "\n")
	for _, want := range []string{"hostname glovebox", "iw reg set CA", "nmcli connection reload"} {
		if !strings.Contains(cmds, want) {
			t.Errorf("missing command %q in:\n%s", want, cmds)
		}
	}
}

func TestApplyKeepsSSHKeysOnce(t *testing.T) {
	p, _ := newProvisioner(t)
	for i := 0; i < 2; i++ {
		write(t, p.Root, bootFile, "[system]\nssh_key = \"ssh-ed25519 AAAA key1\"\n")
		if _, err := p.Apply(context.Background(), bootFile, etcFile); err != nil {
			t.Fatal(err)
		}
	}
	if keys := read(t, p.Root, "/home/glovebox/.ssh/authorized_keys"); keys != "ssh-ed25519 AAAA key1\n" {
		t.Errorf("authorized_keys = %q", keys)
	}
}

func TestApplyBadFileLeavesNote(t *testing.T) {
	p, _ := newProvisioner(t)
	write(t, p.Root, bootFile, "[system]\nwifi_ssid = \"home\"\nwifi_password = \"short\"\n")
	if _, err := p.Apply(context.Background(), bootFile, etcFile); err == nil {
		t.Fatal("want an error")
	}
	if !exists(p.Root, bootFile) {
		t.Error("the file should stay so it can be fixed")
	}
	if note := read(t, p.Root, "/boot/firmware/glovebox-status.txt"); !strings.Contains(note, "wifi_password") {
		t.Errorf("note = %q", note)
	}
	if exists(p.Root, etcFile) {
		t.Error("nothing should be applied")
	}
}

func TestUnblockRadios(t *testing.T) {
	root := t.TempDir()
	for name, v := range map[string][2]string{
		"rfkill0": {"bluetooth", "1"},
		"rfkill1": {"wlan", "0"},
		"rfkill2": {"nfc", "1"},
	} {
		write(t, root, "/sys/class/rfkill/"+name+"/type", v[0]+"\n")
		write(t, root, "/sys/class/rfkill/"+name+"/soft", v[1]+"\n")
	}
	got := UnblockRadios(root)
	if len(got) != 1 || got[0] != "bluetooth" {
		t.Errorf("unblocked %v", got)
	}
	if read(t, root, "/sys/class/rfkill/rfkill0/soft") != "0" || strings.TrimSpace(read(t, root, "/sys/class/rfkill/rfkill2/soft")) != "1" {
		t.Error("wrong switches changed")
	}
}

// The template shipped on the boot partition must set nothing, so an
// unedited card keeps it for the user to fill in.
func TestShippedTemplateIsInert(t *testing.T) {
	for _, name := range []string{"glovebox-boot.toml", "glovebox.toml"} {
		path := filepath.Join("..", "..", "pi-gen-sources", "00-glovebox", "files", name)
		cfg, keys, err := config.LoadFile(path)
		if err != nil || keys != 0 {
			t.Errorf("%s: keys=%d err=%v", name, keys, err)
		}
		if !cfg.Drive.Enabled || cfg.Archive.Enabled {
			t.Errorf("%s: defaults should have the drive on and archiving off", name)
		}
	}
}

func TestApplyMovesRcloneConfig(t *testing.T) {
	p, _ := newProvisioner(t)
	write(t, p.Root, bootFile, "[archive]\ntype = \"rclone\"\n")
	write(t, p.Root, "/boot/firmware/rclone.conf", "[gdrive]\ntype = drive\ntoken = secret\n")
	if _, err := p.Apply(context.Background(), bootFile, etcFile); err != nil {
		t.Fatal(err)
	}
	if got := read(t, p.Root, "/etc/glovebox/rclone.conf"); !strings.Contains(got, "token = secret") {
		t.Errorf("rclone.conf = %q", got)
	}
	if exists(p.Root, "/boot/firmware/rclone.conf") {
		t.Error("the boot partition's rclone.conf (with tokens) should be gone")
	}
}

func TestWiFiCountryWithoutCmdline(t *testing.T) {
	p, r := newProvisioner(t)
	if err := os.Remove(filepath.Join(p.Root, "boot", "firmware", "cmdline.txt")); err != nil {
		t.Fatal(err)
	}
	if err := p.SetWiFiCountry(context.Background(), "CA"); err != nil {
		t.Errorf("not a Raspberry Pi: err = %v", err)
	}
	if !strings.Contains(strings.Join(r.Commands(), "\n"), "iw reg set CA") {
		t.Errorf("commands: %v", r.Commands())
	}
}

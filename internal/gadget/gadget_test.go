package gadget

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/lubaxe/glovebox/internal/system"
)

func testGadget(t *testing.T) (*Gadget, *system.FakeRunner) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs symlinks")
	}
	dir := t.TempDir()
	udc := filepath.Join(dir, "udc")
	if err := os.MkdirAll(filepath.Join(udc, "3f980000.usb"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(udc, "3f980000.usb", "state"), []byte("configured\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	machineID := filepath.Join(dir, "machine-id")
	if err := os.WriteFile(machineID, []byte("0123456789abcdef\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &system.FakeRunner{}
	g := New(r)
	g.ConfigFS = filepath.Join(dir, "configfs")
	g.UDCDir = udc
	g.MachineIDFile = machineID
	return g, r
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

func TestEnable(t *testing.T) {
	g, r := testGadget(t)
	g.SerialConsole = true
	err := g.Enable(context.Background(), []LUN{{File: "/backingfiles/cam_disk.bin", Product: "CAM drive with a long name"}})
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(g.ConfigFS, "glovebox")
	for path, want := range map[string]string{
		"idVendor":                            "0x1d6b",
		"UDC":                                 "3f980000.usb",
		"configs/c.1/MaxPower":                "200",
		"functions/mass_storage.0/lun.0/file": "/backingfiles/cam_disk.bin",
		"functions/mass_storage.0/lun.0/removable":      "1",
		"functions/mass_storage.0/lun.0/inquiry_string": "gloveboxCAM drive with a1.0",
	} {
		if got := read(t, filepath.Join(root, path)); got != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
	if serial := read(t, filepath.Join(root, "strings/0x409/serialnumber")); !strings.HasPrefix(serial, "glovebox-") || len(serial) != len("glovebox-")+16 {
		t.Errorf("serialnumber = %q", serial)
	}
	for _, link := range []string{"configs/c.1/mass_storage.0", "configs/c.1/acm.usb0"} {
		if _, err := os.Readlink(filepath.Join(root, link)); err != nil {
			t.Errorf("%s: %v", link, err)
		}
	}
	cmds := strings.Join(r.Commands(), "\n")
	if !strings.Contains(cmds, "modprobe libcomposite") || !strings.Contains(cmds, "serial-getty@ttyGS0") {
		t.Errorf("commands = %q", cmds)
	}

	st, err := g.State()
	if err != nil {
		t.Fatal(err)
	}
	if !st.Enabled || !st.Connected() || st.UDC != "3f980000.usb" || len(st.Files) != 1 {
		t.Errorf("state = %+v", st)
	}

	if err := g.Enable(context.Background(), []LUN{{File: "x"}}); err == nil {
		t.Error("enabling twice should fail")
	}
}

func TestEnableWithoutController(t *testing.T) {
	g, _ := testGadget(t)
	g.UDCDir = t.TempDir()
	if err := g.Enable(context.Background(), []LUN{{File: "x"}}); err == nil || !strings.Contains(err.Error(), "dwc2") {
		t.Errorf("err = %v, want a hint about dwc2", err)
	}
}

func TestStateWhenMissing(t *testing.T) {
	g, _ := testGadget(t)
	st, err := g.State()
	if err != nil || st.Enabled {
		t.Errorf("state = %+v, %v", st, err)
	}
}

func TestDisableUnbinds(t *testing.T) {
	g, _ := testGadget(t)
	if err := g.Enable(context.Background(), []LUN{{File: "/backingfiles/cam_disk.bin"}}); err != nil {
		t.Fatal(err)
	}
	// In a real configfs the attribute files vanish with their directories;
	// in this temp dir they don't, so removing directories fails. What
	// matters is that the gadget gets unbound and the function unlinked.
	_ = g.Disable(context.Background())
	root := filepath.Join(g.ConfigFS, "glovebox")
	if got := read(t, filepath.Join(root, "UDC")); got != "" {
		t.Errorf("UDC = %q, want empty", got)
	}
	if _, err := os.Lstat(filepath.Join(root, "configs/c.1/mass_storage.0")); !os.IsNotExist(err) {
		t.Error("function link should be removed")
	}
	if st, _ := g.State(); st.Enabled {
		t.Error("state should say disabled")
	}
}

func TestDisableWhenMissing(t *testing.T) {
	g, _ := testGadget(t)
	if err := g.Disable(context.Background()); err != nil {
		t.Error(err)
	}
}

func TestEjectAndLoad(t *testing.T) {
	g, _ := testGadget(t)
	if err := g.Enable(context.Background(), []LUN{{File: "/b/cam_disk.bin", Product: "CAM"}, {File: "/b/music_disk.bin", Product: "MUSIC"}}); err != nil {
		t.Fatal(err)
	}
	if err := g.Eject(1); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(g.lunDir(1), "forced_eject")); got != "1" {
		t.Errorf("forced_eject = %q", got)
	}
	if err := g.Load(1, "/b/music_disk.bin"); err != nil {
		t.Fatal(err)
	}
	st, _ := g.State()
	if len(st.Files) != 2 || st.Files[1] != "/b/music_disk.bin" {
		t.Errorf("files = %v", st.Files)
	}
}

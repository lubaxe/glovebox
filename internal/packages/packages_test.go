package packages

import (
	"bufio"
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/lubaxe/glovebox/internal/system"
)

// fakeDpkg answers dpkg-query from installed, and apt-get install -s with plan.
type fakeDpkg struct {
	installed map[string]bool
	audit     string
	plan      string
	fail      string // command prefix that fails
}

func (f *fakeDpkg) runner() *system.FakeRunner {
	return &system.FakeRunner{Handler: func(cmd string) ([]byte, error) {
		if f.fail != "" && strings.Contains(cmd, f.fail) {
			return []byte("E: failed"), &system.CmdError{Cmd: cmd, ExitCode: 100, Output: "E: failed"}
		}
		switch {
		case cmd == "dpkg-query --version":
			return nil, nil
		case cmd == "dpkg --audit":
			return []byte(f.audit), nil
		case strings.Contains(cmd, "--configure -a"):
			f.audit = ""
		case strings.HasPrefix(cmd, "dpkg-query -W"):
			var out strings.Builder
			for _, p := range strings.Fields(cmd)[3:] {
				status := "un "
				if f.installed[p] {
					status = "ii "
				}
				out.WriteString(p + ":arm64 " + status + "\n")
			}
			return []byte(out.String()), &system.CmdError{Cmd: cmd, ExitCode: 1}
		case strings.Contains(cmd, "apt-get") && strings.Contains(cmd, " install -s "):
			return []byte(f.plan), nil
		case strings.Contains(cmd, "apt-get") && strings.Contains(cmd, " install -y "):
			for _, p := range strings.Fields(cmd) {
				if !strings.HasPrefix(p, "-") {
					f.installed[p] = true
				}
			}
		}
		return nil, nil
	}}
}

// plain installs with apt's own directories.
func plain(r system.Runner) *Installer {
	in := New(r)
	in.Scratch = ""
	return in
}

func allInstalled() map[string]bool {
	m := map[string]bool{}
	for _, p := range Required {
		m[p] = true
	}
	return m
}

func aptCalls(r *system.FakeRunner) (calls []string) {
	for _, c := range r.Commands() {
		if strings.Contains(c, "apt-get") {
			f := strings.Fields(c)
			calls = append(calls, strings.Join(f[slices.Index(f, "Dpkg::Options::=--force-confold")+1:], " "))
		}
	}
	return calls
}

func TestNothingMissingNeedsNoNetwork(t *testing.T) {
	f := &fakeDpkg{installed: allInstalled()}
	r := f.runner()
	got, err := plain(r).Ensure(context.Background())
	if err != nil || len(got) != 0 {
		t.Fatalf("Ensure = %v, %v", got, err)
	}
	if calls := aptCalls(r); len(calls) != 0 {
		t.Errorf("apt ran: %q", calls)
	}
}

func TestInstallsEverythingMissingAtOnce(t *testing.T) {
	// a Pi that skipped versions: two packages added since its version
	f := &fakeDpkg{installed: allInstalled(), plan: "Inst rclone (1.60 Debian:12/stable [arm64])\nInst exfatprogs (1.2 Debian:12/stable [arm64])\n"}
	delete(f.installed, "rclone")
	delete(f.installed, "exfatprogs")
	r := f.runner()
	var steps []string
	in := plain(r)
	in.Step = func(s string) { steps = append(steps, s) }
	got, err := in.Ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"exfatprogs", "rclone"}; !slices.Equal(got, want) {
		t.Errorf("installed %q, want %q", got, want)
	}
	want := []string{"update -q", "install -s --no-install-recommends exfatprogs rclone",
		"install -y -q --no-install-recommends exfatprogs rclone", "clean"}
	if calls := aptCalls(r); !slices.Equal(calls, want) {
		t.Errorf("apt calls\n got %q\nwant %q", calls, want)
	}
	if len(steps) == 0 || steps[len(steps)-1] != "Installing exfatprogs, rclone" {
		t.Errorf("steps = %q", steps)
	}
	if missing, err := in.Missing(context.Background()); err != nil || len(missing) != 0 {
		t.Errorf("still missing %q, %v", missing, err)
	}
}

func TestScratchOnMountedStorage(t *testing.T) {
	f := &fakeDpkg{installed: allInstalled(), plan: "Inst rclone (1.60)\n"}
	delete(f.installed, "rclone")
	mounted := false
	r := f.runner()
	handler := r.Handler
	scratch := filepath.Join(t.TempDir(), ".glovebox-apt")
	var during []string
	r.Handler = func(cmd string) ([]byte, error) {
		switch {
		case strings.HasPrefix(cmd, "mountpoint -q "):
			if !mounted {
				return nil, &system.CmdError{Cmd: cmd, ExitCode: 1}
			}
			return nil, nil
		case cmd == "dpkg --print-architecture":
			return []byte("arm64\n"), nil
		case strings.Contains(cmd, " update -q"):
			if _, err := os.Stat(filepath.Join(scratch, "lists", "partial")); err != nil {
				t.Errorf("no lists directory: %v", err)
			}
			during = append(during, cmd)
		}
		return handler(cmd)
	}
	in := New(r)
	in.Scratch = scratch
	if _, err := in.Ensure(context.Background()); err == nil || !strings.Contains(err.Error(), "isn't mounted") {
		t.Fatalf("unmounted: %v", err)
	}
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Errorf("scratch made without its storage: %v", err)
	}
	mounted = true
	if got, err := in.Ensure(context.Background()); err != nil || len(got) != 1 {
		t.Fatalf("Ensure = %v, %v", got, err)
	}
	if len(during) != 1 {
		t.Fatalf("updates = %q", during)
	}
	for _, want := range []string{"Dir::State::Lists=" + filepath.Join(scratch, "lists"), "Dir::Cache=" + filepath.Join(scratch, "cache"),
		"Acquire::GzipIndexes=true", "APT::Architectures::=arm64"} {
		if !strings.Contains(during[0], want) {
			t.Errorf("apt without %s: %s", want, during[0])
		}
	}
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Errorf("scratch left behind: %v", err)
	}
	for _, c := range r.Commands() {
		if strings.HasSuffix(c, " clean") {
			t.Errorf("apt clean on scratch: %s", c)
		}
	}
}

func TestFinishesInterruptedInstallFirst(t *testing.T) {
	f := &fakeDpkg{installed: allInstalled(), audit: "The following packages are only half configured:\n rsync\n"}
	r := f.runner()
	if _, err := plain(r).Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	cmds := r.Commands()
	if len(cmds) < 2 || cmds[0] != "dpkg --audit" || !strings.HasSuffix(cmds[1], "dpkg --force-confdef --force-confold --configure -a") {
		t.Errorf("commands = %q", cmds)
	}
	f.fail = "--configure -a"
	f.audit = "half configured"
	if _, err := plain(f.runner()).Ensure(context.Background()); err == nil || !strings.Contains(err.Error(), "interrupted") {
		t.Errorf("failed repair = %v", err)
	}
}

func TestRefusesPlansThatTouchBootOrRemove(t *testing.T) {
	for name, plan := range map[string]string{
		"kernel":    "Inst rclone (1.60)\nInst linux-image-6.6.51+rpt-rpi-v8 (1:6.6.51-1+rpt3)\n",
		"firmware":  "Inst raspi-firmware (1:1.20241008-1)\n",
		"initramfs": "Inst initramfs-tools (0.142)\n",
		"remove":    "Remv dhcpcd5 [1:9.4.1]\nInst rclone (1.60)\n",
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeDpkg{installed: allInstalled(), plan: plan}
			delete(f.installed, "rclone")
			r := f.runner()
			if _, err := plain(r).Ensure(context.Background()); err == nil {
				t.Fatal("plan accepted")
			}
			for _, c := range aptCalls(r) {
				if strings.HasPrefix(c, "install -y") {
					t.Fatalf("installed anyway: %q", c)
				}
			}
		})
	}
}

func TestOfflineIsAnError(t *testing.T) {
	f := &fakeDpkg{installed: allInstalled(), fail: "update -q"}
	delete(f.installed, "rclone")
	if _, err := plain(f.runner()).Ensure(context.Background()); err == nil || !strings.Contains(err.Error(), "package lists") {
		t.Errorf("Ensure = %v", err)
	}
}

func TestNoDpkgIsUnsupported(t *testing.T) {
	r := &system.FakeRunner{Handler: func(cmd string) ([]byte, error) {
		return nil, errors.New("executable file not found")
	}}
	if _, err := plain(r).Ensure(context.Background()); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Ensure = %v", err)
	}
}

// The installer and the image install Required, so a new card or install
// starts with what an updated Pi gets.
func TestListsMatch(t *testing.T) {
	script, err := os.ReadFile("../../scripts/install.sh")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`apt-get install -y -q --no-install-recommends ((?:[^\n\\]|\\\n)*)`).FindSubmatch(script)
	if m == nil {
		t.Fatal("install.sh has no apt-get install line")
	}
	installer := strings.Fields(strings.ReplaceAll(string(m[1]), "\\\n", " "))
	image := map[string]bool{}
	file, err := os.Open("../../pi-gen-sources/00-glovebox/00-packages-nr")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	for s := bufio.NewScanner(file); s.Scan(); {
		for _, p := range strings.Fields(s.Text()) {
			image[p] = true
		}
	}
	for _, p := range Required {
		if !slices.Contains(installer, p) {
			t.Errorf("install.sh doesn't install %s", p)
		}
		if !image[p] {
			t.Errorf("the image doesn't install %s", p)
		}
	}
	for _, p := range installer {
		if !slices.Contains(Required, p) && p != "curl" {
			t.Errorf("install.sh installs %s, which isn't in Required", p)
		}
	}
}

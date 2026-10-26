package wifi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lubaxe/glovebox/internal/system"
)

func newManager(t *testing.T, handler func(cmd string) ([]byte, error)) (*Manager, *system.FakeRunner) {
	t.Helper()
	root := t.TempDir()
	r := &system.FakeRunner{Handler: handler}
	m := New(r, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.Root = root
	if err := os.MkdirAll(filepath.Join(root, "sys", "class", "net", "wlan0"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sys", "class", "net", "wlan0", "address"), []byte("dc:a6:32:12:ab:cd\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return m, r
}

func TestClientKeyfile(t *testing.T) {
	k := ClientKeyfile(" My Home; Wi-Fi", `pa\ss`)
	for _, want := range []string{"id=glovebox-wifi\n", "interface-name=wlan0\n", `ssid=\sMy Home; Wi-Fi` + "\n", `psk=pa\\ss` + "\n"} {
		if !strings.Contains(k, want) {
			t.Errorf("%q not in:\n%s", want, k)
		}
	}
	if open := ClientKeyfile("Cafe", ""); strings.Contains(open, "wifi-security") {
		t.Errorf("open network has security:\n%s", open)
	}
}

func TestParseScan(t *testing.T) {
	out := "Home:72:WPA2\nNeighbour\\:5G:40:WPA1 WPA2\n:90:WPA2\nHome:80:WPA2\nCafe:55:\n"
	got := parseScan(out)
	want := []Network{{"Home", 80, true}, {"Cafe", 55, false}, {"Neighbour:5G", 40, true}}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%d: got %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestJoin(t *testing.T) {
	m, r := newManager(t, func(cmd string) ([]byte, error) {
		if strings.Contains(cmd, "device show wlan0") {
			return []byte("GENERAL.STATE:100 (connected)\nGENERAL.CONNECTION:glovebox-wifi\nIP4.ADDRESS[1]:192.168.1.50/24\n"), nil
		}
		if strings.Contains(cmd, "802-11-wireless.ssid") {
			return []byte("Home\n"), nil
		}
		return nil, nil
	})
	var country string
	m.SetCountry = func(_ context.Context, cc string) error { country = cc; return nil }
	res := m.Join(context.Background(), "Home", "correct horse", "CA")
	if !res.OK || res.Address != "192.168.1.50" || country != "CA" {
		t.Fatalf("res = %+v, country = %q", res, country)
	}
	conn, err := os.ReadFile(m.connectionFile(ClientConnection))
	if err != nil || !strings.Contains(string(conn), "psk=correct horse") {
		t.Errorf("connection = %q, %v", conn, err)
	}
	if !m.joined() {
		t.Error("joining should be remembered")
	}
	if !strings.Contains(strings.Join(r.Commands(), "\n"), "nmcli --wait 45 connection up glovebox-wifi ifname wlan0") {
		t.Errorf("commands: %v", r.Commands())
	}
	st := m.Status(context.Background())
	if !st.Connected || st.SSID != "Home" || st.Address != "192.168.1.50" || st.LastJoin == nil || !st.LastJoin.OK {
		t.Errorf("status = %+v", st)
	}

	if res := m.Join(context.Background(), "Home", "short", ""); res.OK || !strings.Contains(res.Error, "8 to 63") {
		t.Errorf("short password: %+v", res)
	}
}

func TestJoinWrongPassword(t *testing.T) {
	m, _ := newManager(t, func(cmd string) ([]byte, error) {
		if strings.Contains(cmd, "connection up") {
			return []byte("Error: Connection activation failed: Secrets were required, but not provided."), &system.CmdError{Cmd: cmd, ExitCode: 4}
		}
		return nil, nil
	})
	res := m.Join(context.Background(), "Home", "wrong password", "")
	if res.OK || !strings.Contains(res.Error, "password was wrong") {
		t.Errorf("res = %+v", res)
	}
	if m.joined() {
		t.Error("a failed join isn't a join")
	}
}

func TestWantAP(t *testing.T) {
	m, _ := newManager(t, nil)
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	m.Now = func() time.Time { return now }
	if !m.wantAP() {
		t.Error("never on Wi-Fi: the setup network should be on")
	}
	m.markJoined()
	_ = os.Chtimes(m.flag("wifi-joined"), now, now)
	if !m.wantAP() {
		t.Error("just joined, wizard not finished: keep it on for a while")
	}
	now = now.Add(31 * time.Minute)
	if m.wantAP() {
		t.Error("joined long ago: off")
	}
	now = now.Add(-30 * time.Minute)
	if err := m.FinishSetup(); err != nil {
		t.Fatal(err)
	}
	if m.wantAP() {
		t.Error("joined and finished: off")
	}
}

func TestRunStartsAndStopsTheSetupNetwork(t *testing.T) {
	connected := false
	m, r := newManager(t, func(cmd string) ([]byte, error) {
		if strings.Contains(cmd, "device show wlan0") && connected {
			return []byte("GENERAL.STATE:100 (connected)\nGENERAL.CONNECTION:Home\n"), nil
		}
		return nil, nil
	})
	m.Grace = 0
	if err := m.FinishSetup(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	waitFor(t, func() bool { return m.Status(context.Background()).AccessPoint == "Glovebox-ABCD" })
	ap, err := os.ReadFile(m.connectionFile(APConnection))
	if err != nil || !strings.Contains(string(ap), "mode=ap") || !strings.Contains(string(ap), "method=shared") || strings.Contains(string(ap), "psk") {
		t.Errorf("ap connection = %q, %v", ap, err)
	}
	dns, _ := os.ReadFile(m.path("/etc/NetworkManager/dnsmasq-shared.d/glovebox-setup.conf"))
	if !strings.Contains(string(dns), "address=/#/192.168.66.1") {
		t.Errorf("dns = %q", dns)
	}

	// the Pi gets on Wi-Fi: the setup is finished, so the network goes
	m.Now = func() time.Time { return time.Now() }
	connected = true
	m.mu.Lock()
	m.apOn = true
	m.mu.Unlock()
	cancel()
	<-done
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)
	waitFor(t, func() bool { return m.Status(context.Background()).AccessPoint == "" })
	if !strings.Contains(strings.Join(r.Commands(), "\n"), "iw dev ap0 del") {
		t.Errorf("commands: %v", r.Commands())
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPortalSendsOtherSitesToTheWizard(t *testing.T) {
	app := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "app") })
	h := Portal(app)
	for host, want := range map[string]string{
		"connectivitycheck.gstatic.com": "http://192.168.66.1/#/welcome",
		"captive.apple.com":             "http://192.168.66.1/#/welcome",
		"192.168.66.1":                  "",
		"glovebox.local":                "",
	} {
		req := httptest.NewRequest("GET", "http://"+host+"/generate_204", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if got := rec.Header().Get("Location"); got != want {
			t.Errorf("%s: Location %q, want %q", host, got, want)
		}
	}
}

func TestResetFileBringsTheSetupNetworkBack(t *testing.T) {
	m, _ := newManager(t, nil)
	m.markJoined()
	if err := m.FinishSetup(); err != nil {
		t.Fatal(err)
	}
	m.SetupDone = func() bool { return true } // archiving was set up
	m.Now = func() time.Time { return time.Now().Add(time.Hour) }
	if m.wantAP() {
		t.Fatal("set up: no setup network")
	}
	trigger := m.path(m.ResetFile)
	if err := os.MkdirAll(filepath.Dir(trigger), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(trigger, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.reset(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !m.wantAP() || m.setupDone() {
		t.Error("after the reset the setup network should come back and the wizard run again")
	}
	if _, err := os.Stat(trigger); !os.IsNotExist(err) {
		t.Error("the trigger file should be deleted")
	}
	// still asked for after a restart, until the wizard is finished
	m2 := &Manager{Root: m.Root, StateDir: m.StateDir, SetupDone: m.SetupDone, Now: m.Now}
	if m2.setupDone() {
		t.Error("a restart shouldn't count as finishing the wizard")
	}
	if err := m.FinishSetup(); err != nil || !m.setupDone() {
		t.Errorf("finished: setupDone = %v, err = %v", m.setupDone(), err)
	}
}

type bootWriteFunc func(context.Context, func() error) error

func (write bootWriteFunc) WithWrite(ctx context.Context, fn func() error) error {
	return write(ctx, fn)
}

func TestResetBootWriteFailureBlocksChanges(t *testing.T) {
	m, _ := newManager(t, nil)
	m.markJoined()
	if err := m.FinishSetup(); err != nil {
		t.Fatal(err)
	}
	trigger := m.path(m.ResetFile)
	if err := os.MkdirAll(filepath.Dir(trigger), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(trigger, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	guardErr := errors.New("boot is read-only")
	m.BootWriter = bootWriteFunc(func(context.Context, func() error) error { return guardErr })
	if err := m.reset(context.Background()); !errors.Is(err, guardErr) {
		t.Fatalf("err=%v", err)
	}
	for _, name := range []string{trigger, m.flag("wifi-joined"), m.flag("setup-done")} {
		if _, err := os.Stat(name); err != nil {
			t.Errorf("guard failure modified %s: %v", name, err)
		}
	}
	if _, err := os.Stat(m.flag("setup-again")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("unexpected setup-again marker: %v", err)
	}
}

func TestResetUsesBootWriteScope(t *testing.T) {
	m, _ := newManager(t, nil)
	calls := 0
	m.BootWriter = bootWriteFunc(func(_ context.Context, fn func() error) error {
		calls++
		if err := fn(); err != nil {
			return err
		}
		if _, err := os.Stat(m.path(m.ResetFile)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("reset marker removal must happen within scope: %v", err)
		}
		if _, err := os.Stat(m.flag("setup-again")); err != nil {
			t.Errorf("reset state must be saved within scope: %v", err)
		}
		return nil
	})
	if err := m.reset(context.Background()); err != nil || calls != 0 {
		t.Fatalf("missing marker: err=%v calls=%d", err, calls)
	}
	if err := os.MkdirAll(filepath.Dir(m.path(m.ResetFile)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.path(m.ResetFile), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.reset(context.Background()); err != nil || calls != 1 {
		t.Fatalf("reset: err=%v calls=%d", err, calls)
	}
}

func TestResetPropagatesRemovalFailure(t *testing.T) {
	m, _ := newManager(t, nil)
	trigger := m.path(m.ResetFile)
	if err := os.MkdirAll(trigger, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(trigger, "keep"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.reset(context.Background()); err == nil {
		t.Fatal("reset marker removal failure must be returned")
	}
}

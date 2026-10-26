// Package wifi joins the Pi to a Wi-Fi network and, until Glovebox is set
// up, offers its own open network with a sign-in page that runs the setup
// wizard. Both go through NetworkManager.
//
// The access point is a second interface (ap0) next to the Wi-Fi client
// (wlan0), so the Pi can try a home network while a phone stays on the
// setup network. The chip runs both on one channel: when the client joins a
// network on another channel, the access point moves along and phones
// reconnect.
package wifi

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lubaxe/glovebox/internal/bootfs"
	"github.com/lubaxe/glovebox/internal/system"
)

// Names and addresses of the two connections.
const (
	ClientConnection = "glovebox-wifi"
	APConnection     = "glovebox-ap"
	ClientInterface  = "wlan0"
	APInterface      = "ap0"
	// APAddress is the Pi's address on its own network.
	APAddress = "192.168.66.1"
)

// Manager runs Wi-Fi.
type Manager struct {
	// Root is prepended to system paths; "/" on the Pi.
	Root       string
	Runner     system.Runner
	Log        *slog.Logger
	BootWriter bootfs.Writer
	// StateDir keeps the flags saying whether Wi-Fi was ever joined and
	// setup finished, e.g. /etc/glovebox.
	StateDir string
	// SetCountry sets the Wi-Fi country; nil skips it.
	SetCountry func(ctx context.Context, cc string) error
	// Portal serves the setup network's sign-in page on port 80.
	Portal http.Handler
	// SetupDone reports whether Glovebox counts as set up even without the
	// wizard being finished (e.g. archiving was configured by hand).
	SetupDone func() bool
	// Linger is how long the access point stays on after Wi-Fi was joined
	// if the wizard isn't finished.
	// ResetFile, if it exists at startup, brings the setup network back as
	// if the Pi had never joined Wi-Fi, e.g. after moving house: a file of
	// that name on the boot partition. It's deleted once used.
	ResetFile string
	Linger    time.Duration
	// Grace is how long after starting to wait for Wi-Fi before turning the
	// setup network on, so a Pi that was set up gets the chance to join.
	Grace time.Duration
	Now   func() time.Time

	mu     sync.Mutex
	apOn   bool
	portal *http.Server
	last   JoinResult
}

// New returns a Manager for the running system.
func New(r system.Runner, log *slog.Logger) *Manager {
	return &Manager{Root: "/", Runner: r, Log: log, StateDir: "/etc/glovebox", ResetFile: "/boot/firmware/glovebox-setup", Linger: 30 * time.Minute, Grace: 2 * time.Minute, Now: time.Now}
}

func (m *Manager) path(abs string) string { return filepath.Join(m.Root, filepath.FromSlash(abs)) }

// Escape escapes a value for a NetworkManager keyfile (a GKeyFile).
func Escape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, "\n", `\n`, "\t", `\t`, "\r", `\r`)
	s = r.Replace(s)
	if strings.HasPrefix(s, " ") {
		s = `\s` + s[1:]
	}
	return s
}

// ClientKeyfile is the NetworkManager connection for a WPA2 network, or an
// open one when password is empty.
func ClientKeyfile(ssid, password string) string {
	lines := []string{
		"[connection]",
		"id=" + ClientConnection,
		"type=wifi",
		"interface-name=" + ClientInterface,
		"autoconnect=true",
		"",
		"[wifi]",
		"mode=infrastructure",
		"ssid=" + Escape(ssid),
		"",
	}
	if password != "" {
		lines = append(lines, "[wifi-security]", "key-mgmt=wpa-psk", "psk="+Escape(password), "")
	}
	lines = append(lines, "[ipv4]", "method=auto", "", "[ipv6]", "method=auto", "")
	return strings.Join(lines, "\n")
}

// apKeyfile is the setup network: open, with the Pi handing out addresses
// ("shared") on its own subnet.
func apKeyfile(ssid string) string {
	return strings.Join([]string{
		"[connection]",
		"id=" + APConnection,
		"type=wifi",
		"interface-name=" + APInterface,
		"autoconnect=false",
		"",
		"[wifi]",
		"mode=ap",
		"ssid=" + Escape(ssid),
		"",
		"[ipv4]",
		"method=shared",
		"address1=" + APAddress + "/24",
		"",
		"[ipv6]",
		"method=disabled",
		"",
	}, "\n")
}

// captiveDNS makes the setup network's DNS answer every name with the Pi,
// so phones find no internet there and open the sign-in page.
const captiveDNS = "# Glovebox's setup network: every name is the Pi, for the sign-in page.\naddress=/#/" + APAddress + "\n"

func (m *Manager) connectionFile(name string) string {
	return m.path("/etc/NetworkManager/system-connections/" + name + ".nmconnection")
}

// WriteClient stores the home network's connection, readable only by root.
func WriteClient(connFile, ssid, password string) error {
	if err := os.MkdirAll(filepath.Dir(connFile), 0o755); err != nil {
		return err
	}
	tmp := connFile + ".tmp"
	if err := os.WriteFile(tmp, []byte(ClientKeyfile(ssid, password)), 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, connFile)
}

// Network is a Wi-Fi network in range.
type Network struct {
	SSID   string `json:"ssid"`
	Signal int    `json:"signal"` // percent
	Secure bool   `json:"secure"`
}

// Scan lists the networks in range, strongest first, each name once.
func (m *Manager) Scan(ctx context.Context) ([]Network, error) {
	out, err := m.Runner.Run(ctx, "nmcli", "-t", "-f", "SSID,SIGNAL,SECURITY", "device", "wifi", "list", "ifname", ClientInterface, "--rescan", "yes")
	if err != nil {
		return nil, err
	}
	return parseScan(string(out)), nil
}

func parseScan(out string) []Network {
	seen := map[string]int{}
	nets := []Network{}
	for _, line := range strings.Split(out, "\n") {
		f := splitTerse(line)
		if len(f) < 3 || f[0] == "" {
			continue
		}
		sig, _ := strconv.Atoi(f[1])
		n := Network{SSID: f[0], Signal: sig, Secure: f[2] != "" && f[2] != "--"}
		if i, ok := seen[n.SSID]; ok {
			if sig > nets[i].Signal {
				nets[i] = n
			}
			continue
		}
		seen[n.SSID] = len(nets)
		nets = append(nets, n)
	}
	// strongest first
	for i := 1; i < len(nets); i++ {
		for j := i; j > 0 && nets[j].Signal > nets[j-1].Signal; j-- {
			nets[j], nets[j-1] = nets[j-1], nets[j]
		}
	}
	return nets
}

// splitTerse splits nmcli's terse output, where ':' in values is "\:".
func splitTerse(line string) []string {
	var out []string
	var cur strings.Builder
	for i := 0; i < len(line); i++ {
		switch {
		case line[i] == '\\' && i+1 < len(line):
			i++
			cur.WriteByte(line[i])
		case line[i] == ':':
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(line[i])
		}
	}
	return append(out, cur.String())
}

// Status is the Pi's Wi-Fi state.
type Status struct {
	Connected bool   `json:"connected"`
	SSID      string `json:"ssid,omitempty"`
	Address   string `json:"address,omitempty"`
	Hostname  string `json:"hostname"`
	// AccessPoint is the setup network's name while it's on.
	AccessPoint string `json:"access_point,omitempty"`
	// Joined is whether the Pi has ever been on Wi-Fi; until then the setup
	// network stays on.
	Joined bool `json:"joined"`
	// SetupDone is whether the first-run wizard is finished or skipped.
	SetupDone bool `json:"setup_done"`
	// LastJoin is the result of the last join from the setup page.
	LastJoin *JoinResult `json:"last_join,omitempty"`
}

// JoinResult is what came of joining a network.
type JoinResult struct {
	SSID    string    `json:"ssid"`
	OK      bool      `json:"ok"`
	Error   string    `json:"error,omitempty"`
	Address string    `json:"address,omitempty"`
	At      time.Time `json:"at"`
}

// Status reports the current Wi-Fi state.
func (m *Manager) Status(ctx context.Context) Status {
	st := Status{Joined: m.joined(), SetupDone: m.setupDone()}
	st.Hostname, _ = os.Hostname()
	st.Connected, st.SSID, st.Address = m.client(ctx)
	m.mu.Lock()
	if m.apOn {
		st.AccessPoint = m.apSSID()
	}
	if !m.last.At.IsZero() {
		last := m.last
		st.LastJoin = &last
	}
	m.mu.Unlock()
	return st
}

// client reports whether wlan0 is connected, to which network and with
// which address.
func (m *Manager) client(ctx context.Context) (connected bool, ssid, addr string) {
	out, err := m.Runner.Run(ctx, "nmcli", "-t", "-f", "GENERAL.STATE,GENERAL.CONNECTION,IP4.ADDRESS", "device", "show", ClientInterface)
	if err != nil {
		return false, "", ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		k, v, _ := strings.Cut(line, ":")
		switch {
		case k == "GENERAL.STATE":
			connected = strings.HasPrefix(v, "100") // "100 (connected)"
		case k == "GENERAL.CONNECTION":
			ssid = v
		case strings.HasPrefix(k, "IP4.ADDRESS") && addr == "":
			addr, _, _ = strings.Cut(v, "/")
		}
	}
	if !connected {
		return false, "", ""
	}
	// the connection's name; the network's own name is more useful
	if ssid != "" {
		if out, err := m.Runner.Run(ctx, "nmcli", "-t", "-g", "802-11-wireless.ssid", "connection", "show", ssid); err == nil {
			if name := strings.TrimSpace(string(out)); name != "" {
				ssid = name
			}
		}
	}
	return connected, ssid, addr
}

// Join connects to ssid and stores it as the home network. Over the setup
// network, the phone may drop off for a moment while the radio changes
// channel; the result is kept for Status.
func (m *Manager) Join(ctx context.Context, ssid, password, country string) (res JoinResult) {
	res = JoinResult{SSID: ssid}
	defer func() {
		res.At = m.Now()
		m.mu.Lock()
		m.last = res
		m.mu.Unlock()
	}()
	fail := func(err error) JoinResult {
		res.Error = err.Error()
		return res
	}
	switch {
	case ssid == "":
		return fail(errors.New("pick a network"))
	case password != "" && (len(password) < 8 || len(password) > 63):
		return fail(errors.New("Wi-Fi passwords are 8 to 63 characters"))
	}
	if country != "" && m.SetCountry != nil {
		if err := m.SetCountry(ctx, country); err != nil {
			return fail(err)
		}
	}
	if err := WriteClient(m.connectionFile(ClientConnection), ssid, password); err != nil {
		return fail(err)
	}
	_, _ = m.Runner.Run(ctx, "nmcli", "connection", "reload")
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if out, err := m.Runner.Run(ctx, "nmcli", "--wait", "45", "connection", "up", ClientConnection, "ifname", ClientInterface); err != nil {
		msg := strings.TrimSpace(string(out))
		if strings.Contains(msg, "Secrets were required") || strings.Contains(msg, "802-1x") {
			msg = "the password was wrong"
		}
		if msg == "" {
			msg = err.Error()
		}
		return fail(fmt.Errorf("couldn't join %s: %s", ssid, msg))
	}
	_, _, res.Address = m.client(ctx)
	res.OK = true
	m.markJoined()
	m.Log.Info("joined Wi-Fi", "ssid", ssid, "address", res.Address)
	return res
}

// flags

func (m *Manager) flag(name string) string {
	return m.path(filepath.ToSlash(filepath.Join(m.StateDir, name)))
}

func (m *Manager) joinedAt() (time.Time, bool) {
	fi, err := os.Stat(m.flag("wifi-joined"))
	if err != nil {
		return time.Time{}, false
	}
	return fi.ModTime(), true
}

func (m *Manager) joined() bool { _, ok := m.joinedAt(); return ok }

func (m *Manager) markJoined() {
	if m.joined() {
		return
	}
	if err := os.MkdirAll(filepath.Dir(m.flag("wifi-joined")), 0o755); err == nil {
		_ = os.WriteFile(m.flag("wifi-joined"), []byte(m.Now().Format(time.RFC3339)+"\n"), 0o644)
	}
}

func (m *Manager) setupDone() bool {
	if _, err := os.Stat(m.flag("setup-done")); err == nil {
		return true
	}
	// after a reset only finishing the wizard counts
	if _, err := os.Stat(m.flag("setup-again")); err == nil {
		return false
	}
	return m.SetupDone != nil && m.SetupDone()
}

// FinishSetup records that the first-run wizard is done (or skipped). The
// setup network goes off once the Pi is on Wi-Fi.
func (m *Manager) FinishSetup() error {
	if err := os.MkdirAll(filepath.Dir(m.flag("setup-done")), 0o755); err != nil {
		return err
	}
	_ = os.Remove(m.flag("setup-again"))
	return os.WriteFile(m.flag("setup-done"), []byte(m.Now().Format(time.RFC3339)+"\n"), 0o644)
}

// wantAP says whether the setup network should be on: until the Pi has
// been on Wi-Fi, and after that until the wizard is finished, for at most
// Linger.
func (m *Manager) wantAP() bool {
	at, joined := m.joinedAt()
	if !joined {
		return true
	}
	return !m.setupDone() && m.Now().Sub(at) < m.Linger
}

// Run keeps the setup network on or off as needed until ctx is done.
func (m *Manager) Run(ctx context.Context) {
	if _, err := os.Stat(m.path("/sys/class/net/" + ClientInterface)); err != nil {
		m.Log.Info("no Wi-Fi; no setup network", "interface", ClientInterface)
		return
	}
	if err := m.reset(ctx); err != nil {
		m.Log.Warn("couldn't apply the setup request", "file", m.ResetFile, "err", err)
	}
	start := m.Now()
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	for {
		if connected, _, _ := m.client(ctx); connected {
			m.markJoined()
		}
		want := m.wantAP() && (m.joined() || m.Now().Sub(start) >= m.Grace)
		m.mu.Lock()
		on := m.apOn
		m.mu.Unlock()
		switch {
		case want && !on:
			if err := m.startAP(ctx); err != nil {
				m.Log.Error("couldn't start the setup network", "err", err)
				m.stopAP(context.WithoutCancel(ctx)) // clean up half a start; try again later
			} else {
				m.Log.Info("setup network on: join it to set up Glovebox", "ssid", m.apSSID(), "address", APAddress)
			}
		case !want && on:
			m.stopAP(ctx)
			m.Log.Info("setup network off")
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// apSSID is "Glovebox-" and the end of the Wi-Fi chip's address, so two
// Pis nearby differ.
func (m *Manager) apSSID() string {
	b, err := os.ReadFile(m.path("/sys/class/net/" + ClientInterface + "/address"))
	mac := strings.ReplaceAll(strings.TrimSpace(string(b)), ":", "")
	if err != nil || len(mac) < 4 {
		return "Glovebox-setup"
	}
	return "Glovebox-" + strings.ToUpper(mac[len(mac)-4:])
}

func (m *Manager) startAP(ctx context.Context) error {
	if _, err := os.Stat(m.path("/sys/class/net/" + APInterface)); errors.Is(err, fs.ErrNotExist) {
		if _, err := m.Runner.Run(ctx, "iw", "dev", ClientInterface, "interface", "add", APInterface, "type", "__ap"); err != nil {
			return fmt.Errorf("adding %s: %w", APInterface, err)
		}
	}
	// both interfaces share the radio; don't let it doze while one is idle
	_, _ = m.Runner.Run(ctx, "iw", "dev", ClientInterface, "set", "power_save", "off")
	for name, content := range map[string]string{
		m.connectionFile(APConnection):                                     apKeyfile(m.apSSID()),
		m.path("/etc/NetworkManager/dnsmasq-shared.d/glovebox-setup.conf"): captiveDNS,
	} {
		if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(name, []byte(content), 0o600); err != nil {
			return err
		}
	}
	_, _ = m.Runner.Run(ctx, "nmcli", "connection", "reload")
	upCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	if _, err := m.Runner.Run(upCtx, "nmcli", "--wait", "30", "connection", "up", APConnection); err != nil {
		return err
	}
	m.mu.Lock()
	m.apOn = true
	m.mu.Unlock()
	return m.startPortal()
}

func (m *Manager) stopAP(ctx context.Context) {
	m.mu.Lock()
	m.apOn = false
	srv := m.portal
	m.portal = nil
	m.mu.Unlock()
	if srv != nil {
		_ = srv.Close()
	}
	_, _ = m.Runner.Run(ctx, "nmcli", "connection", "down", APConnection)
	_ = os.Remove(m.connectionFile(APConnection))
	_ = os.Remove(m.path("/etc/NetworkManager/dnsmasq-shared.d/glovebox-setup.conf"))
	_, _ = m.Runner.Run(ctx, "nmcli", "connection", "reload")
	_, _ = m.Runner.Run(ctx, "iw", "dev", APInterface, "del")
}

// startPortal serves the sign-in page on the setup network's port 80.
func (m *Manager) startPortal() error {
	if m.Portal == nil {
		return nil
	}
	l, err := net.Listen("tcp", net.JoinHostPort(APAddress, "80"))
	if err != nil {
		return fmt.Errorf("serving the sign-in page: %w", err)
	}
	srv := &http.Server{Handler: Portal(m.Portal), ReadHeaderTimeout: 10 * time.Second}
	m.mu.Lock()
	m.portal = srv
	m.mu.Unlock()
	go func() { _ = srv.Serve(l) }()
	return nil
}

// Portal wraps the web app for the setup network: requests for any other
// site, like the checks phones make to detect a sign-in page, are sent to
// the setup wizard.
func Portal(app http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		if host != APAddress && !strings.HasPrefix(host, "glovebox") {
			w.Header().Set("Cache-Control", "no-store")
			http.Redirect(w, r, "http://"+APAddress+"/#/welcome", http.StatusFound)
			return
		}
		app.ServeHTTP(w, r)
	})
}

// reset forgets that the Pi joined Wi-Fi and was set up, if ResetFile asks
// for it.
func (m *Manager) reset(ctx context.Context) error {
	if m.ResetFile == "" {
		return nil
	}
	if _, err := os.Stat(m.path(m.ResetFile)); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if err := bootfs.WithWrite(ctx, m.BootWriter, func() error {
		for _, name := range []string{"wifi-joined", "setup-done"} {
			if err := os.Remove(m.flag(name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
		if err := os.MkdirAll(filepath.Dir(m.flag("setup-again")), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(m.flag("setup-again"), nil, 0o644); err != nil {
			return err
		}
		return os.Remove(m.path(m.ResetFile))
	}); err != nil {
		return err
	}
	m.Log.Info("setup requested on the boot partition: the setup network comes back until Wi-Fi is joined and the wizard finished", "file", m.ResetFile)
	return nil
}

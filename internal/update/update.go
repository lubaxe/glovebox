// Package update checks GitHub for a newer Glovebox release and installs
// it: the glovebox and tesla-control binaries and the service unit, from
// the release's files, checked against its SHA256SUMS. Packages the image
// installs (rclone, avahi-daemon, ...) aren't touched; a new card or
// install.sh brings those.
package update

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lubaxe/glovebox/internal/system"
)

// Release is a published release.
type Release struct {
	Version   string    `json:"version"` // the tag, e.g. v0.4.0
	Notes     string    `json:"notes"`   // Markdown
	URL       string    `json:"url"`     // the release's page
	Published time.Time `json:"published"`
}

// Status is what the update page shows.
type Status struct {
	Current string `json:"current"`
	// Release is false for a build that isn't a release (a test build or
	// dev): any release then counts as an update.
	Release   bool      `json:"release"`
	Latest    *Release  `json:"latest,omitempty"`
	Available bool      `json:"available"`
	Checked   time.Time `json:"checked,omitzero"`
	// State is "idle", "checking", "installing" or "restarting".
	State string `json:"state"`
	Step  string `json:"step,omitempty"` // what installing is doing
	Error string `json:"error,omitempty"`
	// Supported is false where Glovebox can't update itself (no release
	// build for this processor, or not installed in /usr/local/bin).
	Supported bool   `json:"supported"`
	Why       string `json:"why,omitempty"`
}

// File is a release file and where it's installed.
type File struct {
	Asset string // the release file's name
	Path  string
	Mode  os.FileMode
	// Optional files are only replaced when Path already exists.
	Optional bool
}

// Updater checks for and installs updates. Its methods are safe to call
// from several goroutines.
type Updater struct {
	Repo    string // owner/name
	Current string // the running version
	Files   []File
	// Restart starts the new version, after the files are in place.
	Restart func()
	Runner  system.Runner // for systemctl daemon-reload
	Log     *slog.Logger
	Client  *http.Client
	API     string // https://api.github.com, or a test server
	// MaxAge is how long a check is reused before Status checks again.
	MaxAge time.Duration
	// Unsupported, if set, says why Glovebox can't update itself here.
	Unsupported string

	mu     sync.Mutex
	status Status
}

// New returns an Updater for this machine's install, or one whose Status
// says why it can't update.
func New(repo, current string, runner system.Runner, restart func(), log *slog.Logger) *Updater {
	u := &Updater{
		Repo: repo, Current: current, Restart: restart, Runner: runner, Log: log,
		Client: &http.Client{Timeout: 2 * time.Minute}, API: "https://api.github.com", MaxAge: 6 * time.Hour,
	}
	arch := ""
	switch runtime.GOARCH {
	case "arm64":
		arch = "arm64"
	case "arm":
		arch = "armv6" // the ARMv6 build runs on every 32-bit Pi
	}
	switch exe, _ := os.Executable(); {
	case runtime.GOOS != "linux" || arch == "":
		u.Unsupported = fmt.Sprintf("there's no Glovebox release for %s/%s", runtime.GOOS, runtime.GOARCH)
	case exe != "/usr/local/bin/glovebox":
		u.Unsupported = fmt.Sprintf("Glovebox runs from %s, not /usr/local/bin/glovebox", exe)
	}
	u.Files = []File{
		{Asset: "glovebox-linux-" + arch, Path: "/usr/local/bin/glovebox", Mode: 0o755},
		{Asset: "tesla-control-linux-" + arch, Path: "/usr/local/bin/tesla-control", Mode: 0o755, Optional: true},
		{Asset: "glovebox.service", Path: "/etc/systemd/system/glovebox.service", Mode: 0o644, Optional: true},
	}
	return u
}

// Status returns the last check's result, checking first if it's older
// than MaxAge (only when check is true, so a page can show what's known
// at once).
func (u *Updater) Status(ctx context.Context, check bool) Status {
	u.mu.Lock()
	stale := u.status.State == "idle" || u.status.State == ""
	stale = stale && time.Since(u.status.Checked) > u.MaxAge
	u.mu.Unlock()
	if check && stale {
		u.Check(ctx)
	}
	return u.snapshot()
}

func (u *Updater) snapshot() Status {
	u.mu.Lock()
	defer u.mu.Unlock()
	s := u.status
	s.Current, s.Release = u.Current, parseVersion(u.Current) != nil
	s.Supported, s.Why = u.Unsupported == "", u.Unsupported
	if s.State == "" {
		s.State = "idle"
	}
	return s
}

// Busy reports whether an update is being installed, so keep-awake can
// keep the car (and the Pi's power) on until it's done.
func (u *Updater) Busy() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.status.State == "installing" || u.status.State == "restarting"
}

// Check asks GitHub for the latest release.
func (u *Updater) Check(ctx context.Context) Status {
	u.mu.Lock()
	if u.Unsupported != "" {
		u.mu.Unlock()
		return u.snapshot()
	}
	if u.status.State == "installing" || u.status.State == "restarting" {
		u.mu.Unlock()
		return u.snapshot()
	}
	u.status.State = "checking"
	u.mu.Unlock()

	rel, err := u.latest(ctx)
	u.mu.Lock()
	u.status.State, u.status.Checked = "idle", time.Now()
	if err != nil {
		u.status.Error = err.Error()
	} else {
		u.status.Error = ""
		u.status.Latest = &rel
		u.status.Available = newer(rel.Version, u.Current)
	}
	u.mu.Unlock()
	if err != nil {
		u.Log.Warn("checking for updates", "err", err)
	}
	return u.snapshot()
}

type ghRelease struct {
	TagName     string    `json:"tag_name"`
	Body        string    `json:"body"`
	HTMLURL     string    `json:"html_url"`
	PublishedAt time.Time `json:"published_at"`
	Assets      []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

func (u *Updater) latestRaw(ctx context.Context) (ghRelease, error) {
	var rel ghRelease
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/repos/%s/releases/latest", u.API, u.Repo), nil)
	if err != nil {
		return rel, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "glovebox/"+u.Current)
	resp, err := u.Client.Do(req)
	if err != nil {
		return rel, fmt.Errorf("can't reach GitHub: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests:
		return rel, errors.New("GitHub says to wait: too many checks from this network; try again in an hour")
	case resp.StatusCode != http.StatusOK:
		return rel, fmt.Errorf("GitHub answered %s", resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&rel); err != nil {
		return rel, fmt.Errorf("reading GitHub's answer: %w", err)
	}
	if rel.TagName == "" {
		return rel, errors.New("GitHub's answer has no version")
	}
	return rel, nil
}

func (u *Updater) latest(ctx context.Context) (Release, error) {
	rel, err := u.latestRaw(ctx)
	if err != nil {
		return Release{}, err
	}
	return Release{Version: rel.TagName, Notes: whatsNew(rel.Body), URL: rel.HTMLURL, Published: rel.PublishedAt}, nil
}

// whatsNew keeps the release notes' "What's new" part, leaving out the
// install instructions and image sizes the release workflow adds.
func whatsNew(body string) string {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	if i := strings.Index(body, "\n## Install"); i >= 0 {
		body = body[:i]
	} else if strings.HasPrefix(body, "## Install") {
		body = ""
	}
	return strings.TrimSpace(body)
}

// Install downloads the latest release, puts its files in place and
// restarts Glovebox. It returns once the work has started; Status shows
// how it goes.
func (u *Updater) Install() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	switch {
	case u.Unsupported != "":
		return errors.New(u.Unsupported)
	case u.status.State != "idle" && u.status.State != "":
		return fmt.Errorf("already %s", u.status.State)
	case u.status.Latest == nil || !u.status.Available:
		return errors.New("there's no update to install; check again first")
	}
	u.status.State, u.status.Step, u.status.Error = "installing", "Asking GitHub for the files", ""
	go u.install()
	return nil
}

func (u *Updater) step(s string) {
	u.mu.Lock()
	u.status.Step = s
	u.mu.Unlock()
}

func (u *Updater) install() {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	version, err := u.installFiles(ctx)
	u.mu.Lock()
	if err != nil {
		u.status.State, u.status.Step, u.status.Error = "idle", "", err.Error()
		u.mu.Unlock()
		u.Log.Error("installing the update", "err", err)
		return
	}
	u.status.State, u.status.Step = "restarting", "Starting "+version
	u.mu.Unlock()
	u.Log.Info("update installed; restarting", "from", u.Current, "to", version)
	if u.Restart != nil {
		u.Restart()
	}
}

func (u *Updater) installFiles(ctx context.Context) (string, error) {
	rel, err := u.latestRaw(ctx)
	if err != nil {
		return "", err
	}
	urls := map[string]string{}
	for _, a := range rel.Assets {
		urls[a.Name] = a.URL
	}

	u.step("Downloading the checksums")
	if urls["SHA256SUMS"] == "" {
		return "", fmt.Errorf("%s has no SHA256SUMS", rel.TagName)
	}
	var sums bytes.Buffer
	if err := u.download(ctx, urls["SHA256SUMS"], &sums, 1<<20); err != nil {
		return "", err
	}
	want := parseSums(sums.String())

	// Download everything next to where it goes (same filesystem, so the
	// rename that installs it is atomic) before replacing anything.
	type staged struct {
		File
		tmp string
	}
	var ready []staged
	defer func() {
		for _, s := range ready {
			os.Remove(s.tmp)
		}
	}()
	for _, f := range u.Files {
		if f.Optional {
			if _, err := os.Stat(f.Path); err != nil {
				continue
			}
		}
		if urls[f.Asset] == "" || want[f.Asset] == "" {
			return "", fmt.Errorf("%s has no %s", rel.TagName, f.Asset)
		}
		u.step("Downloading " + f.Asset)
		tmp, err := u.stage(ctx, urls[f.Asset], want[f.Asset], f)
		if err != nil {
			return "", fmt.Errorf("%s: %w", f.Asset, err)
		}
		ready = append(ready, staged{f, tmp})
	}

	// The new glovebox has to run here and be the version it says.
	u.step("Checking the new version")
	for _, s := range ready {
		if !strings.HasPrefix(s.Asset, "glovebox-linux-") {
			continue
		}
		out, err := u.Runner.Run(ctx, s.tmp, "-version")
		if err != nil {
			return "", fmt.Errorf("the new version doesn't run on this board: %w", err)
		}
		if got := strings.TrimSpace(string(out)); got != rel.TagName {
			return "", fmt.Errorf("the new version says it's %q, not %s", got, rel.TagName)
		}
	}

	u.step("Installing " + rel.TagName)
	unit := false
	for i, s := range ready {
		if strings.HasSuffix(s.Path, ".service") {
			if same(s.tmp, s.Path) {
				continue
			}
			unit = true
		}
		// The running binary keeps working after it's replaced; the old
		// one stays as .old to go back to by hand.
		if s.Mode&0o111 != 0 {
			_ = os.Remove(s.Path + ".old")
			if err := os.Link(s.Path, s.Path+".old"); err != nil && !os.IsNotExist(err) {
				u.Log.Warn("keeping the old version", "file", s.Path, "err", err)
			}
		}
		if err := os.Rename(s.tmp, s.Path); err != nil {
			return "", fmt.Errorf("installing %s: %w", s.Path, err)
		}
		ready[i].tmp = "" // nothing left to clean up
	}
	if unit {
		if _, err := u.Runner.Run(ctx, "systemctl", "daemon-reload"); err != nil {
			return "", err
		}
	}
	return rel.TagName, nil
}

// stage downloads url to a temporary file next to f.Path and checks it.
func (u *Updater) stage(ctx context.Context, url, sum string, f File) (string, error) {
	tmp, err := os.CreateTemp(filepath.Dir(f.Path), "."+filepath.Base(f.Path)+".new-*")
	if err != nil {
		return "", err
	}
	name := tmp.Name()
	h := sha256.New()
	err = u.download(ctx, url, io.MultiWriter(tmp, h), 200<<20)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil && hex.EncodeToString(h.Sum(nil)) != sum {
		err = errors.New("the download doesn't match SHA256SUMS")
	}
	if err == nil {
		err = os.Chmod(name, f.Mode)
	}
	if err != nil {
		os.Remove(name)
		return "", err
	}
	return name, nil
}

func (u *Updater) download(ctx context.Context, url string, w io.Writer, max int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "glovebox/"+u.Current)
	resp, err := u.Client.Do(req)
	if err != nil {
		return fmt.Errorf("downloading: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("downloading: %s", resp.Status)
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, max+1))
	if err != nil {
		return fmt.Errorf("downloading: %w", err)
	}
	if n > max {
		return errors.New("the download is too large")
	}
	return nil
}

func parseSums(text string) map[string]string {
	sums := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		sum, name, ok := strings.Cut(strings.TrimSpace(sc.Text()), " ")
		if !ok {
			continue
		}
		sums[strings.TrimPrefix(strings.TrimSpace(name), "*")] = strings.ToLower(sum)
	}
	return sums
}

func same(a, b string) bool {
	x, err1 := os.ReadFile(a)
	y, err2 := os.ReadFile(b)
	return err1 == nil && err2 == nil && bytes.Equal(x, y)
}

// parseVersion parses vMAJOR.MINOR.PATCH, or returns nil.
func parseVersion(v string) []int {
	parts := strings.Split(strings.TrimPrefix(v, "v"), ".")
	if !strings.HasPrefix(v, "v") || len(parts) != 3 {
		return nil
	}
	out := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return nil
		}
		out[i] = n
	}
	return out
}

// newer says whether latest is an update for current. Any release is one
// for a build that isn't a release.
func newer(latest, current string) bool {
	l, c := parseVersion(latest), parseVersion(current)
	if l == nil {
		return false
	}
	if c == nil {
		return true
	}
	for i := range 3 {
		if l[i] != c[i] {
			return l[i] > c[i]
		}
	}
	return false
}

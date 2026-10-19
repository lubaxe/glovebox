// Command uidemo serves the web UI with sample footage and made-up status,
// for working on the UI (and taking screenshots) without a Pi or a car.
// It needs ffmpeg to make the sample videos, once:
//
//	go run ./tools/uidemo -footage ~/glovebox-demo
//
// then open http://localhost:8090. To show real footage instead, put six
// unencrypted clips of one minute (front.mp4, back.mp4, left_repeater.mp4,
// right_repeater.mp4, left_pillar.mp4, right_pillar.mp4) and a thumb.png in
// <footage>/videos first; thumb-1.png, thumb-2.png... give events their own
// thumbnails.
//
// -bandwidth 25 limits what the demo serves to 25 Mbit/s, like the Pi's
// Wi-Fi, and logs what the viewer fetches, to try the low-bandwidth mode.
package main

import (
	"context"
	"crypto/rand"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lubaxe/glovebox/internal/archive"
	"github.com/lubaxe/glovebox/internal/clips"
	"github.com/lubaxe/glovebox/internal/drive"
	"github.com/lubaxe/glovebox/internal/gadget"
	"github.com/lubaxe/glovebox/internal/keepawake"
	"github.com/lubaxe/glovebox/internal/setup"
	"github.com/lubaxe/glovebox/internal/snapshot"
	"github.com/lubaxe/glovebox/internal/store"
	"github.com/lubaxe/glovebox/internal/sysinfo"
	"github.com/lubaxe/glovebox/internal/system"
	"github.com/lubaxe/glovebox/internal/update"
	"github.com/lubaxe/glovebox/internal/web"
	"github.com/lubaxe/glovebox/internal/wifi"
)

var cameras = []string{"front", "back", "left_repeater", "right_repeater", "left_pillar", "right_pillar"}

func main() {
	addr := flag.String("addr", "localhost:8090", "address to serve on")
	footage := flag.String("footage", filepath.Join(os.TempDir(), "glovebox-demo"), "where to make the sample footage")
	state := flag.String("archive", "home", `archive state to show: "home", "archiving" or "away"`)
	paired := flag.Bool("paired", true, "show the car as paired")
	extras := flag.Bool("extras", false, "show music and Boombox drives")
	firstRun := flag.Bool("firstrun", false, "start as a Pi that isn't set up, on its setup network")
	setupDone := flag.Bool("setupdone", false, "start as a Pi on home Wi-Fi with setup finished, for Guided setup")
	updates := flag.String("update", "available", `software update to show: "available" or "current"`)
	realClip := flag.String("real-clip", "", "optional local unencrypted front MP4, copied only into the isolated demo snapshot")
	bandwidth := flag.Float64("bandwidth", 0, "serve at most this many Mbit/s, like the Pi's Wi-Fi (0: no limit)")
	flag.Parse()

	videos := filepath.Join(*footage, "videos")
	if *realClip == "" {
		if err := makeVideos(videos); err != nil {
			log.Fatal(err)
		}
	}
	mount := filepath.Join(*footage, "snapshot")
	if err := os.RemoveAll(mount); err != nil {
		log.Fatal(err)
	}
	if *realClip != "" {
		if err := makeRealDrive(mount, *realClip); err != nil {
			log.Fatal(err)
		}
	} else if err := makeDrive(mount, videos, time.Now()); err != nil {
		log.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	snaps := fakeSnapshots{{ID: "snap-000042", Taken: time.Now().Add(-5 * time.Minute), Mount: mount}}
	lib := clips.NewLibrary(func() ([]clips.Snapshot, error) {
		return []clips.Snapshot{{ID: snaps[0].ID, Mount: mount}}, nil
	})
	car := &keepawake.Car{Command: "tesla-control", Dir: filepath.Join(*footage, "ble"), Runner: &system.FakeRunner{}}
	_ = car.Forget()
	if *paired {
		_ = car.SetVIN("5YJ3E1EA0PF000001")
		_ = os.WriteFile(filepath.Join(car.Dir, "role"), []byte(keepawake.Driver+"\n"), 0o600)
		_ = car.CheckPairing(context.Background())
	}
	s := &web.Server{
		Version: "demo", Status: fakeStatus{}, Log: logger,
		Drive: fakeDrive{extras: *extras}, Snapshots: snaps, Clips: lib,
		Archive: fakeArchive{state: *state}, Car: car, KeepAwake: &fakeKeepAwake{},
		Setup: demoSetup(*footage), Sizing: &fakeSizing{},
	}
	s.Update = newFakeUpdater(s, *updates == "available")
	switch {
	case *firstRun:
		s.WiFi = &fakeWiFi{}
	case *setupDone:
		s.WiFi = &fakeWiFi{done: true, joined: &wifi.JoinResult{SSID: "Springfield Home", OK: true, Address: "192.168.1.50", At: time.Now()}}
	}
	log.Printf("serving the demo UI on http://%s", *addr)
	log.Fatal(http.ListenAndServe(*addr, limit(s.Handler(), *bandwidth)))
}

// limit serves at most mbps Mbit/s across all requests, and logs the rate
// video is fetched at every 10 seconds.
func limit(h http.Handler, mbps float64) http.Handler {
	t := &throttle{bytesPerSec: mbps * 1e6 / 8}
	go func() {
		for range time.Tick(10 * time.Second) {
			t.mu.Lock()
			n := t.video
			t.video = 0
			t.mu.Unlock()
			if n > 0 {
				log.Printf("video served: %.2f Mbit/s", float64(n)*8/10/1e6)
			}
		}
	}()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		video := strings.HasPrefix(r.URL.Path, "/media/") || strings.HasPrefix(r.URL.Path, "/preview/")
		h.ServeHTTP(&throttledWriter{ResponseWriter: w, t: t, video: video}, r)
	})
}

type throttle struct {
	mu          sync.Mutex
	bytesPerSec float64
	next        time.Time
	video       int64
}

// wait blocks until n more bytes fit in the bandwidth.
func (t *throttle) wait(n int, video bool) {
	t.mu.Lock()
	if video {
		t.video += int64(n)
	}
	if t.bytesPerSec <= 0 {
		t.mu.Unlock()
		return
	}
	now := time.Now()
	if t.next.Before(now) {
		t.next = now
	}
	t.next = t.next.Add(time.Duration(float64(n) / t.bytesPerSec * float64(time.Second)))
	until := t.next
	t.mu.Unlock()
	time.Sleep(time.Until(until))
}

type throttledWriter struct {
	http.ResponseWriter
	t     *throttle
	video bool
}

func (w *throttledWriter) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		n := min(len(p), 16<<10)
		w.t.wait(n, w.video)
		k, err := w.ResponseWriter.Write(p[:n])
		written += k
		if err != nil {
			return written, err
		}
		p = p[n:]
	}
	return written, nil
}

// makeVideos makes a short sample video per camera, and a thumbnail.
func makeVideos(dir string) error {
	if _, err := os.Stat(filepath.Join(dir, "thumb.png")); err == nil {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	font := "DejaVuSans.ttf"
	if win := `C\:/Windows/Fonts/segoeui.ttf`; os.PathSeparator == '\\' {
		font = win
	}
	colors := map[string][2]string{
		"front": {"0x1d3557", "0x8ecae6"}, "back": {"0x2b2d42", "0xadb5bd"},
		"left_repeater": {"0x264653", "0x83c5be"}, "right_repeater": {"0x3d405b", "0xa8dadc"},
		"left_pillar": {"0x22333b", "0x9aa5b1"}, "right_pillar": {"0x283618", "0xb7c4a0"},
	}
	for _, cam := range cameras {
		c := colors[cam]
		src := fmt.Sprintf("gradients=s=960x720:r=15:c0=%s:c1=%s:speed=0.004:d=60", c[0], c[1])
		text := fmt.Sprintf("drawtext=fontfile='%s':text='%s':fontcolor=white@0.85:fontsize=44:x=(w-tw)/2:y=(h-th)/2,"+
			"drawtext=fontfile='%s':text='%%{pts\\:hms}':fontcolor=white@0.7:fontsize=28:x=24:y=20", font, label(cam), font)
		out := filepath.Join(dir, cam+".mp4")
		cmd := exec.Command("ffmpeg", "-y", "-loglevel", "error", "-f", "lavfi", "-i", src, "-vf", text,
			"-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p", "-b:v", "400k", out)
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("ffmpeg (is it installed?): %w", err)
		}
	}
	return exec.Command("ffmpeg", "-y", "-loglevel", "error", "-ss", "5", "-i", filepath.Join(dir, "front.mp4"),
		"-frames:v", "1", "-vf", "scale=480:-1", filepath.Join(dir, "thumb.png")).Run()
}

func label(cam string) string {
	return map[string]string{
		"front": "Front", "back": "Rear", "left_repeater": "Left repeater", "right_repeater": "Right repeater",
		"left_pillar": "Left pillar", "right_pillar": "Right pillar",
	}[cam] + " camera (sample)"
}

// makeDrive lays out a car drive with events, recent clips and encrypted
// clips, linking to the sample videos.
func makeDrive(root, videos string, now time.Time) error {
	stamp := func(t time.Time) string { return t.Format("2006-01-02_15-04-05") }
	link := func(src, dst string) error {
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.Link(src, dst); err == nil {
			return nil
		}
		return copyFile(src, dst)
	}
	clip := func(dir string, start time.Time, encrypted bool) error {
		for _, cam := range cameras {
			dst := filepath.Join(dir, stamp(start)+"-"+cam+".mp4")
			if encrypted {
				if err := randomFile(dst, 300<<10); err != nil {
					return err
				}
				continue
			}
			if err := link(filepath.Join(videos, cam+".mp4"), dst); err != nil {
				return err
			}
		}
		return nil
	}
	thumbs := 0
	event := func(kind string, at time.Time, minutes int, reason, city string, encrypted bool) error {
		base := filepath.Join(root, "TeslaCam")
		if encrypted {
			base = filepath.Join(base, "EncryptedClips")
		}
		dir := filepath.Join(base, kind, stamp(at))
		for m := minutes; m > 0; m-- {
			if err := clip(dir, at.Add(-time.Duration(m)*time.Minute+10*time.Second), encrypted); err != nil {
				return err
			}
		}
		if encrypted {
			return nil
		}
		thumbs++
		thumb := filepath.Join(videos, fmt.Sprintf("thumb-%d.png", thumbs))
		if _, err := os.Stat(thumb); err != nil {
			thumb = filepath.Join(videos, "thumb.png")
		}
		if err := link(thumb, filepath.Join(dir, "thumb.png")); err != nil {
			return err
		}
		meta := fmt.Sprintf(`{"timestamp":"%s","city":"%s","est_lat":"39.7817","est_lon":"-89.6501","reason":"%s","camera":"0"}`,
			at.Format("2006-01-02T15:04:05"), city, reason)
		return os.WriteFile(filepath.Join(dir, "event.json"), []byte(meta), 0o644)
	}

	day := now.Truncate(time.Minute)
	for _, e := range []struct {
		kind      string
		ago       time.Duration
		minutes   int
		reason    string
		encrypted bool
	}{
		{"SentryClips", 40 * time.Minute, 3, "sentry_aware_object_detection", false},
		{"SentryClips", 5 * time.Hour, 2, "sentry_aware_accel", false},
		{"SentryClips", 26 * time.Hour, 3, "sentry_locked_handle_pulls", false},
		{"SentryClips", 2 * time.Hour, 2, "", true},
		{"SavedClips", 3 * time.Hour, 3, "user_interaction_dashcam_icon_tapped", false},
		{"SavedClips", 30 * time.Hour, 2, "user_interaction_honk", false},
		{"SavedClips", 50 * time.Minute, 1, "", true},
	} {
		if err := event(e.kind, day.Add(-e.ago), e.minutes, e.reason, "Springfield", e.encrypted); err != nil {
			return err
		}
	}
	for m := 1; m <= 14; m++ {
		if err := clip(filepath.Join(root, "TeslaCam", "RecentClips"), day.Add(-time.Duration(m)*time.Minute), false); err != nil {
			return err
		}
	}
	for m := 20; m <= 27; m++ {
		if err := clip(filepath.Join(root, "TeslaCam", "EncryptedClips", "RecentClips"), day.Add(-time.Duration(m)*time.Minute), true); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func randomFile(path string, size int64) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if _, err := io.CopyN(f, rand.Reader, size); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// ---- made-up status ----

type fakeStatus struct{}

func (fakeStatus) Read() (sysinfo.Info, error) {
	temp, signal := 47.2, -58
	return sysinfo.Info{
		Hostname: "glovebox", Model: "Raspberry Pi Zero 2 W Rev 1.0", UptimeSec: 3*3600 + 17*60,
		Load1: 0.32, MemTotal: 415 << 20, MemAvail: 268 << 20, CPUTempC: &temp, WiFiSignal: &signal,
	}, nil
}

type fakeDrive struct{ extras bool }

func (d fakeDrive) Status() (drive.Status, error) {
	st := drive.Status{
		CamDisk: "/backingfiles/cam_disk.bin", CamSize: 52 << 30,
		Gadget:    gadget.State{Enabled: true, HostState: "configured"},
		Snapshots: 6, FreeBytes: 31 << 30,
		Extras: []drive.ExtraStatus{},
	}
	if d.extras {
		synced := time.Now().Add(-40 * time.Minute)
		st.Extras = []drive.ExtraStatus{
			{Name: "music", Label: "MUSIC", Size: 8 << 30, Source: "Music", LastSync: &synced},
			{Name: "boombox", Label: "BOOMBOX", Size: 256 << 20, Source: "Boombox", LastSync: &synced},
		}
	}
	return st, nil
}
func (fakeDrive) TakeSnapshot() {}

type fakeSnapshots []snapshot.Snapshot

func (s fakeSnapshots) List() ([]snapshot.Snapshot, error)    { return s, nil }
func (s fakeSnapshots) Release(context.Context, string) error { return nil }

type fakeArchive struct{ state string }

func (a fakeArchive) Status() archive.Status {
	last := &archive.Result{Started: time.Now().Add(-25 * time.Minute), Seconds: 412, Uploaded: 96, Bytes: 4 << 30,
		Events: []string{"sentry/x", "saved/y"}}
	st := archive.Status{Target: "//192.168.1.10/TeslaCam", State: a.state, Archived: 1342, Last: last}
	if a.state == archive.Archiving {
		st.Done, st.Total, st.CatchingUp = 37, 96, true
		st.Current = "SentryClips/2026-10-16_21-00-00/2026-10-16_20-59-10-front.mp4"
	}
	return st
}
func (fakeArchive) Trigger() {}

// fakeKeepAwake is idle until the Car page's button holds it.
type fakeKeepAwake struct {
	mu    sync.Mutex
	since time.Time
	until time.Time
}

func (k *fakeKeepAwake) Status() keepawake.Status {
	k.mu.Lock()
	defer k.mu.Unlock()
	if time.Now().Before(k.until) {
		return keepawake.Status{Active: true, Since: k.since, HeldUntil: k.until,
			LastWake: time.Now().Add(-40 * time.Second), NextWake: time.Now().Add(20 * time.Second), CarState: "AWAKE",
			Nudge: "charge port: not allowed for this key; charging current re-set to 32 A"}
	}
	return keepawake.Status{LastWake: time.Now().Add(-18 * time.Minute), CarState: "AWAKE"}
}

func (k *fakeKeepAwake) Hold(d time.Duration) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.since, k.until = time.Now(), time.Now().Add(d)
}

// demoSetup is a real setup page on a config in dir, with a fake ssh-keygen.
func demoSetup(dir string) *setup.Setup {
	// the config wants absolute paths; on Windows, ones on the current drive
	dir = filepath.ToSlash(strings.TrimPrefix(dir, filepath.VolumeName(dir)))
	conf := dir + "/glovebox.toml"
	content := fmt.Sprintf(`[archive]
enabled = true
[archive.smb]
server = "192.168.1.10"
share = "TeslaCam"
credentials = "%[1]s/smb-credentials"
[archive.rsync]
key = "%[1]s/ssh/id_ed25519"
known_hosts = "%[1]s/ssh/known_hosts"
[archive.rclone]
config = "%[1]s/rclone.conf"
[drive.music]
size = "8G"
source = "Music"
`, dir)
	if err := os.WriteFile(conf, []byte(content), 0o600); err != nil {
		log.Fatal(err)
	}
	_ = os.WriteFile(dir+"/smb-credentials", []byte("username=carcam\npassword=demo\n"), 0o600)
	keygen := &system.FakeRunner{Handler: func(cmd string) ([]byte, error) {
		f := strings.Fields(cmd)
		return nil, os.WriteFile(f[len(f)-1]+".pub", []byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDemoKeyOnlyForTheDemo glovebox@glovebox\n"), 0o644)
	}}
	return &setup.Setup{ConfigPath: conf, Runner: keygen, Gateway: func() string { return "192.168.1.1" },
		Options: archive.Options{Runner: keygen, RunDir: os.TempDir()}}
}

// fakeWiFi is a Pi on its setup network that joins any network after a
// moment.
type fakeWiFi struct {
	mu     sync.Mutex
	joined *wifi.JoinResult
	done   bool
}

func (f *fakeWiFi) Status(context.Context) wifi.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := wifi.Status{Hostname: "glovebox", SetupDone: f.done, LastJoin: f.joined}
	if !f.done {
		st.AccessPoint = "Glovebox-ABCD"
	}
	if f.joined != nil && f.joined.OK {
		st.Connected, st.SSID, st.Address, st.Joined = true, f.joined.SSID, f.joined.Address, true
	}
	return st
}

func (f *fakeWiFi) Scan(context.Context) ([]wifi.Network, error) {
	time.Sleep(time.Second)
	return []wifi.Network{{SSID: "Springfield Home", Signal: 78, Secure: true}, {SSID: "Neighbours", Signal: 40, Secure: true}, {SSID: "Cafe", Signal: 22}}, nil
}

func (f *fakeWiFi) Join(_ context.Context, ssid, password, _ string) wifi.JoinResult {
	time.Sleep(2 * time.Second)
	res := wifi.JoinResult{SSID: ssid, At: time.Now()}
	if password == "wrong password" {
		res.Error = "couldn't join " + ssid + ": the password was wrong"
	} else {
		res.OK, res.Address = true, "192.168.1.50"
	}
	f.mu.Lock()
	f.joined = &res
	f.mu.Unlock()
	return res
}

func (f *fakeWiFi) FinishSetup() error {
	f.mu.Lock()
	f.done = true
	f.mu.Unlock()
	return nil
}

// fakeUpdater has a v0.6.0 to install, and goes through the steps of
// installing it.
type fakeUpdater struct {
	mu     sync.Mutex
	srv    *web.Server
	status update.Status
}

func newFakeUpdater(srv *web.Server, available bool) *fakeUpdater {
	// with nothing to install, it runs the latest version
	current, latest := "v0.5.0", "v0.6.0"
	if !available {
		current = latest
	}
	srv.Version = current
	return &fakeUpdater{srv: srv, status: update.Status{
		Current: current, Release: true, Supported: true, State: "idle", Available: available, Checked: time.Now().Add(-2 * time.Minute),
		Latest: &update.Release{
			Version: latest, URL: "https://github.com/lubaxe/glovebox/releases/tag/" + latest, Published: time.Now().Add(-26 * time.Hour),
			Notes: "## What's new\n\n### Glovebox encryption\n- Encrypt the footage on the Pi with a key that's yours, under **Settings → Security**. " +
				"The drive only opens once Glovebox has seen your car over Bluetooth.\n\n" +
				"### Recorded telemetry\n- The player shows the speed, gear, steering and Autopilot state the car recorded.",
		},
	}}
}

func (f *fakeUpdater) Status(context.Context, bool) update.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status
}

func (f *fakeUpdater) Check(context.Context) update.Status {
	time.Sleep(1500 * time.Millisecond)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status.Checked = time.Now()
	return f.status
}

func (f *fakeUpdater) Install() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.status.Available {
		return fmt.Errorf("there's no update to install; check again first")
	}
	f.status.State = "installing"
	go func() {
		steps := []string{"Downloading the checksums", "Downloading glovebox-linux-arm64", "Downloading tesla-control-linux-arm64", "Checking the new version", "Installing " + f.status.Latest.Version}
		for _, s := range steps {
			f.mu.Lock()
			f.status.Step = s
			f.mu.Unlock()
			time.Sleep(1500 * time.Millisecond)
		}
		f.mu.Lock()
		f.status.State, f.status.Step = "restarting", ""
		f.mu.Unlock()
		time.Sleep(4 * time.Second)
		f.mu.Lock()
		f.status = update.Status{Current: f.status.Latest.Version, Release: true, Supported: true, State: "idle", Latest: f.status.Latest, Checked: time.Now()}
		f.srv.Version = f.status.Current // the demo's "restart"
		f.mu.Unlock()
	}()
	return nil
}

// makeRealDrive keeps real-clip QA local and separate from generated demo data.
// The source file is opened read-only; it is never included in the repository.
func makeRealDrive(mount, source string) error {
	name := filepath.Base(source)
	stamp := strings.TrimSuffix(name, "-front.mp4")
	if _, err := time.Parse("2006-01-02_15-04-05", stamp); err != nil {
		return fmt.Errorf("real clip must use Tesla's timestamp-front.mp4 filename: %w", err)
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	directory := filepath.Join(mount, "TeslaCam", "SavedClips", stamp)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	output, err := os.Create(filepath.Join(directory, name))
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	return os.WriteFile(filepath.Join(directory, "event.json"), []byte(fmt.Sprintf("{\"timestamp\":\"%s\",\"reason\":\"Local viewer test\"}", stamp[:10]+"T"+strings.ReplaceAll(stamp[11:], "-", ":"))), 0o644)
}

// fakeSizing is a 64 GB card set up the old way: the car's drive takes
// nearly all of it.
type fakeSizing struct{ recreate int64 }

func (f *fakeSizing) Storage() (drive.Storage, error) {
	const g = 1 << 30
	return drive.Storage{
		Total: 576 * g / 10, Free: 42 * g / 10, MinFree: 4 * g,
		Car: drive.Car{Size: 52 * g, Allocated: 53 * g, Known: true, Recent: 16 * g, Sentry: 46 * g / 10,
			Saved: g / 2, Other: g / 10, Free: 31 * g, RecentMinutes: 61},
		Extras:  []drive.ExtraUsage{},
		Kept:    store.Usage{Bytes: 118 * g / 10, Minutes: 43, ArchivedBytes: 6 * g, ArchivedMinutes: 22},
		Outside: 4 * g / 10, Rate: 164 * g / 10, RateMeasured: true,
		MinCarSize: 25 * g, MaxCarSize: 41 * g, Recreate: f.recreate,
	}, nil
}

func (f *fakeSizing) Resize(_ context.Context, size int64) error { f.recreate = size; return nil }
func (f *fakeSizing) CancelResize() error                        { f.recreate = 0; return nil }

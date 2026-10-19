// Command glovebox is the service that runs on the Pi.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/lubaxe/glovebox/internal/archive"
	"github.com/lubaxe/glovebox/internal/clips"
	"github.com/lubaxe/glovebox/internal/config"
	"github.com/lubaxe/glovebox/internal/crypt"
	"github.com/lubaxe/glovebox/internal/drive"
	"github.com/lubaxe/glovebox/internal/gadget"
	"github.com/lubaxe/glovebox/internal/keepawake"
	"github.com/lubaxe/glovebox/internal/logfile"
	"github.com/lubaxe/glovebox/internal/provision"
	"github.com/lubaxe/glovebox/internal/setup"
	"github.com/lubaxe/glovebox/internal/snapshot"
	"github.com/lubaxe/glovebox/internal/store"
	"github.com/lubaxe/glovebox/internal/sysinfo"
	"github.com/lubaxe/glovebox/internal/system"
	"github.com/lubaxe/glovebox/internal/telemetry"
	"github.com/lubaxe/glovebox/internal/update"
	"github.com/lubaxe/glovebox/internal/web"
	"github.com/lubaxe/glovebox/internal/wifi"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "glovebox:", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", config.DefaultPath, "path to the config file")
	bootConfig := flag.String("boot-config", "/boot/firmware/glovebox.toml",
		"config on the boot partition, applied at startup if present (empty to skip)")
	showVersion := flag.Bool("version", false, "print the version and exit")
	runDir := flag.String("run-dir", "/run/glovebox", "directory for mount points, sockets and short-lived key files")
	noUSB := flag.Bool("no-usb", false, "don't present the drives over USB (for testing a second instance on a Pi)")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return nil
	}

	// Apply the boot partition's glovebox.toml (first boot, or a new one the
	// user dropped on the card) before reading the config.
	bootLog := slog.New(slog.NewTextHandler(os.Stdout, nil))
	if radios := provision.UnblockRadios("/"); len(radios) > 0 {
		bootLog.Info("unblocked radios", "radios", radios) // Wi-Fi for archiving, Bluetooth for keep-awake
	}
	if *bootConfig != "" {
		prov := provision.New(system.Exec{Log: bootLog}, bootLog)
		if _, err := prov.Apply(context.Background(), *bootConfig, *configPath); err != nil {
			bootLog.Error("couldn't apply the boot partition's config; see glovebox-status.txt on the card", "err", err)
		}
	}

	cfg, missing, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	level, _ := cfg.Log.SlogLevel() // already validated by Load
	// systemd's journal adds timestamps, so leave them out there. The journal
	// is in memory, so a copy goes to backingfiles (once it's mounted) to
	// survive the car cutting the power.
	logFile := &logfile.File{Max: 2 << 20}
	defer logFile.Close()
	log := slog.New(logfile.Fanout{
		slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
			Level: level,
			ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
				if len(groups) == 0 && a.Key == slog.TimeKey {
					return slog.Attr{}
				}
				return a
			},
		}),
		slog.NewTextHandler(logFile, &slog.HandlerOptions{Level: level}),
	})
	log.Info("starting", "version", version, "config", *configPath)
	if missing {
		log.Warn("config file not found, using defaults", "path", *configPath)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	api := &web.Server{Version: version, Status: sysinfo.NewReader(), Log: log}
	api.Setup = &setup.Setup{
		ConfigPath: *configPath, Options: archiveOptions(log, *runDir),
		Runner: system.Exec{Log: log}, Gateway: setup.Gateway,
	}
	api.Restart = func() {
		// for new settings or a new version: systemd stops this process and
		// starts a new one; the drives stay connected unless the new settings
		// change them
		log.Info("restarting")
		go system.Exec{Log: log}.Run(context.Background(), "systemctl", "--no-block", "restart", "glovebox.service")
	}
	updater := update.New("lubaxe/glovebox", version, system.Exec{Log: log}, api.Restart, log)
	api.Update = updater

	wifiMgr := wifi.New(system.Exec{Log: log}, log)
	if cfg.System.SetupNetwork {
		wifiMgr.SetCountry = provision.New(system.Exec{Log: log}, log).SetWiFiCountry
		// set up by hand, before the wizard existed
		wifiMgr.SetupDone = func() bool { return cfg.Archive.Enabled }
		api.WiFi = wifiMgr
	}

	// The car is talked to for pairing, keeping it awake and, on an
	// encrypted drive, the check that unlocks it. One Car serialises all of
	// it: only one Bluetooth conversation can run at a time.
	var car *keepawake.Car
	if cfg.KeepAwake.Enabled {
		car = &keepawake.Car{
			Command: cfg.KeepAwake.Command, Dir: cfg.KeepAwake.KeyDir, ConfigVIN: cfg.KeepAwake.VIN,
			Runner: system.Exec{Log: log}, Log: log,
		}
	}

	// The handler is swapped once the drive has started, so a request never
	// sees it half set up.
	handler := &swapHandler{}
	if cfg.Drive.Enabled {
		ds := &driveStarter{cfg: cfg, log: log, logFile: logFile, car: car, busy: updater.Busy, runDir: *runDir, noUSB: *noUSB}
		enc := newEncryption(cfg, car, log, *configPath, *bootConfig, *runDir)
		api.Encryption = enc
		if enc.TakeRequest() {
			// asked for in the web UI: before anything uses backingfiles
			encryptDrive(ctx, cfg, enc, log, *runDir, *noUSB)
		}
		encrypted, err := enc.Encrypted(ctx)
		if err != nil {
			log.Error("can't tell whether the drive is encrypted", "err", err)
		}
		if encrypted {
			// The drive is presented once the car has been seen, or the user
			// unlocks it with their key; meanwhile the web UI shows why and
			// takes the key.
			locked := *api
			if car != nil {
				locked.Car = car // to pair again, should the check fail for that
			}
			handler.Set(locked.Handler())
			log.Info("the drive is encrypted; it's presented once the car has been seen", "car_check", enc.Mode)
			go func() {
				if err := enc.Run(ctx); err != nil {
					return // shutting down
				}
				unlocked := *api
				ds.device = enc.Volume.Mapper()
				ds.start(ctx, &unlocked)
				handler.Set(unlocked.Handler())
			}()
		} else {
			ds.start(ctx, api)
			handler.Set(api.Handler())
		}
		// The gadget stays up on shutdown: the car keeps recording while the
		// service restarts.
	} else {
		log.Info("drive disabled in config")
		handler.Set(api.Handler())
	}

	if cfg.System.SetupNetwork {
		wifiMgr.Portal = handler
		go wifiMgr.Run(ctx)
	}

	srv := &http.Server{
		Addr:              cfg.Web.Listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	errc := make(chan error, 1)
	go func() {
		log.Info("http server listening", "addr", cfg.Web.Listen)
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
	}

	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("http shutdown: %w", err)
	}
	return nil
}

// driveStarter starts the drive and everything that needs backingfiles: at
// startup, or once the encrypted drive is unlocked.
type driveStarter struct {
	cfg     config.Config
	log     *slog.Logger
	logFile *logfile.File
	car     *keepawake.Car
	// busy reports an update under way, which keeps the car awake too
	busy   func() bool
	runDir string
	noUSB  bool
	// device is the open encrypted volume to mount; empty for a plain drive
	device string
}

// start fills in api's drive, clip, archive and car parts.
func (ds *driveStarter) start(ctx context.Context, api *web.Server) {
	cfg, log := ds.cfg, ds.log
	// On a freshly flashed card this creates the backingfiles partition.
	bf := &provision.Backingfiles{
		Mountpoint: cfg.Drive.Backingfiles, Fstab: "/etc/fstab",
		MinSize: 4 << 30, Device: ds.device, Runner: system.Exec{Log: log},
	}
	if created, err := bf.Ensure(ctx); err != nil {
		log.Error("backingfiles not available", "err", err)
	} else {
		if created {
			log.Info("created the backingfiles partition", "mountpoint", cfg.Drive.Backingfiles)
		}
		if err := ds.logFile.Open(filepath.Join(cfg.Drive.Backingfiles, "glovebox.log")); err != nil {
			log.Warn("can't keep a log on backingfiles", "err", err)
		} else {
			log.Info("logging to backingfiles too", "file", filepath.Join(cfg.Drive.Backingfiles, "glovebox.log"))
		}
	}
	// The archive state is needed before the drive starts: the cleanup
	// at startup only deletes events it says are archived.
	var state *archive.State
	if cfg.Archive.Enabled {
		var err error
		state, err = archive.LoadState(filepath.Join(cfg.Drive.Backingfiles, "archive-state.json"))
		switch {
		case errors.Is(err, archive.ErrStateReset):
			log.Warn("archive state was unreadable (power cut?); checking the share for what's already there", "err", err)
		case err != nil:
			log.Error("reading archive state; archiving disabled", "err", err)
		}
		if state != nil {
			if state.UseTarget(cfg.Archive.Destination()) {
				log.Warn("the archive target changed; archiving everything to the new one", "target", cfg.Archive.Destination())
			}
			if err := state.Save(); err != nil {
				log.Error("saving archive state", "err", err)
			}
		}
	}
	d, snaps, kept := newDrive(cfg, log, state, ds.gadget(), ds.runDir)
	// Keep serving the API if the drive can't start, so the problem can
	// be seen and fixed remotely.
	if err := d.Start(ctx); err != nil {
		log.Error("drive failed to start", "err", err)
		return
	}
	lib := clips.NewLibrary(clipSnapshots(snaps, kept))
	if kept != nil && !cfg.Archive.RecentAtHome {
		// minutes the car sat parked aren't drives; someone parked outside who
		// backs up Recent clips at home wants them, though
		lib.HideRecent(kept.IsParked)
	}
	api.Drive, api.Snapshots, api.Clips = d, snaps, lib
	if st, ok := api.Setup.(*setup.Setup); ok {
		api.Sizing = sizing{d, st}
	}
	// The Car page works before archiving is set up, so the setup
	// wizard can pair; keeping the car awake needs archiving.
	car := ds.car
	if car != nil {
		api.Car = car
	}
	if state == nil {
		return
	}
	a, err := newArchiver(cfg, lib, d, state, log, ds.runDir)
	if err != nil {
		log.Error("archiving disabled", "err", err)
		return
	}
	api.Archive = a
	go a.Run(ctx)
	if car != nil {
		k := &keepawake.KeepAwake{
			Car:      car,
			Interval: time.Duration(cfg.KeepAwake.Interval),
			Max:      time.Duration(cfg.KeepAwake.MaxDuration),
			Cooldown: time.Duration(cfg.KeepAwake.Cooldown),
			// also while updating: the car's sleep would cut the Pi's power
			Busy: func() bool { return a.Status().CatchingUp || ds.busy() },
			Log:  log,
		}
		api.KeepAwake = k
		if _, paired := car.Paired(); paired {
			log.Info("keeping the car awake while archiving what it recorded away", "vin", car.VIN(), "every", k.Interval, "max", k.Max)
		} else {
			log.Info("keep-awake is waiting for the car to be paired (Car page in the web UI)")
		}
		go k.Run(ctx)
	}
}

func (ds *driveStarter) gadget() drive.Gadget {
	if ds.noUSB {
		ds.log.Warn("not presenting the drives over USB (-no-usb)")
		return &noGadget{}
	}
	g := gadget.New(system.Exec{Log: ds.log})
	g.SerialConsole = ds.cfg.Drive.SerialConsole
	g.MaxPowerMA = ds.cfg.Drive.MaxPowerMA
	return g
}

func newDrive(cfg config.Config, log *slog.Logger, state *archive.State, g drive.Gadget, runDir string) (*drive.Drive, *snapshot.Manager, *store.Store) {
	runner := system.Exec{Log: log}
	snaps := snapshot.NewManager(cfg.Drive.Backingfiles, runner, log)
	snaps.MountRoot = filepath.Join(runDir, "snapshots")
	d := drive.New(cfg.Drive.Backingfiles, int64(cfg.Drive.CamSize),
		time.Duration(cfg.Snapshots.Interval), int64(cfg.Snapshots.MinFree),
		g, snaps, runner, log)
	d.KeepFreePercent = cfg.Drive.KeepFreePercent
	for name, x := range map[string]config.ExtraDriveConfig{drive.Music: cfg.Drive.Music, drive.LightShow: cfg.Drive.LightShow, drive.Boombox: cfg.Drive.Boombox} {
		if x.Size > 0 {
			extra, _ := drive.NewExtra(name, int64(x.Size), x.Source)
			d.Extras = append(d.Extras, extra)
		}
	}
	// in the order the car sees them
	slices.SortFunc(d.Extras, func(a, b drive.Extra) int { return drive.Order(a.Name) - drive.Order(b.Name) })
	if state != nil {
		d.Archived = state.Done
	}
	kept, err := store.Open(filepath.Join(cfg.Drive.Backingfiles, "clips"), log)
	if err != nil {
		// snapshots then keep the drives, until space runs low
		log.Error("clip store unavailable", "err", err)
		return d, snaps, nil
	}
	kept.Motion = clipMotion
	d.Store = kept
	return d, snaps, kept
}

// clipMotion reads from the gear and speed the car records in a clip whether
// it moved.
func clipMotion(ctx context.Context, r io.ReaderAt, size int64) (store.Motion, error) {
	res, err := telemetry.Extract(ctx, r, size)
	if err != nil || len(res.Samples) == 0 {
		return store.NoTelemetry, err
	}
	for _, s := range res.Samples {
		if s.Gear != gearPark || s.Speed > 0.3 {
			return store.Moving, nil
		}
	}
	return store.Parked, nil
}

// gearPark is Park in Tesla's dashcam schema.
const gearPark = 0

func newArchiver(cfg config.Config, lib *clips.Library, d *drive.Drive, st *archive.State, log *slog.Logger, runDir string) (*archive.Archiver, error) {
	target, err := archive.NewTarget(cfg.Archive, archiveOptions(log, runDir))
	if err != nil {
		return nil, err
	}
	a := archive.New(target, lib, d, st, log)
	a.Sentry, a.Saved, a.Recent = cfg.Archive.Sentry, cfg.Archive.Saved, cfg.Archive.Recent
	a.RecentAtHome = cfg.Archive.RecentAtHome
	a.HomeDelay = time.Duration(cfg.Archive.HomeDelay)
	a.CheckInterval = time.Duration(cfg.Archive.CheckInterval)
	a.RescanInterval = time.Duration(cfg.Archive.RescanInterval)
	a.OnResult = func(r archive.Result) { log.Info(r.Summary()) }
	if len(d.Extras) > 0 {
		a.SyncDrives = func(ctx context.Context) { d.SyncExtras(ctx, target.Mirror) }
	}
	log.Info("archiving enabled", "type", cfg.Archive.Type, "target", target)
	return a, nil
}

// archiveOptions are what archive targets need from the system.
func archiveOptions(log *slog.Logger, runDir string) archive.Options {
	return archive.Options{Runner: system.Exec{Log: log}, RunDir: runDir}
}

// clipSnapshots lists the snapshot manager's snapshots, and the clip store
// if there is one, for the clip library.
func clipSnapshots(m *snapshot.Manager, kept *store.Store) func() ([]clips.Snapshot, error) {
	return func() ([]clips.Snapshot, error) {
		snaps, err := m.List()
		if err != nil {
			return nil, err
		}
		out := make([]clips.Snapshot, len(snaps))
		for i, s := range snaps {
			out[i] = clips.Snapshot{ID: s.ID, Mount: s.Mount}
		}
		if kept != nil {
			out = append(out, clips.Snapshot{ID: store.ID, Mount: kept.Dir, Version: kept.Version()})
		}
		return out, nil
	}
}

// newEncryption sets up unlocking the encrypted drive. Its keys live next
// to the config, its recovery files on the boot partition.
func newEncryption(cfg config.Config, car *keepawake.Car, log *slog.Logger, configPath, bootConfig, runDir string) *crypt.Service {
	cryptDir := filepath.Join(filepath.Dir(configPath), "crypt")
	boot := "/boot/firmware"
	if bootConfig != "" {
		boot = filepath.Dir(bootConfig)
	}
	enc := &crypt.Service{
		Volume: &crypt.Volume{
			Device: cfg.Drive.Encryption.Device, KeyFile: filepath.Join(cryptDir, "device.key"),
			TempDir: runDir, Runner: system.Exec{Log: log},
		},
		Recovery:    crypt.Recovery{Dir: filepath.Join(boot, "glovebox-recovery"), RecipientFile: filepath.Join(cryptDir, "recovery.pub")},
		Mode:        cfg.Drive.Encryption.CarCheck,
		RequestFile: filepath.Join(cryptDir, "encrypt-requested"),
		ErrorFile:   filepath.Join(cryptDir, "encrypt-error"),
		Log:         log,
	}
	if car != nil {
		enc.Check = car.Verify
	}
	return enc
}

// encryptDrive turns the plain backingfiles partition into an encrypted
// one, erasing it, as asked for in the web UI.
func encryptDrive(ctx context.Context, cfg config.Config, enc *crypt.Service, log *slog.Logger, runDir string, noUSB bool) {
	log.Warn("encrypting the drive, as asked for in the web UI: everything on backingfiles is erased")
	runner := system.Exec{Log: log}
	conv := &crypt.Conversion{
		Mountpoint: cfg.Drive.Backingfiles, Fstab: "/etc/fstab",
		SnapshotRoot: filepath.Join(runDir, "snapshots"),
		Volume:       enc.Volume, Recovery: enc.Recovery, Runner: runner,
		Release: func(ctx context.Context) error {
			if noUSB {
				return nil
			}
			return gadget.New(runner).Disable(ctx)
		},
	}
	if err := conv.Run(ctx); err != nil {
		log.Error("encrypting the drive failed", "err", err)
		enc.RecordEncryptError(err)
		return
	}
	enc.Created()
	log.Info("the drive is encrypted", "device", enc.Volume.Device)
}

// swapHandler serves with whichever handler was set last.
type swapHandler struct{ h atomic.Pointer[http.Handler] }

func (s *swapHandler) Set(h http.Handler) { s.h.Store(&h) }

func (s *swapHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h := s.h.Load(); h != nil {
		(*h).ServeHTTP(w, r)
		return
	}
	http.Error(w, "starting", http.StatusServiceUnavailable)
}

// noGadget stands in for the USB gadget with -no-usb, for testing a second
// instance on a Pi whose real drive stays connected.
type noGadget struct {
	mu sync.Mutex
	st gadget.State
}

func (g *noGadget) Enable(_ context.Context, luns []gadget.LUN) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.st = gadget.State{Enabled: true, HostState: "not attached"}
	for _, l := range luns {
		g.st.Files = append(g.st.Files, l.File)
	}
	return nil
}

func (g *noGadget) Disable(context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.st = gadget.State{}
	return nil
}

func (g *noGadget) Eject(int) error        { return nil }
func (g *noGadget) Load(int, string) error { return nil }

func (g *noGadget) State() (gadget.State, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.st, nil
}

// sizing changes the size of the car's drive: the drive recreates it at the
// next start, and the size goes in the config so it sticks.
type sizing struct {
	d *drive.Drive
	s *setup.Setup
}

func (z sizing) Storage() (drive.Storage, error) { return z.d.Storage() }

func (z sizing) Resize(ctx context.Context, size int64) error {
	if err := z.d.RequestRecreate(size); err != nil {
		return err
	}
	if err := z.s.SetCamSize(size); err != nil {
		_ = z.d.CancelRecreate()
		return err
	}
	// keep the newest minutes of driving before the drive goes
	if err := z.d.SnapshotNow(ctx); err != nil {
		_ = z.d.CancelRecreate()
		return fmt.Errorf("taking a last snapshot: %w", err)
	}
	return nil
}

func (z sizing) CancelResize() error { return z.d.CancelRecreate() }

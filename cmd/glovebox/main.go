// Command glovebox is the service that runs on the Pi.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/lubaxe/glovebox/internal/archive"
	"github.com/lubaxe/glovebox/internal/clips"
	"github.com/lubaxe/glovebox/internal/config"
	"github.com/lubaxe/glovebox/internal/drive"
	"github.com/lubaxe/glovebox/internal/gadget"
	"github.com/lubaxe/glovebox/internal/keepawake"
	"github.com/lubaxe/glovebox/internal/logfile"
	"github.com/lubaxe/glovebox/internal/provision"
	"github.com/lubaxe/glovebox/internal/snapshot"
	"github.com/lubaxe/glovebox/internal/sysinfo"
	"github.com/lubaxe/glovebox/internal/system"
	"github.com/lubaxe/glovebox/internal/web"
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
	if cfg.Drive.Enabled {
		// On a freshly flashed card this creates the backingfiles partition.
		bf := &provision.Backingfiles{
			Mountpoint: cfg.Drive.Backingfiles, Fstab: "/etc/fstab",
			MinSize: 4 << 30, Runner: system.Exec{Log: log},
		}
		if created, err := bf.Ensure(ctx); err != nil {
			log.Error("backingfiles not available", "err", err)
		} else {
			if created {
				log.Info("created the backingfiles partition", "mountpoint", cfg.Drive.Backingfiles)
			}
			if err := logFile.Open(filepath.Join(cfg.Drive.Backingfiles, "glovebox.log")); err != nil {
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
		}
		d, snaps := newDrive(cfg, log, state)
		// Keep serving the API if the drive can't start, so the problem can
		// be seen and fixed remotely.
		if err := d.Start(ctx); err != nil {
			log.Error("drive failed to start", "err", err)
		} else {
			lib := clips.NewLibrary(clipSnapshots(snaps))
			api.Drive, api.Snapshots, api.Clips = d, snaps, lib
			if state != nil {
				if a, err := newArchiver(cfg, lib, d, state, log); err != nil {
					log.Error("archiving disabled", "err", err)
				} else {
					api.Archive = a
					go a.Run(ctx)
					if cfg.KeepAwake.Enabled {
						car := &keepawake.Car{
							Command: cfg.KeepAwake.Command, Dir: cfg.KeepAwake.KeyDir, ConfigVIN: cfg.KeepAwake.VIN,
							Runner: system.Exec{Log: log},
						}
						k := &keepawake.KeepAwake{
							Car:      car,
							Interval: time.Duration(cfg.KeepAwake.Interval),
							Max:      time.Duration(cfg.KeepAwake.MaxDuration),
							Cooldown: time.Duration(cfg.KeepAwake.Cooldown),
							Busy:     func() bool { return a.Status().CatchingUp },
							Log:      log,
						}
						api.Car, api.KeepAwake = car, k
						if _, paired := car.Paired(); paired {
							log.Info("keeping the car awake while archiving what it recorded away", "vin", car.VIN(), "every", k.Interval, "max", k.Max)
						} else {
							log.Info("keep-awake is waiting for the car to be paired (Car page in the web UI)")
						}
						go k.Run(ctx)
					}
				}
			}
		}
		// The gadget stays up on shutdown: the car keeps recording while the
		// service restarts.
	} else {
		log.Info("drive disabled in config")
	}

	srv := &http.Server{
		Addr:              cfg.Web.Listen,
		Handler:           api.Handler(),
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

func newDrive(cfg config.Config, log *slog.Logger, state *archive.State) (*drive.Drive, *snapshot.Manager) {
	runner := system.Exec{Log: log}
	g := gadget.New(runner)
	g.SerialConsole = cfg.Drive.SerialConsole
	g.MaxPowerMA = cfg.Drive.MaxPowerMA
	snaps := snapshot.NewManager(cfg.Drive.Backingfiles, runner, log)
	d := drive.New(cfg.Drive.Backingfiles, int64(cfg.Drive.CamSize),
		time.Duration(cfg.Snapshots.Interval), int64(cfg.Snapshots.MinFree),
		g, snaps, runner, log)
	d.KeepFreePercent = cfg.Drive.KeepFreePercent
	if state != nil {
		d.Archived = state.Done
	}
	return d, snaps
}

func newArchiver(cfg config.Config, lib *clips.Library, d *drive.Drive, st *archive.State, log *slog.Logger) (*archive.Archiver, error) {
	user, password, domain, err := archive.ReadCredentials(cfg.Archive.Credentials)
	if err != nil {
		return nil, fmt.Errorf("reading archive credentials: %w", err)
	}
	dialect, err := archive.ParseDialect(cfg.Archive.SMBVersion)
	if err != nil {
		return nil, err
	}
	target := &archive.SMB{
		Server: cfg.Archive.Server, Share: cfg.Archive.Share, Folder: cfg.Archive.Folder,
		User: user, Password: password, Domain: domain,
		Dialect: dialect, Parallel: cfg.Archive.Parallel,
	}
	a := archive.New(target, lib, d, st, log)
	a.Sentry, a.Saved, a.Recent = cfg.Archive.Sentry, cfg.Archive.Saved, cfg.Archive.Recent
	a.HomeDelay = time.Duration(cfg.Archive.HomeDelay)
	a.CheckInterval = time.Duration(cfg.Archive.CheckInterval)
	a.RescanInterval = time.Duration(cfg.Archive.RescanInterval)
	a.OnResult = func(r archive.Result) { log.Info(r.Summary()) }
	log.Info("archiving enabled", "target", target, "user", user, "smb", cfg.Archive.SMBVersion, "parallel", cfg.Archive.Parallel)
	return a, nil
}

// clipSnapshots lists the snapshot manager's snapshots for the clip library.
func clipSnapshots(m *snapshot.Manager) func() ([]clips.Snapshot, error) {
	return func() ([]clips.Snapshot, error) {
		snaps, err := m.List()
		if err != nil {
			return nil, err
		}
		out := make([]clips.Snapshot, len(snaps))
		for i, s := range snaps {
			out[i] = clips.Snapshot{ID: s.ID, Mount: s.Mount}
		}
		return out, nil
	}
}

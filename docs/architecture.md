# Glovebox architecture

Glovebox is a single Go service (`glovebox`) on a minimal Raspberry Pi OS image. It replaced the original bash implementation (`run/`, `setup/`), which was removed once the image ran the Go service.

## Target hardware

- Raspberry Pi Zero 2 W: 4× Cortex-A53, 512 MB RAM (about 415 MB usable), 2.4 GHz Wi-Fi only, one USB OTG port
- 64-bit Raspberry Pi OS (Debian 13 trixie), built by `pi-gen-sources`
- A high-endurance microSD card holds the OS, the backing files and the snapshots

Budget: the service should stay under ~50 MB RSS in normal operation.

## How footage gets from the car to the NAS

```
car ──USB──▶ gadget (configfs mass storage)
                 │ exposes
                 ▼
     /backingfiles/cam_disk.bin   (exFAT image, the car's "USB stick", lives on XFS)
                 │ cp --reflink (instant copy-on-write)
                 ▼
     /backingfiles/snapshots/snap-NNNNNN/snap.bin
                 │ loop device, fsck -p on the COPY, mount read-only
                 ▼
     clip index (SavedClips / SentryClips / RecentClips / TrackMode)
                 │ when the NAS is reachable (car is home)
                 ▼
     NAS (SMB/NFS/rsync)  ── notifications ──▶ phone
```

Rules that make this safe:

1. **The Pi never mounts or writes `cam_disk.bin` while the car is connected.** The car caches the filesystem and assumes it owns the disk; a second writer corrupts it.
2. **All reading happens on snapshots.** A snapshot is crash-consistent (like pulling the power at that instant), so it gets `fsck -p` before mounting. The live image is never repaired while attached.
3. **Clips being written at snapshot time may be truncated** in that snapshot; the next snapshot has them complete. Archiving skips suspiciously short files and picks them up later.
4. **Snapshots are the history.** The car keeps one hour of RecentClips; old snapshots keep more. Free space is managed by deleting the oldest snapshots, never by touching the live image.
5. **Archiving is idempotent and resumable.** Each archived file is recorded (path, size, snapshot) so a power cut mid-upload just continues next time.

## Packages

```
cmd/glovebox/          main: config, logging, starts the modules, HTTP server, shutdown
internal/config/     TOML config (/etc/glovebox/glovebox.toml), validation, defaults
internal/sysinfo/    uptime, memory, disk, temperature, Wi-Fi (from /proc and /sys)
internal/storage/    backingfiles partition (XFS), disk images (create, size, fsck)
internal/gadget/     USB mass storage gadget via configfs: attach/detach images
internal/snapshot/   reflink snapshots, mount/unmount, retention, free-space manager
internal/clips/      index of clips and events across snapshots
internal/archive/    NAS reachability, upload queue, backends (SMB/NFS/rsync)
internal/awake/      keep the car awake while archiving (BLE / webhook)
internal/notify/     push notifications (ntfy, Pushover, webhook, ...)
internal/web/        JSON API + embedded web UI
```

Everything that touches the system (`mount`, `losetup`, configfs, `fsck`) goes through small interfaces so the logic can be unit-tested on any machine; only the thin adapters need a real Pi.

## Milestones

1. **Skeleton**: service, config, logging, `/api/status`, systemd unit, cross-compile and deploy to a Pi, CI.
2. **Drive + snapshots**: create the backing files, present the cam disk to a USB host, take/mount/release snapshots. Testable with the Pi plugged into a PC standing in for the car.
3. **Clip index + web UI**: browse events, play clips from snapshots.
4. **Archiving**: automatic upload to the NAS when home, resumable, Sentry/Saved first.
5. **Keep-awake + notifications**.
6. **Image**: minimal image running the Go service; first-boot setup from `glovebox.toml` on the boot partition (`internal/provision`): Wi-Fi, SSH key, NAS login and the backingfiles partition. The bash scripts are deleted.

## Development

- Cross-compile on any machine: `GOOS=linux GOARCH=arm64 go build ./cmd/glovebox`
- `scripts/deploy.ps1` builds, copies the binary to a Pi over SSH and restarts the service.
- A PC can stand in for the car: plug the Pi's **USB** port into it and the cam disk shows up as a USB drive labelled `CAM`. With `serial_console = true` the same cable also gives a serial console (a COM port on Windows). The gadget needs the USB controller to itself, so don't load `g_serial` from `cmdline.txt`.
- Restarting the service leaves the drive connected; it only checks the live image with fsck at boot, before the car can see it.

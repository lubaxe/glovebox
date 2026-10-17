# Glovebox architecture

Glovebox is a single Go service (`glovebox`) on a minimal Raspberry Pi OS image. Everything it does, from presenting the car's drive to the setup wizard, runs in that one process.

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
                 │ when the target is reachable (car is home)
                 ▼
     NAS or cloud (SMB / NFS / rsync / rclone)
```

Rules that make this safe:

1. **The Pi never mounts or writes `cam_disk.bin` while the car is connected.** The car caches the filesystem and assumes it owns the disk; a second writer corrupts it.
2. **All reading happens on snapshots.** A snapshot is crash-consistent (like pulling the power at that instant), so it gets `fsck -p` before mounting. The live image is never repaired while attached.
3. **Clips being written at snapshot time may be truncated** in that snapshot; the next snapshot has them complete. Archiving records sizes and uploads a file again once it has grown.
4. **Snapshots are the history.** The car keeps one hour of RecentClips; old snapshots keep more. Free space is managed by deleting the oldest snapshots, never by touching the live image.
5. **Archiving is idempotent and resumable.** Each archived file is recorded (path and size, per target) so a power cut mid-upload just continues next time.
6. **Extra drives (music, light show, Boombox) are only changed in a copy.** A sync updates a reflink clone of the image and swaps it in, ejecting just that drive, only when something changed.

## Packages

```
cmd/glovebox/        main: config, logging, starts the modules, HTTP server, shutdown
internal/config/     TOML config (/etc/glovebox/glovebox.toml), validation, defaults
internal/provision/  first boot: the backingfiles partition, the boot partition's glovebox.toml
internal/wifi/       Wi-Fi through NetworkManager: join, scan, the setup network (ap0)
internal/setup/      the setup page and wizard: read and save settings, connection test, SSH key, rclone.conf
internal/sysinfo/    uptime, memory, disk, temperature, Wi-Fi (from /proc and /sys)
internal/storage/    disk images (create, fsck, trim), reflink clones, free space
internal/loopdev/    loop devices for disk images
internal/gadget/     USB mass storage gadget via configfs: drives, eject and load
internal/drive/      the car's drives: cam disk, extra drives and their sync, snapshot schedule, cleanup
internal/snapshot/   reflink snapshots, mount/unmount, retention
internal/clips/      index of clips and events across snapshots
internal/archive/    reachability, upload queue and state, targets (SMB/NFS/rsync/rclone), mirroring folders
internal/keepawake/  keep the car awake over Bluetooth while archiving (tesla-control)
internal/mp4fix/     serve Tesla's MP4s so Chromium can play them
internal/logfile/    a log on backingfiles that survives power cuts
internal/system/     running commands and background processes, with fakes for tests
internal/web/        JSON API + embedded web UI (setup.js holds the setup page and wizard)
```

Everything that touches the system (`mount`, `losetup`, configfs, `fsck`, `nmcli`, `rsync`, `rclone`) goes through small interfaces so the logic can be unit-tested on any machine; only the thin adapters need a real Pi.

## Done and next

Done: the drive and snapshots, the clip index and web UI, archiving to SMB, NFS, rsync and rclone, keep-awake, the minimal image, extra drives, the setup page, the setup network and the first-run wizard.

Next: notifications when a backup finishes, and a read-only root filesystem for more resilience to power cuts.

## Development

- Cross-compile on any machine: `GOOS=linux GOARCH=arm64 go build ./cmd/glovebox`
- `scripts/deploy.ps1` builds, copies the binary to a Pi over SSH and restarts the service. It doesn't install packages; a Pi set up from an older image may lack ones newer versions need (see `pi-gen-sources/00-glovebox/00-packages-nr`).
- A PC can stand in for the car: plug the Pi's **USB** port into it and the cam disk shows up as a USB drive labelled `CAM`. With `serial_console = true` the same cable also gives a serial console (a COM port on Windows). The gadget needs the USB controller to itself, so don't load `g_serial` from `cmdline.txt`.
- Restarting the service leaves the drive connected; it only checks the live image with fsck at boot, before the car can see it.
- `go run ./tools/uidemo -firstrun -extras` serves the web UI on sample footage, as a Pi on its setup network with extra drives.

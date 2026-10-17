# How Glovebox works

This is the detailed version. For setup, see the [README](../README.md).

```
car ──USB──▶ Glovebox presents /backingfiles/cam_disk.bin as a USB drive
                 │ every 58 min, when space runs low, and before each backup
                 ▼
     copy-on-write snapshot ─▶ fsck the copy ─▶ mount read-only
                 │
                 ├──▶ clip index ─▶ web UI (browse and play)
                 └──▶ NAS upload when home (Sentry, then Saved, then Recent, newest first)
```

**The car's drive is never touched while the car can see it.** The car caches the filesystem and assumes nothing else writes to it, so Glovebox reads footage only from snapshots. Snapshots are instant `cp --reflink` clones on an XFS filesystem: they take no extra space until the car changes the live drive.

## First boot

On a freshly flashed card Glovebox:

- applies `glovebox.toml` from the boot partition: Wi-Fi, hostname, SSH key and NAS login. Passwords are moved to root-only files, the stored config keeps no secrets, and `glovebox-status.txt` on the boot partition says what was done (or what was wrong with the file).
- creates the `backingfiles` partition (XFS with reflink) in the card's free space. If that's interrupted, the next start finishes the job.
- creates the car's drive inside it and presents it over USB.

To change settings later, put a new `glovebox.toml` on the boot partition and restart the Pi.

## Drive and snapshots

- At startup, before the car can see the drive, Glovebox creates the cam disk if it's missing (exFAT, labelled `CAM`, with a `TeslaCam` folder) or repairs it with `fsck`. The car cuts power without unmounting, so errors are common.
- The drive is presented over USB using the kernel's configfs gadget. Optionally the same cable also gives a serial login console.
- The car's drive takes all of `/backingfiles` except `snapshots.min_free` (4 GB): about 52 GB on a 64 GB card. Set `drive.cam_size` for a fixed size.
- A snapshot is taken every `snapshots.interval` (default 58 minutes, just under the car's one-hour RecentClips buffer, so no minute of driving falls between two snapshots). Each snapshot is repaired with `fsck` (the copy, never the live drive) and mounted read-only. Snapshots with nothing new are skipped.
- A snapshot taken while the car is mid-write holds a shorter, still readable copy of that clip; the next snapshot has it complete.
- Snapshots share blocks with the drive, so they only use space for what the car overwrites after them. After each snapshot the oldest ones are deleted until `snapshots.min_free` is free again. Free space is checked every 30 seconds, and once the car has used half the reserve a snapshot is taken early so older ones can go. While driving with a full drive that's about every 6 minutes on HW4.
- The car sees its drive about **16 seconds** after power-on: the service starts before the network and does its own work (fsck, cleanup, presenting the drive) in about a second.

## Clip index and web UI

- The index covers Sentry, Saved and Recent clips across every snapshot. Each clip is served from its most complete copy, and clips the car has already deleted stay available from older snapshots.
- The web UI (`http://<pi>:8080`) has an overview, Sentry, Saved and Recent pages with thumbnails, reasons, city and map links, a player that shows all cameras in sync (clip strip, scrubbing, 1×/2×/4× speed, enlarge a camera, keyboard shortcuts, downloads), and the Car page for Bluetooth pairing.
- Tesla's MP4 files don't play in Chromium-based browsers because of their `ctts` box; Glovebox rewrites that box name on the fly while serving, without changing the files.
- Only indexed clips, thumbnails and `event.json` files can be served; the rest of the drive and the Pi aren't reachable through the media route.

## Encrypted clips

Cars with **Encrypt Dashcam Recordings** on (Controls → Safety) record into `TeslaCam/EncryptedClips/`, with the usual `RecentClips`, `SavedClips` and `SentryClips` inside. Only the car, or [dashcam.tesla.com](https://dashcam.tesla.com) signed in to the owner's Tesla account, can decrypt them.

Glovebox indexes them like any other clips but marks them encrypted: the web UI shows them with a lock and, instead of a player, offers the whole event or drive as one zip and opens dashcam.tesla.com. That site only accepts files from the computer it runs on, so they have to be downloaded and dropped on it; there's no way to hand them over directly. The backup copies them as they are to `EncryptedClips/` on the share.

## Backing up to a NAS

- Glovebox checks every 30 seconds whether the NAS answers on port 445. Once it has for 20 seconds, the car is home: Glovebox takes a fresh snapshot and uploads what isn't backed up yet, Sentry first, then Saved, newest event first, so the important footage gets out before the car sleeps and cuts the Pi's power. While home it looks for new clips every 10 minutes.
- Uploads use a built-in SMB client, not a kernel mount (which can hang when Wi-Fi drops). Each file is written under a temporary `.partial` name, its size checked, then renamed, so a file on the NAS is always complete. Each attempt uses its own temporary name, because a NAS can keep a file locked for hours after the car cuts the power mid-upload.
- `/backingfiles/archive-state.json` records each uploaded file and its size: nothing is uploaded twice, an interrupted run resumes, and a clip that was first caught mid-recording is uploaded again once complete. It's written so a power cut can't corrupt it; if it's ever unreadable, Glovebox starts over and skips files already on the share.
- RecentClips (all driving footage) can be backed up too with `archive.recent = true`. It's off by default: an hour of HW4 driving is roughly 15–20 GB.
- Measured on a Pi Zero 2 W: about 2.5–3.5 MB/s over its 2.4 GHz Wi-Fi, so an hour of HW4 driving takes 1.5–2 hours to copy.

## Keeping the car awake

A sleeping car cuts power to its USB ports, which would stop an upload partway through. Once a key is paired (on the web UI's Car page), Glovebox tells the car to stay awake over Bluetooth while it uploads what the car recorded away from home, using Tesla's [`tesla-control`](https://github.com/teslamotors/vehicle-command) (included in the image).

- The key is paired with the **Charging Manager** role. Tested on a 2026 Model 3: it can wake the car and read its charge state, but the car refuses unlock, remote start, flashing the lights and opening the charge port.
- Only the first upload after the car gets home keeps it awake. An awake car keeps recording, faster than the Wi-Fi can upload, so what it records while parked is uploaded whenever it happens to be awake but never holds it awake.
- As a backstop, the car is kept awake for at most `keep_awake.max_duration` (1 hour) at a time, then left alone for `keep_awake.cooldown` (6 hours).
- The key, the VIN and the pairing state live in `/etc/glovebox/ble/`. The key shows up in the car under Controls → Locks → Keys, where it can be removed.

Pairing from the command line instead of the web UI:

```sh
sudo tesla-control -ble -vin <VIN> add-key-request /etc/glovebox/ble/public.pem charging_manager cloud_key
# lay a key card on the phone charging pad and confirm on the car's screen, then:
sudo tesla-control -ble -vin <VIN> -key-file /etc/glovebox/ble/private.pem wake
```

## Keeping room on the car's drive

The car never deletes Sentry or Saved events itself; when its drive fills, it stops saving new ones. At startup, before presenting the drive, Glovebox:

- removes files `fsck` recovered from deleted data
- while less than `drive.keep_free_percent` (default 25%) of the drive is free, deletes Sentry and Saved files that the backup state shows are on the NAS at full size, oldest event first. Events the car recorded encrypted (`TeslaCam/EncryptedClips`, with "Encrypt Dashcam Recordings" on) count the same way. **Files that aren't backed up are never deleted.**
- runs `fstrim`, so space freed on the drive is freed in the backing file too

Deleted events stay browsable in the web UI from older snapshots until those are cleaned up.

## Logs

The journal is kept in memory to spare the SD card, so it's lost when the car cuts the power. A copy of Glovebox's own log is kept in `/backingfiles/glovebox.log` (2 MB, then rotated to `glovebox.log.1`), written so the last lines before a power cut survive.

## API

| Route | |
|---|---|
| `GET /api/status` | Pi health: uptime, memory, temperature, Wi-Fi, disk |
| `GET /api/drive` | USB gadget state (is the car connected), snapshot count, last cleanup |
| `GET /api/snapshots`, `POST /api/snapshots`, `DELETE /api/snapshots/{id}` | List, take, delete snapshots |
| `GET /api/snapshots/{id}/files` | Files in a snapshot |
| `GET /api/events?kind=sentry\|saved`, `GET /api/events/{kind}/{id}[?encrypted=1]` | Events and their clips |
| `GET /api/recent` | Recent clips |
| `GET /media/{snapshot}/{path}` | A clip, thumbnail or event.json (supports Range) |
| `GET /api/events/{kind}/{id}/zip[?encrypted=1]`, `GET /api/recent/zip?from=…&to=…[&encrypted=1]` | Every camera of an event, or of the recent clips between two start times, as one zip |
| `GET /api/archive`, `POST /api/archive/run` | Backup status, start a run now |
| `GET /api/car`, `PUT /api/car/vin`, `POST /api/car/pair`, `POST /api/car/check`, `POST /api/car/forget`, `GET /api/car/state` | Bluetooth pairing and keep-awake. Changes must be sent as JSON. |

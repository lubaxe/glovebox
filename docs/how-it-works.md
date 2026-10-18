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

- creates the `backingfiles` partition (XFS with reflink) in the card's free space. If that's interrupted, the next start finishes the job.
- creates the car's drive inside it and presents it over USB.
- if the Pi isn't on Wi-Fi within two minutes, offers the setup network (below), where a wizard sets up everything else.

Settings live in `/etc/glovebox/glovebox.toml`, written by the setup page; secrets (the SMB password, Glovebox's SSH key, `rclone.conf`) are in root-only files in `/etc/glovebox`.

The boot partition's `glovebox.toml` is optional, for setting up without the wizard. If it sets anything, it's applied at startup: Wi-Fi, hostname, SSH key and any other setting; an `rclone.conf` next to it becomes `/etc/glovebox/rclone.conf`. Passwords are moved to root-only files, the stored config keeps no secrets, and the file is replaced by `glovebox-status.txt` saying what was done (or what was wrong with it).

## Setup network and first-run wizard

Until the Pi has joined Wi-Fi once (a fresh card without `wifi_ssid`, or a mistyped password), Glovebox offers its own open network, **Glovebox-XXXX** (the end of the Pi's Wi-Fi address), after giving Wi-Fi two minutes to come up. It's a second interface, `ap0`, next to the Wi-Fi client `wlan0`, run by NetworkManager with the Pi at `192.168.66.1` handing out addresses. Its DNS answers every name with the Pi, and Glovebox serves the web app on port 80 there, so phones open it as the network's sign-in page.

- The first visit to the web app, on that network or at home, starts the wizard: Wi-Fi (networks in range, password, country), where backups go (with the connection test), extra drives, and the car's VIN and pairing. It can be run again any time with **Guided setup** on the Settings page, as a walk-through for changing settings: what's set up already (Wi-Fi, backups, extra drives, pairing) is shown folded, with a button to change just that part. On home Wi-Fi it never involves the setup network.
- Joining Wi-Fi happens while the setup network stays up, so the phone can carry on. The radio uses one channel for both, so the phone may drop off for a moment; the page picks up the result when it's back.
- Once the Pi has joined Wi-Fi, the setup network stays until the wizard is finished or skipped, for at most 30 minutes, and then never comes back (`/etc/glovebox/wifi-joined`, `/etc/glovebox/setup-done`). A Pi that already backs up is counted as set up.
- An empty file named `glovebox-setup` on the boot partition brings the setup network back (e.g. after moving house): Glovebox forgets that it joined Wi-Fi and was set up, and deletes the file.
- `system.setup_network = false` turns all this off.
- Avahi announces the Pi as `<hostname>.local`, e.g. `http://glovebox.local:8080`.

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
- The web UI (`http://<pi>:8080`) has an overview, Sentry, Saved and Recent pages with thumbnails, reasons, city and map links, a player that shows all cameras in sync (clip strip, scrubbing, 1×/2×/4× speed, enlarge a camera, keyboard shortcuts, downloads), the Car page for Bluetooth pairing, and the Settings page for the backup target and extra drives.
- Tesla's MP4 files don't play in Chromium-based browsers because of their `ctts` box; Glovebox rewrites that box name on the fly while serving, without changing the files.
- Only indexed clips, thumbnails and `event.json` files can be served; the rest of the drive and the Pi aren't reachable through the media route.

## Encrypted clips

Cars with **Encrypt Dashcam Recordings** on (Controls → Safety) record into `TeslaCam/EncryptedClips/`, with the usual `RecentClips`, `SavedClips` and `SentryClips` inside. Only the car, or [dashcam.tesla.com](https://dashcam.tesla.com) signed in to the owner's Tesla account, can decrypt them.

Glovebox indexes them like any other clips but marks them encrypted: the web UI shows them with a lock and, instead of a player, offers the whole event or drive as one zip and opens dashcam.tesla.com. That site only accepts files from the computer it runs on, so they have to be downloaded and dropped on it; there's no way to hand them over directly. The backup copies them as they are to `EncryptedClips/` on the share.

## Backing up to a NAS

The backup target is an SMB share, an NFS export, a folder on a server reached with rsync over SSH, or any rclone remote (`archive.type`). The web UI's Settings page configures it and tests it: it reaches the server, logs in, writes a small `.glovebox-test` file, reads its size back and deletes it.

- Glovebox checks every 30 seconds whether the target answers: the NAS's port (445 for SMB, 2049 for NFS, 22 for SSH), or `archive.check_address` if set. rclone remotes can be anywhere, so they need that address, e.g. the home router. Once it has for 20 seconds, the car is home: Glovebox takes a fresh snapshot and uploads what isn't backed up yet, Sentry first, then Saved, newest event first, so the important footage gets out before the car sleeps and cuts the Pi's power. While home it looks for new clips every 10 minutes.
- **SMB** uploads use a built-in client, not a kernel mount (which can hang when Wi-Fi drops). Each file is written under a temporary `.partial` name, its size checked, then renamed, so a file on the NAS is always complete. Each attempt uses its own temporary name, because a NAS can keep a file locked for hours after the car cuts the power mid-upload. Glovebox writes the temporary name to its archive state before uploading. The next backup run deletes any temporary file the power cut left behind, retrying for up to 10 runs while the NAS still holds it open. NFS uploads work the same way.
- **NFS** exports are mounted only while backing up, with `soft` so a NAS that goes away makes writes fail instead of hang. Files are written and renamed the same way.
- **rsync** keeps one SSH connection open per run and shares it between files, since an SSH handshake costs the Pi Zero 2 W about a second. rsync writes to a temporary name and renames it once complete. Glovebox logs in with its own key (`/etc/glovebox/ssh/id_ed25519`, made on the Settings page) and remembers the server's key from the first connection.
- **rclone** runs as a server on localhost (`rclone rcd`) for the duration of a run and gets one request per file, so it starts and logs in to the remote once. Its config is `/etc/glovebox/rclone.conf`.
- `/backingfiles/archive-state.json` records each uploaded file and its size: nothing is uploaded twice, an interrupted run resumes, and a clip that was first caught mid-recording is uploaded again once complete. It's written so a power cut can't corrupt it; if it's ever unreadable, Glovebox starts over and skips files already on the share.
- RecentClips (all driving footage) can be backed up too: tick it in setup (`archive.recent`). It's off by default: an hour of HW4 driving is roughly 15–20 GB.
- Measured on a Pi Zero 2 W: about 2.5–3.5 MB/s over its 2.4 GHz Wi-Fi, so an hour of HW4 driving takes 1.5–2 hours to copy.

## Music, light show and Boombox drives

Besides the dashcam drive (`CAM`), the car can get up to three more: `MUSIC`, `LIGHTSHOW` and `BOOMBOX` (`[drive.music]`, `[drive.lightshow]`, `[drive.boombox]`), each a sparse exFAT image in `/backingfiles` that's off while its size is 0. They're presented as more drives of the same USB device, after `CAM`.

- Each mirrors a folder on the backup target (`source`). After the first backup run once the car is home, and after a run started from the web UI, Glovebox updates each drive: it makes a copy-on-write clone of the image, brings the clone up to date (new and changed files copied, files gone from the source deleted, Mac and Windows litter skipped), and compares the files before and after.
- Only if something changed is the new version swapped in: that one drive is ejected (`forced_eject`), the clone renamed over the image, and the drive loaded again. The car sees that drive unplugged and plugged back in; the dashcam drive stays connected throughout. If the copy fails partway, the drive is left as it was.
- The light show's files go in the drive's `LightShow` folder and the Boombox sounds in its `Boombox` folder, where the car looks for them; music fills the whole drive.
- The dashcam drive is sized to leave room for these drives as they fill. Changing a drive's size recreates it, and its files come back with the next sync. Adding or removing a drive reconnects all of them when Glovebox restarts, which the car takes like the Pi restarting.

## Keeping the car awake

A sleeping car cuts power to its USB ports, which would stop an upload partway through. Once a key is paired (on the web UI's Car page), Glovebox tells the car to stay awake over Bluetooth while it uploads what the car recorded away from home, using Tesla's [`tesla-control`](https://github.com/teslamotors/vehicle-command) (included in the image).

- The key is paired with the **Charging Manager** role. Tested on a 2026 Model 3: it can wake the car and read its charge state, but the car refuses unlock, remote start, flashing the lights and opening the charge port.
- Only the first upload after the car gets home keeps it awake. An awake car keeps recording, faster than the Wi-Fi can upload, so what it records while parked is uploaded whenever it happens to be awake but never holds it awake.
- It's also kept awake while a software update installs, so the Pi doesn't lose power partway through.
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
| `GET /api/drive` | USB gadget state (is the car connected), snapshot count, last cleanup, extra drives and their last sync |
| `GET /api/snapshots`, `POST /api/snapshots`, `DELETE /api/snapshots/{id}` | List, take, delete snapshots |
| `GET /api/snapshots/{id}/files` | Files in a snapshot |
| `GET /api/events?kind=sentry\|saved`, `GET /api/events/{kind}/{id}[?encrypted=1]` | Events and their clips |
| `GET /api/recent` | Recent clips |
| `GET /media/{snapshot}/{path}` | A clip, thumbnail or event.json (supports Range) |
| `GET /api/events/{kind}/{id}/zip[?encrypted=1]`, `GET /api/recent/zip?from=…&to=…[&encrypted=1]` | Every camera of an event, or of the recent clips between two start times, as one zip |
| `GET /api/archive`, `POST /api/archive/run` | Backup status, start a run now |
| `GET /api/wifi`, `GET /api/wifi/networks`, `POST /api/wifi/join`, `POST /api/setup/finish` | First-run wizard: Wi-Fi state and the setup network, networks in range, join one, finish (or skip) the wizard. Changes must be sent as JSON. |
| `GET /api/setup`, `PUT /api/setup`, `POST /api/setup/test`, `POST /api/setup/ssh-key`, `PUT /api/setup/rclone-config` | The Settings page: read and save the backup target and extra drives (then Glovebox restarts), test a connection, make the SSH key for rsync, store an rclone.conf. Changes must be sent as JSON. |
| `GET /api/update`, `POST /api/update/check`, `POST /api/update/install` | Software update on the Settings page: the running and latest versions (`?check=1` asks GitHub when the last check is over six hours old), check now, and install the latest release, which restarts Glovebox. |
| `GET /api/car`, `PUT /api/car/vin`, `POST /api/car/pair`, `POST /api/car/check`, `POST /api/car/forget`, `GET /api/car/state` | Bluetooth pairing and keep-awake. Changes must be sent as JSON. |

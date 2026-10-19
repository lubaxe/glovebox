# How Glovebox works

This is the detailed version. For setup, see the [README](../README.md).

```
car ──USB──▶ Glovebox presents /backingfiles/cam_disk.bin as a USB drive
                 │ every 15 min, when space runs low, and before each backup
                 ▼
     copy-on-write snapshot ─▶ fsck the copy ─▶ mount read-only
                 │
                 ├──▶ clip store (each new minute of driving, cloned out on its own)
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
- The car's drive takes half of `/backingfiles`, at least 32 GB and at most 64 GB (never more than fits with `snapshots.min_free` to spare): 32 GB on a 64 GB card. The rest keeps drives until the car is home (below). Set `drive.cam_size` for a fixed size. A drive made before this default keeps its size.
- A snapshot is taken every `snapshots.interval` (default 15 minutes; it must stay under the car's one-hour RecentClips buffer, so no minute of driving falls between two snapshots; a config with the old default, 58 minutes, is read as 15). Each snapshot is repaired with `fsck` (the copy, never the live drive) and mounted read-only. Snapshots with nothing new are skipped.
- A snapshot taken while the car is mid-write holds a shorter, still readable copy of that clip; the next snapshot has it complete.
- Snapshots share blocks with the drive, so they only use space for what the car overwrites after them. That's everything it records, once its drive has been written full, about 16 GB an hour on HW4, driving or parked on Sentry, so snapshots aren't kept for long. Instead:
- **The clip store.** After each snapshot, every new Recent clip from a minute of driving is cloned out of the snapshot image on its own into `/backingfiles/clips/` (an XFS reflink of the clip's byte range, found by reading the exFAT directory; the car writes each clip in one run). A kept clip costs nothing until the car writes over it, and then exactly its own size. Then every snapshot but the newest is released.
- **Parked minutes are left out.** The car records its gear and speed in each clip while it's on, and nothing while parked on Sentry or sitting awake. Once a clip has shown that the car records them, a minute that's all Park, or has none, isn't kept: a day at work with Sentry on costs the Sentry events, which the car saves in `SentryClips` anyway, not eight hours of parking lot. Encrypted clips (the car's own encryption) can't be read, so they're all kept.
- Free space is checked every 30 seconds, and once the car has used half of `snapshots.min_free` (4 GB) a snapshot is taken early so the older one can go. If space is still short, kept clips are deleted, backed-up ones first, then the oldest; minutes of driving deleted before they were backed up are counted, logged and shown on the home page.
- **Settings → Storage** shows how the card is used, as one colored bar: the car's recent clips, Sentry and saved clips, the space it records over, extra drives, kept drives (backed up or not yet), and free space. A slider picks how many hours of driving to keep, using your car's own recording rate measured from its clips, and shows the card as it would be. Applying it recreates the car's drive at the new size: only once everything is backed up, after typing ERASE, at the next start. Kept drives stay; the car's drive starts empty.
- The car sees its drive about **16 seconds** after power-on: the service starts before the network and does its own work (fsck, cleanup, presenting the drive) in about a second.

## Clip index and web UI

- The index covers Sentry, Saved and Recent clips across the snapshots and the clip store. Each clip is served from its most complete copy, and drives the car has already deleted stay available from the clip store.
- The web UI (`http://<pi>:8080`) has an overview, Sentry, Saved and Recent pages with thumbnails, reasons, city and map links, a player that shows all cameras in sync (clip strip, scrubbing, 1×/2×/4× speed, enlarge a camera, keyboard shortcuts, downloads), the Car page for Bluetooth pairing, and the Settings page for the backup target and extra drives.
- Tesla's MP4 files don't play in Chromium-based browsers because of their `ctts` box; Glovebox rewrites that box name on the fly while serving, without changing the files.
- Only indexed clips, thumbnails and `event.json` files can be served; the rest of the drive and the Pi aren't reachable through the media route.

## Encrypted clips

Cars with **Encrypt Dashcam Recordings** on (Controls → Safety) record into `TeslaCam/EncryptedClips/`, with the usual `RecentClips`, `SavedClips` and `SentryClips` inside. Only the car, or [dashcam.tesla.com](https://dashcam.tesla.com) signed in to the owner's Tesla account, can decrypt them.

Glovebox indexes them like any other clips but marks them encrypted: the web UI shows them with a lock and, instead of a player, offers the whole event or drive as one zip and opens dashcam.tesla.com. That site only accepts files from the computer it runs on, so they have to be downloaded and dropped on it; there's no way to hand them over directly. The backup copies them as they are to `EncryptedClips/` on the share.

## Encrypted on the Pi

Instead of the car's encryption, Glovebox can encrypt the footage on the Pi with a key that belongs to you. Clips stay plain for the car's own Dashcam viewer, Glovebox's player and the backup, but the card on its own can't be read. Turn it on in Settings → **Security**, after turning off Encrypt Dashcam Recordings in the car. The three choices (nothing, Glovebox, Tesla) are compared in the [README](../README.md#encryption).

1. **Your key.** Glovebox makes an [age](https://age-encryption.org) key pair in your browser and gives you the key file to keep; only its public half goes to the Pi. You can use a key of your own (`age-keygen`) instead.
2. **Encrypt the drive.** This needs your key, the car paired on the Car page, and everything backed up, because it **erases** the backingfiles partition: the car's drive, snapshots and log. Glovebox restarts, makes the partition a LUKS2 volume (Adiantum, which the Pi Zero 2 W runs at over 100 MB/s without AES instructions), and the car gets a fresh, empty drive a minute or two later.

How it opens:

- The volume opens with a random device key kept on the Pi (`/etc/glovebox/crypt/device.key`), but only once Glovebox has seen **its own car**: at every start it pings the car over Bluetooth with the paired key, and the car's reply is only accepted if it's authenticated with the session key that only that car and that key can make. With the car awake that takes about 2 seconds (9 on the first Bluetooth connection). It tries again until the car answers, quickly at first and then every 30 seconds; until then the car has no drive and can't record, and the web UI shows the drive as locked, with the attempts and the last error.
- Once open, it stays open until the power is cut. Restarting Glovebox (new settings, an update) doesn't check again.
- **Unlock with your key** on the locked page opens it without the car. The volume's master key is sealed to your public key in `glovebox-recovery/` on the boot partition, with a header backup and a README that shows how to open the card on a Linux PC.
- `drive.encryption.car_check = "audit"` checks for the car and logs the result but opens the drive either way, for trying it out in the car.
- Your key can be replaced while the drive is unlocked; the master key is sealed again to the new one. While it's locked it can't, so nobody on the network can swap in their own key and unlock with it.
- The card's third partition is labelled `glovebox-crypt` and opens as `/dev/mapper/backingfiles`; Glovebox mounts it itself, so it has no line in `/etc/fstab`. Its key file, your public key and the request to encrypt are in `/etc/glovebox/crypt/`.

What it doesn't do: the device key is on the Pi, so someone who takes the whole Pi and knows where to look can decrypt the card without the car. The car check stops someone powering up a stolen Pi and browsing the footage; it isn't a lock in hardware, which the Pi Zero 2 W doesn't have. While the drive is open, anyone who can reach the web page can watch the footage, as on an unencrypted drive. The NAS copy isn't encrypted.

## Backing up to a NAS

The backup target is an SMB share, an NFS export, a folder on a server reached with rsync over SSH, or any rclone remote (`archive.type`). The web UI's Settings page configures it and tests it: it reaches the server, logs in, writes a small `.glovebox-test` file, reads its size back and deletes it.

- Glovebox checks every 30 seconds whether the target answers: the NAS's port (445 for SMB, 2049 for NFS, 22 for SSH), or `archive.check_address` if set. rclone remotes can be anywhere, so they need that address, e.g. the home router. Once it has for 20 seconds, the car is home: Glovebox takes a fresh snapshot and uploads what isn't backed up yet, Sentry first, then Saved, newest event first, so the important footage gets out before the car sleeps and cuts the Pi's power. The minute being recorded as the car pulls in only reaches the drive in full a minute or two later, so before this first backup ends (and keep-awake lets the car sleep) Glovebox waits until 2½ minutes after arriving, takes another snapshot and uploads what's new or has grown, repeating while that finds something. While home it looks for new clips every 10 minutes.
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

- Every `keep_awake.interval` (1 minute) it wakes the car and nudges it: it asks it to close the charge port (what teslausb and Sentry-USB do; with a cable plugged in the car refuses, which still counts), then sets the charging current to the value it already has. Tested on a 2026 Model 3, the charge port is what does it: waking the car, reading its state, re-setting the charging current and Keep Accessory Power On (which doesn't power the glovebox port) all left the car cutting the USB power about 12 minutes after it woke.
- So the key is paired with the **Driver** role: the car refuses to close the charge port for Charging Manager and Fleet Manager keys. Glovebox sends nothing but those commands, and a key used from the command line doesn't do passive entry like a phone. But the key could unlock and drive the car if someone took it from the Pi, so turn on PIN to Drive. A Charging Manager key paired by Glovebox before 0.5 is replaced with a new Driver key when you pair again (the Car page asks you to); remove the old one in the car.
- Recent clips recorded while the car is home aren't archived (`archive.recent_at_home = false`): when the car gets home, Glovebox notes the newest Recent clip (by the car's own clock) and skips later ones until the car has been gone for 2 minutes. An awake car in a garage records the garage, and each upload would leave more for the next. Turn it on for a car parked outside.
- The Car page also has a **Keep awake for 30 min** button, and the web UI shows when the car is being kept awake. With `[log] level = "debug"` every Bluetooth command is logged with how long it took.
- Only the first upload after the car gets home keeps it awake. An awake car keeps recording, faster than the Wi-Fi can upload, so what it records while parked is uploaded whenever it happens to be awake but never holds it awake.
- It's also kept awake while a software update installs, so the Pi doesn't lose power partway through.
- As a backstop, the car is kept awake for at most `keep_awake.max_duration` (1 hour) at a time, then left alone for `keep_awake.cooldown` (6 hours).
- The key, the VIN and the pairing state live in `/etc/glovebox/ble/`. The key shows up in the car under Controls → Locks → Keys, where it can be removed.

Pairing from the command line instead of the web UI:

```sh
sudo tesla-control -ble -vin <VIN> add-key-request /etc/glovebox/ble/public.pem driver cloud_key
echo driver | sudo tee /etc/glovebox/ble/role
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
| `GET /api/encryption`, `PUT /api/encryption/recipient`, `POST /api/encryption/unlock`, `POST /api/encryption/encrypt` | Drive encryption: its state and what encrypting needs, set your public key (on an encrypted drive only while it's unlocked), unlock with your private key, and encrypt the drive (`{"confirm": "ERASE"}`; restarts Glovebox). Changes must be sent as JSON. |

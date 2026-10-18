<p align="center">
  <img src="docs/logo.svg" width="84" alt="">
</p>

<h1 align="center">Glovebox</h1>

<p align="center">
  <b>TeslaCam and Sentry Mode backup to your own NAS, from a Raspberry Pi Zero 2 W in your glovebox.</b><br>
  No cloud required. No subscription. Nothing leaves your home network unless you pick a cloud drive.
</p>

<p align="center">
  <a href="https://getglovebox.dev/">Website</a> ·
  <a href="https://getglovebox.dev/guides/quick-start/">Setup guide</a> ·
  <a href="https://getglovebox.dev/guides/compatibility/">Compatibility guide</a>
</p>

<p align="center">
  <a href="https://github.com/lubaxe/glovebox/actions/workflows/go.yml"><img src="https://github.com/lubaxe/glovebox/actions/workflows/go.yml/badge.svg" alt="Tests"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-GPL--3.0-blue" alt="GPL-3.0 license"></a>
  <img src="https://img.shields.io/badge/Raspberry%20Pi-Zero%202%20W-c51a4a" alt="Raspberry Pi Zero 2 W">
  <img src="https://img.shields.io/badge/Go-1.27-00add8" alt="Go 1.27">
  <a href="https://buymeacoffee.com/lubaxe"><img src="https://img.shields.io/badge/Buy%20me%20a%20coffee-ffdd00?logo=buymeacoffee&logoColor=000" alt="Buy me a coffee"></a>
</p>

<p align="center">
  <img src="docs/screenshots/overview.png" alt="The Glovebox overview page: car connected, backed up to the NAS, keep-awake paired, and the latest Sentry and saved events">
</p>

Glovebox plugs into your Tesla's USB port and becomes its dashcam drive. Your car records to it as usual. When you park at home, your Sentry Mode events, saved clips and (if you like) all your driving footage copy themselves over Wi-Fi to your NAS (Unraid, Synology, TrueNAS or any server, over SMB, NFS or rsync) or to cloud storage with rclone. Browse and play everything from any browser on your network.

<sub>Glovebox is free and open source. If it's useful to you, you can <a href="https://buymeacoffee.com/lubaxe">buy me a coffee</a> ☕</sub>

## Features

- **Set up from your phone.** Flash the card and plug it in. Glovebox opens its own Wi-Fi network with a setup wizard: your home Wi-Fi, where backups go (with a connection test), extra drives and your car. No files to edit. Run the wizard again whenever you want to change something; it skips what's already set up.
- **Automatic backup when you get home.** Glovebox notices your NAS, copies what's new (Sentry first, then saved clips, newest first) and picks up where it left off if the car drives away mid-copy. It backs up to an SMB share, an NFS export, a server with rsync over SSH, or any of the 70-odd storage services rclone supports.
- **Keeps the car awake until the backup is done.** A sleeping Tesla cuts USB power. Glovebox pairs with your car over Bluetooth using a *limited* key that can wake it, but can't unlock, open or drive it.
- **Never gets in the car's way.** The car's drive is never touched while the car is using it. Glovebox reads footage from instant copy-on-write snapshots, repairs those instead, and presents the drive to the car about 16 seconds after power-on.
- **Bigger dashcam drive, more history.** The car gets nearly the whole card (52 GB on a 64 GB card), and snapshots keep clips the car has already overwritten.
- **Music, light shows and Boombox.** Optional extra drives for the car, filled from folders on your NAS and updated whenever you get home. Only a drive that changed is swapped, so the dashcam keeps recording.
- **Works with encrypted dashcam recordings.** If your car encrypts its clips, Glovebox backs them up as they are, and downloads a whole drive or event in one go for you to watch on dashcam.tesla.com.
- **Clean web app.** A synchronized six-camera player, Sentry and saved events with thumbnails and locations, your footage grouped into drives, and live status. Works on your phone.
- **Updates itself.** The Settings page says when there's a new release and installs it with one button, while the car keeps recording.
- **Looks after the car's drive.** Clears out events that are safely on your NAS so the car never runs out of room to save new ones. Nothing that isn't backed up is ever deleted.

## Screenshots

<table>
  <tr>
    <td width="50%"><img src="docs/screenshots/player.png" alt="Six cameras playing in sync"><br><sub><b>Player:</b> all six cameras in sync, with scrubbing, speed and downloads.</sub></td>
    <td width="50%"><img src="docs/screenshots/sentry.png" alt="Sentry events grouped by day"><br><sub><b>Sentry:</b> events with thumbnails, reasons and places. Encrypted ones are marked.</sub></td>
  </tr>
  <tr>
    <td><img src="docs/screenshots/car.png" alt="Step-by-step Bluetooth pairing"><br><sub><b>Car:</b> pair once with a key card, step by step.</sub></td>
    <td><img src="docs/screenshots/encrypted.png" alt="An encrypted drive with a one-click download"><br><sub><b>Encrypted clips:</b> download a whole drive in one go for dashcam.tesla.com.</sub></td>
  </tr>
  <tr>
    <td><img src="docs/screenshots/setup.png" alt="Guided setup with Wi-Fi, backups, extra drives and the car folded into summaries"><br><sub><b>Guided setup:</b> change just the part you need; the rest stays folded.</sub></td>
    <td align="center"><img src="docs/screenshots/setup-phone.png" width="260" alt="The setup wizard on a phone, choosing the home Wi-Fi network"><br><sub><b>First run:</b> set up from your phone, no files to edit.</sub></td>
  </tr>
  <tr>
    <td><img src="docs/screenshots/backups.png" alt="Choosing SMB, NFS, rsync over SSH or rclone, and a connection test that says which step failed"><br><sub><b>Backups:</b> SMB, NFS, rsync over SSH or rclone, with a connection test that says which step failed.</sub></td>
    <td align="center"><img src="docs/screenshots/mobile.png" width="260" alt="Glovebox on a phone"><br><sub><b>On your phone</b>, from the garage.</sub></td>
  </tr>
</table>

<sub>Screenshots use generated sample footage.</sub>

## Getting started

You need a **Raspberry Pi Zero 2 W**, a **microSD card** (64 GB or more, ideally high-endurance), a **USB data cable** for your car, and **somewhere to back up to**. Details are in [What you need](#what-you-need).

1. **Flash the card.** Download `glovebox-<version>-image.zip` from the [latest release](https://github.com/lubaxe/glovebox/releases/latest) and write it with [Raspberry Pi Imager](https://www.raspberrypi.com/software/) (Choose OS → **Use custom**). When Imager offers to customise settings, choose **No**.
2. **Plug it in.** Put the card in the Pi and connect the Pi's **USB** port (not **PWR**) to the car's glovebox USB port. A phone charger or a computer works too, for setting up at your desk. The car sees its dashcam drive within about 20 seconds.
3. **Join Glovebox-XXXX.** After a couple of minutes the Pi offers its own Wi-Fi network, named **Glovebox-** and four characters. Join it with your phone; the setup page opens by itself (if it doesn't, open `http://192.168.66.1`).
4. **Follow the wizard:**
   - **Wi-Fi:** pick your home network and enter its password. The Pi Zero 2 W only does 2.4 GHz.
   - **Backups:** pick an SMB share, NFS export, rsync over SSH or rclone, fill in the details and press *Test the connection*. Glovebox checks it can reach the server, log in, and write, read back and delete a test file.
   - **Extra drives** (optional): music, a light show or Boombox sounds, copied from folders on your server.
   - **Your car:** enter your VIN, then pair keep-awake on the **Car** page: press *Send pairing request*, lay a key card on the phone charging pad and confirm on the car's screen.
   - **Finish.** The setup network turns off. From now on Glovebox is at `http://glovebox.local:8080` on your home network.

When you park at home, clips start copying to your NAS on their own. The Overview page shows the progress.

**Already running Raspberry Pi OS Lite, or using another board?** The image is only for the Pi Zero 2 W, but the installer checks whatever it runs on (Zero W, Pi 4 and 5, other boards that can act as a USB drive) and installs if Glovebox can work there. Then open `http://<the board's name>.local:8080` and the same wizard starts:

```sh
curl -fsSL https://github.com/lubaxe/glovebox/releases/latest/download/install.sh | sudo bash
```

The full guide, with troubleshooting, is in [docs/install.md](docs/install.md).

## Changing settings

- **Guided setup**, on the **Settings** page, walks through the wizard again. What's set up already is folded into a summary; open just the part you want to change.
- **All settings**, the rest of the **Settings** page, shows every backup and drive setting on one page.
- **Moved house or changed your Wi-Fi?** Put an empty file named `glovebox-setup` on the card's boot partition and restart the Pi: the setup network comes back.

For setups without a phone, or to prepare many cards, every setting can also go in `glovebox.toml` on the boot partition before first boot; see [Setting up without the wizard](docs/install.md#setting-up-without-the-wizard). All settings and their defaults are in [deploy/glovebox.toml](deploy/glovebox.toml).

## How it works

```mermaid
flowchart LR
    car["🚗 Tesla"] -- "USB: records to it<br>like a normal drive" --> pi
    subgraph pi ["Glovebox (Pi Zero 2 W)"]
        drive["Car's drive"] -- "instant snapshots" --> snaps["Snapshots<br>(read-only copies)"]
        snaps --> web["Web app"]
        snaps --> backup["Backup"]
    end
    backup -- "Wi-Fi, when home" --> nas[("Your NAS<br>or cloud")]
    pi -. "Bluetooth: stay awake<br>until the backup is done" .-> car
    web --> you["📱 You"]
```

1. **The car records** to Glovebox exactly as it would to a USB stick.
2. **Glovebox snapshots the drive** every hour, before each backup, and whenever space runs low. Snapshots are instant and only use space for what the car changes afterwards. Everything Glovebox reads comes from a snapshot, so it never touches files the car might be writing.
3. **At home, it backs up.** When your NAS is reachable, Glovebox copies new clips over SMB, NFS, rsync or rclone, verifying each file before marking it done. If the car drives off, it carries on next time.
4. **The car stays awake** for that first backup after arriving home, at most an hour, so it doesn't cut the power halfway. After that it's free to sleep.

The details, from partitions to power cuts, are in [docs/how-it-works.md](docs/how-it-works.md).

## Lean by design

The Glovebox image is Raspberry Pi OS cut down to what a dashcam drive needs, so it downloads faster, boots faster and leaves more of the card for footage.

- **Less than half the size** of Raspberry Pi OS Lite, to download and on the card, with less than half the packages.
- **Nearly the whole card for footage:** on a 64 GB card the car gets a dashcam drive of about 52 GB, plus a few GB for snapshots.
- **The car sees its drive about 16 seconds after power-on**, and the Pi is fully booted soon after.
- **About a third of the Pi Zero 2 W's memory** in use.

How: the image is built from Raspberry Pi's own build system, keeping only its base stages and leaving out the desktop, compilers, kernel headers, the Pi 5 kernel, cloud-init, swap, man pages and translations. Glovebox starts before the network, presents the car's drive within about a second, and only then gets on with snapshots and backups. Logs are kept in memory to save writes to the card, with a small copy on the storage partition that survives power cuts.

## What you need

| | |
|---|---|
| **A Raspberry Pi Zero 2 W** | Small enough for the glovebox, and it's the right kind of Pi: it can act as a USB drive. |
| **A microSD card** | A high-endurance card, 64 GB or more; 128–256 GB for more history on HW4 cars. For example SanDisk High Endurance or Samsung PRO Endurance. |
| **A USB cable** | Micro-USB to whatever your car's glovebox port is (USB-C on recent cars). It must carry data, not just power. It goes into the Pi's **USB** port, not **PWR**: one cable powers the Pi and carries the drive. |
| **Somewhere to back up to** | A NAS or server reachable from your garage over 2.4 GHz Wi-Fi, with an SMB share, an NFS export or SSH and rsync; or a cloud drive through rclone. A user and folder just for Glovebox is a good idea. |
| **A Tesla** | Tested on a 2026 Model 3 (HW4). Glovebox uses the standard TeslaCam drive, so other models should work too. |
| **A key card** (optional) | To pair keep-awake, once. |

## Is it safe?

- **For your car:** the Bluetooth key has Tesla's **Charging Manager** role. We tested it: the car lets it wake up and read the charge level, and refuses unlocking, remote start, flashing the lights and opening the charge port. You can remove it any time under Controls → Locks → Keys.
- **For your footage:** the car's drive is never written to while the car can see it. Glovebox only ever deletes events from the car's drive once they're on your NAS at full size, and only at startup, before the car is connected.
- **For your privacy:** with a NAS, footage goes from the car to your NAS and never leaves your home network. Only if you pick a cloud drive (through rclone) does it go there, and nowhere else. Glovebox itself never contacts any other server; the Pi only sets its clock from a public time server.
- **The setup network** is open, so anyone nearby could join it while it's on. It's only on until Glovebox is set up, before there's any footage, and turns off for good once you finish the wizard (at most 30 minutes after the Pi first joins your Wi-Fi).
- **On your network:** the web app has no login. Anyone on your home network can view clips and change settings, so keep the Pi on a network you trust.

## FAQ

**Do I have to edit any files?**
No. Everything is set up in the wizard on your phone. `glovebox.toml` on the boot partition is still there for setting up without a phone; see [Setting up without the wizard](docs/install.md#setting-up-without-the-wizard).

**I don't see the Glovebox-XXXX network.**
Give the Pi two minutes after plugging it in: it waits that long for a Wi-Fi network it already knows. The network only appears until Glovebox is set up. To bring it back later, put an empty file named `glovebox-setup` on the boot partition and restart the Pi.

**Does it work with encrypted dashcam recordings?**
Yes. Glovebox backs them up as they are and shows them with a lock. To watch them, open them at [dashcam.tesla.com](https://dashcam.tesla.com) signed in to your Tesla account, or turn off Controls → Safety → Encrypt Dashcam Recordings for clips you can play anywhere.

**How fast is the backup?**
About 2.5–3.5 MB/s over the Pi Zero 2 W's Wi-Fi. Sentry and saved events (a few hundred MB each) take a minute or two. An hour of HW4 driving footage is 15–20 GB, which is why backing up all driving footage is an option you turn on in setup.

**Will it drain my battery?**
Glovebox keeps the car awake only for the first backup after you arrive home, at most an hour, then lets it sleep for at least six hours. Clips the car records while parked are backed up whenever the car is awake anyway, but never keep it awake.

**What happens when the card fills up?**
The car's drive has a fixed size, and the car manages it like any USB drive. Glovebox clears out backed-up events so the car keeps room for new ones, and drops its oldest snapshots when it needs space.

**Where can it back up to?**
An SMB share (built-in client, SMB 2.1 by default as it's fastest on the Pi Zero 2 W), an NFS export (mounted only while backing up), a folder on any server with rsync over SSH (Glovebox makes its own key and shows it in setup), or any rclone remote: Google Drive, OneDrive, Dropbox, S3, Backblaze B2, SFTP, WebDAV and more. For rclone, set up the remote on a computer with `rclone config` and paste its `rclone.conf` in setup. Since a cloud drive answers from anywhere, rclone also needs a *home check address*, such as your router, that only answers at home.

**How do the music, light show and Boombox drives work?**
Turn one on by giving it a size in setup and naming a folder on your NAS to copy it from. Whenever the car gets home, after the backup, Glovebox brings each drive up to date in a copy of it. Only if something changed does it swap the copy in: the car sees that one drive unplugged and plugged back in, and keeps recording to the dashcam drive throughout. Light shows (an `.fseq` and an `.mp3` or `.wav` with the same name) end up in the drive's `LightShow` folder and Boombox sounds in its `Boombox` folder, where the car looks for them.

**Can I use a different Raspberry Pi, or another board?**
The image is only for the Pi Zero 2 W, the board Glovebox is built and tested for. On anything else, install Glovebox with `install.sh`: it first checks the board (run it with `GLOVEBOX_CHECK_ONLY=1` to only check) and stops if Glovebox can't work there. The board needs a USB port that can act as a drive (Pi Zero, Zero W and Zero 2 W; Pi 4 and 5 on their USB-C port; some other boards), Wi-Fi, and ideally Bluetooth. A Pi 4 or 5 needs more power than many cars' USB ports give; Pi 1, 2 and 3B can't act as a USB drive at all.

## Development

Glovebox is a single Go service (`cmd/glovebox`) on a minimal 64-bit Raspberry Pi OS image.

- `go test ./...` runs anywhere; the hardware parts (loop devices, mounts, the USB gadget, NetworkManager, Bluetooth) sit behind small interfaces with fakes.
- `go run ./tools/uidemo` serves the web app with generated sample footage, for working on the UI without a Pi (needs ffmpeg). `-firstrun` starts it as a Pi on its setup network, `-extras` adds extra drives.
- `scripts/deploy.ps1 -Target user@host` cross-compiles for linux/arm64 and installs the service on a Pi over SSH.
- [docs/architecture.md](docs/architecture.md) describes the design, [docs/how-it-works.md](docs/how-it-works.md) the behaviour and the API.

### Building the image

The image is Raspberry Pi OS (Debian 13 "trixie", 64-bit) cut down to what Glovebox needs, with Glovebox and Tesla's `tesla-control` compiled in. See [pi-gen-sources/Readme.md](pi-gen-sources/Readme.md).

- **Releases:** bump `VERSION` and push to the `release` branch. CI builds the image, the programs and `install.sh`, measures the image, and publishes release `v<VERSION>` with checksums.
- **Test builds:** run **Build image** by hand in GitHub Actions for a pre-release.
- **Locally:** run `pi-gen-sources/build-image.sh` on Debian/Ubuntu or WSL2.

## Feedback

Bug reports and ideas are welcome in [Issues](https://github.com/lubaxe/glovebox/issues).

## Credits

Glovebox was inspired by, and started from, [teslausb](https://github.com/marcone/teslausb). Thanks to its authors and contributors. Keep-awake uses Tesla's open-source [vehicle-command](https://github.com/teslamotors/vehicle-command) tools. Their licenses, and those of the libraries Glovebox is built with, are in [THIRD-PARTY-NOTICES.md](THIRD-PARTY-NOTICES.md).

## License

Copyright © 2026 lubaxe.

Glovebox is free software, licensed under the [GNU General Public License v3.0](LICENSE): you can use it, change it and share it, and if you distribute it, changed or not, you must do so under the same license, with its source and this copyright notice. For other licensing, for example to build Glovebox into a commercial product without the GPL's terms, [get in touch](https://github.com/lubaxe/glovebox/issues).

Glovebox is not affiliated with or endorsed by Tesla, Inc. "Tesla" and "TeslaCam" are trademarks of Tesla, Inc., used here only to say what Glovebox works with.

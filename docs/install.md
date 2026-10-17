# Installing Glovebox

There are two ways to set up a Pi:

- **[Flash the Glovebox image](#flash-the-glovebox-image)** onto a new card. This is the easiest, and the image is cut down to exactly what Glovebox needs, so it boots faster and leaves more room for footage.
- **[Install on Raspberry Pi OS](#install-on-raspberry-pi-os)** you've already set up, without reflashing.

Either way you finish the same way: fill in `glovebox.toml`, then pair keep-awake in the web app.

## What you need

- A **Raspberry Pi Zero 2 W**.
- A **microSD card**, 64 GB or larger, ideally a high-endurance one (SanDisk High Endurance, Samsung PRO Endurance, ...). Dashcams write constantly.
- A **USB data cable** from the Pi's **USB** port (the one in the middle, not **PWR**) to your car's glovebox USB port: micro-USB to USB-C on recent Teslas. Charge-only cables won't work.
- A **NAS or computer with an SMB share**, reachable over Wi-Fi from where you park.

## Flash the Glovebox image

1. Download `glovebox-<version>-image.zip` from the [latest release](https://github.com/lubaxe/glovebox/releases/latest).
2. Open [Raspberry Pi Imager](https://www.raspberrypi.com/software/), pick **Raspberry Pi Zero 2 W**, then **Choose OS → Use custom** and select the zip. Pick your card and write it. When Imager offers to customise settings, say **No**: Glovebox uses its own settings file instead.
3. Take the card out and put it back in. A drive called **bootfs** appears. Open **`glovebox.toml`** on it with any text editor and fill in:
   - **Wi-Fi**: your home network's name, password and country.
   - **NAS**: the server's address, the share, and the user name and password.
   - Optionally an **SSH key**, if you want to log in to the Pi, and your car's **VIN**.

   To use a setting, delete the `# ` in front of it. Save the file and eject the card.
4. Put the card in the Pi and plug the Pi into the car (or a computer, to try it out first).

On the first boot Glovebox sets up Wi-Fi, makes room for the car's drive, and presents the drive. The first boot takes a little longer; after that the car sees its drive about 16 seconds after power-on. It also leaves **`glovebox-status.txt`** on the boot partition saying what it did, or what was wrong with `glovebox.toml`.

## Install on Raspberry Pi OS

This is for **Raspberry Pi OS Lite (64-bit)**, the current version (trixie), on a Pi Zero 2 W that's already on Wi-Fi with SSH working. A fresh install from Raspberry Pi Imager with Wi-Fi and SSH set in its settings is perfect.

Log in to the Pi and run:

```sh
curl -fsSL https://github.com/lubaxe/glovebox/releases/latest/download/install.sh | sudo bash
```

The installer:

- installs Glovebox, Tesla's `tesla-control`, and the tools they need (`xfsprogs`, `exfatprogs`, `dosfstools`)
- turns on USB device mode (`dtoverlay=dwc2` in `config.txt`), so the Pi can act as a USB drive
- creates the space for the car's drive and Glovebox's snapshots. Raspberry Pi OS gives the whole card to the system, so this is a large file, `/var/lib/glovebox/backingfiles.xfs`, mounted at `/backingfiles`. It takes all the free space except 3 GB; set `GLOVEBOX_BACKINGFILES_SIZE=48G` (for example) before `sudo bash` to choose.
- puts the settings template at `/boot/firmware/glovebox.toml` and starts Glovebox

Then:

1. Fill in the NAS settings: `sudo nano /boot/firmware/glovebox.toml`. Leave Wi-Fi alone if the Pi is already connected.
2. Restart Glovebox (`sudo systemctl restart glovebox`), or reboot if the installer said to.
3. Check `/boot/firmware/glovebox-status.txt` to see that the settings were applied.

Your existing user, SSH settings and anything else on the Pi stay as they are. An SSH key in `glovebox.toml` goes to the Pi's main user (the one Imager created).

## After either one

1. Find the Pi's address in your router's device list (it shows up as `glovebox`) and open `http://<address>:8080`.
2. Go to the **Car** page, enter your VIN, press **Send pairing request**, lay a key card on the phone charging pad behind the cupholders, and confirm on the car's screen. Then press **Check pairing**.
3. Plug the Pi into the car. The car's dashcam icon should show it's recording within about 20 seconds.

When you park at home, clips start copying to your NAS on their own. The Overview page shows the progress.

## Changing settings later

Put a new `glovebox.toml` on the boot partition, with just the settings you want to change, and restart the Pi. Every setting, with its default, is listed in [deploy/glovebox.toml](../deploy/glovebox.toml). The applied configuration lives in `/etc/glovebox/glovebox.toml`.

## Troubleshooting

| Problem | What to check |
|---|---|
| The car doesn't see the drive | The cable must go into the Pi's **USB** port, not **PWR**, and must carry data. Use the glovebox port: the console ports in some cars only charge. |
| Nothing on the network | `glovebox-status.txt` on the boot partition says whether Wi-Fi was set up. The Pi only supports 2.4 GHz networks. |
| Backups don't start | The NAS must be reachable on port 445 from the garage. The Overview page and `/backingfiles/glovebox.log` show what happened. |
| Pairing shows nothing on the car's screen | Nothing appears until you lay the key card on the phone charging pad. The request expires after about 30 seconds, so send it again just before. |
| Clips show a lock | Your car encrypts its recordings. See [Encrypted clips](how-it-works.md#encrypted-clips). |

## Uninstalling (Raspberry Pi OS)

```sh
sudo systemctl disable --now glovebox
sudo umount /backingfiles
sudo rm /etc/systemd/system/glovebox.service /usr/local/bin/glovebox /usr/local/bin/tesla-control
sudo rm -r /etc/glovebox /var/lib/glovebox /backingfiles /usr/share/doc/glovebox
```

Then remove the `/backingfiles` line from `/etc/fstab` and the Glovebox lines at the end of `/boot/firmware/config.txt`. Removing `/var/lib/glovebox` deletes the car's drive and every snapshot on it.

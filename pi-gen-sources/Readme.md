### Building the Glovebox image

The image is 64-bit Raspberry Pi OS (Debian 13 "trixie") built with [pi-gen](https://github.com/RPi-Distro/pi-gen), cut down to what Glovebox needs, with the Glovebox service compiled from this checkout.

`build-image.sh` compiles Glovebox for linux/arm64, clones pi-gen at a pinned tag, runs `prepare.sh`, and builds the image. It needs a Debian or Ubuntu host with Go; on Windows, use WSL2 (`wsl --install -d Ubuntu`). The **Build image** GitHub Actions workflow runs the same script.

```
pi-gen-sources/build-image.sh
```

The image ends up in `~/glovebox-build/pi-gen/deploy`.

- pi-gen must live on a Linux filesystem. Under WSL2 the Glovebox checkout can stay on the Windows drive (e.g. `/mnt/c/Work/glovebox`), but `WORKDIR` (default `~/glovebox-build`) cannot.
- `PI_GEN_REF` selects the pi-gen tag (default `2026-10-06-raspios-trixie-arm64`). It has to match `RELEASE` in `pi-gen-config`.
- `--docker` builds with pi-gen's `build-docker.sh` instead of directly on the host with `sudo`.

#### What's in it

pi-gen's `stage0` (base system and kernel) and `stage1` (bootable minimum), then `00-glovebox` instead of Raspberry Pi OS Lite's `stage2`:

- packages: only `00-glovebox/00-packages-nr` (SSH, NetworkManager and the Zero 2 W's Wi-Fi firmware, Bluetooth, XFS/exFAT/FAT tools, `fdisk`, and `nano`, `less`, `htop`, `iw`, `ping`), all without recommended extras
- one kernel (the Zero 2 W's, `linux-image-rpi-v8`), no kernel headers
- no docs, man pages or translations (`00a-nodoc` runs right after the base system is bootstrapped; packages' copyright files stay), no locales, no apt package lists
- Glovebox, its systemd unit, and `glovebox.toml` on the boot partition for setting up the card
- USB device mode (`dtoverlay=dwc2`), Wi-Fi power saving off, logs in RAM, Bluetooth unblocked
- SSH with keys only; the `glovebox` user has no password (pi-gen insists on one at build time, so `build-image.sh` passes a random one that the image then removes) and passwordless `sudo`
- the root partition keeps its built size; Glovebox creates the `backingfiles` partition in the free space at first boot

#### Manual build

1. Build Glovebox: `GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o glovebox ./cmd/glovebox`, and `tesla-control` from a checkout of [vehicle-command](https://github.com/teslamotors/vehicle-command) at the commit pinned in `build-image.sh` (`go build ./cmd/tesla-control`)
2. Clone pi-gen and check out a tag matching `RELEASE` in `pi-gen-config`
3. From the pi-gen folder, run `prepare.sh <glovebox binary> <tesla-control binary>`, then add `FIRST_USER_PASS=<anything>` to `config`
4. Run `build.sh` or `build-docker.sh`. The image will be in the `deploy` folder. Flash it with Raspberry Pi Imager ("Use custom").

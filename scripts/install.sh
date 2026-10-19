#!/bin/bash -eu
#
# Installs Glovebox on a board that already runs a Debian-based Linux,
# typically Raspberry Pi OS Lite, for anyone who'd rather not flash the
# Glovebox image (which is only for the Pi Zero 2 W):
#
#   curl -fsSL https://github.com/lubaxe/glovebox/releases/latest/download/install.sh | sudo bash
#
# It first checks that the board can work as the car's USB drive and has
# what Glovebox needs, and stops if it can't. Things that only limit
# Glovebox (no Bluetooth, a power-hungry board, an untested model) are
# warnings: it installs anyway. Then it installs glovebox and Tesla's
# tesla-control, the packages they need, turns on USB device mode (on
# Raspberry Pis), and makes a "backingfiles" XFS filesystem for the car's
# drive and snapshots. The OS usually gives the whole card to its root
# filesystem, so that's a large file there rather than a partition.
#
# Environment:
#   GLOVEBOX_BACKINGFILES_SIZE  size of the backingfiles file, e.g. 48G
#                               (default: all free space but 3 GB)
#   GLOVEBOX_CHECK_ONLY=1       only check the board, change nothing

REPO=lubaxe/glovebox
VERSION=latest # the release workflow sets this to the release's version
BACKINGFILES_FILE=/var/lib/glovebox/backingfiles.xfs

say() { printf '\n\033[1m%s\033[0m\n' "$*"; }
die() { printf '\nglovebox install: %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "run it as root: ... | sudo bash"

# ---- compatibility ----------------------------------------------------------

problems=()
warnings=()
good() { printf '  \033[32m✓\033[0m %s\n' "$*"; }
warn() { printf '  \033[33m!\033[0m %s\n' "$*"; warnings+=("$*"); }
bad() { printf '  \033[31m✗\033[0m %s\n' "$*"; problems+=("$*"); }

say "Checking this board"

case "$(uname -m)" in
  aarch64) arch=arm64 ;;
  armv6l | armv7l | armv8l) arch=armv6 ;; # the ARMv6 build runs on every 32-bit Pi
  *) arch="" ;;
esac
if [ -n "$arch" ]
then
  good "processor: $(uname -m)"
else
  bad "processor: $(uname -m); Glovebox is built for ARM (aarch64 or 32-bit ARM)"
fi

model=$(tr -d '\0' < /proc/device-tree/model 2> /dev/null || true)
[ -n "$model" ] || model="unknown board"
# How the board's USB port becomes the car's drive:
#   dwc2          a Pi Zero's data port
#   peripheral    the USB-C power port of a Pi 4, 5 or Compute Module
#   own           not a Raspberry Pi: its own device tree has to allow it
device_mode=""
case "$model" in
  *"Zero 2"*)
    good "board: $model (the board Glovebox is built and tested for)"
    device_mode=dwc2 ;;
  *"Zero W"*)
    good "board: $model"
    warn "the original Zero W is single-core and 32-bit: backups and starting up are slower than on a Zero 2 W"
    device_mode=dwc2 ;;
  *"Raspberry Pi Zero"*)
    bad "board: $model has no Wi-Fi, so it can't back up (the USB port is taken by the car)" ;;
  # (also the 400 and 500)
  *"Raspberry Pi 5"* | *"Raspberry Pi 4"* | *"Compute Module 4"* | *"Compute Module 5"*)
    good "board: $model (works as a USB drive through its USB-C power port)"
    warn "this board needs about 3 A, more than many cars' USB ports give: it may restart or drop the drive; a Pi Zero 2 W is the safer choice for the car"
    warn "untested: Glovebox is built and tested on the Pi Zero 2 W"
    device_mode=peripheral ;;
  *"Raspberry Pi 3 Model A"*)
    good "board: $model"
    warn "the 3A+ can only be a USB drive through its USB-A port, with a special cable (USB-A to USB-A, 5 V wire cut) and separate power"
    warn "untested: Glovebox is built and tested on the Pi Zero 2 W"
    device_mode=dwc2 ;;
  *"Raspberry Pi 3"* | *"Raspberry Pi 2"* | *"Raspberry Pi Model"* | *"Compute Module 3"* | *"Compute Module Rev"*)
    bad "board: $model can't act as a USB drive (its USB ports only work as host ports)" ;;
  *"Raspberry Pi"*)
    warn "board: $model is not one Glovebox knows; trying anyway"
    device_mode=dwc2 ;;
  *)
    udcs=(/sys/class/udc/*)
    if [ -e "${udcs[0]}" ]
    then
      good "board: $model has a USB device controller (${udcs[0]##*/})"
      warn "untested: not a Raspberry Pi. Make sure the controller's port is the one you plug into the car and is set to peripheral or OTG mode in the device tree"
      device_mode=own
    else
      bad "board: $model has no USB device controller, so it can't act as the car's USB drive (if it has an OTG port, turn on peripheral mode in its device tree first)"
    fi ;;
esac

if [ -d /run/systemd/system ] && command -v apt-get > /dev/null
then
  good "system: $(. /etc/os-release && echo "${PRETTY_NAME:-Linux}")"
else
  bad "system: Glovebox needs a Debian-based Linux with systemd (apt-get and systemd)"
fi

kernel=$(uname -r)
kmajor=${kernel%%.*}
kminor=${kernel#*.}; kminor=${kminor%%.*}
if [ "$kmajor" -gt 5 ] || { [ "$kmajor" -eq 5 ] && [ "$kminor" -ge 10 ]; }
then
  good "kernel: $kernel"
else
  bad "kernel: $kernel is too old; Glovebox needs 5.10 or newer (exFAT and XFS copy-on-write)"
fi
for m in xfs exfat loop libcomposite usb_f_mass_storage
do
  if grep -qw "$m" /proc/filesystems 2> /dev/null || [ -d "/sys/module/$m" ] || modprobe -n -q "$m" 2> /dev/null
  then
    :
  else
    bad "kernel: no '$m' module (needed for the car's drive and snapshots)"
  fi
done

if [ -d /sys/class/net/wlan0 ]
then
  good "Wi-Fi: wlan0"
else
  warn "no Wi-Fi (wlan0): Glovebox backs up over Wi-Fi when the car is home"
fi
if systemctl is-active -q NetworkManager 2> /dev/null
then
  good "network: NetworkManager"
else
  warn "NetworkManager isn't running: Glovebox's setup network and its Wi-Fi settings won't work; set up Wi-Fi in the system instead"
fi
hcis=(/sys/class/bluetooth/*)
if [ -e "${hcis[0]}" ]
then
  good "Bluetooth: ${hcis[0]##*/}"
else
  warn "no Bluetooth: Glovebox can't keep the car awake while it backs up"
fi

boot=""
for b in /boot/firmware /boot
do
  if [ -f "$b/config.txt" ]
  then
    boot=$b
    break
  fi
done
if [ "$device_mode" = dwc2 ] || [ "$device_mode" = peripheral ]
then
  if [ -n "$boot" ]
  then
    good "boot partition: $boot"
    if grep -qE 'g_ether|g_serial|g_mass_storage|g_multi' "$boot/cmdline.txt" 2> /dev/null
    then
      warn "$boot/cmdline.txt loads another USB gadget (g_ether, g_serial, ...); remove it, Glovebox needs the port to itself"
    fi
  else
    bad "can't find the boot partition's config.txt to turn on USB device mode"
  fi
fi

avail=$(df --output=avail -B1M / | tail -1 | tr -d ' ')
if [ "$avail" -ge 500 ]
then
  good "free space on /: ${avail} MB"
else
  bad "free space on /: only ${avail} MB; the packages need about 500 MB"
fi

if [ ${#problems[@]} -gt 0 ]
then
  printf '\n'
  die "Glovebox can't work on this system:$(printf '\n  - %s' "${problems[@]}")"
fi
if [ ${#warnings[@]} -gt 0 ]
then
  echo
  echo "Glovebox should work here, with the limits above."
fi
if [ -n "${GLOVEBOX_CHECK_ONLY:-}" ]
then
  echo "Checked only (GLOVEBOX_CHECK_ONLY); nothing was changed."
  exit 0
fi

# ---- install ----------------------------------------------------------------

if [ "$VERSION" = latest ]
then
  base="https://github.com/$REPO/releases/latest/download"
else
  base="https://github.com/$REPO/releases/download/$VERSION"
fi

say "Installing packages"
apt-get update -q
apt-get install -y -q --no-install-recommends xfsprogs cryptsetup-bin exfatprogs dosfstools curl ca-certificates \
	rsync rclone nfs-common openssh-client dnsmasq-base avahi-daemon
apt-get clean # the downloaded packages, not needed any more

say "Downloading Glovebox ($VERSION)"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
files=("glovebox-linux-$arch" "tesla-control-linux-$arch" glovebox.service glovebox-boot.toml LICENSE THIRD-PARTY-NOTICES.md)
for f in "${files[@]}" SHA256SUMS
do
  curl -fsSL -o "$tmp/$f" "$base/$f" || die "couldn't download $base/$f"
done
(cd "$tmp" && grep -E " ($(IFS='|'; echo "${files[*]}"))\$" SHA256SUMS | sha256sum -c --quiet) ||
  die "the downloads don't match SHA256SUMS"

install -m 755 "$tmp/glovebox-linux-$arch" /usr/local/bin/glovebox
install -m 755 "$tmp/tesla-control-linux-$arch" /usr/local/bin/tesla-control
install -m 644 "$tmp/glovebox.service" /etc/systemd/system/glovebox.service
install -d /usr/share/doc/glovebox
install -m 644 "$tmp/LICENSE" "$tmp/THIRD-PARTY-NOTICES.md" /usr/share/doc/glovebox/
install -d -m 700 /etc/glovebox
if [ "$boot" = /boot/firmware ] && [ ! -f "$boot/glovebox.toml" ] && [ ! -f /etc/glovebox/glovebox.toml ]
then
  # the optional settings file, where Glovebox looks for it
  install -m 644 "$tmp/glovebox-boot.toml" "$boot/glovebox.toml"
fi

say "Turning on USB device mode"
case "$device_mode" in
  own)
    echo "not a Raspberry Pi: leaving the board's device tree as it is" ;;
  *)
    overlay="dtoverlay=dwc2"
    [ "$device_mode" = peripheral ] && overlay="dtoverlay=dwc2,dr_mode=peripheral"
    if grep -qE '^[[:space:]]*dtoverlay=dwc2' "$boot/config.txt"
    then
      echo "already on"
    else
      # [all] so it isn't caught in a model-specific section
      printf '\n[all]\n# Glovebox: the Pi acts as the car'"'"'s USB drive\n%s\n' "$overlay" >> "$boot/config.txt"
      reboot_needed=1
    fi ;;
esac

say "Making room for the car's drive"
if mountpoint -q /backingfiles
then
  echo "/backingfiles is already mounted"
else
  if [ ! -f "$BACKINGFILES_FILE" ]
  then
    free=$(df --output=avail -B1 / | tail -1)
    if [ -n "${GLOVEBOX_BACKINGFILES_SIZE:-}" ]
    then
      size=$GLOVEBOX_BACKINGFILES_SIZE
    else
      # leave 3 GB for the system (updates, logs)
      size=$(( (free - 3 * 2**30) / 2**30 ))G
    fi
    case "$size" in
      [0-7]G|-*) die "only $((free / 2**30)) GB free on the card; Glovebox needs at least 8 GB (a 64 GB or larger card is best)" ;;
    esac
    echo "creating a $size backingfiles file in $BACKINGFILES_FILE"
    install -d -m 700 "$(dirname "$BACKINGFILES_FILE")"
    fallocate -l "$size" "$BACKINGFILES_FILE"
    mkfs.xfs -q -m reflink=1 -L backingfiles "$BACKINGFILES_FILE"
  fi
  mkdir -p /backingfiles
  if ! grep -qE '[[:space:]]/backingfiles[[:space:]]' /etc/fstab
  then
    echo "$BACKINGFILES_FILE /backingfiles xfs loop,noatime,nofail 0 0" >> /etc/fstab
  fi
  systemctl daemon-reload
  mount /backingfiles
fi

say "Starting Glovebox"
systemctl daemon-reload
systemctl enable glovebox
if [ -n "${reboot_needed:-}" ]
then
  echo "USB device mode needs a reboot; Glovebox starts by itself afterwards."
else
  systemctl restart glovebox
fi

ip=$(hostname -I 2> /dev/null | awk '{print $1}')
[ -n "$ip" ] || ip="<the board's address>"
say "Done"
cat << EOF
Next:
  1. $( [ -n "${reboot_needed:-}" ] && echo "Reboot: sudo reboot" || echo "Glovebox is running." )
  2. Open http://$(hostname).local:8080 (or http://$ip:8080) in a browser: a
     setup wizard asks where to back up to, tests the connection, and pairs
     keep-awake with your car.
  3. Plug the board's USB device port into the car's glovebox USB port (on a
     Pi Zero, the port marked USB, not PWR; on a Pi 4 or 5, the USB-C port).
EOF
if [ ${#warnings[@]} -gt 0 ]
then
  printf '\nKeep in mind:%s\n' "$(printf '\n  - %s' "${warnings[@]}")"
fi

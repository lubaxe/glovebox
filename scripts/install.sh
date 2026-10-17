#!/bin/bash -eu
#
# Installs Glovebox on a Raspberry Pi Zero 2 W that already runs Raspberry Pi
# OS Lite (64-bit), for anyone who'd rather not flash the Glovebox image:
#
#   curl -fsSL https://github.com/lubaxe/glovebox/releases/latest/download/install.sh | sudo bash
#
# It installs glovebox and Tesla's tesla-control, the packages they need,
# turns on USB device mode, and makes a "backingfiles" XFS filesystem for the
# car's drive and snapshots. Raspberry Pi OS gives the whole card to its root
# filesystem, so that's a large file there rather than a partition.
#
# Environment:
#   GLOVEBOX_BACKINGFILES_SIZE  size of the backingfiles file, e.g. 48G
#                               (default: all free space but 3 GB)

REPO=lubaxe/glovebox
VERSION=latest # the release workflow sets this to the release's version
BACKINGFILES_FILE=/var/lib/glovebox/backingfiles.xfs

say() { printf '\n\033[1m%s\033[0m\n' "$*"; }
die() { printf '\nglovebox install: %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "run it as root: ... | sudo bash"
[ "$(uname -m)" = aarch64 ] || die "this needs 64-bit Raspberry Pi OS (uname -m says $(uname -m))"
model=$(tr -d '\0' < /proc/device-tree/model 2> /dev/null || true)
case "$model" in
  *"Zero 2"*) ;;
  *"Raspberry Pi"*) echo "Note: Glovebox is built and tested for the Pi Zero 2 W; this is a $model." ;;
  *) die "this doesn't look like a Raspberry Pi ($model)" ;;
esac
boot=/boot/firmware
[ -f "$boot/config.txt" ] || boot=/boot
[ -f "$boot/config.txt" ] || die "can't find the boot partition's config.txt"

if [ "$VERSION" = latest ]
then
  base="https://github.com/$REPO/releases/latest/download"
else
  base="https://github.com/$REPO/releases/download/$VERSION"
fi

say "Installing packages"
apt-get update -q
apt-get install -y -q --no-install-recommends xfsprogs exfatprogs dosfstools curl ca-certificates

say "Downloading Glovebox ($VERSION)"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
for f in glovebox-linux-arm64 tesla-control-linux-arm64 glovebox.service glovebox-boot.toml LICENSE THIRD-PARTY-NOTICES.md SHA256SUMS
do
  curl -fsSL -o "$tmp/$f" "$base/$f" || die "couldn't download $base/$f"
done
(cd "$tmp" && grep -E ' (glovebox-linux-arm64|tesla-control-linux-arm64|glovebox.service|glovebox-boot.toml|LICENSE|THIRD-PARTY-NOTICES.md)$' SHA256SUMS | sha256sum -c --quiet) ||
  die "the downloads don't match SHA256SUMS"

install -m 755 "$tmp/glovebox-linux-arm64" /usr/local/bin/glovebox
install -m 755 "$tmp/tesla-control-linux-arm64" /usr/local/bin/tesla-control
install -m 644 "$tmp/glovebox.service" /etc/systemd/system/glovebox.service
install -d /usr/share/doc/glovebox
install -m 644 "$tmp/LICENSE" "$tmp/THIRD-PARTY-NOTICES.md" /usr/share/doc/glovebox/
install -d -m 700 /etc/glovebox
if [ ! -f "$boot/glovebox.toml" ] && [ ! -f /etc/glovebox/glovebox.toml ]
then
  install -m 644 "$tmp/glovebox-boot.toml" "$boot/glovebox.toml"
fi

say "Turning on USB device mode"
if grep -qE '^[[:space:]]*dtoverlay=dwc2' "$boot/config.txt"
then
  echo "already on"
else
  # [all] so it isn't caught in a model-specific section
  printf '\n[all]\n# Glovebox: the Pi acts as the car'"'"'s USB drive\ndtoverlay=dwc2\n' >> "$boot/config.txt"
  reboot_needed=1
fi

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
[ -n "$ip" ] || ip="<the Pi's address>"
say "Done"
cat << EOF
Next:
  1. Fill in $boot/glovebox.toml (sudo nano $boot/glovebox.toml): your NAS,
     and anything else you want to change. Wi-Fi and SSH can stay as they are.
  2. $( [ -n "${reboot_needed:-}" ] && echo "Reboot: sudo reboot" || echo "Restart Glovebox: sudo systemctl restart glovebox" )
     Glovebox applies the file, moves passwords to root-only files, and
     leaves glovebox-status.txt next to it saying what it did.
  3. Open http://$ip:8080 and pair keep-awake on the Car page.
  4. Plug the Pi's USB port (not PWR) into the car's glovebox USB port.
EOF

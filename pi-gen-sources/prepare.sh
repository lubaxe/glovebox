#!/bin/bash -eu
#
# Sets up an RPi-Distro/pi-gen checkout to build the glovebox image.
# Run from the pi-gen folder:
#   prepare.sh <glovebox binary> <tesla-control binary>   (both linux/arm64)

SRC=$(dirname "$(readlink -f "$0")")
DEST=$(readlink -f .)

if [[ "$DEST" != */pi-gen ]]
then
  echo "$0 should be called from the RPi-Distro pi-gen folder"
  exit 1
fi
if [ $# -ne 2 ] || [ ! -f "$1" ] || [ ! -f "$2" ]
then
  echo "usage: $0 <glovebox binary> <tesla-control binary>   (both linux/arm64)"
  exit 1
fi
GLOVEBOX_BIN=$(readlink -f "$1")
TESLA_CONTROL_BIN=$(readlink -f "$2")

cp "$SRC/pi-gen-config" config

# stage0: only the Zero 2 W's kernel (no Pi 5 kernel, no kernel headers),
# no locales, and no docs, man pages or translations from the start
rm -f stage0/02-firmware/01-packages
echo "initramfs-tools raspi-firmware linux-image-rpi-v8" > stage0/02-firmware/01-packages-nr
rm -rf stage0/01-locale
# install stage1's few packages (raspi-config, netbase, systemd-timesyncd)
# without their recommended extras
for f in stage1/*/00-packages
do
  mv "$f" "$f-nr"
done
rm -rf stage0/00a-nodoc
cp -r "$SRC/00a-nodoc" stage0/00a-nodoc

# no first-boot user rename (userconf-pi); glovebox has its own setup
rm -f export-image/01-user-rename/00-packages
# export-image normally runs "apt-get update" and a dist-upgrade after all
# stages. Our stage installs from the same repositories minutes earlier, and
# the image's root partition is sized before that step, without package
# lists, so fetching them again would fill it. Keep only the cleanup.
cat > export-image/02-set-sources/01-run.sh << 'EOF'
#!/bin/bash -e
rm -f "${ROOTFS_DIR}/etc/apt/apt.conf.d/51cache"
rm -f "${ROOTFS_DIR}/etc/apt/sources.list.d/00-temp.list"
find "${ROOTFS_DIR}/var/lib/apt/lists/" -type f -delete
on_chroot << CHROOT
apt-get clean
CHROOT
EOF

rm -rf stage_glovebox
mkdir -p stage_glovebox
touch stage_glovebox/EXPORT_IMAGE
cp stage2/prerun.sh stage_glovebox/prerun.sh
cp -r "$SRC/00-glovebox" stage_glovebox/
cp "$GLOVEBOX_BIN" stage_glovebox/00-glovebox/files/glovebox
cp "$TESLA_CONTROL_BIN" stage_glovebox/00-glovebox/files/tesla-control
cp "$SRC/../LICENSE" "$SRC/../THIRD-PARTY-NOTICES.md" stage_glovebox/00-glovebox/files/
cp "$SRC/../deploy/glovebox.service" stage_glovebox/00-glovebox/files/glovebox.service
chmod +x stage_glovebox/00-glovebox/00-run.sh stage0/00a-nodoc/00-run.sh

echo 'Build config set. Now use "./build.sh" or "./build-docker.sh" to build the glovebox image.'

#!/bin/bash -e
# The glovebox image: pi-gen's stage0 and stage1 plus this stage, instead of
# Raspberry Pi OS Lite's stage2. Only what glovebox needs is installed (see
# 00-packages-nr); glovebox itself sets the Pi up at first boot from
# glovebox.toml on the boot partition.

# glovebox
install -m 755 files/glovebox "${ROOTFS_DIR}/usr/local/bin/glovebox"
install -d "${ROOTFS_DIR}/usr/local/sbin"
install -m 755 files/glovebox-boot-maintenance "${ROOTFS_DIR}/usr/local/sbin/glovebox-boot-maintenance"
# Tesla's Bluetooth tool, for keep-awake
install -m 755 files/tesla-control "${ROOTFS_DIR}/usr/local/bin/tesla-control"
# their licenses
install -d "${ROOTFS_DIR}/usr/share/doc/glovebox"
install -m 644 files/LICENSE files/THIRD-PARTY-NOTICES.md "${ROOTFS_DIR}/usr/share/doc/glovebox/"
install -m 644 files/glovebox.service "${ROOTFS_DIR}/etc/systemd/system/glovebox.service"
install -d -m 700 "${ROOTFS_DIR}/etc/glovebox"
install -m 600 files/glovebox.toml "${ROOTFS_DIR}/etc/glovebox/glovebox.toml"
# the file to edit on a PC before first boot
install -m 644 files/glovebox-boot.toml "${ROOTFS_DIR}/boot/firmware/glovebox.toml"

# USB device mode, so the Pi can be the car's drive
echo "dtoverlay=dwc2" >> "${ROOTFS_DIR}/boot/firmware/config.txt"

# Keep the root partition at its built size: glovebox puts the backingfiles
# partition in the free space after it at first boot.
sed -i 's/ resize\b//' "${ROOTFS_DIR}/boot/firmware/cmdline.txt"

# Wi-Fi power saving costs about 5% throughput and makes latency spiky,
# which slows archive uploads. The Pi runs on the car's power anyway.
install -d "${ROOTFS_DIR}/etc/NetworkManager/conf.d"
printf '[connection]\nwifi.powersave = 2\n' > "${ROOTFS_DIR}/etc/NetworkManager/conf.d/50-glovebox-wifi-powersave.conf"

# Logs live in RAM: the car cuts power without warning, and constant log
# writes wear out the SD card.
install -d "${ROOTFS_DIR}/etc/systemd/journald.conf.d"
printf '[Journal]\nStorage=volatile\nRuntimeMaxUse=20M\n' > "${ROOTFS_DIR}/etc/systemd/journald.conf.d/50-glovebox.conf"

# Bluetooth starts unblocked (glovebox also unblocks Wi-Fi and Bluetooth at
# every start)
install -d "${ROOTFS_DIR}/var/lib/systemd/rfkill"
for addr in 3f215040.serial soc; do
	echo 0 > "${ROOTFS_DIR}/var/lib/systemd/rfkill/platform-${addr}:bluetooth"
done

# SSH with keys only; the login user has no password at all
install -d "${ROOTFS_DIR}/etc/ssh/sshd_config.d"
cat > "${ROOTFS_DIR}/etc/ssh/sshd_config.d/50-glovebox.conf" << 'EOF'
PasswordAuthentication no
KbdInteractiveAuthentication no
PermitRootLogin no
EOF
echo "${FIRST_USER_NAME} ALL=(ALL) NOPASSWD: ALL" > "${ROOTFS_DIR}/etc/sudoers.d/010_${FIRST_USER_NAME}-nopasswd"
chmod 440 "${ROOTFS_DIR}/etc/sudoers.d/010_${FIRST_USER_NAME}-nopasswd"
# host keys are generated on first boot, so every card has its own
rm -f "${ROOTFS_DIR}"/etc/ssh/ssh_host_*

on_chroot << EOF
systemctl enable glovebox ssh
usermod -p '*' ${FIRST_USER_NAME}
usermod -p '*' root
for group in sudo netdev adm; do getent group \$group > /dev/null || groupadd -r \$group; done
usermod -aG sudo,netdev,adm ${FIRST_USER_NAME}
apt-get clean
EOF

# package indexes aren't needed to run; "apt update" fetches them again
rm -rf "${ROOTFS_DIR}"/var/lib/apt/lists/*

#!/bin/bash -e
# Runs in pi-gen's stage0 right after the base system is bootstrapped:
# keep documentation, man pages and translations out of the image. Only the
# copyright files stay, which the packages' licenses require.

install -d "${ROOTFS_DIR}/etc/dpkg/dpkg.cfg.d"
cat > "${ROOTFS_DIR}/etc/dpkg/dpkg.cfg.d/01-glovebox-nodoc" << 'EOF'
path-exclude /usr/share/doc/*
path-include /usr/share/doc/*/copyright
path-exclude /usr/share/man/*
path-exclude /usr/share/info/*
path-exclude /usr/share/locale/*
path-include /usr/share/locale/locale.alias
path-exclude /usr/share/lintian/*
EOF

# and remove what debootstrap already installed
find "${ROOTFS_DIR}/usr/share/doc" -type f ! -name copyright -delete
find "${ROOTFS_DIR}/usr/share/doc" -mindepth 1 -type d -empty -delete
rm -rf "${ROOTFS_DIR}"/usr/share/man/* "${ROOTFS_DIR}"/usr/share/info/* "${ROOTFS_DIR}"/usr/share/lintian/*
find "${ROOTFS_DIR}/usr/share/locale" -mindepth 1 -maxdepth 1 ! -name locale.alias -exec rm -rf {} +

#!/bin/bash -eu
#
# Build a glovebox image with pi-gen, from this checkout: compiles glovebox for
# the Pi and builds a minimal 64-bit Raspberry Pi OS image around it.
# Runs on a Debian/Ubuntu host with Go installed, including WSL2 on Windows.
#
# usage: build-image.sh [--docker]
#
#   --docker   build with pi-gen's build-docker.sh instead of build.sh
#
# Environment:
#   WORKDIR     where pi-gen is cloned and built (default ~/glovebox-build).
#               Must be on a Linux filesystem, not /mnt/c.
#   PI_GEN_REF  pi-gen tag or branch to build with. Must match RELEASE in
#               pi-gen-config.
#   GLOVEBOX_VERSION  the version glovebox reports (default: git describe)

SRC=$(dirname "$(readlink -f "$0")")
WORKDIR=${WORKDIR:-$HOME/glovebox-build}
PI_GEN_REF=${PI_GEN_REF:-2026-10-06-raspios-trixie-arm64}

USE_DOCKER=false
for arg in "$@"
do
  case "$arg" in
    --docker) USE_DOCKER=true ;;
    *) sed -n '3,16p' "$0"; exit 1 ;;
  esac
done

case "$(readlink -f "$WORKDIR")" in
  /mnt/*)
    echo "WORKDIR ($WORKDIR) must be on a Linux filesystem, not a Windows drive"
    exit 1
    ;;
esac

mkdir -p "$WORKDIR"

VERSION=${GLOVEBOX_VERSION:-$(git -C "$SRC/.." -c safe.directory='*' describe --always --dirty 2> /dev/null || echo dev)}
echo "Building glovebox $VERSION for linux/arm64"
(cd "$SRC/.." && GOOS=linux GOARCH=arm64 CGO_ENABLED=0   go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$WORKDIR/glovebox" ./cmd/glovebox)

# Tesla's tool for talking to the car over Bluetooth, for keep-awake. Its
# go.mod has replace directives, so "go install pkg@version" refuses it;
# build it from a checkout of a pinned commit instead.
VEHICLE_COMMAND=${VEHICLE_COMMAND:-a4b43c1eff0e}
echo "Building tesla-control at $VEHICLE_COMMAND"
if [ ! -d "$WORKDIR/vehicle-command" ]
then
  git clone -q https://github.com/teslamotors/vehicle-command.git "$WORKDIR/vehicle-command"
fi
git -C "$WORKDIR/vehicle-command" fetch -q origin
git -C "$WORKDIR/vehicle-command" checkout -q "$VEHICLE_COMMAND"
(cd "$WORKDIR/vehicle-command" && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$WORKDIR/tesla-control" ./cmd/tesla-control)

cd "$WORKDIR"
if [ ! -d pi-gen ]
then
  git clone https://github.com/RPi-Distro/pi-gen.git
fi
cd pi-gen
# throw away changes from a previous prepare.sh run before switching refs
git reset -q --hard
git clean -q -fdx -e work -e deploy
git fetch -q --tags origin
git checkout -q "$PI_GEN_REF"
echo "Using pi-gen $PI_GEN_REF"

"$SRC/prepare.sh" "$WORKDIR/glovebox" "$WORKDIR/tesla-control"
# pi-gen insists on a password for the login user; the image removes it
echo "FIRST_USER_PASS=$(head -c 24 /dev/urandom | base64 | tr -dc 'A-Za-z0-9')" >> config

if [ "$USE_DOCKER" = "true" ]
then
  # reuse the container from a previous build if there is one
  if docker ps -a --filter name=pigen_work -q | grep -q .
  then
    export CONTINUE=1
  fi
  ./build-docker.sh
else
  # install pi-gen's build dependencies ("tool:package" or "package" per line)
  missing=()
  while IFS= read -r dep
  do
    pkg=${dep##*:}
    cmd=${dep%%:*}
    if ! command -v "$cmd" &> /dev/null && ! dpkg -s "$pkg" &> /dev/null
    then
      missing+=("$pkg")
    fi
  done < <(grep -v '^ *$' depends)
  # Ubuntu's qemu-user binaries are dynamically linked, so the binfmt
  # handlers from qemu-user-binfmt can't run inside pi-gen's chroot.
  # Use the statically linked qemu-user-static handlers there instead.
  if [ "$(. /etc/os-release && echo "$ID")" = "ubuntu" ]
  then
    for i in "${!missing[@]}"
    do
      [ "${missing[$i]}" = "qemu-user-binfmt" ] && unset 'missing[i]'
    done
    dpkg -s qemu-user &> /dev/null || missing+=(qemu-user)
    dpkg -s qemu-user-static &> /dev/null || missing+=(qemu-user-static)
  fi
  if [ ${#missing[@]} -gt 0 ]
  then
    echo "Installing pi-gen dependencies: ${missing[*]}"
    sudo apt-get update
    sudo apt-get install -y "${missing[@]}"
  fi
  sudo ./build.sh
fi

echo
echo "Images in $WORKDIR/pi-gen/deploy:"
ls -l deploy/
if grep -qi microsoft /proc/version
then
  # wslpath -w turns /home/... into \\wsl.localhost\<distro>\home\...
  echo "From Windows: $(wslpath -w deploy 2> /dev/null || readlink -f deploy)"
fi

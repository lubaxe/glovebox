# Power loss and read-only boot maintenance

Glovebox normally keeps the standard Raspberry Pi boot filesystem at
**exactly `/boot/firmware`, mounted as `vfat`, read-only**. Runtime boot writes
use short guarded writable windows. This reduces exposure of the kernel,
firmware and FAT metadata when the car cuts USB power; it does not make the SD
card, the writable root filesystem, or OS updates power-loss-proof.

## Installation and existing devices

The image installs `/usr/local/sbin/glovebox-boot-maintenance` and sets the exact
`/boot/firmware` entry in the final image's `/etc/fstab` to `ro`. The installer
downloads the same helper as the checksummed `glovebox-boot-maintenance` release
asset. It stops an existing Glovebox service before package installation or boot
writes, persists `ro` for that exact fstab entry, and leaves boot read-only after
a successful install. On command failure it syncs and restores the original
runtime mount mode. A missing service on first installation is not an error.

Existing installed devices can use the normal web update. On its first restart,
the new installed binary migrates the standard exact-vfat boot layout: it
installs the embedded helper at the same path and persists the read-only fstab
entry. Rerunning the installer is not required for that migration. This is a
startup migration, not an apt hook or an additional asset fetched by the old
updater. Development/custom executable locations and `-no-usb` runs are outside
that automatic installed-device migration; check startup logs if it fails.

If persistent setup fails after runtime protection succeeds, the drive and web
interface remain available. Startup logs and `/api/status` warnings report the
failure; the web interface shows a **Needs attention** chip and a footer
alert. This is a degraded persistent-setup warning, not a whole-device outage:
the currently running boot mount remains read-only, but helper installation or
fstab persistence may be incomplete. Migration retries idempotently on each
restart. Resolve the warning before relying on protection at the next boot or
using the maintenance command. A runtime protection error itself still fails
closed before provisioning rather than starting unprotected.

Protection targets standard `/boot/firmware` only. A custom `-boot-config` path
outside that directory must be managed independently. Generic own-UDC boards
retain root-filesystem installation without a boot remount or boot-protection
claim. Legacy Pi `/boot` installs also remain supported as explicitly unmanaged:
the installer warns, requires the boot files' containing filesystem to already
be writable, and uses the same `--no-boot` service-stop coordination without
remounting boot or changing its fstab entry. It rechecks writability after
stopping the service. Only standard `/boot/firmware` gets automatic read-only
protection. When that standard path is selected, a directory that is not an
exact vfat mount is an error, not a reason to fall back to unmanaged mode.
Neither the helper nor migration may treat a directory on the root filesystem
as a boot mount or remount `/` as a fallback.

## OS, kernel and firmware updates

Use a stable external supply, not the car's USB power, and keep a backup/recovery
card. Boot read-only protection is not a transactional kernel update or an A/B
boot scheme. An interrupted writable window can still leave the Pi unbootable.
The car's drive and Glovebox web interface are unavailable while the service is
stopped. Run maintenance over SSH, disconnected from the car's recording port.

Run all commands that might write boot files in one foreground maintenance
operation, for example:

This example is for the standard protected layout. Generic/legacy unmanaged
installs can use `--no-boot` for service coordination, but must manage their
boot filesystem's writability and protection independently; that option never
remounts any filesystem and does not provide the standard boot guarantee.

```sh
sudo /usr/local/sbin/glovebox-boot-maintenance --protect \
  bash -eu -o pipefail -c 'apt-get update && apt-get full-upgrade'
```

The helper takes a nonblocking `flock` on
`/run/lock/glovebox-boot-maintenance.lock`, checks the exact mount and filesystem,
stops `glovebox.service` and confirms it stopped, remounts only `/boot/firmware`
writable, runs the command, syncs, restores protection, then starts the service
only if it was running before. Without `--protect`, it restores the original
mode even on success; `--protect` requests `ro` after success. Command failure
and catchable HUP/INT/TERM restore the original mode, not an unconditional `ro`.
An originally inactive service stays inactive. If the restoration cannot be
verified, the helper reports failure and leaves Glovebox stopped. Investigate
the mount and filesystem before manually starting the service. Inspect a failed
package operation and repair it inside another maintenance window before reboot.

Do not start/restart Glovebox inside the maintenance command, run another
Glovebox binary, start background jobs, or reboot from inside the command.
Reboot separately after it returns successfully and confirms the desired mode:

```sh
findmnt --kernel --mountpoint /boot/firmware -o TARGET,FSTYPE,OPTIONS
sudo reboot
```

The runtime boot manager's lock is process-local. It does not share the helper's
`flock`: **stopping the service is the coordination mechanism**. The helper lock
serializes cooperating maintenance/installer invocations only. It does not block
an administrator, another updater, or a service manager from independently
starting Glovebox or writing/remounting boot. Ensure nothing else competes for
that window. `SIGKILL`, power loss and a kernel crash cannot run shell cleanup.

### Unattended updates

An existing unattended OS/kernel/firmware updater may now fail because boot is
read-only. Inventory its timers, services and configuration before upgrading.
Schedule boot-writing OS updates through the maintenance command, or explicitly
disable/reconfigure competing automatic firmware/kernel updates using that
updater's documented controls. Ensure an already-running update has finished
before opening your own window. Do not merely run a second apt command and rely
on its package-manager lock to coordinate boot writes.

Glovebox does **not** silently disable user services, intercept arbitrary apt
hooks, or wrap every package-manager entry point. A plain `apt`, `dpkg`, firmware
updater or a vendor post-install script run outside the controlled window is
not covered. Unattended services that automatically reboot or restart Glovebox
must also be configured not to do so inside maintenance. Schedule a separate
reboot only after the helper has closed the writable window successfully.

## Validation without a Pi

Run `node --test scripts/boot-maintenance.test.mjs` for mocked shell regression
coverage, and `bash check.sh` for ShellCheck. The shell tests replace mount,
service, sync and lock commands and relocate the lock into a temporary fixture;
they never remount a live filesystem or contact a Pi. They exercise ordering
and failure cleanup, not a real systemd, flock contention, SD-card durability,
or interrupted-power recovery. Validate the final generated image's fstab and
perform controlled hardware recovery testing before deployment.

import assert from 'node:assert/strict';
import {spawnSync} from 'node:child_process';
import {existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync} from 'node:fs';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import {fileURLToPath} from 'node:url';
import test from 'node:test';

const bash = process.env.BASH || (process.platform === 'win32' ? 'C:/Program Files/Git/bin/bash.exe' : 'bash');
const helper = readFileSync(new URL('../internal/bootfs/assets/glovebox-boot-maintenance', import.meta.url), 'utf8');
const mocks = String.raw`
source "$TEST_TMP/helper"
id() { printf '0\n'; }
flock() { printf 'flock %s\n' "$*" >> "$TEST_TMP/log"; return "$LOCK_STATUS"; }
findmnt() {
  [[ "$*" = '--kernel --mountpoint /boot/firmware --noheadings --raw --output TARGET,FSTYPE,OPTIONS' ]] || return 2
  [[ "$MISSING_MOUNT" = 0 ]] || return 1
  printf '%s %s %s\n' "$(cat "$TEST_TMP/target")" "$FILESYSTEM" "$(cat "$TEST_TMP/mode")"
}
systemctl() {
  case "$1" in
    show)
      if [[ "$2" = --property=LoadState ]]; then
        echo "$LOAD_STATE"
        return "$LOAD_STATUS"
      fi
      cat "$TEST_TMP/service" ;;
    stop)
      echo stop >> "$TEST_TMP/log"
      [[ "$STOP_STATUS" = 0 ]] || return "$STOP_STATUS"
      [[ "$STOP_STUCK" = 1 ]] || echo inactive > "$TEST_TMP/service"
      ;;
    start) echo start >> "$TEST_TMP/log"; echo active > "$TEST_TMP/service" ;;
    *) return 2 ;;
  esac
}
mount() {
  [[ $# = 3 && "$1" = -o && "$3" = /boot/firmware ]] || return 2
  printf 'mount %s\n' "$*" >> "$TEST_TMP/log"
  [[ "$2" != "$FAIL_REMOUNT" ]] || return 1
  case "$2" in
    remount,ro) echo ro > "$TEST_TMP/mode" ;;
    remount,rw) echo rw > "$TEST_TMP/mode" ;;
    *) return 2 ;;
  esac
}
sync() { echo sync >> "$TEST_TMP/log"; return "$SYNC_STATUS"; }
work() {
  echo work >> "$TEST_TMP/log"
  [[ "$INVALIDATE_MOUNT" = 0 ]] || echo / > "$TEST_TMP/target"
  if [[ "$SIGNAL" != none ]]; then kill -s "$SIGNAL" "$BASHPID"; fi
  return "$WORK_STATUS"
}
boot_maintenance $PROTECT work
`;

function fixture(run) {
  const directory = mkdtempSync(join(tmpdir(), 'glovebox-boot-test-'));
  try {
    writeFileSync(join(directory, 'helper'), helper.replace('exec 9>/run/lock/glovebox-boot-maintenance.lock', 'exec 9>"$TEST_TMP/maintenance.lock"'));
    return run(directory);
  } finally {
    rmSync(directory, {recursive: true, force: true});
  }
}

function maintenance(options = {}) {
  return fixture(directory => {
    writeFileSync(join(directory, 'mode'), options.mode || 'ro');
    writeFileSync(join(directory, 'service'), options.service || 'active');
    writeFileSync(join(directory, 'target'), options.target || '/boot/firmware');
    const result = spawnSync(bash, ['-c', mocks], {encoding: 'utf8', env: {...process.env,
      TEST_TMP: directory, LOCK_STATUS: '0', FILESYSTEM: 'vfat', MISSING_MOUNT: '0', STOP_STATUS: '0',
      STOP_STUCK: '0', FAIL_REMOUNT: '', SYNC_STATUS: '0', WORK_STATUS: '0', INVALIDATE_MOUNT: '0',
      SIGNAL: 'none', PROTECT: '', LOAD_STATE: 'loaded', LOAD_STATUS: '0', ...options.env}});
    assert.ifError(result.error);
    return {...result, mode: readFileSync(join(directory, 'mode'), 'utf8').trim(),
      log: existsSync(join(directory, 'log')) ? readFileSync(join(directory, 'log'), 'utf8').trim().split('\n') : []};
  });
}

test('active service stops before writes and restarts only after sync and read-only restoration', () => {
  const result = maintenance();
  assert.equal(result.status, 0, result.stderr);
  assert.equal(result.mode, 'ro');
  assert.deepEqual(result.log, ['flock -n 9', 'stop', 'mount -o remount,rw /boot/firmware', 'work', 'sync', 'mount -o remount,ro /boot/firmware', 'start']);
});

test('inactive service stays stopped', () => {
  const result = maintenance({service: 'inactive'});
  assert.equal(result.status, 0, result.stderr);
  assert.ok(!result.log.includes('start'));
});

test('first install skips show ActiveState and stop for a missing unit', () => {
  const result = maintenance({env: {LOAD_STATE: 'not-found', LOAD_STATUS: '1'}});
  assert.equal(result.status, 0, result.stderr);
  assert.ok(!result.log.includes('stop'));
  assert.ok(!result.log.includes('start'));
  assert.ok(result.log.includes('work'));
  assert.equal(result.mode, 'ro');
});

test('generic installation stops service but never touches any boot mount', () => {
  const result = maintenance({target: '/', env: {PROTECT: '--no-boot'}});
  assert.equal(result.status, 0, result.stderr);
  assert.deepEqual(result.log, ['flock -n 9', 'stop', 'work', 'sync', 'start']);
});

test('legacy unmanaged installation coordinates service without mount fallback', () => {
  const result = maintenance({target: '/boot', env: {PROTECT: '--no-boot', MISSING_MOUNT: '1'}});
  assert.equal(result.status, 0, result.stderr);
  assert.deepEqual(result.log, ['flock -n 9', 'stop', 'work', 'sync', 'start']);
});

test('installer selects protection only for standard Pi boot, never as fallback', () => {
  const installer = readFileSync(new URL('./install.sh', import.meta.url), 'utf8');
  const selection = installer.match(/^maintenance_options=\(--no-boot\)\n[^\n]+\n/m)?.[0];
  assert.ok(selection);
  for (const [device, boot, expected] of [
    ['dwc2', '/boot', '--no-boot'], ['peripheral', '/boot', '--no-boot'],
    ['own', '', '--no-boot'], ['own', '/boot/firmware', '--no-boot'],
    ['dwc2', '/boot/firmware', '--protect'], ['peripheral', '/boot/firmware', '--protect'],
  ]) {
    const result = spawnSync(bash, ['-c', selection + 'printf "%s\\n" "${maintenance_options[@]}"'], {
      encoding: 'utf8', env: {...process.env, device_mode: device, boot},
    });
    assert.ifError(result.error);
    assert.equal(result.status, 0, result.stderr);
    assert.equal(result.stdout.trim(), expected);
  }
});

test('legacy boot writability checks inspect the containing filesystem without remounting it', () => {
  const installer = readFileSync(new URL('./install.sh', import.meta.url), 'utf8');
  const check = installer.match(/^unmanaged_boot_writable\(\) \{\n.*?^\}/ms)?.[0];
  assert.ok(check);
  for (const [options, expected] of [
    ['rw,relatime', 0], ['ro,relatime', 1], ['ro,rw', 1], ['relatime', 1], ['rw\nrw', 1],
  ]) {
    const command = check + '\nfindmnt() { [[ "$*" = "--kernel --target /boot/config.txt --noheadings --raw --output OPTIONS" ]] || return 2; printf "%s\\n" "$MOCK_OPTIONS"; }; unmanaged_boot_writable';
    const result = spawnSync(bash, ['-c', command], {encoding: 'utf8', env: {...process.env, boot: '/boot', MOCK_OPTIONS: options}});
    assert.ifError(result.error);
    assert.equal(result.status, expected, result.stderr);
  }
});

for (const mode of ['ro', 'rw']) {
  test('ordinary maintenance restores original ' + mode + ' mode', () => {
    const result = maintenance({mode});
    assert.equal(result.status, 0, result.stderr);
    assert.equal(result.mode, mode);
  });
  test('failed protected maintenance restores original ' + mode + ' mode', () => {
    const result = maintenance({mode, env: {PROTECT: '--protect', WORK_STATUS: '37'}});
    assert.equal(result.status, 37, result.stderr);
    assert.equal(result.mode, mode);
    assert.ok(result.log.includes('sync'));
  });
}

test('successful installation protects a previously writable mount', () => {
  const result = maintenance({mode: 'rw', env: {PROTECT: '--protect'}});
  assert.equal(result.status, 0, result.stderr);
  assert.equal(result.mode, 'ro');
});

for (const options of [
  {target: '/'}, {target: '/boot'}, {target: '/boot/firmware\n/'},
  {env: {FILESYSTEM: 'ext4'}}, {env: {MISSING_MOUNT: '1'}}, {mode: 'ro,rw'}, {mode: 'errors=remount-ro'},
]) {
  test('rejects non-exact or invalid mount ' + JSON.stringify(options), () => {
    const result = maintenance(options);
    assert.notEqual(result.status, 0);
    assert.deepEqual(result.log, ['flock -n 9']);
  });
}

test('flock rejects overlapping maintenance before service changes', () => {
  const result = maintenance({env: {LOCK_STATUS: '1'}});
  assert.notEqual(result.status, 0);
  assert.deepEqual(result.log, ['flock -n 9']);
});

for (const env of [{STOP_STATUS: '1'}, {STOP_STUCK: '1'}]) {
  test('failed service stop never opens a writable window ' + JSON.stringify(env), () => {
    const result = maintenance({env});
    assert.notEqual(result.status, 0);
    assert.deepEqual(result.log, ['flock -n 9', 'stop']);
    assert.equal(result.mode, 'ro');
  });
}

test('failed writable remount skips command and cleans up', () => {
  const result = maintenance({env: {FAIL_REMOUNT: 'remount,rw'}});
  assert.notEqual(result.status, 0);
  assert.ok(!result.log.includes('work'));
  assert.deepEqual(result.log.slice(-2), ['sync', 'start']);
  assert.equal(result.mode, 'ro');
});

test('failed restoration leaves service stopped and reports danger', () => {
  const result = maintenance({env: {FAIL_REMOUNT: 'remount,ro'}});
  assert.notEqual(result.status, 0);
  assert.ok(!result.log.includes('start'));
  assert.match(result.stderr, /FAILED to restore/);
});

test('failed successful-install protection returns failure and restores originally writable mode', () => {
  const result = maintenance({mode: 'rw', env: {PROTECT: '--protect', FAIL_REMOUNT: 'remount,ro'}});
  assert.notEqual(result.status, 0);
  assert.equal(result.mode, 'rw');
  assert.equal(result.log.at(-1), 'start');
});

test('changed mount is not remounted during cleanup', () => {
  const result = maintenance({env: {INVALIDATE_MOUNT: '1'}});
  assert.notEqual(result.status, 0);
  assert.ok(!result.log.includes('start'));
  assert.equal(result.log.filter(entry => entry.startsWith('mount ')).length, 1);
});

test('sync failure returns failure and still restores read-only', () => {
  const result = maintenance({env: {SYNC_STATUS: '1'}});
  assert.notEqual(result.status, 0);
  assert.equal(result.mode, 'ro');
});

test('TERM uses failure cleanup', () => {
  const result = maintenance({env: {SIGNAL: 'TERM'}});
  assert.equal(result.status, 143, result.stderr);
  assert.equal(result.mode, 'ro');
  assert.ok(result.log.includes('sync'));
});

function rewriteFstab(contents, syncStatus = '0') {
  return fixture(directory => {
    const fstab = join(directory, 'fstab');
    writeFileSync(fstab, contents);
    const result = spawnSync(bash, ['-c', 'source "$TEST_TMP/helper"; sync() { printf "%s\\n" "$*" >> "$TEST_TMP/sync-log"; return "$SYNC_STATUS"; }; persist_boot_ro "$TEST_TMP/fstab"'], {
      encoding: 'utf8', env: {...process.env, TEST_TMP: directory, SYNC_STATUS: syncStatus},
    });
    assert.ifError(result.error);
    return {...result, contents: readFileSync(fstab, 'utf8'), syncLog: existsSync(join(directory, 'sync-log')) ? readFileSync(join(directory, 'sync-log'), 'utf8') : ''};
  });
}

test('fstab rewrite preserves root, other boot paths and boot options; is idempotent', () => {
  const original = '# /boot/firmware is protected\nPARTUUID=root / ext4 defaults,rw 0 1\nPARTUUID=other /boot vfat defaults 0 2\nPARTUUID=boot /boot/firmware vfat defaults,rw,noatime,ro 0 2\n';
  const result = rewriteFstab(original);
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.contents, /^PARTUUID=boot \/boot\/firmware vfat defaults,noatime,ro 0 2$/m);
  assert.ok(result.contents.startsWith(original.split('PARTUUID=boot')[0]));
  assert.match(result.syncLog, /-f .*fstab\.glovebox\..*\n-f .*\n/);
  const unchanged = rewriteFstab(result.contents);
  assert.equal(unchanged.contents, result.contents);
  assert.equal(unchanged.syncLog, '');
});

test('fstab data-sync failure prevents rename and preserves the old file', () => {
  const original = 'PARTUUID=boot /boot/firmware vfat defaults 0 2\n';
  const result = rewriteFstab(original, '1');
  assert.notEqual(result.status, 0);
  assert.equal(result.contents, original);
  assert.equal(result.syncLog.trim().split('\n').length, 1);
});

for (const contents of [
  'PARTUUID=root / ext4 defaults 0 1\n',
  'PARTUUID=boot /boot/firmware ext4 defaults 0 2\n',
  'PARTUUID=boot /boot/firmware vfat defaults\n',
  'PARTUUID=boot /boot/firmware vfat defaults 0 2\nPARTUUID=boot /boot/firmware vfat rw 0 2\n',
]) {
  test('invalid fstab is unchanged: ' + JSON.stringify(contents), () => {
    const result = rewriteFstab(contents);
    assert.notEqual(result.status, 0);
    assert.equal(result.contents, contents);
  });
}

test('image prepare packages the canonical helper and final export protects generated fstab', () => {
  fixture(directory => {
    const repository = fileURLToPath(new URL('../', import.meta.url));
    for (const path of ['pi-gen/stage0/02-firmware', 'pi-gen/stage1/base', 'pi-gen/stage2', 'pi-gen/export-image/02-set-sources', 'root/usr/local/sbin', 'root/etc']) {
      mkdirSync(join(directory, path), {recursive: true});
    }
    for (const path of ['glovebox', 'tesla-control', 'pi-gen/stage1/base/00-packages', 'pi-gen/stage2/prerun.sh']) {
      writeFileSync(join(directory, path), 'fixture\n');
    }
    const prepared = spawnSync(bash, ['-c', 'cd "$TEST_TMP/pi-gen"; bash "$REPOSITORY/pi-gen-sources/prepare.sh" "$TEST_TMP/glovebox" "$TEST_TMP/tesla-control"'], {
      encoding: 'utf8', env: {...process.env, TEST_TMP: directory, REPOSITORY: repository},
    });
    assert.ifError(prepared.error);
    assert.equal(prepared.status, 0, prepared.stderr);
    const packaged = readFileSync(join(directory, 'pi-gen/stage_glovebox/00-glovebox/files/glovebox-boot-maintenance'), 'utf8');
    assert.equal(packaged, helper);
    writeFileSync(join(directory, 'root/usr/local/sbin/glovebox-boot-maintenance'), packaged);
    writeFileSync(join(directory, 'root/etc/fstab'), 'PARTUUID=root / ext4 defaults 0 1\nPARTUUID=boot /boot/firmware vfat defaults 0 2\n');
    const finalized = spawnSync(bash, ['-c', 'sync() { :; }; source "$TEST_TMP/pi-gen/export-image/045-glovebox-boot-ro/00-run.sh"'], {
      encoding: 'utf8', env: {...process.env, TEST_TMP: directory, ROOTFS_DIR: join(directory, 'root')},
    });
    assert.ifError(finalized.error);
    assert.equal(finalized.status, 0, finalized.stderr);
    assert.equal(readFileSync(join(directory, 'root/etc/fstab'), 'utf8'), 'PARTUUID=root / ext4 defaults 0 1\nPARTUUID=boot /boot/firmware vfat defaults,ro 0 2\n');
    // pi-gen runs export-image steps in name order, and 05-finalise packs
    // the image: the fstab change has to come before it, however the
    // build sorts
    const steps = ['04-set-partuuid', '045-glovebox-boot-ro', '05-finalise'];
    assert.deepEqual([...steps].sort(), steps);
    assert.ok('045-glovebox-boot-ro'.localeCompare('05-finalise', 'en-US') < 0);
  });
});

test('installer, release and lint all reference the canonical helper', () => {
  const installer = readFileSync(new URL('./install.sh', import.meta.url), 'utf8');
  const release = readFileSync(new URL('../.github/workflows/build-image.yml', import.meta.url), 'utf8');
  const lint = readFileSync(new URL('../check.sh', import.meta.url), 'utf8');
  assert.match(installer, /files=\(.*glovebox-boot-maintenance/);
  assert.match(installer, /install -m 755 "\$tmp\/glovebox-boot-maintenance" \/usr\/local\/sbin\/glovebox-boot-maintenance/);
  assert.match(release, /cp internal\/bootfs\/assets\/glovebox-boot-maintenance "\$out\/glovebox-boot-maintenance"/);
  assert.match(lint, /\.\/internal\/bootfs\/assets\/glovebox-boot-maintenance/);
});

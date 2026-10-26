package bootfs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

type migrationRunner func(context.Context, string, ...string) ([]byte, error)

func (runner migrationRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return runner(ctx, name, args...)
}

func migrationManager(listing string) *Manager {
	return New("/boot/firmware", migrationRunner(func(context.Context, string, ...string) ([]byte, error) { return []byte(listing), nil }))
}

const migrationMount = `{"filesystems":[{"target":"/boot/firmware","fstype":"vfat","options":"ro,relatime"}]}`
const migrationFstab = "# boot policy\nUUID=root / ext4 defaults,rw 0 1\nUUID=boot\t/boot/firmware\tvfat\tdefaults,rw,noatime  0  2 # boot\nUUID=data /data vfat rw 0 2\n"

func migrationWrite(t *testing.T, filename, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(filename, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filename, mode); err != nil {
		t.Fatal(err)
	}
}

func migrationAssertContent(t *testing.T, filename string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s = %q, want %q", filename, got, want)
	}
}

func migrationStat(t *testing.T, filename string) os.FileInfo {
	t.Helper()
	info, err := os.Stat(filename)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func migrationAssertNoTemps(t *testing.T, directory string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(directory, ".*.glovebox-*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("temporary files = %v, error = %v", matches, err)
	}
}

func TestInstallProtectionPersistsAndIsIdempotent(t *testing.T) {
	directory := t.TempDir()
	fstab, helper := filepath.Join(directory, "fstab"), filepath.Join(directory, "helper")
	migrationWrite(t, fstab, migrationFstab, 0640)
	before := migrationStat(t, fstab)
	manager := migrationManager(migrationMount)
	if err := manager.InstallProtection(context.Background(), fstab, helper); err != nil {
		t.Fatal(err)
	}
	migrationAssertContent(t, fstab, []byte(strings.Replace(migrationFstab, "defaults,rw,noatime", "defaults,ro,noatime", 1)))
	migrationAssertContent(t, helper, maintenanceHelper)
	after := migrationStat(t, fstab)
	if after.Mode() != before.Mode() {
		t.Fatalf("fstab mode = %v, want %v", after.Mode(), before.Mode())
	}
	if runtime.GOOS != "windows" {
		if migrationStat(t, helper).Mode().Perm() != 0755 {
			t.Fatal("helper not executable")
		}
		oldStat, newStat := reflect.Indirect(reflect.ValueOf(before.Sys())), reflect.Indirect(reflect.ValueOf(after.Sys()))
		for _, field := range []string{"Uid", "Gid"} {
			if oldStat.FieldByName(field).Uint() != newStat.FieldByName(field).Uint() {
				t.Fatalf("fstab %s changed", field)
			}
		}
	}
	timestamp := time.Unix(1700000000, 0)
	for _, filename := range []string{fstab, helper} {
		if err := os.Chtimes(filename, timestamp, timestamp); err != nil {
			t.Fatal(err)
		}
	}
	fstabBefore, helperBefore := migrationStat(t, fstab), migrationStat(t, helper)
	if err := manager.InstallProtection(context.Background(), fstab, helper); err != nil {
		t.Fatal(err)
	}
	for filename, previous := range map[string]os.FileInfo{fstab: fstabBefore, helper: helperBefore} {
		current := migrationStat(t, filename)
		if !current.ModTime().Equal(previous.ModTime()) || !os.SameFile(previous, current) {
			t.Fatalf("no-op rewrote %s", filename)
		}
	}
	migrationAssertNoTemps(t, directory)
}

func TestProtectionFstabFormattingAndOptions(t *testing.T) {
	for _, test := range []struct{ options, want string }{
		{"rw,noatime", "ro,noatime"}, {"defaults", "defaults,ro"}, {"ro,noatime", "ro,noatime"},
		{"rw,ro,rw,noatime", "ro,noatime"}, {"defaults,errors=remount-ro,rw", "defaults,errors=remount-ro,ro"},
	} {
		t.Run(test.options, func(t *testing.T) {
			prefix, suffix := "  UUID=boot\t/boot/firmware  vfat\t", "\t0 2 # keep\r\n# tail"
			got, err := protectionFstab([]byte(prefix+test.options+suffix), "/boot/firmware")
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != prefix+test.want+suffix {
				t.Fatalf("fstab = %q", got)
			}
		})
	}
	got, err := protectionFstab([]byte(`UUID=boot /boot/firmware vfat defaults 0 2
UUID=data /media/some\040disk ext4 rw 0 2`), "/boot/firmware")
	if err != nil || !strings.Contains(string(got), `/media/some\040disk ext4 rw`) {
		t.Fatalf("escaped other mount: %q, %v", got, err)
	}
}

func TestInstallProtectionRejectsBeforeMutation(t *testing.T) {
	valid := "UUID=boot /boot/firmware vfat rw 0 2\n"
	for name, content := range map[string]string{
		"empty": "", "missing": "UUID=root / ext4 rw 0 1\n", "duplicate": valid + valid,
		"mixed duplicate": valid + "UUID=other /boot/firmware ext4 rw 0 2\n",
		"incorrect type":  "UUID=boot /boot/firmware ext4 rw 0 2\n",
		"malformed":       "UUID=boot /boot/firmware vfat\n", "bad number": "UUID=boot /boot/firmware vfat rw zero 2\n",
		"extra field": valid[:len(valid)-1] + " extra\n", "empty option": "UUID=boot /boot/firmware vfat rw,,defaults 0 2\n",
		"noncanonical":   "UUID=boot /boot/firmware/ vfat rw 0 2\n",
		"late malformed": valid + "broken\n",
	} {
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			fstab, helper := filepath.Join(directory, "fstab"), filepath.Join(directory, "helper")
			migrationWrite(t, fstab, content, 0644)
			migrationWrite(t, helper, "old helper", 0755)
			before := migrationStat(t, fstab)
			if err := migrationManager(migrationMount).InstallProtection(context.Background(), fstab, helper); err == nil {
				t.Fatal("accepted invalid fstab")
			}
			migrationAssertContent(t, fstab, []byte(content))
			migrationAssertContent(t, helper, []byte("old helper"))
			after := migrationStat(t, fstab)
			if !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
				t.Fatal("changed rejected fstab")
			}
			migrationAssertNoTemps(t, directory)
		})
	}
}

func TestInstallProtectionUnmanagedNoChanges(t *testing.T) {
	for _, listing := range []string{
		`{"filesystems":[]}`,
		`{"filesystems":[{"target":"/","fstype":"ext4","options":"rw"}]}`,
		`{"filesystems":[{"target":"/boot/firmware","fstype":"ext4","options":"rw"}]}`,
	} {
		directory := t.TempDir()
		fstab, helper := filepath.Join(directory, "missing-fstab"), filepath.Join(directory, "missing-helper")
		if err := migrationManager(listing).InstallProtection(context.Background(), fstab, helper); err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(directory)
		if err != nil || len(entries) != 0 {
			t.Fatalf("unmanaged changed files: %v, %v", entries, err)
		}
	}
}

func TestInstallProtectionQueryFailuresDoNotTouchFiles(t *testing.T) {
	for _, listing := range []string{
		"not json",
		`{"filesystems":[{"target":"/boot/firmware","fstype":"vfat","options":"ro,rw"}]}`,
		`{"filesystems":[{"target":"/boot/firmware","fstype":"vfat","options":"ro"},{"target":"/boot/firmware","fstype":"vfat","options":"ro"}]}`,
	} {
		directory := t.TempDir()
		fstab, helper := filepath.Join(directory, "fstab"), filepath.Join(directory, "helper")
		migrationWrite(t, fstab, migrationFstab, 0644)
		migrationWrite(t, helper, "old helper", 0755)
		if err := migrationManager(listing).InstallProtection(context.Background(), fstab, helper); err == nil {
			t.Fatal("accepted failed mount detection")
		}
		migrationAssertContent(t, fstab, []byte(migrationFstab))
		migrationAssertContent(t, helper, []byte("old helper"))
		migrationAssertNoTemps(t, directory)
	}
	queryErr := errors.New("findmnt failed")
	manager := New("/boot/firmware", migrationRunner(func(context.Context, string, ...string) ([]byte, error) {
		return nil, queryErr
	}))
	if err := manager.InstallProtection(context.Background(), "nonexistent-fstab", "nonexistent-helper"); !errors.Is(err, queryErr) {
		t.Fatalf("query error = %v", err)
	}
}

func TestInstallProtectionMissingFilesAndHelperFailure(t *testing.T) {
	directory := t.TempDir()
	fstab, helper := filepath.Join(directory, "fstab"), filepath.Join(directory, "missing", "helper")
	manager := migrationManager(migrationMount)
	if err := manager.InstallProtection(context.Background(), fstab, helper); err == nil {
		t.Fatal("created missing fstab")
	}
	if _, err := os.Stat(fstab); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing fstab: %v", err)
	}
	migrationWrite(t, fstab, migrationFstab, 0644)
	if err := manager.InstallProtection(context.Background(), fstab, helper); err == nil {
		t.Fatal("expected missing helper parent error")
	}
	migrationAssertContent(t, fstab, []byte(migrationFstab))
	if err := os.Mkdir(filepath.Dir(helper), 0755); err != nil {
		t.Fatal(err)
	}
	if err := manager.InstallProtection(context.Background(), fstab, helper); err != nil {
		t.Fatalf("retry failed: %v", err)
	}
	migrationAssertContent(t, helper, maintenanceHelper)
	migrationAssertNoTemps(t, directory)
}

func TestInstallProtectionRepairsPartialInstallation(t *testing.T) {
	for _, phase := range []string{"helper only", "fstab only", "outdated helper", "wrong helper mode"} {
		t.Run(phase, func(t *testing.T) {
			if phase == "wrong helper mode" && runtime.GOOS == "windows" {
				t.Skip("Windows has no executable permission bits")
			}
			directory := t.TempDir()
			fstab, helper := filepath.Join(directory, "fstab"), filepath.Join(directory, "helper")
			content := migrationFstab
			if phase == "fstab only" {
				content = strings.Replace(content, "defaults,rw,noatime", "defaults,ro,noatime", 1)
			}
			migrationWrite(t, fstab, content, 0644)
			switch phase {
			case "helper only":
				migrationWrite(t, helper, string(maintenanceHelper), 0755)
			case "outdated helper":
				migrationWrite(t, helper, "old version", 0755)
			case "wrong helper mode":
				migrationWrite(t, helper, string(maintenanceHelper), 0644)
			}
			if err := migrationManager(migrationMount).InstallProtection(context.Background(), fstab, helper); err != nil {
				t.Fatal(err)
			}
			migrationAssertContent(t, helper, maintenanceHelper)
			migrationAssertContent(t, fstab, []byte(strings.Replace(migrationFstab, "defaults,rw,noatime", "defaults,ro,noatime", 1)))
			if runtime.GOOS != "windows" && migrationStat(t, helper).Mode().Perm() != 0755 {
				t.Fatal("helper mode not repaired")
			}
		})
	}
}

func TestInstallProtectionRejectsSymlinksAndSameFile(t *testing.T) {
	for _, target := range []string{"fstab", "helper", "same file", "hard link", "directory"} {
		t.Run(target, func(t *testing.T) {
			directory := t.TempDir()
			fstab, helper := filepath.Join(directory, "fstab"), filepath.Join(directory, "helper")
			original := filepath.Join(directory, "original")
			migrationWrite(t, original, migrationFstab, 0644)
			migrationWrite(t, fstab, migrationFstab, 0644)
			switch target {
			case "fstab":
				fstab = filepath.Join(directory, "fstab-link")
				if err := os.Symlink(original, fstab); err != nil {
					t.Skipf("symlink unsupported: %v", err)
				}
			case "helper":
				if err := os.Symlink(original, helper); err != nil {
					t.Skipf("symlink unsupported: %v", err)
				}
			case "same file":
				helper = fstab
			case "hard link":
				if err := os.Link(fstab, helper); err != nil {
					t.Skipf("hardlink unsupported: %v", err)
				}
			case "directory":
				if err := os.Mkdir(helper, 0755); err != nil {
					t.Fatal(err)
				}
			}
			if err := migrationManager(migrationMount).InstallProtection(context.Background(), fstab, helper); err == nil {
				t.Fatal("accepted unsafe target")
			}
			migrationAssertContent(t, fstab, []byte(migrationFstab))
			migrationAssertContent(t, original, []byte(migrationFstab))
			if target == "fstab" || target == "helper" {
				filename := fstab
				if target == "helper" {
					filename = helper
				}
				info, err := os.Lstat(filename)
				if err != nil || info.Mode()&os.ModeSymlink == 0 {
					t.Fatalf("symlink replaced: %v", err)
				}
			}
			migrationAssertNoTemps(t, directory)
		})
	}
}

func TestInstallProtectionCancellationAndSerialization(t *testing.T) {
	directory := t.TempDir()
	fstab, helper := filepath.Join(directory, "fstab"), filepath.Join(directory, "helper")
	migrationWrite(t, fstab, migrationFstab, 0644)
	manager := migrationManager(migrationMount)
	if err := manager.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := manager.InstallProtection(ctx, fstab, helper)
	manager.release()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("acquire = %v", err)
	}
	migrationAssertContent(t, fstab, []byte(migrationFstab))
	ctx, cancel = context.WithCancel(context.Background())
	manager = New("/boot/firmware", migrationRunner(func(context.Context, string, ...string) ([]byte, error) { cancel(); return []byte(migrationMount), nil }))
	if err := manager.InstallProtection(ctx, fstab, helper); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled query = %v", err)
	}
	if _, err := os.Stat(helper); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled query wrote helper: %v", err)
	}
	if err := migrationManager(migrationMount).InstallProtection(context.Background(), fstab, helper); err != nil {
		t.Fatal(err)
	}
}

type migrationUnknownOwner struct{ os.FileInfo }

func (info migrationUnknownOwner) Sys() any { return nil }

func TestMigrationReplaceFailuresPreserveOriginal(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target")
	if err := os.Mkdir(target, 0755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(target, "sentinel")
	migrationWrite(t, sentinel, "original", 0644)
	if err := migrationReplace(target, []byte("replacement"), 0644, nil); err == nil {
		t.Fatal("rename over directory succeeded")
	}
	migrationAssertContent(t, sentinel, []byte("original"))
	migrationAssertNoTemps(t, directory)
	if runtime.GOOS == "windows" {
		return
	}
	filename := filepath.Join(directory, "fstab")
	migrationWrite(t, filename, "original", 0640)
	before := migrationStat(t, filename)
	if err := migrationReplace(filename, []byte("replacement"), 0640, migrationUnknownOwner{before}); err == nil {
		t.Fatal("accepted unknown ownership")
	}
	migrationAssertContent(t, filename, []byte("original"))
	if !os.SameFile(before, migrationStat(t, filename)) {
		t.Fatal("failed pre-rename operation replaced original")
	}
	migrationAssertNoTemps(t, directory)
}

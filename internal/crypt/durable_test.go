package crypt

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

type scopeWriter struct {
	t      *testing.T
	calls  int
	active bool
	err    error
	after  error
	ctx    context.Context
}

func (w *scopeWriter) WithWrite(ctx context.Context, fn func() error) error {
	w.t.Helper()
	w.calls++
	if w.active {
		w.t.Fatal("nested writable scope")
	}
	w.ctx = ctx
	if w.err != nil {
		return w.err
	}
	w.active = true
	defer func() { w.active = false }()
	return errors.Join(fn(), w.after)
}

func TestRecoveryWritableScopes(t *testing.T) {
	rec, identity := newRecovery(t)
	w := &scopeWriter{t: t}
	rec.BootWriter = w
	if err := rec.SetRecipient(identity.Recipient().String()); err != nil {
		t.Fatal(err)
	}
	if err := rec.SaveVolumeKey(identity.Recipient().String(), []byte("key")); err != nil {
		t.Fatal(err)
	}
	if w.calls != 2 || w.active {
		t.Fatalf("scope calls=%d, active=%v", w.calls, w.active)
	}
	failure := errors.New("scope denied")
	w.err = failure
	if err := rec.SaveVolumeKey(identity.Recipient().String(), []byte("changed")); !errors.Is(err, failure) {
		t.Fatalf("SaveVolumeKey = %v", err)
	}
	key, err := rec.VolumeKey(identity.String())
	if err != nil || string(key) != "key" {
		t.Fatalf("previous key = %q, %v", key, err)
	}
}

func TestServiceSetRecipientSingleScope(t *testing.T) {
	fake := &fakeLUKS{exists: true, open: true, vk: []byte("key")}
	service, _ := newService(t, fake, nil)
	service.Created()
	w := &scopeWriter{t: t}
	service.Recovery.BootWriter = w
	_, replacement := newRecovery(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := service.SetRecipient(ctx, replacement.Recipient().String()); err != nil {
		t.Fatal(err)
	}
	if w.calls != 1 || w.ctx != ctx || w.active {
		t.Fatalf("scope calls=%d, ctx=%v, active=%v", w.calls, w.ctx, w.active)
	}
	key, err := service.Recovery.VolumeKey(replacement.String())
	if err != nil || string(key) != "key" {
		t.Fatalf("replacement key = %q, %v", key, err)
	}
}

func TestConversionPreflightPreventsDestruction(t *testing.T) {
	for _, test := range []string{"scope", "directory", "output", "canceled", "key-path", "key-length", "key-directory"} {
		t.Run(test, func(t *testing.T) {
			volume, runner := newVolume(t, &fakeLUKS{})
			recovery, identity := newRecovery(t)
			if err := recovery.SetRecipient(identity.Recipient().String()); err != nil {
				t.Fatal(err)
			}
			volume.Device = "unused-device"
			writer := &scopeWriter{t: t}
			recovery.BootWriter = writer
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch test {
			case "scope":
				writer.err = errors.New("read-only boot")
			case "directory":
				recovery.Dir = recovery.RecipientFile
			case "output":
				if err := os.Mkdir(recovery.HeaderPath(), 0o755); err != nil {
					t.Fatal(err)
				}
			case "canceled":
				cancel()
			case "key-path":
				volume.KeyFile = filepath.Join(recovery.RecipientFile, "device.key")
			case "key-length":
				if err := os.MkdirAll(filepath.Dir(volume.KeyFile), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(volume.KeyFile, []byte("short"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "key-directory":
				if err := os.MkdirAll(volume.KeyFile, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			released := false
			conversion := &Conversion{
				Volume: volume, Recovery: recovery, Runner: runner,
				Release: func(context.Context) error { released = true; return nil },
			}
			if err := conversion.Run(ctx); err == nil {
				t.Fatal("conversion succeeded")
			}
			if released || len(runner.Commands()) != 0 {
				t.Fatalf("destructive actions: release=%v, commands=%q", released, runner.Commands())
			}
		})
	}
}

func TestBackupHeaderPreservesPrevious(t *testing.T) {
	for _, test := range []string{"generation", "partial", "missing", "empty", "success"} {
		t.Run(test, func(t *testing.T) {
			fake := &fakeLUKS{exists: true}
			volume, runner := newVolume(t, fake)
			path := filepath.Join(t.TempDir(), HeaderFile)
			if err := os.WriteFile(path, []byte("previous"), 0o600); err != nil {
				t.Fatal(err)
			}
			failure := errors.New("header generation failed")
			runner.Handler = func(command string) ([]byte, error) {
				if !strings.HasPrefix(command, "cryptsetup luksHeaderBackup") {
					return fake.run(command)
				}
				arguments := strings.Fields(command)
				temporary := arguments[len(arguments)-1]
				if _, err := os.Stat(temporary); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("cryptsetup output already exists: %v", err)
				}
				if filepath.Dir(filepath.Dir(temporary)) != filepath.Dir(path) {
					t.Fatalf("not a sibling temporary directory: %s", temporary)
				}
				if test == "partial" || test == "success" {
					if err := os.WriteFile(temporary, []byte("replacement"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if test == "empty" {
					if err := os.WriteFile(temporary, nil, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if test == "generation" || test == "partial" {
					return nil, failure
				}
				return nil, nil
			}
			err := volume.BackupHeader(context.Background(), path)
			if (err == nil) != (test == "success") {
				t.Fatalf("BackupHeader = %v", err)
			}
			want := "previous"
			if test == "success" {
				want = "replacement"
			}
			data, err := os.ReadFile(path)
			if err != nil || string(data) != want {
				t.Fatalf("backup = %q, %v", data, err)
			}
			entries, err := os.ReadDir(filepath.Dir(path))
			if err != nil || len(entries) != 1 {
				t.Fatalf("temporary output remains: %v, %v", entries, err)
			}
		})
	}
}

type failedSyncCloser struct {
	syncError  error
	closeError error
	closed     bool
}

func (file *failedSyncCloser) Sync() error  { return file.syncError }
func (file *failedSyncCloser) Close() error { file.closed = true; return file.closeError }

func TestSyncAndCloseErrors(t *testing.T) {
	syncError := errors.New("sync failed")
	closeError := errors.New("close failed")
	for _, file := range []*failedSyncCloser{
		{syncError: syncError}, {closeError: closeError}, {syncError: syncError, closeError: closeError},
	} {
		err := syncAndClose(file)
		if !file.closed || file.syncError != nil && !errors.Is(err, syncError) || file.closeError != nil && !errors.Is(err, closeError) {
			t.Fatalf("sync/close error lost: %v, %+v", err, file)
		}
	}
}

func TestReplacementDirectorySyncFailure(t *testing.T) {
	directory := t.TempDir()
	sourceDir := filepath.Join(directory, "temporary")
	if err := os.Mkdir(sourceDir, 0o700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(sourceDir, "source")
	if err := os.WriteFile(source, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("directory sync failed")
	var synced []string
	err := replaceWithSync(source, filepath.Join(directory, "destination"), func(path string) error {
		synced = append(synced, path)
		return failure
	})
	if !errors.Is(err, failure) || !slices.Equal(synced, []string{directory, sourceDir}) {
		t.Fatalf("replace = %v, synced %q", err, synced)
	}
}

func TestAtomicReplacementCleanup(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "destination")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(path, []byte("new"), 0o600); err == nil {
		t.Fatal("replaced directory with a file")
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 1 || entries[0].Name() != "destination" {
		t.Fatalf("temporary files remain: %v, %v", entries, err)
	}
}

func TestDirectorySyncMissingPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not support directory fsync")
	}
	if err := syncDirectory(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing directory sync succeeded")
	}
}

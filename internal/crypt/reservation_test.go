package crypt

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func assertRecoveryReservation(t *testing.T, directory string, present bool) string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(directory, ".recovery-reservation-*"))
	if err != nil {
		t.Fatal(err)
	}
	if !present {
		if len(paths) != 0 {
			t.Fatalf("reservation remains: %q", paths)
		}
		return ""
	}
	if len(paths) != 1 {
		t.Fatalf("expected one reservation, found %q", paths)
	}
	info, err := os.Stat(paths[0])
	if err != nil || info.Size() != recoveryReservationSize {
		t.Fatalf("reservation not fully written: %v, %v", info, err)
	}
	return paths[0]
}

func TestConversionReservationFailurePreventsDestruction(t *testing.T) {
	for _, test := range []string{"ENOSPC", "canceled"} {
		t.Run(test, func(t *testing.T) {
			volume, runner := newVolume(t, &fakeLUKS{})
			recovery, identity := newRecovery(t)
			if err := recovery.SetRecipient(identity.Recipient().String()); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(recovery.HeaderPath(), []byte("existing-header"), 0o600); err != nil {
				t.Fatal(err)
			}
			volume.Device = "unused-device"
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			failure := error(syscall.ENOSPC)
			if test == "canceled" {
				failure = context.Canceled
			}
			scope := &scopeWriter{t: t}
			recovery.BootWriter = scope
			released := false
			conversion := &Conversion{
				Volume: volume, Recovery: recovery, Runner: runner,
				Release: func(context.Context) error { released = true; return nil },
				writeReservation: func(ctx context.Context, writer io.Writer, size int64) error {
					if size != recoveryReservationSize || !scope.active {
						t.Fatalf("reservation size=%d, scope=%v", size, scope.active)
					}
					data, err := os.ReadFile(recovery.HeaderPath())
					if err != nil || string(data) != "existing-header" {
						t.Fatalf("previous header changed before reservation: %q, %v", data, err)
					}
					if _, err := writer.Write(make([]byte, 64<<10)); err != nil {
						return err
					}
					if test == "canceled" {
						cancel()
						return writeRecoveryReservation(ctx, writer, size)
					}
					return &os.PathError{Op: "write", Path: "reservation", Err: syscall.ENOSPC}
				},
			}
			if err := conversion.Run(ctx); !errors.Is(err, failure) {
				t.Fatalf("conversion = %v, want %v", err, failure)
			}
			if released || len(runner.Commands()) != 0 {
				t.Fatalf("destructive actions: release=%v commands=%q", released, runner.Commands())
			}
			assertRecoveryReservation(t, recovery.Dir, false)
			data, err := os.ReadFile(recovery.HeaderPath())
			if err != nil || string(data) != "existing-header" {
				t.Fatalf("previous header lost: %q, %v", data, err)
			}
		})
	}
}

func TestConversionReservationCleanupOnFailures(t *testing.T) {
	for _, stage := range []string{"release", "losetup", "wipefs", "cryptsetup luksFormat", "cryptsetup open", "cryptsetup luksDump", "cryptsetup luksHeaderBackup"} {
		t.Run(stage, func(t *testing.T) {
			fake := &fakeLUKS{}
			volume, runner := newVolume(t, fake)
			recovery, identity := newRecovery(t)
			if err := recovery.SetRecipient(identity.Recipient().String()); err != nil {
				t.Fatal(err)
			}
			volume.Device = filepath.Join(t.TempDir(), "partition")
			if err := os.WriteFile(volume.Device, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			failure := errors.New("injected failure")
			runner.Handler = func(command string) ([]byte, error) {
				if strings.HasPrefix(command, stage) {
					return nil, failure
				}
				return fake.run(command)
			}
			conversion := &Conversion{
				Volume: volume, Recovery: recovery, Runner: runner,
				Release: func(context.Context) error {
					assertRecoveryReservation(t, recovery.Dir, true)
					if stage == "release" {
						return failure
					}
					return nil
				},
			}
			if err := conversion.Run(context.Background()); !errors.Is(err, failure) {
				t.Fatalf("conversion = %v, want injected failure", err)
			}
			assertRecoveryReservation(t, recovery.Dir, false)
		})
	}
}

func TestConversionRefusesExportsIfReservationReleaseFails(t *testing.T) {
	fake := &fakeLUKS{}
	volume, runner := newVolume(t, fake)
	recovery, identity := newRecovery(t)
	if err := recovery.SetRecipient(identity.Recipient().String()); err != nil {
		t.Fatal(err)
	}
	volume.Device = filepath.Join(t.TempDir(), "partition")
	if err := os.WriteFile(volume.Device, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	conversion := &Conversion{
		Volume: volume, Recovery: recovery, Runner: runner,
		Release: func(context.Context) error {
			return os.Remove(assertRecoveryReservation(t, recovery.Dir, true))
		},
	}
	if err := conversion.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "releasing recovery output space") {
		t.Fatalf("conversion = %v", err)
	}
	for _, command := range runner.Commands() {
		if strings.HasPrefix(command, "cryptsetup luksHeaderBackup") || strings.HasPrefix(command, "mkfs.xfs") {
			t.Fatalf("export or format after failed release: %s", command)
		}
	}
	if _, err := os.Stat(filepath.Join(recovery.Dir, VolumeKeyFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("sealed-key export after failed release: %v", err)
	}
}

type reservationTestWriter struct {
	write func([]byte) (int, error)
}

func (writer reservationTestWriter) Write(data []byte) (int, error) { return writer.write(data) }

func TestReservationWriting(t *testing.T) {
	if recoveryReservationSize < 2*luksMetadataSize+luksKeyslotsSize+1<<20 {
		t.Fatal("reservation does not cover the bounded new header and recovery exports")
	}
	var written int64
	writer := reservationTestWriter{write: func(data []byte) (int, error) {
		written += int64(len(data))
		return len(data), nil
	}}
	if err := writeRecoveryReservation(context.Background(), writer, recoveryReservationSize); err != nil || written != recoveryReservationSize {
		t.Fatalf("reservation write: %d bytes, %v", written, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	written = 0
	writer.write = func(data []byte) (int, error) {
		written += int64(len(data))
		cancel()
		return len(data), nil
	}
	if err := writeRecoveryReservation(ctx, writer, recoveryReservationSize); !errors.Is(err, context.Canceled) || written != 64<<10 {
		t.Fatalf("canceled write: %d bytes, %v", written, err)
	}
	writer.write = func(data []byte) (int, error) { return len(data) - 1, nil }
	if err := writeRecoveryReservation(context.Background(), writer, recoveryReservationSize); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short write = %v", err)
	}
}

func TestReservationReclaimsInterruptedOnes(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{".recovery-reservation-1", ".recovery-reservation-2"} {
		if err := os.WriteFile(filepath.Join(dir, name), make([]byte, 1<<10), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// not a reservation, and a directory with the prefix: both left alone
	if err := os.WriteFile(filepath.Join(dir, "volume-key.age"), []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, ".recovery-reservation-dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	reservation, err := reserveRecoverySpace(context.Background(), dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := filepath.Glob(filepath.Join(dir, ".recovery-reservation-*"))
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, path := range paths {
		if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() {
			files = append(files, path)
		}
	}
	if len(files) != 1 || files[0] != reservation.path {
		t.Fatalf("reservations = %q, want only %s", files, reservation.path)
	}
	if info, err := os.Stat(reservation.path); err != nil || info.Size() != recoveryReservationSize {
		t.Fatalf("current reservation: %v, %v", info, err)
	}
	if err := reservation.release(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"volume-key.age", ".recovery-reservation-dir"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s was removed: %v", name, err)
		}
	}
}

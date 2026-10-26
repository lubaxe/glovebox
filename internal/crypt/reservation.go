package crypt

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const recoveryReservationSize int64 = 32 << 20

func reclaimRecoveryReservations(ctx context.Context, directory string) (err error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	removed := false
	defer func() {
		if removed {
			err = errors.Join(err, syncDirectory(directory))
		}
	}()
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !strings.HasPrefix(entry.Name(), ".recovery-reservation-") {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		if err := os.Remove(path); err != nil {
			return err
		}
		removed = true
	}
	return nil
}

type recoveryReservation struct {
	path     string
	removed  bool
	released bool
}

func (reservation *recoveryReservation) release() error {
	if reservation.released {
		return nil
	}
	if !reservation.removed {
		if err := os.Remove(reservation.path); err != nil {
			return err
		}
		reservation.removed = true
	}
	if err := syncDirectory(filepath.Dir(reservation.path)); err != nil {
		return err
	}
	reservation.released = true
	return nil
}

func reserveRecoverySpace(ctx context.Context, directory string, write func(context.Context, io.Writer, int64) error) (reservation *recoveryReservation, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := reclaimRecoveryReservations(ctx, directory); err != nil {
		return nil, err
	}
	file, err := os.CreateTemp(directory, ".recovery-reservation-*")
	if err != nil {
		return nil, err
	}
	reservation = &recoveryReservation{path: file.Name()}
	defer func() {
		if err != nil {
			err = errors.Join(err, reservation.release())
			reservation = nil
		}
	}()
	if write == nil {
		write = writeRecoveryReservation
	}
	if err := write(ctx, file, recoveryReservationSize); err != nil {
		return reservation, errors.Join(err, file.Close())
	}
	if err := syncAndClose(file); err != nil {
		return reservation, err
	}
	if err := syncDirectory(directory); err != nil {
		return reservation, err
	}
	if err := ctx.Err(); err != nil {
		return reservation, err
	}
	return reservation, nil
}

func writeRecoveryReservation(ctx context.Context, writer io.Writer, size int64) error {
	buffer := make([]byte, 64<<10)
	for size > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		chunk := buffer[:min(int64(len(buffer)), size)]
		written, err := writer.Write(chunk)
		if err != nil {
			return err
		}
		if written != len(chunk) {
			return io.ErrShortWrite
		}
		size -= int64(written)
	}
	return ctx.Err()
}

package bootfs

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

//go:embed assets/glovebox-boot-maintenance
var maintenanceHelper []byte

var fstabField = regexp.MustCompile(`[^ \t\r\n]+`)

func (m *Manager) InstallProtection(ctx context.Context, fstabPath, helperPath string) error {
	if err := m.acquire(ctx); err != nil {
		return err
	}
	defer m.release()
	managed, _, err := m.state(ctx)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil || !managed {
		return err
	}
	fstab, fstabInfo, err := migrationReadFile(fstabPath, false)
	if err != nil {
		return fmt.Errorf("bootfs: read fstab: %w", err)
	}
	updated, err := protectionFstab(fstab, m.mountpoint)
	if err != nil {
		return err
	}
	helper, helperInfo, err := migrationReadFile(helperPath, true)
	if err != nil {
		return fmt.Errorf("bootfs: read maintenance helper: %w", err)
	}
	fstabAbsolute, err := filepath.Abs(fstabPath)
	if err != nil {
		return err
	}
	helperAbsolute, err := filepath.Abs(helperPath)
	if err != nil {
		return err
	}
	if fstabAbsolute == helperAbsolute || helperInfo != nil && os.SameFile(fstabInfo, helperInfo) {
		return errors.New("bootfs: fstab and helper must be different files")
	}
	if len(maintenanceHelper) == 0 {
		return errors.New("bootfs: empty embedded maintenance helper")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	helperModeCorrect := helperInfo != nil && migrationMode(helperInfo) == 0755
	if runtime.GOOS == "windows" && helperInfo != nil {
		helperModeCorrect = helperInfo.Mode().Perm()&0200 != 0
	}
	if !bytes.Equal(helper, maintenanceHelper) || !helperModeCorrect {
		if err := migrationReplace(helperPath, maintenanceHelper, 0755, helperInfo); err != nil {
			return fmt.Errorf("bootfs: install maintenance helper: %w", err)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !bytes.Equal(fstab, updated) {
		if err := migrationReplace(fstabPath, updated, migrationMode(fstabInfo), fstabInfo); err != nil {
			return fmt.Errorf("bootfs: persist boot protection: %w", err)
		}
	}
	return ctx.Err()
}

func migrationReadFile(filename string, missingOK bool) ([]byte, os.FileInfo, error) {
	info, err := os.Lstat(filename)
	if missingOK && errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("not a regular file: %s", filename)
	}
	content, err := os.ReadFile(filename)
	return content, info, err
}

func protectionFstab(content []byte, mountpoint string) ([]byte, error) {
	lines := strings.Split(string(content), "\n")
	matches := 0
	for lineNumber, line := range lines {
		entry, _, _ := strings.Cut(line, "#")
		fields := fstabField.FindAllStringIndex(entry, -1)
		if len(fields) == 0 {
			continue
		}
		if len(fields) < 4 || len(fields) > 6 {
			return nil, fmt.Errorf("bootfs: malformed fstab line %d", lineNumber+1)
		}
		for _, field := range fields[4:] {
			if _, err := strconv.ParseUint(entry[field[0]:field[1]], 10, 32); err != nil {
				return nil, fmt.Errorf("bootfs: malformed fstab numeric field on line %d", lineNumber+1)
			}
		}
		targetField := fields[1]
		target := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`, `\043`, "#").Replace(entry[targetField[0]:targetField[1]])
		if path.Clean(target) == mountpoint && target != mountpoint {
			return nil, fmt.Errorf("bootfs: noncanonical fstab target %q", target)
		}
		if target != mountpoint {
			continue
		}
		matches++
		typeField := fields[2]
		if entry[typeField[0]:typeField[1]] != "vfat" {
			return nil, errors.New("bootfs: boot fstab entry is not vfat")
		}
		optionField := fields[3]
		options := strings.Split(entry[optionField[0]:optionField[1]], ",")
		updated := make([]string, 0, len(options)+1)
		readOnly := false
		for _, option := range options {
			if option == "" {
				return nil, errors.New("bootfs: empty boot fstab option")
			}
			if option == "rw" || option == "ro" {
				if !readOnly {
					updated = append(updated, "ro")
					readOnly = true
				}
			} else {
				updated = append(updated, option)
			}
		}
		if !readOnly {
			updated = append(updated, "ro")
		}
		lines[lineNumber] = line[:optionField[0]] + strings.Join(updated, ",") + line[optionField[1]:]
	}
	if matches != 1 {
		return nil, fmt.Errorf("bootfs: expected one boot fstab entry, found %d", matches)
	}
	return []byte(strings.Join(lines, "\n")), nil
}

func migrationMode(info os.FileInfo) os.FileMode {
	return info.Mode() & (os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky)
}

func migrationReplace(filename string, content []byte, mode os.FileMode, owner os.FileInfo) (err error) {
	directory := filepath.Dir(filename)
	temporary, err := os.CreateTemp(directory, "."+filepath.Base(filename)+".glovebox-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer func() {
		if temporary != nil {
			err = errors.Join(err, temporary.Close())
		}
		if removeErr := os.Remove(temporaryName); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, removeErr)
		}
	}()
	if _, err = temporary.Write(content); err != nil {
		return err
	}
	if owner != nil && runtime.GOOS != "windows" {
		stat := reflect.Indirect(reflect.ValueOf(owner.Sys()))
		if !stat.IsValid() || stat.Kind() != reflect.Struct {
			return errors.New("bootfs: cannot determine file ownership")
		}
		uid, gid := stat.FieldByName("Uid"), stat.FieldByName("Gid")
		if !uid.IsValid() || !gid.IsValid() || !uid.CanUint() || !gid.CanUint() {
			return errors.New("bootfs: cannot determine file ownership")
		}
		if err = temporary.Chown(int(uid.Uint()), int(gid.Uint())); err != nil {
			return err
		}
	}
	if err = temporary.Chmod(mode); err != nil {
		return err
	}
	if err = temporary.Sync(); err != nil {
		return err
	}
	err = temporary.Close()
	temporary = nil
	if err != nil {
		return err
	}
	if err = os.Rename(temporaryName, filename); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		return nil
	}
	parent, err := os.Open(directory)
	if err != nil {
		return err
	}
	return errors.Join(parent.Sync(), parent.Close())
}

package storage

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lubaxe/glovebox/internal/system"
)

func TestMBR(t *testing.T) {
	const size = 64 << 20
	b, err := mbr(size, partTypeExFAT)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != 512 || b[510] != 0x55 || b[511] != 0xAA {
		t.Fatal("missing boot signature")
	}
	e := b[446:462]
	if e[4] != 0x07 {
		t.Errorf("type = %#x, want 0x07", e[4])
	}
	if start := binary.LittleEndian.Uint32(e[8:12]); start != 2048 {
		t.Errorf("start = %d, want 2048", start)
	}
	if count := binary.LittleEndian.Uint32(e[12:16]); count != size/512-2048 {
		t.Errorf("sectors = %d, want %d", count, size/512-2048)
	}
	for _, other := range [][]byte{b[462:478], b[478:494], b[494:510]} {
		for _, x := range other {
			if x != 0 {
				t.Fatal("only the first partition entry should be used")
			}
		}
	}
}

func TestMBRLimits(t *testing.T) {
	if _, err := mbr(1<<20, partTypeExFAT); err == nil {
		t.Error("1 MiB disk: want an error")
	}
	if _, err := mbr(3<<40, partTypeExFAT); err == nil {
		t.Error("3 TiB disk: want an error")
	}
}

func TestFsckExitCodes(t *testing.T) {
	for _, tc := range []struct {
		code         int
		wantRepaired bool
		wantErr      bool
	}{
		{0, false, false},
		{1, true, false},
		{4, false, true},
	} {
		r := &system.FakeRunner{Handler: func(string) ([]byte, error) {
			if tc.code == 0 {
				return nil, nil
			}
			return nil, &system.CmdError{Cmd: "fsck", ExitCode: tc.code}
		}}
		repaired, err := Fsck(context.Background(), r, "/dev/loop9p1")
		if repaired != tc.wantRepaired || (err != nil) != tc.wantErr {
			t.Errorf("exit %d: repaired=%v err=%v", tc.code, repaired, err)
		}
		if got := r.Commands(); len(got) != 1 || got[0] != "fsck /dev/loop9p1 -- -p" {
			t.Errorf("commands = %q", got)
		}
	}
}

func TestCreateDiskImageRefusesExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cam_disk.bin")
	if err := os.WriteFile(path, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := CreateDiskImage(context.Background(), &system.FakeRunner{}, path, 64<<20, "CAM", "TeslaCam")
	if !errors.Is(err, os.ErrExist) {
		t.Fatalf("err = %v, want ErrExist", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "keep me" {
		t.Error("existing file was modified")
	}
}

func TestCreateDiskImageCleansUpOnFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cam_disk.bin")
	r := &system.FakeRunner{Handler: func(cmd string) ([]byte, error) {
		if strings.HasPrefix(cmd, "losetup --find") {
			return nil, &system.CmdError{Cmd: "losetup", ExitCode: 1, Output: "no free loop devices"}
		}
		return nil, nil
	}}
	if err := CreateDiskImage(context.Background(), r, path, 64<<20, "CAM", "TeslaCam"); err == nil {
		t.Fatal("want an error")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Error("half-made image should be removed")
	}
}

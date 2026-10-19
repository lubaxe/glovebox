package provision

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/lubaxe/glovebox/internal/system"
)

// fakeDisk answers the commands Backingfiles runs.
type fakeDisk struct {
	t       *testing.T
	disk    string // fake disk path; partition nodes are created next to it
	label   string
	parts   int
	size    int64 // bytes
	mounted bool
	hasFS   bool
	// p3 as left by an interrupted first start: its size in sectors and
	// what blkid -p finds on it ("" for nothing)
	p3Size int64
	p3FS   string
}

func (f *fakeDisk) run(cmd string) ([]byte, error) {
	switch {
	case strings.HasPrefix(cmd, "mountpoint "):
		if f.mounted {
			return nil, nil
		}
		return nil, &system.CmdError{Cmd: cmd, ExitCode: 32}
	case strings.HasPrefix(cmd, "blkid -L"):
		if f.hasFS {
			return []byte("/dev/mmcblk0p3\n"), nil
		}
		return nil, &system.CmdError{Cmd: cmd, ExitCode: 2}
	case strings.HasPrefix(cmd, "blkid -p"):
		if f.p3FS == "" {
			return nil, &system.CmdError{Cmd: cmd, ExitCode: 2}
		}
		return []byte(f.p3FS + "\n"), nil
	case strings.HasPrefix(cmd, "sfdisk --json"):
		p3 := max(f.p3Size, 1000)
		parts := []string{
			`{"node":"p1","start":16384,"size":1048576,"type":"c"}`,
			`{"node":"p2","start":1064960,"size":4194304,"type":"83"}`,
			fmt.Sprintf(`{"node":"p3","start":5259264,"size":%d,"type":"83"}`, p3),
			`{"node":"p4","start":5260264,"size":1000,"type":"83"}`,
		}[:f.parts]
		return []byte(fmt.Sprintf(`{"partitiontable":{"label":%q,"sectorsize":512,"partitions":[%s]}}`,
			f.label, strings.Join(parts, ","))), nil
	case strings.HasPrefix(cmd, "blockdev --getsize64"):
		return []byte(fmt.Sprint(f.size)), nil
	case strings.HasPrefix(cmd, "partx --add"):
		if err := os.WriteFile(partitionNode(f.disk, f.parts+1), nil, 0o644); err != nil {
			f.t.Fatal(err)
		}
	case strings.HasPrefix(cmd, "findmnt"), strings.HasPrefix(cmd, "lsblk"):
		f.t.Errorf("tests set Disk, so %q shouldn't run", cmd)
	}
	return nil, nil
}

func newBackingfiles(t *testing.T, f *fakeDisk) (*Backingfiles, *system.FakeRunner) {
	t.Helper()
	dir := t.TempDir()
	f.t = t
	f.disk = filepath.Join(dir, "mmcblk0")
	r := &system.FakeRunner{Handler: f.run}
	fstab := filepath.Join(dir, "fstab")
	if err := os.WriteFile(fstab, []byte("proc /proc proc defaults 0 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return &Backingfiles{
		Mountpoint: filepath.Join(dir, "backingfiles"),
		Fstab:      fstab,
		Disk:       f.disk,
		MinSize:    4 << 30,
		Runner:     r,
	}, r
}

func TestBackingfilesAlreadyMounted(t *testing.T) {
	b, r := newBackingfiles(t, &fakeDisk{mounted: true})
	created, err := b.Ensure(context.Background())
	if err != nil || created || len(r.Commands()) != 1 {
		t.Errorf("created=%v err=%v cmds=%v", created, err, r.Commands())
	}
}

func TestBackingfilesExistingPartitionIsMounted(t *testing.T) {
	b, r := newBackingfiles(t, &fakeDisk{hasFS: true, label: "dos", parts: 3})
	created, err := b.Ensure(context.Background())
	if err != nil || created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	cmds := strings.Join(r.Commands(), "\n")
	if strings.Contains(cmds, "sfdisk") || strings.Contains(cmds, "mkfs") || !strings.Contains(cmds, "mount -t xfs -o noatime /dev/mmcblk0p3") {
		t.Errorf("commands:\n%s", cmds)
	}
}

func TestBackingfilesMountsEncryptedVolume(t *testing.T) {
	// no partition labelled backingfiles: it's inside the encrypted volume
	b, r := newBackingfiles(t, &fakeDisk{label: "dos", parts: 3})
	b.Device = "/dev/mapper/backingfiles"
	created, err := b.Ensure(context.Background())
	if err != nil || created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	want := []string{"mountpoint -q " + b.Mountpoint, "mount -t xfs -o noatime /dev/mapper/backingfiles " + b.Mountpoint}
	if got := r.Commands(); !slices.Equal(got, want) {
		t.Errorf("commands = %q", got)
	}
	if fstab, _ := os.ReadFile(b.Fstab); strings.Contains(string(fstab), "backingfiles") {
		t.Errorf("fstab = %q", fstab)
	}
}

func TestBackingfilesCreatesPartition(t *testing.T) {
	f := &fakeDisk{label: "dos", parts: 2, size: 64 << 30}
	b, r := newBackingfiles(t, f)
	created, err := b.Ensure(context.Background())
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	// p2 ends at 1064960+4194304 = 5259264 sectors, already 1 MiB aligned
	script := r.Inputs["sfdisk --append --no-reread --no-tell-kernel "+f.disk]
	if script != "start=5259264, type=83\n" {
		t.Errorf("sfdisk script = %q", script)
	}
	part := f.disk + "p3"
	cmds := strings.Join(r.Commands(), "\n")
	for _, want := range []string{
		"partx --add --nr 3 " + f.disk,
		"mkfs.xfs -f -K -m reflink=1 -L backingfiles " + part,
		"mount -t xfs -o noatime " + part + " " + b.Mountpoint,
	} {
		if !strings.Contains(cmds, want) {
			t.Errorf("missing %q in:\n%s", want, cmds)
		}
	}
	fstab, _ := os.ReadFile(b.Fstab)
	if !strings.Contains(string(fstab), "LABEL=backingfiles "+b.Mountpoint+" xfs noatime,nofail 0 2") {
		t.Errorf("fstab:\n%s", fstab)
	}
	// running again doesn't add a second fstab line
	_ = b.addFstab()
	if again, _ := os.ReadFile(b.Fstab); strings.Count(string(again), "LABEL=backingfiles") != 1 {
		t.Errorf("fstab:\n%s", again)
	}
}

func TestBackingfilesFormatsLeftoverPartition(t *testing.T) {
	f := &fakeDisk{label: "dos", parts: 3, size: 64 << 30, p3Size: 100 << 21} // 100 GiB of sectors
	b, r := newBackingfiles(t, f)
	created, err := b.Ensure(context.Background())
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	cmds := strings.Join(r.Commands(), "\n")
	if strings.Contains(cmds, "sfdisk --append") || !strings.Contains(cmds, "mkfs.xfs -f -K -m reflink=1 -L backingfiles "+f.disk+"p3") {
		t.Errorf("commands:\n%s", cmds)
	}
}

func TestBackingfilesRefusals(t *testing.T) {
	for name, f := range map[string]*fakeDisk{
		"gpt disk":            {label: "gpt", parts: 2, size: 64 << 30},
		"four partitions":     {label: "dos", parts: 4, size: 64 << 30},
		"no room":             {label: "dos", parts: 2, size: 4 << 30},
		"p3 has a filesystem": {label: "dos", parts: 3, size: 64 << 30, p3Size: 100 << 21, p3FS: "ext4"},
		"p3 too small":        {label: "dos", parts: 3, size: 64 << 30},
	} {
		t.Run(name, func(t *testing.T) {
			b, r := newBackingfiles(t, f)
			if _, err := b.Ensure(context.Background()); err == nil {
				t.Fatal("want an error")
			}
			for _, c := range r.Commands() {
				if strings.HasPrefix(c, "sfdisk --append") || strings.HasPrefix(c, "mkfs") {
					t.Errorf("ran %q", c)
				}
			}
		})
	}
}

func TestPartitionNode(t *testing.T) {
	for disk, want := range map[string]string{"/dev/sda": "/dev/sda3", "/dev/mmcblk0": "/dev/mmcblk0p3", "/dev/loop4": "/dev/loop4p3"} {
		if got := partitionNode(disk, 3); got != want {
			t.Errorf("%s: %s, want %s", disk, got, want)
		}
	}
}

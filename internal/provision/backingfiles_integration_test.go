//go:build integration

package provision

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lubaxe/glovebox/internal/storage"
	"github.com/lubaxe/glovebox/internal/system"
)

// TestIntegrationBackingfiles runs the real partitioning on a loop device
// laid out like the image: boot and root partitions, then free space.
// Run as root on the Pi:
//
//	GOOS=linux GOARCH=arm64 go test -c -tags integration -o provtest ./internal/provision
//	sudo ./provtest -test.run Integration -test.v
func TestIntegrationBackingfiles(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	ctx := context.Background()
	r := system.Exec{}
	dir, err := os.MkdirTemp("/var/tmp", "glovebox-provtest-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	img := filepath.Join(dir, "disk.img")
	f, err := os.Create(img)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(6 << 30); err != nil { // sparse 6 GiB "card"
		t.Fatal(err)
	}
	f.Close()
	layout := "label: dos\nstart=16384, size=512MiB, type=c\nsize=1GiB, type=83\n"
	if _, err := r.RunInput(ctx, layout, "sfdisk", "--no-reread", "--no-tell-kernel", img); err != nil {
		t.Fatal(err)
	}
	out, err := r.Run(ctx, "losetup", "--find", "--show", "--partscan", img)
	if err != nil {
		t.Fatal(err)
	}
	disk := strings.TrimSpace(string(out))
	defer r.Run(context.Background(), "losetup", "--detach", disk)
	before, _ := r.Run(ctx, "sfdisk", "--dump", disk)

	b := &Backingfiles{
		Mountpoint: filepath.Join(dir, "mnt"),
		Fstab:      filepath.Join(dir, "fstab"),
		Disk:       disk,
		MinSize:    2 << 30,
		Label:      "bftest",
		Runner:     r,
	}
	created, err := b.Ensure(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Run(context.Background(), "umount", b.Mountpoint)
	if !created {
		t.Fatal("created = false")
	}

	after, err := r.Run(ctx, "sfdisk", "--dump", disk)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("partition table after:\n%s", after)
	for _, line := range strings.Split(string(before), "\n") {
		if strings.HasPrefix(line, disk+"p") && !strings.Contains(string(after), line) {
			t.Errorf("existing partition changed: %q", line)
		}
	}
	if !strings.Contains(string(after), disk+"p3") {
		t.Error("p3 missing")
	}
	out, _ = r.Run(ctx, "findmnt", "-n", "-o", "SOURCE,FSTYPE", b.Mountpoint)
	t.Logf("mounted: %s", strings.TrimSpace(string(out)))
	if !strings.Contains(string(out), "xfs") {
		t.Errorf("not an XFS mount: %q", out)
	}
	if err := storage.CheckReflink(b.Mountpoint); err != nil {
		t.Errorf("reflink: %v", err)
	}
	total, free, err := storage.DiskSpace(b.Mountpoint)
	t.Logf("backingfiles: %d MiB, %d MiB free (err %v)", total>>20, free>>20, err)
	fstab, _ := os.ReadFile(b.Fstab)
	t.Logf("fstab: %s", strings.TrimSpace(string(fstab)))

	// second run: already mounted, nothing to do
	if created, err := b.Ensure(ctx); err != nil || created {
		t.Errorf("second Ensure: created=%v err=%v", created, err)
	}
}

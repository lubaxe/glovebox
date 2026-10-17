package drive

import (
	"os"
	"path/filepath"
	"testing"
)

// fakeDrive lays out a cam drive in a temp dir and accounts for free space
// as total minus the bytes of files still present.
type fakeDrive struct {
	root  string
	total int64
}

func newFakeDrive(t *testing.T, total int64, files map[string]int) *fakeDrive {
	t.Helper()
	d := &fakeDrive{root: t.TempDir(), total: total}
	for p, size := range files {
		full := filepath.Join(d.root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

func (d *fakeDrive) space(string) (int64, int64, error) {
	var used int64
	_ = filepath.Walk(d.root, func(_ string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			used += fi.Size()
		}
		return nil
	})
	return d.total, d.total - used, nil
}

func (d *fakeDrive) exists(p string) bool {
	_, err := os.Stat(filepath.Join(d.root, filepath.FromSlash(p)))
	return err == nil
}

const (
	oldSentry = "TeslaCam/SentryClips/2026-10-01_10-00-00/"
	oldSaved  = "TeslaCam/SavedClips/2026-10-02_10-00-00/"
	newSentry = "TeslaCam/SentryClips/2026-10-03_10-00-00/"
	newest    = "TeslaCam/SentryClips/2026-10-04_10-00-00/"
)

func driveFiles() map[string]int {
	return map[string]int{
		oldSentry + "2026-10-01_09-59-00-front.mp4":          20,
		oldSentry + "event.json":                             1,
		oldSaved + "2026-10-02_09-59-00-front.mp4":           20,
		newSentry + "2026-10-03_09-59-00-front.mp4":          20,
		newest + "2026-10-04_09-59-00-front.mp4":             20,
		"TeslaCam/RecentClips/2026-10-04_10-00-00-front.mp4": 10,
		"FSCK0000.REC":                      5,
		"TeslaCam/RecentClips/FSCK0001.REC": 5,
	}
}

func allArchived(string, int64) bool { return true }

func TestCleanupRemovesJunk(t *testing.T) {
	d := newFakeDrive(t, 1000, driveFiles())
	c, err := cleanCamDrive(d.root, 25, d.space, allArchived)
	if err != nil {
		t.Fatal(err)
	}
	if c.Junk != 2 || d.exists("FSCK0000.REC") || d.exists("TeslaCam/RecentClips/FSCK0001.REC") {
		t.Errorf("junk = %d", c.Junk)
	}
	if c.Deleted != 0 {
		t.Errorf("plenty of space, but deleted %d files", c.Deleted)
	}
}

func TestCleanupDeletesOldestArchivedEventsUntilEnoughSpace(t *testing.T) {
	// 96 bytes used of 120 after junk; 25% target = 30 free, so 6 short:
	// deleting the oldest event (21 bytes) is enough
	d := newFakeDrive(t, 120, driveFiles())
	c, err := cleanCamDrive(d.root, 25, d.space, allArchived)
	if err != nil {
		t.Fatal(err)
	}
	if d.exists(oldSentry) || !d.exists(oldSaved) || !d.exists(newSentry) || !d.exists(newest) {
		t.Errorf("wrong events deleted: %+v", c)
	}
	if c.Deleted != 2 || c.Events != 1 || c.Freed != 21 || c.FreeAfter < 30 {
		t.Errorf("cleanup = %+v", c)
	}
	if !d.exists("TeslaCam/RecentClips/2026-10-04_10-00-00-front.mp4") {
		t.Error("recent clips are the car's to manage")
	}
}

func TestCleanupNeverDeletesUnarchived(t *testing.T) {
	d := newFakeDrive(t, 100, driveFiles()) // badly short of space
	onlyOldSaved := func(rel string, _ int64) bool { return rel == oldSaved+"2026-10-02_09-59-00-front.mp4" }
	c, err := cleanCamDrive(d.root, 25, d.space, onlyOldSaved)
	if err != nil {
		t.Fatal(err)
	}
	if !d.exists(oldSentry+"2026-10-01_09-59-00-front.mp4") || !d.exists(newSentry) || !d.exists(newest) {
		t.Error("deleted a file that isn't archived")
	}
	if d.exists(oldSaved) || c.Deleted != 1 || c.NotArchived == 0 {
		t.Errorf("cleanup = %+v", c)
	}
}

func TestCleanupWithoutArchiveDeletesNoEvents(t *testing.T) {
	d := newFakeDrive(t, 100, driveFiles())
	c, err := cleanCamDrive(d.root, 25, d.space, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Deleted != 0 || !d.exists(oldSentry) {
		t.Errorf("cleanup = %+v", c)
	}
}

func TestCleanupChecksSize(t *testing.T) {
	// archived when it was shorter; the longer copy on the drive isn't safe yet
	d := newFakeDrive(t, 100, driveFiles())
	archivedSmaller := func(_ string, size int64) bool { return size <= 1 }
	c, err := cleanCamDrive(d.root, 25, d.space, archivedSmaller)
	if err != nil {
		t.Fatal(err)
	}
	if !d.exists(oldSentry+"2026-10-01_09-59-00-front.mp4") || c.Deleted != 1 {
		t.Errorf("cleanup = %+v", c)
	}
}

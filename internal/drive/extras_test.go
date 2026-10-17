package drive

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/lubaxe/glovebox/internal/gadget"
	"github.com/lubaxe/glovebox/internal/storage"
	"github.com/lubaxe/glovebox/internal/system"
)

// withExtras gives d a music and a Boombox drive, with images that are
// plain files: creating one makes a file of the size, cloning copies it
// and "mounting" one gives a folder that stands for its contents.
func withExtras(t *testing.T, d *Drive) map[string]string {
	t.Helper()
	music, _ := NewExtra(Music, 64<<20, "Music")
	boom, _ := NewExtra(Boombox, 32<<20, "Sounds")
	d.Extras = []Extra{music, boom}
	contents := map[string]string{} // image -> folder with its files
	d.createImage = func(_ context.Context, _ system.Runner, path string, size int64, _, _ string) error {
		f, err := os.Create(path)
		if err != nil {
			return err
		}
		defer f.Close()
		return f.Truncate(wholeMiB(size))
	}
	d.clone = func(src, dst string) error {
		b, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		if dir, ok := contents[src]; ok {
			copyDir(t, dir, contentDir(t, contents, dst))
		}
		return os.WriteFile(dst, b, 0o644)
	}
	d.maintainImage = func(_ context.Context, _ system.Runner, path string, work func(string) error) (storage.Maintenance, error) {
		if work == nil {
			return storage.Maintenance{}, nil
		}
		return storage.Maintenance{}, work(contentDir(t, contents, path))
	}
	d.allocated = func(string) (int64, error) { return 0, nil }
	d.EjectPause = 0
	return contents
}

func contentDir(t *testing.T, contents map[string]string, img string) string {
	if dir, ok := contents[img]; ok {
		return dir
	}
	dir := t.TempDir()
	contents[img] = dir
	return dir
}

func copyDir(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, e os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if e.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestStartPresentsExtraDrives(t *testing.T) {
	d, g, _, _ := newDrive(t)
	withExtras(t, d)
	// a light show drive that was turned off
	if err := os.WriteFile(d.ExtraDisk(LightShow), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := d.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if len(g.enabled) != 1 {
		t.Fatalf("enabled = %v", g.enabled)
	}
	want := []gadget.LUN{{File: d.CamDisk(), Product: "CAM"}, {File: d.ExtraDisk(Music), Product: "MUSIC"}, {File: d.ExtraDisk(Boombox), Product: "BOOMBOX"}}
	if !slices.Equal(g.enabled[0], want) {
		t.Errorf("luns = %v, want %v", g.enabled[0], want)
	}
	if fi, err := os.Stat(d.ExtraDisk(Music)); err != nil || fi.Size() != 64<<20 {
		t.Errorf("music drive: %v %v", fi, err)
	}
	if _, err := os.Stat(d.ExtraDisk(LightShow)); !errors.Is(err, os.ErrNotExist) {
		t.Error("the light show drive should be deleted")
	}
	st, _ := d.Status()
	if len(st.Extras) != 2 || st.Extras[0].Name != Music || st.Extras[0].Source != "Music" || st.Extras[0].Size != 64<<20 {
		t.Errorf("status extras = %+v", st.Extras)
	}
}

func TestCamDiskLeavesRoomForExtras(t *testing.T) {
	d, _, _, _ := newDrive(t)
	withExtras(t, d)
	d.CamSize = 0
	d.diskSpace = func(string) (int64, int64, error) { return 64 << 30, 10 << 30, nil }
	// 10G free - 1G snapshots - 96M for the extras: 8G in whole GiB
	if got, err := d.camDiskSize(); err != nil || got != 8<<30 {
		t.Errorf("camDiskSize = %d, %v", got>>20, err)
	}
}

func TestChangedDrivesReconnect(t *testing.T) {
	d, g, _, _ := newDrive(t)
	if err := os.WriteFile(d.CamDisk(), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	withExtras(t, d)
	// presented before the music drive was added
	g.state = gadget.State{Enabled: true, Files: []string{d.CamDisk()}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := d.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if g.disabled != 1 || len(g.enabled) != 1 || len(g.enabled[0]) != 3 {
		t.Errorf("disabled=%d enabled=%v", g.disabled, g.enabled)
	}

	// same drives, but the music one's size changed
	d2, g2, _, _ := newDrive(t)
	d2.Backingfiles = d.Backingfiles
	withExtras(t, d2)
	d2.Extras[0].Size = 128 << 20
	g2.state = gadget.State{Enabled: true, Files: d2.lunFiles()}
	if err := d2.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if g2.disabled != 1 {
		t.Error("a resized drive needs reconnecting")
	}
	if fi, _ := os.Stat(d2.ExtraDisk(Music)); fi.Size() != 128<<20 {
		t.Errorf("music drive is %d bytes", fi.Size())
	}
}

func TestSyncExtraSwapsInChanges(t *testing.T) {
	d, g, _, _ := newDrive(t)
	contents := withExtras(t, d)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := d.Start(ctx); err != nil {
		t.Fatal(err)
	}
	img := d.ExtraDisk(Boombox)
	addSound := func(ctx context.Context, dir string) error {
		if _, err := os.Stat(filepath.Join(dir, "horn.mp3")); err == nil {
			return nil // like a mirror that finds nothing new
		}
		return os.WriteFile(filepath.Join(dir, "horn.mp3"), []byte("honk"), 0o644)
	}

	changed, err := d.SyncExtra(ctx, Boombox, addSound)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	// the Boombox drive is lun 2, after cam and music
	if !slices.Equal(g.ejected, []int{2}) || !slices.Equal(g.loaded, []string{img}) {
		t.Errorf("ejected=%v loaded=%v", g.ejected, g.loaded)
	}
	if _, err := os.Stat(filepath.Join(contents[img+".new"], "Boombox", "horn.mp3")); err != nil {
		t.Errorf("the new version doesn't have the file: %v", err)
	}
	if _, err := os.Stat(img + ".new"); !errors.Is(err, os.ErrNotExist) {
		t.Error("the new version should have replaced the drive")
	}

	// nothing new: the car's drive is left alone
	contents[img] = contents[img+".new"]
	delete(contents, img+".new")
	changed, err = d.SyncExtra(ctx, Boombox, addSound)
	if err != nil || changed || len(g.ejected) != 1 {
		t.Errorf("changed=%v err=%v ejected=%v", changed, err, g.ejected)
	}

	// a failed sync leaves the drive as it was
	_, err = d.SyncExtra(ctx, Boombox, func(context.Context, string) error { return errors.New("nas went away") })
	if err == nil || len(g.ejected) != 1 {
		t.Errorf("err=%v ejected=%v", err, g.ejected)
	}
	st, _ := d.Status()
	if st.Extras[1].LastError != "nas went away" || st.Extras[1].LastChange == nil {
		t.Errorf("status = %+v", st.Extras[1])
	}
}

func TestSyncExtrasUsesSources(t *testing.T) {
	d, _, _, _ := newDrive(t)
	withExtras(t, d)
	d.Extras[1].Source = "" // Boombox isn't synced
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := d.Start(ctx); err != nil {
		t.Fatal(err)
	}
	var sources []string
	d.SyncExtras(ctx, func(_ context.Context, source, dir string) error {
		sources = append(sources, source)
		return nil
	})
	if !slices.Equal(sources, []string{"Music"}) {
		t.Errorf("synced %v", sources)
	}
}

func TestTurnedOffDriveIsDeletedAfterReconnecting(t *testing.T) {
	d, g, _, _ := newDrive(t)
	withExtras(t, d)
	if err := os.WriteFile(d.CamDisk(), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(d.ExtraDisk(Music), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// presented with the music drive, which has since been turned off
	g.state = gadget.State{Enabled: true, Files: []string{d.CamDisk(), d.ExtraDisk(Music)}}
	d.Extras = nil
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := d.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(d.ExtraDisk(Music)); !errors.Is(err, os.ErrNotExist) {
		t.Error("the turned-off music drive should be deleted once it's no longer presented")
	}
}

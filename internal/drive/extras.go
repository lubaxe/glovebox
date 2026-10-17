package drive

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// Extra is a drive the car reads besides the cam drive: music, a light show
// or Boombox sounds. Its files mirror a folder on the archive target.
type Extra struct {
	Name   string // file name prefix, e.g. "music" for music_disk.bin
	Label  string // filesystem label and drive name, e.g. "MUSIC"
	Folder string // folder on the drive that's synced; "" for all of it
	Size   int64
	Source string // folder on the archive target; "" means it's never synced
}

// extraKinds are the extra drives the car knows, in the order they're
// presented after the cam drive. The car looks for a light show in a
// LightShow folder and for Boombox sounds in a Boombox folder; music can be
// anywhere.
var extraKinds = []Extra{
	{Name: "music", Label: "MUSIC"},
	{Name: "lightshow", Label: "LIGHTSHOW", Folder: "LightShow"},
	{Name: "boombox", Label: "BOOMBOX", Folder: "Boombox"},
}

// Extra drives' names.
const (
	Music     = "music"
	LightShow = "lightshow"
	Boombox   = "boombox"
)

// NewExtra returns the extra drive called name with its size and source.
func NewExtra(name string, size int64, source string) (Extra, error) {
	for _, k := range extraKinds {
		if k.Name == name {
			k.Size, k.Source = size, source
			return k, nil
		}
	}
	return Extra{}, fmt.Errorf("unknown drive %q", name)
}

// ExtraStatus is what the API reports about an extra drive.
type ExtraStatus struct {
	Name   string `json:"name"`
	Label  string `json:"label"`
	Size   int64  `json:"size_bytes"`
	Source string `json:"source,omitempty"`
	// LastSync is when the drive was last synced, successfully or not;
	// LastChange when that last changed its files.
	LastSync   *time.Time `json:"last_sync,omitempty"`
	LastChange *time.Time `json:"last_change,omitempty"`
	LastError  string     `json:"last_error,omitempty"`
}

// ExtraDisk is the image of the extra drive called name.
func (d *Drive) ExtraDisk(name string) string {
	return filepath.Join(d.Backingfiles, name+"_disk.bin")
}

// wholeMiB is the size CreateDiskImage gives an image of size bytes.
func wholeMiB(size int64) int64 { return size - size%(1<<20) }

// lunFiles are the images the car should see, in order.
func (d *Drive) lunFiles() []string {
	files := []string{d.CamDisk()}
	for _, x := range d.Extras {
		files = append(files, d.ExtraDisk(x.Name))
	}
	return files
}

// extrasReady reports whether every extra drive's image exists at its size.
func (d *Drive) extrasReady() bool {
	for _, x := range d.Extras {
		fi, err := os.Stat(d.ExtraDisk(x.Name))
		if err != nil || fi.Size() != wholeMiB(x.Size) {
			return false
		}
	}
	return true
}

// removeUnusedExtras deletes the images of extra drives that were turned
// off. They aren't presented, so this is safe while the car is connected.
func (d *Drive) removeUnusedExtras(presented []string) {
	for _, k := range extraKinds {
		if slices.ContainsFunc(d.Extras, func(x Extra) bool { return x.Name == k.Name }) {
			continue
		}
		img := d.ExtraDisk(k.Name)
		if slices.Contains(presented, img) {
			continue
		}
		if err := os.Remove(img); err == nil {
			d.Log.Info("deleted the drive that was turned off", "drive", k.Name)
		}
		_ = os.Remove(img + ".new")
	}
}

// prepareExtras creates missing extra drives, recreates ones whose size
// changed (their files come back with the next sync) and repairs the rest.
// Only safe while they aren't presented to the car.
func (d *Drive) prepareExtras(ctx context.Context) error {
	for _, x := range d.Extras {
		img := d.ExtraDisk(x.Name)
		_ = os.Remove(img + ".new") // from a sync cut short
		fi, err := os.Stat(img)
		switch {
		case err == nil && fi.Size() == wholeMiB(x.Size):
			if _, err := d.maintainImage(ctx, d.Runner, img, nil); err != nil {
				d.Log.Warn("couldn't check drive", "drive", x.Name, "err", err)
			}
			continue
		case err == nil:
			d.Log.Info("drive size changed, creating it again; its files come back with the next sync",
				"drive", x.Name, "size", x.Size, "was", fi.Size())
			if err := os.Remove(img); err != nil {
				return err
			}
		case !errors.Is(err, fs.ErrNotExist):
			return err
		}
		d.Log.Info("creating drive", "drive", x.Name, "size", x.Size)
		if err := d.createImage(ctx, d.Runner, img, x.Size, x.Label, x.Folder); err != nil {
			return fmt.Errorf("creating the %s drive: %w", x.Name, err)
		}
	}
	return nil
}

// extrasReserve is how much more space the extra drives can take up as
// they fill: they're sparse, so their full size isn't allocated yet.
func (d *Drive) extrasReserve() int64 {
	var reserve int64
	for _, x := range d.Extras {
		used, err := d.allocated(d.ExtraDisk(x.Name))
		if err != nil {
			used = 0
		}
		reserve += max(wholeMiB(x.Size)-used, 0)
	}
	return reserve
}

// SyncExtras syncs every extra drive that has a source with mirror, which
// makes dir a copy of the source folder. Errors are logged and reported in
// the status; one drive failing doesn't stop the others.
func (d *Drive) SyncExtras(ctx context.Context, mirror func(ctx context.Context, source, dir string) error) {
	for _, x := range d.Extras {
		if x.Source == "" || ctx.Err() != nil {
			continue
		}
		start := time.Now()
		changed, err := d.SyncExtra(ctx, x.Name, func(ctx context.Context, dir string) error {
			return mirror(ctx, x.Source, dir)
		})
		switch {
		case err != nil:
			d.Log.Error("syncing drive", "drive", x.Name, "source", x.Source, "err", err)
		case changed:
			d.Log.Info("synced drive; the car sees the new files", "drive", x.Name, "seconds", int(time.Since(start).Seconds()))
		default:
			d.Log.Info("drive is up to date", "drive", x.Name)
		}
	}
}

// SyncExtra updates the extra drive called name: fill gets a copy of the
// drive's folder to bring up to date. If anything changed, the new version
// replaces the drive the car sees, which ejects the drive and inserts it
// again; the cam drive stays connected. If fill fails, the drive is left as
// it was.
func (d *Drive) SyncExtra(ctx context.Context, name string, fill func(ctx context.Context, dir string) error) (changed bool, err error) {
	lun := slices.IndexFunc(d.Extras, func(x Extra) bool { return x.Name == name })
	if lun < 0 {
		return false, fmt.Errorf("no %s drive", name)
	}
	x := d.Extras[lun]
	lun++ // the cam drive is first

	d.syncMu.Lock()
	defer d.syncMu.Unlock()
	defer func() { d.setExtraResult(name, changed, err) }()

	img := d.ExtraDisk(name)
	work := img + ".new"
	_ = os.Remove(work)
	// a copy-on-write clone: instant, and only what changes takes space
	if err := d.clone(img, work); err != nil {
		return false, fmt.Errorf("copying the %s drive: %w", name, err)
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(work)
		}
	}()
	_, err = d.maintainImage(ctx, d.Runner, work, func(mnt string) error {
		dir := filepath.Join(mnt, x.Folder)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		before, err := manifest(dir)
		if err != nil {
			return err
		}
		if err := fill(ctx, dir); err != nil {
			return err
		}
		after, err := manifest(dir)
		if err != nil {
			return err
		}
		changed = !maps.Equal(before, after)
		return nil
	})
	if err != nil || !changed {
		return false, err
	}

	st, err := d.Gadget.State()
	if err != nil {
		return false, err
	}
	presented := st.Enabled && lun < len(st.Files) && st.Files[lun] == img
	if presented {
		if err := d.Gadget.Eject(lun); err != nil {
			return false, err
		}
		// Leave it out for a moment, like a USB stick pulled and put back.
		// Swapped at once, a host polling the drive (Windows, and maybe the
		// car) never sees it empty and keeps showing the old files.
		select {
		case <-ctx.Done():
		case <-time.After(d.EjectPause):
		}
	}
	keep = true
	if err := os.Rename(work, img); err != nil {
		keep = false
		return false, err
	}
	if presented {
		if err := d.Gadget.Load(lun, img); err != nil {
			return true, err
		}
	}
	return true, nil
}

// fileState is what a sync can change about a file.
type fileState struct {
	size  int64
	mtime time.Time
}

// manifest lists the files and folders under dir.
func manifest(dir string) (map[string]fileState, error) {
	m := map[string]fileState{}
	err := filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		st := fileState{mtime: info.ModTime()}
		if !e.IsDir() {
			st.size = info.Size()
		} else {
			st.size = -1
			st.mtime = time.Time{} // a folder's time changes with what's in it
		}
		m[rel] = st
		return nil
	})
	return m, err
}

func (d *Drive) setExtraResult(name string, changed bool, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.extraStatus == nil {
		d.extraStatus = map[string]*ExtraStatus{}
	}
	st := d.extraStatus[name]
	if st == nil {
		st = &ExtraStatus{}
		d.extraStatus[name] = st
	}
	now := time.Now()
	st.LastSync = &now
	st.LastError = ""
	if err != nil {
		st.LastError = err.Error()
	}
	if changed {
		st.LastChange = &now
	}
}

func (d *Drive) extrasStatus() []ExtraStatus {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := []ExtraStatus{}
	for _, x := range d.Extras {
		st := ExtraStatus{}
		if s := d.extraStatus[x.Name]; s != nil {
			st = *s
		}
		st.Name, st.Label, st.Source = x.Name, x.Label, x.Source
		if fi, err := os.Stat(d.ExtraDisk(x.Name)); err == nil {
			st.Size = fi.Size()
		}
		out = append(out, st)
	}
	return out
}

// Order is where the extra drive called name goes among the drives.
func Order(name string) int {
	return slices.IndexFunc(extraKinds, func(k Extra) bool { return k.Name == name })
}

package drive

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/lubaxe/glovebox/internal/store"
)

// Storage is how backingfiles is used, for the usage bar in Settings.
type Storage struct {
	Total   int64 `json:"total"`
	Free    int64 `json:"free"`
	MinFree int64 `json:"min_free"`
	Car     Car   `json:"car"`
	// Extras are the music, light show and Boombox drives.
	Extras []ExtraUsage `json:"extras"`
	// Kept is the clip store: drives kept until they're archived. Its
	// clips share space with the car's drive until the car writes over
	// them, so Kept.Bytes can be more than Outside.
	Kept store.Usage `json:"kept"`
	// Outside is what's used on backingfiles by everything but the car's
	// drive and the extra drives: kept clips, the snapshot and the log.
	Outside int64 `json:"outside"`
	// Rate is how many bytes an hour of driving takes, from the car's own
	// clips (RateMeasured) or a typical HW4 car.
	Rate         int64 `json:"rate"`
	RateMeasured bool  `json:"rate_measured"`
	// MinCarSize and MaxCarSize bound a new size for the car's drive.
	MinCarSize int64 `json:"min_car_size"`
	MaxCarSize int64 `json:"max_car_size"`
	// Recreate is the size the car's drive will be recreated at on the next
	// start, if that's been asked for.
	Recreate int64 `json:"recreate,omitempty"`
}

// Car is the car's drive: its size on the card, and what's in it as of the
// newest snapshot (Known).
type Car struct {
	Size      int64 `json:"size"`
	Allocated int64 `json:"allocated"`
	Known     bool  `json:"known"`
	Recent    int64 `json:"recent"`
	Sentry    int64 `json:"sentry"`
	Saved     int64 `json:"saved"`
	Other     int64 `json:"other"`
	Free      int64 `json:"free"`
	// RecentMinutes is how many minutes RecentClips holds.
	RecentMinutes int `json:"recent_minutes"`
}

// ExtraUsage is one extra drive.
type ExtraUsage struct {
	Name  string `json:"name"`
	Label string `json:"label"`
	Size  int64  `json:"size"`
}

// typicalRate is an hour of driving on a HW4 car, six cameras: ~16 GB.
const typicalRate = 15 << 30

// recentMinute matches a Recent clip's minute, from its file name.
var recentMinute = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2}_\d{2}-\d{2}-\d{2})-[a-z_]+\.mp4$`)

// Storage reports how backingfiles is used.
func (d *Drive) Storage() (Storage, error) {
	total, free, err := d.diskSpace(d.Backingfiles)
	if err != nil {
		return Storage{}, err
	}
	st := Storage{Total: total, Free: free, MinFree: d.MinFree, Extras: []ExtraUsage{}}
	if fi, err := os.Stat(d.CamDisk()); err == nil {
		st.Car.Size = fi.Size()
		st.Car.Allocated, _ = d.allocated(d.CamDisk())
	}
	var extras int64
	for _, x := range d.Extras {
		fi, err := os.Stat(d.ExtraDisk(x.Name))
		if err != nil {
			continue
		}
		used, _ := d.allocated(d.ExtraDisk(x.Name))
		extras += used
		st.Extras = append(st.Extras, ExtraUsage{Name: x.Name, Label: x.Label, Size: fi.Size()})
	}
	if snaps, err := d.Snapshots.List(); err == nil && len(snaps) > 0 {
		d.carContents(snaps[len(snaps)-1].Mount, &st.Car)
	}
	if d.Store != nil {
		st.Kept = d.Store.Usage(d.Archived)
	}
	st.Outside = max(total-free-st.Car.Allocated-extras, 0)

	st.Rate = typicalRate
	switch {
	case st.Kept.Minutes >= 10:
		st.Rate, st.RateMeasured = st.Kept.Bytes/int64(st.Kept.Minutes)*60, true
	case st.Car.RecentMinutes >= 10:
		st.Rate, st.RateMeasured = st.Car.Recent/int64(st.Car.RecentMinutes)*60, true
	}
	// The car needs its rolling hour, its events with room to grow, and some
	// slack. More only takes room from the drives kept until home.
	st.MinCarSize = roundUpGiB(max(st.Rate+(st.Car.Sentry+st.Car.Saved)*5/4+(2<<30), 16<<30))
	var extraSizes int64
	for _, x := range st.Extras {
		extraSizes += x.Size
	}
	// leaving room for what the store already keeps, which is all its own
	// once the old drive is gone
	st.MaxCarSize = max((total-extraSizes-d.MinFree-st.Kept.Bytes)/(1<<30)*(1<<30), 0)
	st.Recreate, _ = d.recreateSize()
	return st, nil
}

func roundUpGiB(n int64) int64 { return (n + (1 << 30) - 1) / (1 << 30) * (1 << 30) }

// carContents fills in what the car's drive holds, from a mounted snapshot.
func (d *Drive) carContents(mnt string, car *Car) {
	total, free, err := d.diskSpace(mnt)
	if err != nil {
		return
	}
	car.Known, car.Free = true, free
	minutes := map[string]bool{}
	_ = filepath.WalkDir(filepath.Join(mnt, "TeslaCam"), func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return nil
		}
		fi, err := e.Info()
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(mnt, p)
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) > 2 && parts[1] == "EncryptedClips" {
			parts = parts[1:]
		}
		switch {
		case len(parts) < 3:
		case parts[1] == "RecentClips":
			car.Recent += fi.Size()
			if m := recentMinute.FindStringSubmatch(e.Name()); m != nil {
				minutes[m[1]] = true
			}
		case parts[1] == "SentryClips":
			car.Sentry += fi.Size()
		case parts[1] == "SavedClips":
			car.Saved += fi.Size()
		}
		return nil
	})
	car.RecentMinutes = len(minutes)
	car.Other = max(total-free-car.Recent-car.Sentry-car.Saved, 0)
}

// recreateName marks that the car's drive is to be recreated at a new size
// on the next start; it holds the size.
const recreateName = ".recreate-cam-disk"

func (d *Drive) recreateFile() string { return filepath.Join(d.Backingfiles, recreateName) }

func (d *Drive) recreateSize() (int64, error) {
	b, err := os.ReadFile(d.recreateFile())
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
}

// ErrBadSize means a new size for the car's drive is out of bounds.
var ErrBadSize = errors.New("size out of range")

// RequestRecreate asks for the car's drive to be deleted and created again
// at size on the next start. It erases what's on it: the caller makes sure
// that's archived, and sets the size in the config too, so it sticks.
func (d *Drive) RequestRecreate(size int64) error {
	st, err := d.Storage()
	if err != nil {
		return err
	}
	if size%(1<<30) != 0 || size < st.MinCarSize || size > st.MaxCarSize {
		return fmt.Errorf("%w: the car's drive can be %d to %d GiB", ErrBadSize, st.MinCarSize>>30, st.MaxCarSize>>30)
	}
	return os.WriteFile(d.recreateFile(), []byte(strconv.FormatInt(size, 10)+"\n"), 0o644)
}

// CancelRecreate drops a pending request.
func (d *Drive) CancelRecreate() error {
	err := os.Remove(d.recreateFile())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// recreate deletes the car's drive and the snapshots of it, so Start makes a
// new one. The clip store keeps its clips: they're clones of their own.
func (d *Drive) recreate(ctx context.Context, enabled bool) error {
	size, err := d.recreateSize()
	if err != nil {
		return err
	}
	d.Log.Warn("recreating the car's drive at a new size; what was on it is erased", "size", size)
	if enabled {
		if err := d.Gadget.Disable(ctx); err != nil {
			return err
		}
	}
	snaps, err := d.Snapshots.List()
	if err != nil {
		return err
	}
	for _, s := range snaps {
		if err := d.Snapshots.Release(ctx, s.ID); err != nil {
			return err
		}
		if d.Store != nil {
			d.Store.Forget(s.ID)
		}
	}
	if err := os.Remove(d.CamDisk()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	d.CamSize = size
	return os.Remove(d.recreateFile())
}

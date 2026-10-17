package drive

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Cleanup is what one boot-time cleanup of the cam drive did.
type Cleanup struct {
	Junk        int   `json:"junk_files"`        // fsck leftovers removed
	Deleted     int   `json:"deleted_files"`     // archived event files removed
	Freed       int64 `json:"freed_bytes"`       // by Deleted
	Events      int   `json:"emptied_events"`    // event folders removed
	NotArchived int   `json:"kept_not_archived"` // files kept although space was needed
	FreeBefore  int64 `json:"free_bytes_before"`
	FreeAfter   int64 `json:"free_bytes_after"`
	Total       int64 `json:"total_bytes"`
}

var (
	eventDir = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}_\d{2}-\d{2}-\d{2}$`)
	// fsck saves orphaned clusters as FSCK0000.REC; Windows-style chkdsk
	// leaves name~1.MP4 duplicates.
	junkName = regexp.MustCompile(`(?i)^(fsck\d*\.rec|.*~\d\.mp4)$`)
)

// cleanCamDrive cleans the cam drive mounted at root:
//
//  1. removes files fsck recovered from deleted data, and
//  2. while free space is below target, removes Sentry and Saved files that
//     archived reports are safely on the NAS, oldest event first.
//
// Files that aren't archived are never removed, whatever the free space.
// space reports the filesystem's total and free bytes.
func cleanCamDrive(root string, keepFreePercent int,
	space func(string) (total, free int64, err error),
	archived func(rel string, size int64) bool) (Cleanup, error) {

	var c Cleanup
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable parts rather than give up
		}
		if d.Type().IsRegular() && junkName.MatchString(d.Name()) {
			if os.Remove(p) == nil {
				c.Junk++
			}
		}
		return nil
	})
	if err != nil {
		return c, err
	}

	total, free, err := space(root)
	if err != nil {
		return c, err
	}
	c.Total, c.FreeBefore, c.FreeAfter = total, free, free
	target := total * int64(keepFreePercent) / 100
	if archived == nil || free >= target {
		return c, nil
	}

	type event struct{ kind, name string }
	var events []event
	for _, kind := range []string{"SentryClips", "SavedClips"} {
		entries, _ := os.ReadDir(filepath.Join(root, "TeslaCam", kind))
		for _, e := range entries {
			if e.IsDir() && eventDir.MatchString(e.Name()) {
				events = append(events, event{kind, e.Name()})
			}
		}
	}
	// folder names are timestamps, so this is oldest first
	sort.Slice(events, func(i, j int) bool { return events[i].name < events[j].name })

	for _, ev := range events {
		if free >= target {
			break
		}
		dir := filepath.Join(root, "TeslaCam", ev.kind, ev.name)
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.Type().IsRegular() {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			rel := strings.Join([]string{"TeslaCam", ev.kind, ev.name, e.Name()}, "/")
			if !archived(rel, info.Size()) {
				c.NotArchived++
				continue
			}
			if os.Remove(filepath.Join(dir, e.Name())) == nil {
				c.Deleted++
				c.Freed += info.Size()
			}
		}
		if os.Remove(dir) == nil { // only succeeds once it's empty
			c.Events++
		}
		if _, free, err = space(root); err != nil {
			return c, err
		}
		c.FreeAfter = free
	}
	return c, nil
}

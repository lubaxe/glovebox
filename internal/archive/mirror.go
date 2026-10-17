package archive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"time"
)

// MirrorExcludes are names that are never copied to an extra drive, and
// left alone on it: litter from Macs and Windows.
var MirrorExcludes = []string{
	".DS_Store", ".fseventsd", ".Spotlight-V100", ".Trashes", ".TemporaryItems",
	".metadata_never_index", "System Volume Information", "$RECYCLE.BIN",
	"Thumbs.db", "desktop.ini", "._*",
}

func excluded(name string) bool {
	for _, pat := range MirrorExcludes {
		if ok, _ := path.Match(pat, name); ok {
			return true
		}
	}
	return false
}

// modifyWindow is how far modification times may differ for a file to
// count as unchanged; FAT keeps them to 2 seconds.
const modifyWindow = 2 * time.Second

// mirrorFS makes dst an exact copy of src: new and changed files (by size
// and modification time) are copied, and files and folders src doesn't
// have are deleted.
func mirrorFS(ctx context.Context, src fs.FS, dst string) error {
	keep := map[string]bool{}
	err := fs.WalkDir(src, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if p == "." {
			return nil
		}
		if excluded(d.Name()) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		local := filepath.Join(dst, filepath.FromSlash(p))
		if d.IsDir() {
			keep[p] = true
			if fi, err := os.Lstat(local); err == nil && !fi.IsDir() {
				if err := os.Remove(local); err != nil {
					return err
				}
			}
			return os.MkdirAll(local, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		keep[p] = true
		info, err := d.Info()
		if err != nil {
			return err
		}
		if fi, err := os.Lstat(local); err == nil {
			if fi.Mode().IsRegular() && fi.Size() == info.Size() && within(fi.ModTime(), info.ModTime()) {
				return nil
			}
			if err := os.RemoveAll(local); err != nil {
				return err
			}
		}
		return copyIn(src, p, local, info.ModTime())
	})
	if err != nil {
		return err
	}

	// delete what's gone, deepest first so folders are empty by then
	var gone []string
	err = filepath.WalkDir(dst, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dst, p)
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if excluded(d.Name()) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !keep[rel] {
			gone = append(gone, p)
			if d.IsDir() {
				return fs.SkipDir
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	sort.Sort(sort.Reverse(sort.StringSlice(gone)))
	for _, p := range gone {
		if err := os.RemoveAll(p); err != nil {
			return err
		}
	}
	return nil
}

func within(a, b time.Time) bool {
	d := a.Sub(b)
	return d <= modifyWindow && d >= -modifyWindow
}

// copyIn copies src's file p to local, through a temporary file so a cut
// short copy never looks complete.
func copyIn(src fs.FS, p, local string, mtime time.Time) (err error) {
	in, err := src.Open(p)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := local + ".glovebox-partial"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	if _, err = io.Copy(out, in); err != nil {
		out.Close()
		if errors.Is(err, io.ErrUnexpectedEOF) {
			err = fmt.Errorf("%s changed while it was copied", p)
		}
		return fmt.Errorf("copying %s: %w", p, err)
	}
	if err = out.Close(); err != nil {
		return err
	}
	if err = os.Chtimes(tmp, mtime, mtime); err != nil {
		return err
	}
	return os.Rename(tmp, local)
}

// excludeFlags returns MirrorExcludes as --exclude flags for rsync and
// rclone.
func excludeFlags() []string {
	var out []string
	for _, pat := range MirrorExcludes {
		out = append(out, "--exclude="+pat)
	}
	return out
}

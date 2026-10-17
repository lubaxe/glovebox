package archive

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/lubaxe/glovebox/internal/system"
)

// NFS archives to an NFS export. It's mounted only while archiving or
// syncing, with "soft" in the options so a server that goes away (the car
// drove off) makes file operations fail rather than hang.
type NFS struct {
	Server  string
	Export  string // e.g. /mnt/user/TeslaCam
	Folder  string // optional folder inside the export
	Options string // mount options
	// MountDir holds the mount points.
	MountDir string
	Runner   system.Runner
}

func (t *NFS) String() string { return t.Server + ":" + t.Export }

// Reachable implements Target.
func (t *NFS) Reachable(ctx context.Context) error {
	return dial(ctx, hostPort(t.Server, 2049))
}

// Connect implements Target.
func (t *NFS) Connect(ctx context.Context) (Session, error) {
	mnt := filepath.Join(t.MountDir, "archive")
	if err := t.mount(ctx, t.Export, mnt, "rw"); err != nil {
		return nil, err
	}
	root := filepath.Join(mnt, filepath.FromSlash(t.Folder))
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.unmount(mnt)
		return nil, err
	}
	return &dirSession{root: root, close: func() error { return t.unmount(mnt) }}, nil
}

// Mirror implements Target. source is an export path, e.g. /mnt/user/Music.
func (t *NFS) Mirror(ctx context.Context, source, dir string) error {
	mnt := filepath.Join(t.MountDir, "source")
	if err := t.mount(ctx, source, mnt, "ro"); err != nil {
		return err
	}
	defer t.unmount(mnt)
	return mirrorFS(ctx, os.DirFS(mnt), dir)
}

func (t *NFS) mount(ctx context.Context, export, mnt, mode string) error {
	if err := os.MkdirAll(mnt, 0o755); err != nil {
		return err
	}
	// left mounted by a run that was cut short
	if _, err := t.Runner.Run(ctx, "mountpoint", "-q", mnt); err == nil {
		t.unmount(mnt)
	}
	opts := mode
	if t.Options != "" {
		opts += "," + t.Options
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := t.Runner.Run(ctx, "mount", "-t", "nfs", "-o", opts, t.Server+":"+export, mnt); err != nil {
		return fmt.Errorf("mounting %s:%s: %w", t.Server, export, err)
	}
	return nil
}

// unmount detaches mnt even if the server is gone.
func (t *NFS) unmount(mnt string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := t.Runner.Run(ctx, "umount", "-f", "-l", mnt); err != nil {
		return fmt.Errorf("unmounting %s: %w", mnt, err)
	}
	return nil
}

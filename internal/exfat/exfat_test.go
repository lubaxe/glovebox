package exfat

import (
	"bytes"
	"errors"
	"testing"

	"github.com/lubaxe/glovebox/internal/exfat/exfattest"
)

func repeat(b byte, n int) []byte { return bytes.Repeat([]byte{b}, n) }

func testImage() *exfattest.Image {
	im := exfattest.New(64)
	im.WriteFile("TeslaCam/RecentClips/old-front.mp4", repeat('x', 100), false)
	im.Delete("TeslaCam/RecentClips/old-front.mp4")
	im.WriteFile("TeslaCam/RecentClips/2026-10-19_16-30-15-front.mp4", append(repeat('a', 4096), repeat('b', 1904)...), false)
	im.WriteFile("TeslaCam/RecentClips/2026-10-19_16-30-15-back.mp4", append(repeat('c', 4096), repeat('d', 904)...), true)
	return im
}

func open(t *testing.T, im *exfattest.Image) *FS {
	t.Helper()
	r := bytes.NewReader(im.B)
	off, err := PartitionOffset(r)
	if err != nil || off != exfattest.PartitionOff {
		t.Fatalf("partition offset %d, %v", off, err)
	}
	f, err := Open(r, off)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func read(im *exfattest.Image, ext []Extent, size int64) []byte {
	var out []byte
	for _, x := range ext {
		out = append(out, im.B[x.Off:x.Off+x.Len]...)
	}
	return out[:size]
}

func TestReadDirAndExtents(t *testing.T) {
	im := testImage()
	f := open(t, im)
	list, err := f.ReadDir("teslacam/RecentClips")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("got %d entries, want 2 (deleted one skipped): %+v", len(list), list)
	}
	front, back := list[0], list[1]
	if front.Name != "2026-10-19_16-30-15-front.mp4" || front.Size != 6000 || front.Dir {
		t.Fatalf("front: %+v", front)
	}
	ext, err := f.Extents(front)
	if err != nil {
		t.Fatal(err)
	}
	if len(ext) != 1 || ext[0].Len != 2*exfattest.Cluster {
		t.Fatalf("front extents %v, want one run of 2 clusters", ext)
	}
	if got := read(im, ext, front.Size); !bytes.Equal(got, append(repeat('a', 4096), repeat('b', 1904)...)) {
		t.Fatal("front extents don't hold its data")
	}
	if back.Name != "2026-10-19_16-30-15-back.mp4" {
		t.Fatalf("back: %+v", back)
	}
	ext, err = f.Extents(back)
	if err != nil {
		t.Fatal(err)
	}
	if len(ext) != 2 {
		t.Fatalf("back extents %v, want two runs", ext)
	}
	if got := read(im, ext, back.Size); !bytes.Equal(got, append(repeat('c', 4096), repeat('d', 904)...)) {
		t.Fatal("back extents don't hold its data")
	}
}

func TestReadDirMissing(t *testing.T) {
	f := open(t, testImage())
	if _, err := f.ReadDir("TeslaCam/SentryClips"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
	root, err := f.ReadDir("")
	if err != nil || len(root) != 1 || root[0].Name != "TeslaCam" || !root[0].Dir {
		t.Fatalf("root: %+v, %v", root, err)
	}
}

func TestBrokenChain(t *testing.T) {
	im := testImage()
	f := open(t, im)
	list, _ := f.ReadDir("TeslaCam/RecentClips")
	ext, _ := f.Extents(list[1])
	// free the back clip's first cluster in the FAT
	first := uint32((ext[0].Off-exfattest.ClusterOff(2))/exfattest.Cluster) + 2
	fatAt := exfattest.PartitionOff + 24*512 + int(first)*4
	copy(im.B[fatAt:], []byte{0, 0, 0, 0})
	if _, err := f.Extents(list[1]); err == nil {
		t.Fatal("broken chain not reported")
	}
}

func TestNotExFAT(t *testing.T) {
	if _, err := Open(bytes.NewReader(make([]byte, 4096)), 0); err == nil {
		t.Fatal("opened a blank image")
	}
}

// Package exfattest builds small exFAT disk images for tests.
package exfattest

import (
	"encoding/binary"
	"path"
	"strings"
	"unicode/utf16"
)

// Geometry of the images built here.
const (
	Sector        = 512
	Cluster       = 4096
	PartitionOff  = 2048 * Sector // where the partition starts in the disk
	fatSector     = 24            // sectors into the partition
	heapSector    = 32
	rootCluster   = 2
	endOfChain    = 0xFFFFFFFF
	entriesPerDir = Cluster / 32
)

// Image is an MBR disk with one exFAT partition. Directories are one
// cluster each.
type Image struct {
	B     []byte
	count uint32
	next  uint32 // next free cluster
	dirs  map[string]*dir
	files map[string]file
}

type dir struct {
	cluster uint32
	slot    int
}

type file struct {
	parent string
	slot   int
}

// New returns an empty image with room for clusters clusters.
func New(clusters int) *Image {
	im := &Image{
		B:     make([]byte, PartitionOff+heapSector*Sector+clusters*Cluster),
		count: uint32(clusters),
		next:  rootCluster + 1,
		dirs:  map[string]*dir{"": {cluster: rootCluster}},
		files: map[string]file{},
	}
	binary.LittleEndian.PutUint32(im.B[446+8:], PartitionOff/Sector)
	im.B[510], im.B[511] = 0x55, 0xAA
	bs := im.B[PartitionOff:]
	copy(bs[3:], "EXFAT   ")
	binary.LittleEndian.PutUint32(bs[80:], fatSector)
	binary.LittleEndian.PutUint32(bs[88:], heapSector)
	binary.LittleEndian.PutUint32(bs[92:], uint32(clusters))
	binary.LittleEndian.PutUint32(bs[96:], rootCluster)
	bs[108], bs[109] = 9, 3 // 512-byte sectors, 8 per cluster
	im.fat(rootCluster, endOfChain)
	return im
}

// ClusterOff is where cluster c starts in the disk.
func ClusterOff(c uint32) int64 {
	return PartitionOff + heapSector*Sector + int64(c-2)*Cluster
}

func (im *Image) cl(c uint32) []byte {
	off := ClusterOff(c)
	return im.B[off : off+Cluster]
}

func (im *Image) fat(c, next uint32) {
	binary.LittleEndian.PutUint32(im.B[PartitionOff+fatSector*Sector+int(c)*4:], next)
}

func (im *Image) alloc() uint32 {
	c := im.next
	if c-2 >= im.count {
		panic("exfattest: image full")
	}
	im.next++
	return c
}

// MkdirAll creates p and its parents.
func (im *Image) MkdirAll(p string) {
	p = strings.Trim(p, "/")
	if p == "" || im.dirs[p] != nil {
		return
	}
	parent := path.Dir(p)
	if parent == "." {
		parent = ""
	}
	im.MkdirAll(parent)
	c := im.alloc()
	im.fat(c, endOfChain)
	im.entry(parent, path.Base(p), true, true, c, Cluster)
	im.dirs[p] = &dir{cluster: c}
}

// WriteFile adds a file. A fragmented file gets every other cluster, in a
// FAT chain; otherwise it's one run, flagged NoFatChain, like the car's
// clips.
func (im *Image) WriteFile(p string, data []byte, fragmented bool) {
	p = strings.Trim(p, "/")
	parent := path.Dir(p)
	if parent == "." {
		parent = ""
	}
	im.MkdirAll(parent)
	n := (len(data) + Cluster - 1) / Cluster
	var clusters []uint32
	for i := 0; i < n; i++ {
		clusters = append(clusters, im.alloc())
		if fragmented {
			im.alloc() // leave a gap
		}
	}
	for i, c := range clusters {
		copy(im.cl(c), data[i*Cluster:min(len(data), (i+1)*Cluster)])
		if fragmented {
			next := uint32(endOfChain)
			if i+1 < len(clusters) {
				next = clusters[i+1]
			}
			im.fat(c, next)
		}
	}
	first := uint32(0)
	if n > 0 {
		first = clusters[0]
	}
	slot := im.entry(parent, path.Base(p), false, !fragmented, first, int64(len(data)))
	im.files[p] = file{parent: parent, slot: slot}
}

// Delete marks a file's directory entries deleted, as the car does; its data
// stays.
func (im *Image) Delete(p string) {
	f := im.files[strings.Trim(p, "/")]
	d := im.cl(im.dirs[f.parent].cluster)
	n := int(d[32*f.slot+1]) + 1
	for k := 0; k < n; k++ {
		d[32*(f.slot+k)] &^= 0x80
	}
}

func (im *Image) entry(parent, name string, isDir, contiguous bool, first uint32, size int64) int {
	d := im.dirs[parent]
	u := utf16.Encode([]rune(name))
	names := (len(u) + 14) / 15
	if d.slot+2+names > entriesPerDir {
		panic("exfattest: directory full")
	}
	set := im.cl(d.cluster)[32*d.slot:]
	set[0], set[1] = 0x85, byte(1+names)
	if isDir {
		set[4] = 0x10
	}
	st := set[32:64]
	st[0], st[3] = 0xC0, byte(len(u))
	st[1] = 0x01
	if contiguous {
		st[1] |= 0x02
	}
	binary.LittleEndian.PutUint64(st[8:], uint64(size))
	binary.LittleEndian.PutUint32(st[20:], first)
	binary.LittleEndian.PutUint64(st[24:], uint64(size))
	for k := 0; k < names; k++ {
		ne := set[64+32*k:]
		ne[0] = 0xC1
		for j := 0; j < 15 && 15*k+j < len(u); j++ {
			binary.LittleEndian.PutUint16(ne[2+2*j:], u[15*k+j])
		}
	}
	slot := d.slot
	d.slot += 2 + names
	return slot
}

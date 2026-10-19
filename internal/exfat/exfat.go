// Package exfat reads where files lie in an exFAT file system, without
// mounting it. The clip store uses it to clone single clips out of a
// snapshot image (see package store).
package exfat

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf16"
)

// ErrNotFound means a path doesn't exist.
var ErrNotFound = errors.New("no such file or directory")

// FS is an exFAT file system read through an io.ReaderAt.
type FS struct {
	r       io.ReaderAt
	fatOff  int64 // absolute offsets in r
	heapOff int64
	cluster int64 // bytes per cluster
	count   uint32
	root    uint32
}

// Entry is a file or directory.
type Entry struct {
	Name string
	Dir  bool
	Size int64
	// where the data starts, and whether it's one run (the NoFatChain flag)
	// rather than a chain in the FAT
	first      uint32
	contiguous bool
}

// Extent is a run of bytes in the underlying reader.
type Extent struct {
	Off, Len int64
}

// PartitionOffset returns where the first partition of an MBR disk image
// starts.
func PartitionOffset(r io.ReaderAt) (int64, error) {
	var mbr [512]byte
	if _, err := r.ReadAt(mbr[:], 0); err != nil {
		return 0, err
	}
	if mbr[510] != 0x55 || mbr[511] != 0xAA {
		return 0, errors.New("no partition table")
	}
	lba := binary.LittleEndian.Uint32(mbr[446+8:])
	if lba == 0 {
		return 0, errors.New("no first partition")
	}
	return int64(lba) * 512, nil
}

// Open reads the boot sector of the file system that starts at off in r.
func Open(r io.ReaderAt, off int64) (*FS, error) {
	var bs [512]byte
	if _, err := r.ReadAt(bs[:], off); err != nil {
		return nil, err
	}
	if string(bs[3:11]) != "EXFAT   " {
		return nil, errors.New("not an exFAT file system")
	}
	shift, cshift := bs[108], bs[109]
	if shift < 9 || shift > 12 || int(shift)+int(cshift) > 25 {
		return nil, fmt.Errorf("bad exFAT geometry (%d, %d)", shift, cshift)
	}
	sector := int64(1) << shift
	f := &FS{
		r:       r,
		fatOff:  off + int64(binary.LittleEndian.Uint32(bs[80:]))*sector,
		heapOff: off + int64(binary.LittleEndian.Uint32(bs[88:]))*sector,
		cluster: sector << cshift,
		count:   binary.LittleEndian.Uint32(bs[92:]),
		root:    binary.LittleEndian.Uint32(bs[96:]),
	}
	if !f.valid(f.root) {
		return nil, errors.New("bad exFAT root directory")
	}
	return f, nil
}

// ClusterSize is the size of an allocation unit in bytes.
func (f *FS) ClusterSize() int64 { return f.cluster }

func (f *FS) valid(c uint32) bool { return c >= 2 && c-2 < f.count }

func (f *FS) clusterOff(c uint32) int64 { return f.heapOff + int64(c-2)*f.cluster }

// runs returns the extents holding n bytes of data starting at first.
func (f *FS) runs(first uint32, contiguous bool, n int64) ([]Extent, error) {
	if n == 0 {
		return nil, nil
	}
	clusters := (n + f.cluster - 1) / f.cluster
	if !f.valid(first) || clusters > int64(f.count) {
		return nil, errors.New("bad cluster in directory entry")
	}
	if contiguous {
		if int64(first-2)+clusters > int64(f.count) {
			return nil, errors.New("file runs past the end of the volume")
		}
		return []Extent{{f.clusterOff(first), clusters * f.cluster}}, nil
	}
	var out []Extent
	c := first
	var buf [4]byte
	for i := int64(0); i < clusters; i++ {
		if !f.valid(c) {
			return nil, fmt.Errorf("broken cluster chain at %d", c)
		}
		off := f.clusterOff(c)
		if k := len(out) - 1; k >= 0 && out[k].Off+out[k].Len == off {
			out[k].Len += f.cluster
		} else {
			out = append(out, Extent{off, f.cluster})
		}
		if _, err := f.r.ReadAt(buf[:], f.fatOff+int64(c)*4); err != nil {
			return nil, err
		}
		c = binary.LittleEndian.Uint32(buf[:])
	}
	return out, nil
}

// Extents returns where e's data lies in the underlying reader, in order,
// in whole clusters (the last one may run past e.Size).
func (f *FS) Extents(e Entry) ([]Extent, error) {
	return f.runs(e.first, e.contiguous, e.Size)
}

func (f *FS) read(ext []Extent) ([]byte, error) {
	var n int64
	for _, x := range ext {
		n += x.Len
	}
	if n > 64<<20 {
		return nil, errors.New("directory too large")
	}
	buf := make([]byte, 0, n)
	for _, x := range ext {
		b := make([]byte, x.Len)
		if _, err := f.r.ReadAt(b, x.Off); err != nil {
			return nil, err
		}
		buf = append(buf, b...)
	}
	return buf, nil
}

// ReadDir lists the directory at p (slash-separated, "" for the root).
// Names are matched case-insensitively for ASCII, like the car does.
func (f *FS) ReadDir(p string) ([]Entry, error) {
	dir := Entry{Dir: true, first: f.root}
	var ext []Extent
	var err error
	// the root has no size; read its chain to the end
	if ext, err = f.chain(f.root); err != nil {
		return nil, err
	}
	for _, name := range strings.Split(strings.Trim(p, "/"), "/") {
		if name == "" {
			continue
		}
		list, err := f.entries(ext)
		if err != nil {
			return nil, err
		}
		found := false
		for _, e := range list {
			if e.Dir && strings.EqualFold(e.Name, name) {
				dir, found = e, true
				break
			}
		}
		if !found {
			return nil, ErrNotFound
		}
		if ext, err = f.runs(dir.first, dir.contiguous, dir.Size); err != nil {
			return nil, err
		}
	}
	return f.entries(ext)
}

// chain follows a FAT chain to its end.
func (f *FS) chain(first uint32) ([]Extent, error) {
	var out []Extent
	c := first
	var buf [4]byte
	for i := uint32(0); f.valid(c); i++ {
		if i > f.count {
			return nil, errors.New("cluster chain loops")
		}
		off := f.clusterOff(c)
		if k := len(out) - 1; k >= 0 && out[k].Off+out[k].Len == off {
			out[k].Len += f.cluster
		} else {
			out = append(out, Extent{off, f.cluster})
		}
		if _, err := f.r.ReadAt(buf[:], f.fatOff+int64(c)*4); err != nil {
			return nil, err
		}
		c = binary.LittleEndian.Uint32(buf[:])
	}
	return out, nil
}

// Directory entry types (in use).
const (
	typeFile   = 0x85
	typeStream = 0xC0
	typeName   = 0xC1
)

func (f *FS) entries(ext []Extent) ([]Entry, error) {
	data, err := f.read(ext)
	if err != nil {
		return nil, err
	}
	var out []Entry
	for i := 0; i+32 <= len(data); i += 32 {
		t := data[i]
		if t == 0 {
			break // end of directory
		}
		if t != typeFile {
			continue // deleted, secondary or other entries
		}
		sec := int(data[i+1])
		if sec < 2 || i+32*(sec+1) > len(data) || data[i+32] != typeStream {
			continue
		}
		attr := binary.LittleEndian.Uint16(data[i+4:])
		st := data[i+32 : i+64]
		nameLen := int(st[3])
		e := Entry{
			Dir:        attr&0x10 != 0,
			contiguous: st[1]&0x02 != 0,
			first:      binary.LittleEndian.Uint32(st[20:]),
			Size:       int64(binary.LittleEndian.Uint64(st[24:])),
		}
		var name []uint16
		for k := 2; k <= sec && len(name) < nameLen; k++ {
			ne := data[i+32*k : i+32*(k+1)]
			if ne[0] != typeName {
				break
			}
			for j := 2; j < 32 && len(name) < nameLen; j += 2 {
				name = append(name, binary.LittleEndian.Uint16(ne[j:]))
			}
		}
		e.Name = string(utf16.Decode(name))
		out = append(out, e)
		i += 32 * sec
	}
	return out, nil
}

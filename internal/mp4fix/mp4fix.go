// Package mp4fix serves Tesla's MP4 recordings so browsers play them well:
//
//   - their 'ctts' boxes trip up Chrome, so reads present those boxes as
//     'free' boxes, which players skip;
//   - the car writes the index ('moov') after the video data ('mdat'), so a
//     player has to fetch the end of the file before it can show anything.
//     Reads present the file "fast-start", with the index first and its
//     chunk offsets moved to match. Safari, which wants the index of every
//     camera before it plays any, starts much sooner.
//
// The files on disk aren't changed.
package mp4fix

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math"
)

// containers are the boxes on the path from the top level to 'ctts', 'stco'
// and 'co64'.
var containers = map[string]bool{"moov": true, "trak": true, "mdia": true, "minf": true, "stbl": true}

// FindCTTS returns the offsets of the type fields of all 'ctts' boxes in an
// MP4 file of the given size. It returns nil if r isn't an MP4 file.
func FindCTTS(r io.ReaderAt, size int64) ([]int64, error) {
	if !isMP4(r, size) {
		return nil, nil
	}
	found := map[string][]int64{}
	err := walk(r, 0, size, 0, found)
	return found["ctts"], err
}

func isMP4(r io.ReaderAt, size int64) bool {
	h, err := readHeader(r, 0, size)
	return err == nil && h.typ == "ftyp"
}

type header struct {
	typ        string
	size       int64 // whole box, header included
	headerSize int64
}

func readHeader(r io.ReaderAt, off, end int64) (header, error) {
	var h header
	if end-off < 8 {
		return h, io.ErrUnexpectedEOF
	}
	var b [16]byte
	if _, err := r.ReadAt(b[:8], off); err != nil {
		return h, err
	}
	h.typ = string(b[4:8])
	h.headerSize = 8
	switch size := binary.BigEndian.Uint32(b[:4]); size {
	case 0: // box runs to the end of its parent
		h.size = end - off
	case 1: // 64-bit size follows the type
		if end-off < 16 {
			return h, io.ErrUnexpectedEOF
		}
		if _, err := r.ReadAt(b[8:16], off+8); err != nil {
			return h, err
		}
		h.size = int64(binary.BigEndian.Uint64(b[8:16]))
		h.headerSize = 16
	default:
		h.size = int64(size)
	}
	if h.size < h.headerSize || off+h.size > end {
		return h, errors.New("mp4: box extends past its parent")
	}
	return h, nil
}

// walk records, by type, the offsets of the type fields of the 'ctts',
// 'stco' and 'co64' boxes between start and end.
func walk(r io.ReaderAt, start, end int64, depth int, found map[string][]int64) error {
	if depth > len(containers) {
		return nil
	}
	for off := start; end-off >= 8; {
		h, err := readHeader(r, off, end)
		if err != nil {
			// A truncated file (e.g. snapshotted mid-recording) still has
			// usable boxes before the damage.
			return nil
		}
		switch {
		case h.typ == "ctts" || h.typ == "stco" || h.typ == "co64":
			found[h.typ] = append(found[h.typ], off+4)
		case containers[h.typ]:
			if err := walk(r, off+h.headerSize, off+h.size, depth+1, found); err != nil {
				return err
			}
		}
		off += h.size
	}
	return nil
}

// File is an io.ReadSeeker over an MP4 file that reads 'ctts' box types as
// 'free' and, where it can, presents the index before the video data.
type File struct {
	r     io.ReaderAt
	size  int64
	pos   int64
	patch []int64
	// segs, when set, lay out the presented file: pieces of the original
	// and the rewritten index
	segs    []segment
	hadCTTS bool
}

// segment is a piece of the presented file starting at at: n bytes of the
// original from src, or data.
type segment struct {
	at, n, src int64
	data       []byte
}

// maxMoov bounds the index read into memory; Tesla's are about 30 KB.
const maxMoov = 8 << 20

// NewFile wraps r, which holds an MP4 file of the given size.
func NewFile(r io.ReaderAt, size int64) (*File, error) {
	patch, err := FindCTTS(r, size)
	if err != nil {
		return nil, err
	}
	f := &File{r: r, size: size, patch: patch, hadCTTS: len(patch) > 0}
	if isMP4(r, size) {
		if f.segs = faststart(r, size); f.segs != nil {
			f.patch = nil // the rewritten index has them patched
		}
	}
	return f, nil
}

// Patched reports whether the file had any 'ctts' boxes.
func (f *File) Patched() bool { return f.hadCTTS }

// FastStart reports whether reads present the index first.
func (f *File) FastStart() bool { return f.segs != nil }

// faststart lays out the file with 'moov' moved in front of 'mdat'. It
// returns nil, and the file is served as it is, when the file isn't laid
// out as expected: the index already first, more than one of either box,
// chunk offsets that don't point into the video data, or 32-bit offsets
// that would overflow.
func faststart(r io.ReaderAt, size int64) []segment {
	mdat, moov := -1, -1
	var tops []header
	var offs []int64
	for off := int64(0); off < size; {
		h, err := readHeader(r, off, size)
		if err != nil {
			return nil
		}
		switch h.typ {
		case "mdat":
			if mdat >= 0 {
				return nil
			}
			mdat = len(tops)
		case "moov":
			if moov >= 0 {
				return nil
			}
			moov = len(tops)
		}
		tops = append(tops, h)
		offs = append(offs, off)
		off += h.size
	}
	if mdat < 0 || moov < mdat || tops[moov].size > maxMoov {
		return nil
	}
	moovAt, moovSize := offs[moov], tops[moov].size
	index := make([]byte, moovSize)
	if _, err := r.ReadAt(index, moovAt); err != nil {
		return nil
	}
	found := map[string][]int64{}
	if err := walk(bytes.NewReader(index), 0, moovSize, 0, found); err != nil {
		return nil
	}
	for _, at := range found["ctts"] {
		copy(index[at:at+4], "free")
	}
	// Everything from the start of 'mdat' to where 'moov' was moves up by
	// the size of 'moov'; every chunk offset must point in there.
	lo, hi := offs[mdat], moovAt
	if !shiftOffsets(index, found["stco"], 4, lo, hi, moovSize) ||
		!shiftOffsets(index, found["co64"], 8, lo, hi, moovSize) {
		return nil
	}
	// before 'mdat' | 'moov' | 'mdat' up to where 'moov' was | after 'moov'
	segs := []segment{
		{at: 0, n: lo, src: 0},
		{at: lo, n: moovSize, data: index},
		{at: lo + moovSize, n: hi - lo, src: lo},
	}
	if end := moovAt + moovSize; end < size {
		segs = append(segs, segment{at: end, n: size - end, src: end})
	}
	return segs
}

// shiftOffsets adds by to the width-byte chunk offsets of the boxes whose
// type fields are at boxes in index, which must all lie in lo..hi.
func shiftOffsets(index []byte, boxes []int64, width, lo, hi, by int64) bool {
	n := int64(len(index))
	for _, at := range boxes {
		count := at + 4 + 4 // past the type, version and flags
		if count+4 > n {
			return false
		}
		entries := count + 4
		k := int64(binary.BigEndian.Uint32(index[count:]))
		if entries+k*width > n {
			return false
		}
		for i := int64(0); i < k; i++ {
			e := index[entries+i*width:]
			var v int64
			if width == 4 {
				v = int64(binary.BigEndian.Uint32(e))
			} else {
				v = int64(binary.BigEndian.Uint64(e))
			}
			if v < lo || v >= hi {
				return false
			}
			v += by
			if width == 4 {
				if v > math.MaxUint32 {
					return false
				}
				binary.BigEndian.PutUint32(e, uint32(v))
			} else {
				binary.BigEndian.PutUint64(e, uint64(v))
			}
		}
	}
	return true
}

// Read implements io.Reader.
func (f *File) Read(p []byte) (int, error) {
	if f.pos >= f.size {
		return 0, io.EOF
	}
	if rem := f.size - f.pos; int64(len(p)) > rem {
		p = p[:rem]
	}
	if f.segs != nil {
		return f.readSegment(p)
	}
	n, err := f.r.ReadAt(p, f.pos)
	for _, off := range f.patch {
		for i := int64(0); i < 4; i++ {
			if at := off + i - f.pos; at >= 0 && at < int64(n) {
				p[at] = "free"[i]
			}
		}
	}
	f.pos += int64(n)
	if err == io.EOF && n > 0 {
		err = nil
	}
	return n, err
}

// readSegment reads from the segment holding pos, up to its end.
func (f *File) readSegment(p []byte) (int, error) {
	for _, s := range f.segs {
		if f.pos < s.at || f.pos >= s.at+s.n {
			continue
		}
		in := f.pos - s.at
		if rem := s.n - in; int64(len(p)) > rem {
			p = p[:rem]
		}
		var n int
		var err error
		if s.data != nil {
			n = copy(p, s.data[in:])
		} else {
			n, err = f.r.ReadAt(p, s.src+in)
		}
		f.pos += int64(n)
		if err == io.EOF && n > 0 {
			err = nil
		}
		return n, err
	}
	return 0, io.EOF
}

// Seek implements io.Seeker.
func (f *File) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		offset += f.pos
	case io.SeekEnd:
		offset += f.size
	default:
		return 0, errors.New("mp4fix: invalid whence")
	}
	if offset < 0 {
		return 0, errors.New("mp4fix: negative position")
	}
	f.pos = offset
	return offset, nil
}

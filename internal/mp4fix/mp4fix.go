// Package mp4fix serves Tesla's MP4 recordings so Chromium-based browsers
// play them: their 'ctts' boxes trip up Chrome, so reads present those boxes
// as 'free' boxes, which players skip. The files on disk aren't changed.
package mp4fix

import (
	"encoding/binary"
	"errors"
	"io"
)

// containers are the boxes on the path from the top level to 'ctts'.
var containers = map[string]bool{"moov": true, "trak": true, "mdia": true, "minf": true, "stbl": true}

// FindCTTS returns the offsets of the type fields of all 'ctts' boxes in an
// MP4 file of the given size. It returns nil if r isn't an MP4 file.
func FindCTTS(r io.ReaderAt, size int64) ([]int64, error) {
	first, err := readHeader(r, 0, size)
	if err != nil || first.typ != "ftyp" {
		return nil, nil
	}
	var found []int64
	err = walk(r, 0, size, 0, &found)
	return found, err
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

func walk(r io.ReaderAt, start, end int64, depth int, found *[]int64) error {
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
		case h.typ == "ctts":
			*found = append(*found, off+4)
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
// 'free'.
type File struct {
	r     io.ReaderAt
	size  int64
	pos   int64
	patch []int64
}

// NewFile wraps r, which holds an MP4 file of the given size.
func NewFile(r io.ReaderAt, size int64) (*File, error) {
	patch, err := FindCTTS(r, size)
	if err != nil {
		return nil, err
	}
	return &File{r: r, size: size, patch: patch}, nil
}

// Patched reports whether the file had any 'ctts' boxes.
func (f *File) Patched() bool { return len(f.patch) > 0 }

// Read implements io.Reader.
func (f *File) Read(p []byte) (int, error) {
	if f.pos >= f.size {
		return 0, io.EOF
	}
	if rem := f.size - f.pos; int64(len(p)) > rem {
		p = p[:rem]
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

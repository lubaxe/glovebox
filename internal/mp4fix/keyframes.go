package mp4fix

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math"
)

// ErrNoKeyframes means a file can't be presented keyframes-only: it isn't
// an MP4 with a complete index (a clip snapshotted while the car was still
// writing it has none yet), or has no list of keyframes.
var ErrNoKeyframes = errors.New("mp4: no keyframe index")

// maxSamples bounds the sample tables read; a minute of Tesla video has
// about 2,200 samples.
const maxSamples = 1 << 20

// Keyframes presents an MP4 file of the given size with only its video
// track's keyframes, for low-bandwidth playback. Tesla's cameras write one
// every half second, so the copy plays at 2 frames a second at full
// resolution, for about a fifth of the bytes. Each keyframe keeps its
// decode time (the time the viewer plays the full clip by), and the index
// comes first; like File, the original isn't changed.
func Keyframes(r io.ReaderAt, size int64) (*File, error) {
	if !isMP4(r, size) {
		return nil, ErrNoKeyframes
	}
	var ftyp, moov []byte
	for off := int64(0); off < size; {
		h, err := readHeader(r, off, size)
		if err != nil {
			break // a damaged tail; what's before it may do
		}
		switch h.typ {
		case "ftyp", "moov":
			if h.size > maxMoov {
				return nil, ErrNoKeyframes
			}
			b := make([]byte, h.size)
			if _, err := r.ReadAt(b, off); err != nil {
				return nil, err
			}
			if h.typ == "ftyp" {
				ftyp = b
			} else {
				moov = b
			}
		}
		off += h.size
	}
	if ftyp == nil || moov == nil {
		return nil, ErrNoKeyframes
	}
	moovBody, ok := body(moov)
	if !ok {
		return nil, ErrNoKeyframes
	}
	var video []byte
	for _, b := range children(moovBody) {
		if b.typ == "trak" && handler(b.body) == "vide" {
			if video != nil {
				return nil, ErrNoKeyframes // which one?
			}
			video = b.body
		}
	}
	stbl := path(video, "mdia", "minf", "stbl")
	if stbl == nil {
		return nil, ErrNoKeyframes
	}
	keys, err := keyframes(stbl, size)
	if err != nil {
		return nil, err
	}

	// ftyp | moov (with the keyframes' index) | mdat (the keyframes)
	var data int64
	for _, k := range keys {
		data += k.size
	}
	build := func(base int64) []byte {
		return newBox("moov", rebuild(moovBody, stblFor(stbl, keys, base)))
	}
	newMoov := build(0)
	header := int64(len(ftyp) + len(newMoov) + 8)
	if header+data > math.MaxUint32 {
		return nil, ErrNoKeyframes
	}
	newMoov = build(header) // the same size: offsets are fixed width
	head := bytes.Join([][]byte{ftyp, newMoov, newBoxHeader("mdat", 8+data)}, nil)
	segs := []segment{{at: 0, n: int64(len(head)), data: head}}
	at := int64(len(head))
	for _, k := range keys {
		segs = append(segs, segment{at: at, n: k.size, src: k.offset})
		at += k.size
	}
	return &File{r: r, size: at, segs: segs}, nil
}

type keyframe struct {
	offset, size int64
	duration     uint32
}

// keyframes lists the keyframes in a sample table, each lasting until the
// next one; the last one lasts until the end of the track.
func keyframes(stbl []byte, fileSize int64) ([]keyframe, error) {
	sizes, err := sampleSizes(path(stbl, "stsz"))
	if err != nil {
		return nil, err
	}
	n := len(sizes)
	offsets, err := sampleOffsets(stbl, sizes)
	if err != nil {
		return nil, err
	}
	times, end, err := decodeTimes(path(stbl, "stts"), n)
	if err != nil {
		return nil, err
	}
	stss := path(stbl, "stss")
	sync, err := table(stss, 4)
	if err != nil || stss == nil {
		return nil, ErrNoKeyframes
	}
	var keys []keyframe
	var at []uint64
	for _, e := range sync {
		i := int(binary.BigEndian.Uint32(e)) - 1
		if i < 0 || i >= n || (len(at) > 0 && times[i] <= at[len(at)-1]) {
			return nil, ErrNoKeyframes
		}
		if offsets[i]+sizes[i] > fileSize {
			break // past a truncated end
		}
		keys = append(keys, keyframe{offset: offsets[i], size: sizes[i]})
		at = append(at, times[i])
	}
	if len(keys) == 0 {
		return nil, ErrNoKeyframes
	}
	// The first one starts the track, as the first frame does.
	at[0] = 0
	for k := range keys {
		next := end
		if k+1 < len(keys) {
			next = at[k+1]
		}
		if next-at[k] > math.MaxUint32 {
			return nil, ErrNoKeyframes
		}
		keys[k].duration = uint32(next - at[k])
	}
	return keys, nil
}

func sampleSizes(stsz []byte) ([]int64, error) {
	if len(stsz) < 12 {
		return nil, ErrNoKeyframes
	}
	fixed := binary.BigEndian.Uint32(stsz[4:])
	n := int(binary.BigEndian.Uint32(stsz[8:]))
	if n > maxSamples || (fixed == 0 && n > (len(stsz)-12)/4) {
		return nil, ErrNoKeyframes
	}
	sizes := make([]int64, n)
	for i := range sizes {
		if fixed != 0 {
			sizes[i] = int64(fixed)
		} else {
			sizes[i] = int64(binary.BigEndian.Uint32(stsz[12+4*i:]))
		}
	}
	return sizes, nil
}

// sampleOffsets finds where in the file each sample is, from the chunk
// offsets and the samples in each chunk.
func sampleOffsets(stbl []byte, sizes []int64) ([]int64, error) {
	width := 4
	co := path(stbl, "stco")
	if co == nil {
		width, co = 8, path(stbl, "co64")
	}
	chunks, err := table(co, width)
	if err != nil {
		return nil, err
	}
	runs, err := table(path(stbl, "stsc"), 12)
	if err != nil || len(runs) == 0 {
		return nil, ErrNoKeyframes
	}
	offsets := make([]int64, 0, len(sizes))
	for r, run := range runs {
		first := int(binary.BigEndian.Uint32(run)) - 1
		per := int(binary.BigEndian.Uint32(run[4:]))
		last := len(chunks)
		if r+1 < len(runs) {
			last = int(binary.BigEndian.Uint32(runs[r+1])) - 1
		}
		if first < 0 || last > len(chunks) || first > last || per > maxSamples {
			return nil, ErrNoKeyframes
		}
		for c := first; c < last && len(offsets) < len(sizes); c++ {
			var off int64
			if width == 4 {
				off = int64(binary.BigEndian.Uint32(chunks[c]))
			} else {
				off = int64(binary.BigEndian.Uint64(chunks[c]))
			}
			for range per {
				if len(offsets) == len(sizes) {
					break
				}
				offsets = append(offsets, off)
				off += sizes[len(offsets)-1]
			}
		}
	}
	if len(offsets) != len(sizes) {
		return nil, ErrNoKeyframes
	}
	return offsets, nil
}

// decodeTimes returns each sample's decode time and the track's end.
func decodeTimes(stts []byte, n int) ([]uint64, uint64, error) {
	runs, err := table(stts, 8)
	if err != nil {
		return nil, 0, err
	}
	times := make([]uint64, 0, n)
	var t uint64
	for _, run := range runs {
		count := int(binary.BigEndian.Uint32(run))
		delta := uint64(binary.BigEndian.Uint32(run[4:]))
		for range count {
			if len(times) == n {
				break
			}
			times = append(times, t)
			t += delta
		}
	}
	if len(times) != n {
		return nil, 0, ErrNoKeyframes
	}
	return times, t, nil
}

// table returns the entries of a full box that's a count and then entries
// of width bytes.
func table(b []byte, width int) ([][]byte, error) {
	if len(b) < 8 {
		return nil, ErrNoKeyframes
	}
	n := int(binary.BigEndian.Uint32(b[4:]))
	if n > maxSamples || n > (len(b)-8)/width {
		return nil, ErrNoKeyframes
	}
	out := make([][]byte, n)
	for i := range out {
		out[i] = b[8+i*width : 8+(i+1)*width]
	}
	return out, nil
}

// stblFor returns a sample table for the keyframes: the original sample
// descriptions, then one sample per chunk, all of them sync samples, at
// offsets from base. Composition offsets are left out, as File's 'free'
// patch leaves them out of the full clip.
func stblFor(stbl []byte, keys []keyframe, base int64) []byte {
	var stsd []byte
	for _, b := range children(stbl) {
		if b.typ == "stsd" {
			stsd = b.raw
		}
	}
	u32 := binary.BigEndian.AppendUint32
	full := func(n int) []byte { return u32(u32(nil, 0), uint32(n)) } // version, flags, count

	stts := full(0)
	runs := 0
	for k, key := range keys {
		if k > 0 && key.duration == keys[k-1].duration {
			c := len(stts) - 8
			binary.BigEndian.PutUint32(stts[c:], binary.BigEndian.Uint32(stts[c:])+1)
			continue
		}
		stts = u32(u32(stts, 1), key.duration)
		runs++
	}
	binary.BigEndian.PutUint32(stts[4:], uint32(runs))

	stsz := u32(u32(u32(nil, 0), 0), uint32(len(keys)))
	stco := full(len(keys))
	off := base
	for _, key := range keys {
		stsz = u32(stsz, uint32(key.size))
		stco = u32(stco, uint32(off))
		off += key.size
	}
	stsc := u32(u32(u32(full(1), 1), 1), 1) // every chunk: 1 sample, description 1
	return bytes.Join([][]byte{stsd, newBox("stts", stts), newBox("stsc", stsc), newBox("stsz", stsz), newBox("stco", stco)}, nil)
}

// rebuild copies a container's boxes with the video track's 'stbl'
// replaced, rebuilding the containers on the way to it. Other tracks (e.g.
// metadata) are left out.
func rebuild(b []byte, stbl []byte) []byte {
	var out []byte
	for _, c := range children(b) {
		switch {
		case c.typ == "trak" && handler(c.body) != "vide":
		case c.typ == "stbl":
			out = append(out, newBox("stbl", stbl)...)
		case c.typ == "trak" || c.typ == "mdia" || c.typ == "minf":
			out = append(out, newBox(c.typ, rebuild(c.body, stbl))...)
		default:
			out = append(out, c.raw...)
		}
	}
	return out
}

type child struct {
	typ       string
	raw, body []byte
}

// children splits a container's body into its boxes, as far as they're
// well formed.
func children(b []byte) []child {
	var out []child
	r := bytes.NewReader(b)
	end := int64(len(b))
	for off := int64(0); end-off >= 8; {
		h, err := readHeader(r, off, end)
		if err != nil {
			break
		}
		out = append(out, child{typ: h.typ, raw: b[off : off+h.size], body: b[off+h.headerSize : off+h.size]})
		off += h.size
	}
	return out
}

func body(b []byte) ([]byte, bool) {
	c := children(b)
	if len(c) != 1 {
		return nil, false
	}
	return c[0].body, true
}

// path returns the body of the first box down the path of types from a
// container's body, or nil.
func path(b []byte, types ...string) []byte {
	for _, t := range types {
		var next []byte
		for _, c := range children(b) {
			if c.typ == t {
				next = c.body
				break
			}
		}
		if next == nil {
			return nil
		}
		b = next
	}
	return b
}

// handler returns a track's handler type, e.g. "vide".
func handler(trak []byte) string {
	if h := path(trak, "mdia", "hdlr"); len(h) >= 12 {
		return string(h[8:12])
	}
	return ""
}

func newBoxHeader(typ string, size int64) []byte {
	return append(binary.BigEndian.AppendUint32(nil, uint32(size)), typ...)
}

func newBox(typ string, body []byte) []byte {
	return append(newBoxHeader(typ, int64(8+len(body))), body...)
}

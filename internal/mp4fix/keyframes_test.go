package mp4fix

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"testing"
)

func u32s(vs ...uint32) []byte {
	var b []byte
	for _, v := range vs {
		b = binary.BigEndian.AppendUint32(b, v)
	}
	return b
}

func hdlr(kind string) []byte {
	return fullBox("hdlr", append(append(u32s(0), kind...), make([]byte, 13)...))
}

// keyframeMP4 is laid out like the car's files (index last): six video
// samples of 10 ticks, two to a chunk, with keyframes 1 and 4, and a
// metadata track.
func keyframeMP4() (data []byte, samples [][]byte) {
	for i := range 6 {
		samples = append(samples, bytes.Repeat([]byte{byte('a' + i)}, 5+i))
	}
	head := box("ftyp", []byte("isom"))
	mdat := box("mdat", bytes.Join(samples, nil))
	at := uint32(len(head) + 8)
	var chunks []uint32
	for i := 0; i < 6; i += 2 {
		chunks = append(chunks, at)
		at += uint32(len(samples[i]) + len(samples[i+1]))
	}
	stbl := box("stbl",
		fullBox("stsd", u32s(1)),
		fullBox("stts", u32s(1, 6, 10)),
		fullBox("ctts", u32s(1, 6, 20)),
		fullBox("stss", u32s(2, 1, 4)),
		fullBox("stsz", u32s(0, 6, 5, 6, 7, 8, 9, 10)),
		fullBox("stsc", u32s(1, 1, 2, 1)),
		stco(chunks...))
	video := box("trak", box("tkhd", []byte("video")), box("edts", []byte("edit")),
		box("mdia", fullBox("mdhd", []byte("media")), hdlr("vide"), box("minf", box("vmhd", nil), stbl)))
	meta := box("trak", box("tkhd", []byte("meta")), box("mdia", hdlr("meta"), box("minf", box("stbl", stco(chunks[0])))))
	moov := box("moov", box("mvhd", []byte("movie")), video, meta)
	return bytes.Join([][]byte{head, mdat, moov}, nil), samples
}

func TestKeyframes(t *testing.T) {
	data, samples := keyframeMP4()
	f, err := Keyframes(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}

	tops := children(got)
	if len(tops) != 3 || tops[0].typ != "ftyp" || tops[1].typ != "moov" || tops[2].typ != "mdat" {
		t.Fatalf("top-level boxes: %v", tops)
	}
	moov := tops[1].body
	if n := len(children(moov)); n != 2 {
		t.Errorf("moov has %d boxes, want mvhd and the video track", n)
	}
	if path(moov, "trak", "edts") == nil || path(moov, "trak", "mdia", "minf", "vmhd") == nil {
		t.Error("the video track's other boxes weren't kept")
	}
	stbl := path(moov, "trak", "mdia", "minf", "stbl")
	if path(stbl, "ctts") != nil || path(stbl, "stss") != nil {
		t.Error("composition offsets or sync table kept")
	}
	// every sample is a keyframe now; say so to read them back
	stbl = append(append([]byte{}, stbl...), fullBox("stss", u32s(2, 1, 2))...)
	keys, err := keyframes(stbl, int64(len(got)))
	if err != nil || len(keys) != 2 {
		t.Fatalf("keyframes = %v, %v", keys, err)
	}
	for k, want := range [][]byte{samples[0], samples[3]} {
		if b := got[keys[k].offset : keys[k].offset+keys[k].size]; !bytes.Equal(b, want) {
			t.Errorf("keyframe %d reads %q, want %q", k, b, want)
		}
		// each lasts until the next, the last until the end: 3 samples of 10
		if keys[k].duration != 30 {
			t.Errorf("keyframe %d lasts %d", k, keys[k].duration)
		}
	}
	if !bytes.Equal(tops[2].body, append(append([]byte{}, samples[0]...), samples[3]...)) {
		t.Errorf("mdat = %q", tops[2].body)
	}
}

func TestKeyframesSeek(t *testing.T) {
	data, _ := keyframeMP4()
	f, _ := Keyframes(bytes.NewReader(data), int64(len(data)))
	all, _ := io.ReadAll(f)
	for _, at := range []int64{0, 7, int64(len(all)) - 12, int64(len(all)) - 1} {
		if _, err := f.Seek(at, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(f)
		if !bytes.Equal(b, all[at:]) {
			t.Errorf("from %d: %q, want %q", at, b, all[at:])
		}
	}
}

func TestKeyframesRefused(t *testing.T) {
	data, _ := keyframeMP4()
	moovAt := bytes.Index(data, []byte("moov")) - 4
	for name, d := range map[string][]byte{
		"no index yet": data[:moovAt],
		"not an MP4":   []byte("plain text, not a video at all"),
	} {
		if _, err := Keyframes(bytes.NewReader(d), int64(len(d))); !errors.Is(err, ErrNoKeyframes) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func (c child) String() string { return fmt.Sprintf("%s(%d)", c.typ, len(c.raw)) }

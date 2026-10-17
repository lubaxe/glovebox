package mp4fix

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"
)

func box(typ string, payload ...[]byte) []byte {
	body := bytes.Join(payload, nil)
	b := make([]byte, 8, 8+len(body))
	binary.BigEndian.PutUint32(b, uint32(8+len(body)))
	copy(b[4:], typ)
	return append(b, body...)
}

// testMP4 has a video track with a ctts box and an audio track without one.
func testMP4() []byte {
	stbl := func(withCTTS bool) []byte {
		parts := [][]byte{box("stsd", []byte("descr")), box("stts", []byte("times"))}
		if withCTTS {
			parts = append(parts, box("ctts", []byte("offsets")))
		}
		return box("stbl", parts...)
	}
	trak := func(withCTTS bool) []byte {
		return box("trak", box("tkhd", []byte("hdr")), box("mdia", box("minf", stbl(withCTTS))))
	}
	return bytes.Join([][]byte{
		box("ftyp", []byte("isom")),
		box("moov", box("mvhd", []byte("movie")), trak(true), trak(false)),
		box("mdat", []byte("video data with ctts inside it should stay")),
	}, nil)
}

func TestFindCTTS(t *testing.T) {
	data := testMP4()
	offs, err := FindCTTS(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if len(offs) != 1 || string(data[offs[0]:offs[0]+4]) != "ctts" {
		t.Fatalf("offsets = %v", offs)
	}
}

func TestReadPatchesOnlyTheBox(t *testing.T) {
	data := testMP4()
	f, err := NewFile(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if !f.Patched() {
		t.Fatal("Patched = false")
	}
	got, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(got[:bytes.Index(got, []byte("mdat"))], []byte("ctts")) {
		t.Error("ctts box still present")
	}
	if !bytes.Contains(got, []byte("video data with ctts inside it should stay")) {
		t.Error("media data was changed")
	}
	if want := bytes.Replace(data, []byte("cttsoffsets"), []byte("freeoffsets"), 1); !bytes.Equal(got, want) {
		t.Error("output differs from the input in more than the box type")
	}
}

func TestReadAcrossBoundaries(t *testing.T) {
	data := testMP4()
	want := bytes.Replace(data, []byte("cttsoffsets"), []byte("freeoffsets"), 1)
	f, _ := NewFile(bytes.NewReader(data), int64(len(data)))
	// read in 3-byte pieces so the patch straddles reads
	var got []byte
	buf := make([]byte, 3)
	for {
		n, err := f.Read(buf)
		got = append(got, buf[:n]...)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(got, want) {
		t.Error("small reads gave different bytes")
	}
	// seek into the middle of the box type
	pos := int64(bytes.Index(want, []byte("freeoffsets"))) + 2
	if _, err := f.Seek(pos, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	two := make([]byte, 2)
	if _, err := io.ReadFull(f, two); err != nil || string(two) != "ee" {
		t.Errorf("read after seek = %q, %v", two, err)
	}
}

func TestNotMP4(t *testing.T) {
	data := []byte("this is not an mp4 file at all")
	f, err := NewFile(bytes.NewReader(data), int64(len(data)))
	if err != nil || f.Patched() {
		t.Errorf("Patched = %v, err = %v", f.Patched(), err)
	}
}

func TestTruncatedMP4(t *testing.T) {
	data := testMP4()
	cut := data[:bytes.Index(data, []byte("ctts"))+2] // snapshot taken mid-write
	if _, err := FindCTTS(bytes.NewReader(cut), int64(len(cut))); err != nil {
		t.Errorf("truncated file: %v", err)
	}
}

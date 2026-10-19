package mp4fix

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"testing"
)

func fullBox(typ string, body []byte) []byte {
	return box(typ, append([]byte{0, 0, 0, 0}, body...)) // version and flags
}

func stco(offsets ...uint32) []byte {
	b := binary.BigEndian.AppendUint32(nil, uint32(len(offsets)))
	for _, o := range offsets {
		b = binary.BigEndian.AppendUint32(b, o)
	}
	return fullBox("stco", b)
}

func co64(offsets ...uint64) []byte {
	b := binary.BigEndian.AppendUint32(nil, uint32(len(offsets)))
	for _, o := range offsets {
		b = binary.BigEndian.AppendUint64(b, o)
	}
	return fullBox("co64", b)
}

// tailMP4 is laid out like the car's files: ftyp, free, mdat, then moov,
// whose video track (stco, with ctts) and second track (co64) point at
// chunks in mdat.
func tailMP4(t *testing.T) (data []byte, chunks []string) {
	t.Helper()
	chunks = []string{"AAAA", "BBBBBB", "CC", "DDDDDDDD"}
	head := bytes.Join([][]byte{box("ftyp", []byte("isom")), box("free")}, nil)
	mdatAt := uint32(len(head))
	media := []byte(strings(chunks))
	off := func(i int) uint32 {
		o := mdatAt + 8
		for _, c := range chunks[:i] {
			o += uint32(len(c))
		}
		return o
	}
	moov := box("moov", box("mvhd", []byte("movie")),
		box("trak", box("mdia", box("minf", box("stbl",
			box("stts", []byte("times")), box("ctts", []byte("offsets")), stco(off(0), off(2)))))),
		box("trak", box("mdia", box("minf", box("stbl", co64(uint64(off(1)), uint64(off(3))))))))
	return bytes.Join([][]byte{head, box("mdat", media), moov}, nil), chunks
}

func strings(parts []string) string {
	var b bytes.Buffer
	for _, p := range parts {
		b.WriteString(p)
	}
	return b.String()
}

// chunkOffsets reads every stco and co64 entry of a laid-out file.
func chunkOffsets(t *testing.T, data []byte) []int64 {
	t.Helper()
	found := map[string][]int64{}
	if err := walk(bytes.NewReader(data), 0, int64(len(data)), 0, found); err != nil {
		t.Fatal(err)
	}
	var out []int64
	for _, typ := range []string{"stco", "co64"} {
		width := 4
		if typ == "co64" {
			width = 8
		}
		for _, at := range found[typ] {
			n := int(binary.BigEndian.Uint32(data[at+8:]))
			for i := 0; i < n; i++ {
				e := data[int(at)+12+i*width:]
				if width == 4 {
					out = append(out, int64(binary.BigEndian.Uint32(e)))
				} else {
					out = append(out, int64(binary.BigEndian.Uint64(e)))
				}
			}
		}
	}
	return out
}

func TestFastStart(t *testing.T) {
	data, chunks := tailMP4(t)
	f, err := NewFile(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if !f.FastStart() || !f.Patched() {
		t.Fatalf("FastStart = %v, Patched = %v", f.FastStart(), f.Patched())
	}
	got, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(data) {
		t.Fatalf("size %d, want %d", len(got), len(data))
	}
	var order []string
	for off := 0; off < len(got); {
		size := int(binary.BigEndian.Uint32(got[off:]))
		order = append(order, string(got[off+4:off+8]))
		off += size
	}
	if strings(order) != "ftypfreemoovmdat" {
		t.Errorf("top-level boxes = %v", order)
	}
	if bytes.Contains(got, []byte("cttsoffsets")) || !bytes.Contains(got, []byte("freeoffsets")) {
		t.Error("ctts wasn't presented as free")
	}
	// each chunk offset still finds its chunk: stco has chunks 0 and 2,
	// co64 chunks 1 and 3
	offs := chunkOffsets(t, got)
	for i, want := range []string{chunks[0], chunks[2], chunks[1], chunks[3]} {
		if o := offs[i]; string(got[o:o+int64(len(want))]) != want {
			t.Errorf("offset %d (%d) reads %q, want %q", i, o, got[o:o+int64(len(want))], want)
		}
	}
	// the original is unchanged
	if again, _ := tailMP4(t); !bytes.Equal(again, data) {
		t.Error("the original bytes were changed")
	}
}

func TestFastStartSmallReadsAndSeeks(t *testing.T) {
	data, _ := tailMP4(t)
	f, _ := NewFile(bytes.NewReader(data), int64(len(data)))
	want, _ := io.ReadAll(f)
	var got []byte
	buf := make([]byte, 5)
	f.Seek(0, io.SeekStart)
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
	for _, pos := range []int64{0, 7, int64(len(want)) / 2, int64(len(want)) - 3} {
		f.Seek(pos, io.SeekStart)
		b, _ := io.ReadAll(f)
		if !bytes.Equal(b, want[pos:]) {
			t.Errorf("read from %d differs", pos)
		}
	}
}

func TestFastStartLeavesOddFilesAlone(t *testing.T) {
	// index already first
	if f, _ := NewFile(bytes.NewReader(testMP4()), int64(len(testMP4()))); f.FastStart() {
		t.Error("moved an index that was already first")
	}
	// a chunk offset outside mdat
	head := box("ftyp", []byte("isom"))
	bad := bytes.Join([][]byte{head, box("mdat", []byte("data")),
		box("moov", box("trak", box("mdia", box("minf", box("stbl", stco(3)))))),
	}, nil)
	if f, _ := NewFile(bytes.NewReader(bad), int64(len(bad))); f.FastStart() {
		t.Error("moved an index whose offsets don't point into mdat")
	}
	// cut off in the middle of mdat (snapshot taken mid-recording)
	data, _ := tailMP4(t)
	cut := data[:40]
	f, err := NewFile(bytes.NewReader(cut), int64(len(cut)))
	if err != nil || f.FastStart() {
		t.Errorf("truncated: FastStart = %v, err = %v", f.FastStart(), err)
	}
	if got, _ := io.ReadAll(f); !bytes.Equal(got, cut) {
		t.Error("truncated file not served as it is")
	}
}

// TestFastStartRealClip checks a recording from the car, if
// MP4FIX_FIXTURE names one: every chunk offset of the presented file must
// read the same bytes as the original offset does in the original.
func TestFastStartRealClip(t *testing.T) {
	path := os.Getenv("MP4FIX_FIXTURE")
	if path == "" {
		t.Skip("set MP4FIX_FIXTURE to a Tesla recording")
	}
	orig, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	f, err := NewFile(bytes.NewReader(orig), int64(len(orig)))
	if err != nil {
		t.Fatal(err)
	}
	if !f.FastStart() {
		t.Fatal("not laid out fast-start")
	}
	got, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	before, after := chunkOffsets(t, orig), chunkOffsets(t, got)
	if len(before) == 0 || len(before) != len(after) {
		t.Fatalf("%d chunk offsets before, %d after", len(before), len(after))
	}
	for i := range before {
		if !bytes.Equal(orig[before[i]:before[i]+64], got[after[i]:after[i]+64]) {
			t.Fatalf("chunk %d: offset %d no longer finds its data", i, after[i])
		}
	}
	t.Logf("%d chunks moved by %d bytes; index now at the front", len(before), after[0]-before[0])
}

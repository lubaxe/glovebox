package telemetry

import (
	"bytes"
	"context"
	"encoding/binary"
	"math"
	"os"
	"testing"
)

func atom(kind string, data ...[]byte) []byte {
	body := bytes.Join(data, nil)
	out := make([]byte, 8)
	binary.BigEndian.PutUint32(out, uint32(len(body)+8))
	copy(out[4:], kind)
	return append(out, body...)
}
func u32(values ...uint32) []byte {
	var out []byte
	for _, v := range values {
		out = binary.BigEndian.AppendUint32(out, v)
	}
	return out
}
func proto(speed float32) []byte {
	out := []byte{8, 1, 16, 1, 24, 42, 37}
	out = binary.LittleEndian.AppendUint32(out, math.Float32bits(speed))
	out = append(out, 45)
	out = binary.LittleEndian.AppendUint32(out, math.Float32bits(.25))
	out = append(out, 53)
	out = binary.LittleEndian.AppendUint32(out, math.Float32bits(-15))
	return append(out, 56, 1, 72, 1, 80, 2)
}
func sei(data []byte) []byte {
	payload := append(append(bytes.Repeat([]byte{0x42}, 15), 0x69), data...)
	raw := append([]byte{5, byte(len(payload))}, payload...)
	raw = append(raw, 0x80)
	out := []byte{6}
	zeros := 0
	for _, b := range raw {
		if zeros >= 2 && b <= 3 {
			out = append(out, 3)
			zeros = 0
		}
		out = append(out, b)
		if b == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return out
}
func movie(wide bool) []byte {
	var samples [][]byte
	for _, speed := range []float32{0, 10, 20} {
		nal := sei(proto(speed))
		samples = append(samples, append(u32(uint32(len(nal))), nal...))
	}
	ftyp := atom("ftyp", []byte("isom"))
	mdat := atom("mdat", samples...)
	offset := len(ftyp) + 8
	chunks := atom("stco", u32(0, 1, uint32(offset)))
	if wide {
		chunks = atom("co64", append(u32(0, 1), binary.BigEndian.AppendUint64(nil, uint64(offset))...))
	}
	sizes := u32(0, 0, 3)
	for _, sample := range samples {
		sizes = append(sizes, u32(uint32(len(sample)))...)
	}
	desc := atom("stsd", u32(0, 1), atom("avc1", make([]byte, 78), atom("avcC", []byte{1, 100, 0, 10, 255})))
	stbl := atom("stbl", desc, atom("stts", u32(0, 2, 2, 500, 1, 0)), atom("stsc", u32(0, 1, 1, 3, 1)), atom("stsz", sizes), chunks)
	mdia := atom("mdia", atom("hdlr", append(u32(0, 0), []byte("vide")...)), atom("mdhd", u32(0, 0, 0, 1000)), atom("minf", stbl))
	return bytes.Join([][]byte{ftyp, mdat, atom("moov", atom("trak", mdia))}, nil)
}
func TestExtractTimesAndSEI(t *testing.T) {
	for _, wide := range []bool{false, true} {
		data := movie(wide)
		r, err := Extract(context.Background(), bytes.NewReader(data), int64(len(data)))
		if err != nil {
			t.Fatal(err)
		}
		if len(r.Samples) != 3 || r.Samples[1].Time != .5 || r.Samples[2].Time != 1 || r.Duration != 1 {
			t.Fatalf("bad sample timing: %+v", r)
		}
		s := r.Samples[1]
		if s.Speed != 10 || s.Gear != 1 || s.Steering != -15 || s.Accelerator != .25 || !s.Left || !s.Brake || s.Autopilot != 2 {
			t.Fatalf("bad telemetry: %+v", s)
		}
	}
}
func TestDefaultsAndBadPayload(t *testing.T) {
	s, ok := decodeSEI(sei([]byte{8, 1}))
	if !ok || s.Gear != 0 || s.Speed != 0 || s.Brake {
		t.Fatal("proto3 zero defaults must describe a valid P / zero-speed sample")
	}
	for _, data := range [][]byte{{}, {6, 5, 255}, {8, 255}, {8, 1, 37, 0, 0, 192, 127}} {
		if _, err := decodeProto(data); len(data) > 0 && err == nil {
			t.Fatalf("invalid protobuf accepted: %x", data)
		}
	}
}
func TestMalformedAndCancellation(t *testing.T) {
	for _, data := range [][]byte{{}, {0, 0, 0, 1, 109, 111, 111, 118}, {255, 255, 255, 255, 109, 111, 111, 118}} {
		if _, err := Extract(context.Background(), bytes.NewReader(data), int64(len(data))); err == nil {
			t.Fatal("invalid MP4 accepted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	data := movie(false)
	if _, err := Extract(ctx, bytes.NewReader(data), int64(len(data))); err == nil {
		t.Fatal("cancelled extraction succeeded")
	}
}
func TestRealClipIfProvided(t *testing.T) {
	p := os.Getenv("GLOVEBOX_TELEMETRY_FIXTURE")
	if p == "" {
		t.Skip("Set GLOVEBOX_TELEMETRY_FIXTURE for a local recording; no private fixture is checked in")
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	info, _ := f.Stat()
	r, err := Extract(context.Background(), f, info.Size())
	if err != nil || len(r.Samples) == 0 {
		t.Fatalf("no telemetry: %v", err)
	}
	for i, s := range r.Samples {
		if i > 0 && s.Time < r.Samples[i-1].Time || s.Time > r.Duration+.01 {
			t.Fatal("telemetry timestamps outside media timeline")
		}
	}
	t.Logf("validated %d telemetry samples over %.4f seconds", len(r.Samples), r.Duration)
}
func FuzzExtract(f *testing.F) {
	f.Add(movie(false))
	f.Add(editedMovie(u32(0, 1, 750, 250, 1<<16)))
	f.Add([]byte("invalid"))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<20 {
			t.Skip()
		}
		_, _ = Extract(context.Background(), bytes.NewReader(data), int64(len(data)))
	})
}

func editedMovie(edit []byte) []byte {
	data := movie(false)
	top, _ := boxes(data)
	var output []byte
	for _, b := range top {
		if b.kind != "moov" {
			output = append(output, atom(b.kind, b.data)...)
			continue
		}
		track := child(b.data, "trak")
		output = append(output, atom("moov", atom("mvhd", u32(0, 0, 0, 1000)), atom("trak", track, atom("edts", atom("elst", edit))))...)
	}
	return output
}
func TestEditListTiming(t *testing.T) {
	trimmed := editedMovie(u32(0, 1, 750, 250, 1<<16))
	r, err := Extract(context.Background(), bytes.NewReader(trimmed), int64(len(trimmed)))
	if err != nil || r.Duration != .75 || len(r.Samples) != 3 || r.Samples[0].Time != 0 || r.Samples[1].Time != .25 || r.Samples[2].Time != .75 {
		t.Fatalf("trim timing: %+v %v", r, err)
	}
	delayed := editedMovie(u32(0, 2, 500, math.MaxUint32, 1<<16, 1000, 0, 1<<16))
	r, err = Extract(context.Background(), bytes.NewReader(delayed), int64(len(delayed)))
	if err != nil || r.Duration != 1.5 || len(r.Samples) != 3 || r.Samples[0].Time != .5 {
		t.Fatalf("empty interval: %+v %v", r, err)
	}
	nonlinear := editedMovie(u32(0, 1, 1000, 0, 2<<16))
	if _, err := Extract(context.Background(), bytes.NewReader(nonlinear), int64(len(nonlinear))); err == nil {
		t.Fatal("nonlinear playback rate accepted")
	}
}

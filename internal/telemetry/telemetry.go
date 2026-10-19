// Package telemetry reads Tesla's published dashcam SEI schema from indexed
// H.264 MP4 files. It reads sample tables and SEI NAL units, not video pixels.
// Source schema: https://github.com/teslamotors/dashcam/blob/master/dashcam.proto
package telemetry

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
)

const maxSamples = 36000

type Sample struct {
	Time        float64 `json:"time"`
	Version     uint64  `json:"version"`
	Gear        uint64  `json:"gear"`
	Frame       uint64  `json:"frame"`
	Speed       float64 `json:"speed_mps"`
	Accelerator float64 `json:"accelerator"`
	Steering    float64 `json:"steering_deg"`
	Left        bool    `json:"left"`
	Right       bool    `json:"right"`
	Brake       bool    `json:"brake"`
	Autopilot   uint64  `json:"autopilot"`
	Latitude    float64 `json:"latitude"`
	Longitude   float64 `json:"longitude"`
	Heading     float64 `json:"heading"`
	AccelX      float64 `json:"accel_x"`
	AccelY      float64 `json:"accel_y"`
	AccelZ      float64 `json:"accel_z"`
}
type Result struct {
	Samples  []Sample `json:"samples"`
	Duration float64  `json:"duration"`
}
type box struct {
	kind string
	data []byte
}

var errFormat = errors.New("unsupported or malformed MP4 telemetry")

func boxes(data []byte) ([]box, error) {
	out := []box{}
	for len(data) > 0 {
		if len(data) < 8 {
			return nil, errFormat
		}
		size := uint64(binary.BigEndian.Uint32(data))
		header := uint64(8)
		if size == 1 {
			if len(data) < 16 {
				return nil, errFormat
			}
			size = binary.BigEndian.Uint64(data[8:])
			header = 16
		}
		if size == 0 {
			size = uint64(len(data))
		}
		if size < header || size > uint64(len(data)) {
			return nil, errFormat
		}
		out = append(out, box{string(data[4:8]), data[header:size]})
		data = data[size:]
		if len(out) > maxSamples {
			return nil, errFormat
		}
	}
	return out, nil
}
func child(data []byte, name string) []byte {
	list, err := boxes(data)
	if err != nil {
		return nil
	}
	for _, b := range list {
		if b.kind == name {
			return b.data
		}
	}
	return nil
}
func entries(data []byte, width int) ([]byte, int, error) {
	if len(data) < 8 {
		return nil, 0, errFormat
	}
	n := int(binary.BigEndian.Uint32(data[4:8]))
	if n > maxSamples || n > (len(data)-8)/width {
		return nil, 0, errFormat
	}
	return data[8:], n, nil
}

// Extract uses decode timestamps, matching Glovebox's playback view, which
// removes Tesla's problematic composition offsets through package mp4fix.
// Original recordings are never modified. Fragmented MP4 is not guessed at.
func Extract(ctx context.Context, r io.ReaderAt, size int64) (Result, error) {
	result := Result{Samples: []Sample{}}
	var moov []byte
	for off := int64(0); off < size; {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		var hdr [16]byte
		if size-off < 8 {
			return result, errFormat
		}
		if _, err := r.ReadAt(hdr[:8], off); err != nil {
			return result, err
		}
		length := uint64(binary.BigEndian.Uint32(hdr[:]))
		header := int64(8)
		if length == 1 {
			if _, err := r.ReadAt(hdr[8:], off+8); err != nil {
				return result, err
			}
			length = binary.BigEndian.Uint64(hdr[8:])
			header = 16
		}
		if length == 0 {
			length = uint64(size - off)
		}
		if length < uint64(header) || length > uint64(size-off) {
			return result, errFormat
		}
		if string(hdr[4:8]) == "moov" {
			if length > 32<<20 {
				return result, errFormat
			}
			moov = make([]byte, int64(length)-header)
			if _, err := r.ReadAt(moov, off+header); err != nil {
				return result, err
			}
			break
		}
		off += int64(length)
	}
	tracks, err := boxes(moov)
	if err != nil || len(moov) == 0 {
		return result, errFormat
	}
	for _, track := range tracks {
		if track.kind != "trak" {
			continue
		}
		mdia := child(track.data, "mdia")
		handler := child(mdia, "hdlr")
		if len(handler) < 12 || string(handler[8:12]) != "vide" {
			continue
		}
		table := child(child(mdia, "minf"), "stbl")
		desc := child(table, "stsd")
		if len(desc) < 8 {
			continue
		}
		descriptions, err := boxes(desc[8:])
		if err != nil {
			return result, err
		}
		var nalWidth int
		for _, d := range descriptions {
			if (d.kind == "avc1" || d.kind == "avc3") && len(d.data) >= 78 {
				avcc := child(d.data[78:], "avcC")
				if len(avcc) >= 5 {
					nalWidth = int(avcc[4]&3) + 1
				}
				break
			}
		}
		if nalWidth == 0 {
			continue
		}
		mdhd := child(mdia, "mdhd")
		scaleAt := 12
		if len(mdhd) > 0 && mdhd[0] == 1 {
			scaleAt = 20
		}
		if len(mdhd) < scaleAt+4 {
			return result, errFormat
		}
		scale := binary.BigEndian.Uint32(mdhd[scaleAt:])
		if scale == 0 {
			return result, errFormat
		}
		sz := child(table, "stsz")
		if len(sz) < 12 {
			return result, errFormat
		}
		constant := binary.BigEndian.Uint32(sz[4:8])
		count := int(binary.BigEndian.Uint32(sz[8:12]))
		if count > maxSamples || constant == 0 && count > (len(sz)-12)/4 {
			return result, errFormat
		}
		sizes := make([]uint32, count)
		for i := range sizes {
			sizes[i] = constant
			if constant == 0 {
				sizes[i] = binary.BigEndian.Uint32(sz[12+i*4:])
			}
		}
		times, n, err := entries(child(table, "stts"), 8)
		if err != nil {
			return result, err
		}
		starts := make([]float64, 0, count)
		ticks := uint64(0)
		for i := 0; i < n; i++ {
			runs := int(binary.BigEndian.Uint32(times[i*8:]))
			delta := binary.BigEndian.Uint32(times[i*8+4:])
			if runs > count-len(starts) {
				return result, errFormat
			}
			for j := 0; j < runs; j++ {
				starts = append(starts, float64(ticks)/float64(scale))
				ticks += uint64(delta)
			}
		}
		if len(starts) != count {
			return result, errFormat
		}
		result.Duration = float64(ticks) / float64(scale)
		timelineOffset, editedDuration, err := editTimeline(track.data, moov, scale)
		if err != nil {
			return result, err
		}
		if editedDuration >= 0 {
			result.Duration = editedDuration
		}
		chunks, chunkCount, err := entries(child(table, "stco"), 4)
		wide := false
		if err != nil {
			chunks, chunkCount, err = entries(child(table, "co64"), 8)
			wide = true
		}
		if err != nil {
			return result, err
		}
		mapping, mappingCount, err := entries(child(table, "stsc"), 12)
		if err != nil || mappingCount == 0 {
			return result, errFormat
		}
		if binary.BigEndian.Uint32(mapping) != 1 {
			return result, errFormat
		}
		sampleIndex, mapIndex := 0, 0
		for c := 0; c < chunkCount; c++ {
			if err := ctx.Err(); err != nil {
				return result, err
			}
			for mapIndex+1 < mappingCount && int(binary.BigEndian.Uint32(mapping[(mapIndex+1)*12:])) <= c+1 {
				mapIndex++
			}
			perChunk := int(binary.BigEndian.Uint32(mapping[mapIndex*12+4:]))
			if perChunk < 1 || perChunk > count-sampleIndex {
				return result, errFormat
			}
			var offset uint64
			if wide {
				offset = binary.BigEndian.Uint64(chunks[c*8:])
			} else {
				offset = uint64(binary.BigEndian.Uint32(chunks[c*4:]))
			}
			for j := 0; j < perChunk; j++ {
				if sampleIndex%64 == 0 {
					if err := ctx.Err(); err != nil {
						return result, err
					}
				}
				length := uint64(sizes[sampleIndex])
				if offset > uint64(size) || length > uint64(size)-offset {
					return result, errFormat
				}
				metadata, ok, err := readSample(r, int64(offset), int64(length), nalWidth)
				if err != nil {
					return result, err
				}
				if ok {
					metadata.Time = starts[sampleIndex] + timelineOffset
					if metadata.Time < 0 {
						// Keep the last preroll state for the first displayed frame after a trim.
						metadata.Time = 0
						if len(result.Samples) > 0 {
							result.Samples[len(result.Samples)-1] = metadata
						} else {
							result.Samples = append(result.Samples, metadata)
						}
					} else if metadata.Time <= result.Duration {
						result.Samples = append(result.Samples, metadata)
					}
				}
				offset += length
				sampleIndex++
			}
		}
		if sampleIndex != count {
			return result, errFormat
		}
		return result, nil
	}
	return result, nil
}

func readSample(r io.ReaderAt, off, size int64, width int) (Sample, bool, error) {
	var result Sample
	found := false
	end := off + size
	for n := 0; off < end; n++ {
		if n > 128 || end-off < int64(width+1) {
			return result, false, errFormat
		}
		var prefix [5]byte
		if _, err := r.ReadAt(prefix[:width+1], off); err != nil {
			return result, false, err
		}
		length := uint64(0)
		for _, b := range prefix[:width] {
			length = length<<8 | uint64(b)
		}
		off += int64(width)
		if length < 1 || length > uint64(end-off) {
			return result, false, errFormat
		}
		if prefix[width]&31 == 6 && length <= 64<<10 {
			nal := make([]byte, length)
			if _, err := r.ReadAt(nal, off); err != nil {
				return result, false, err
			}
			if sample, ok := decodeSEI(nal); ok {
				result, found = sample, true
			}
		}
		off += int64(length)
	}
	return result, found, nil
}
func decodeSEI(nal []byte) (Sample, bool) {
	if len(nal) < 4 || nal[0]&31 != 6 {
		return Sample{}, false
	}
	rbsp := make([]byte, 0, len(nal))
	zeros := 0
	for _, b := range nal[1:] {
		if zeros >= 2 && b == 3 {
			zeros = 0
			continue
		}
		rbsp = append(rbsp, b)
		if b == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	for pos := 0; pos < len(rbsp)-1; {
		typ := 0
		for pos < len(rbsp) && rbsp[pos] == 255 {
			typ += 255
			pos++
		}
		if pos >= len(rbsp) {
			break
		}
		typ += int(rbsp[pos])
		pos++
		length := 0
		for pos < len(rbsp) && rbsp[pos] == 255 {
			length += 255
			pos++
		}
		if pos >= len(rbsp) {
			break
		}
		length += int(rbsp[pos])
		pos++
		if length > len(rbsp)-pos {
			return Sample{}, false
		}
		payload := rbsp[pos : pos+length]
		pos += length
		if typ != 5 {
			continue
		}
		i := 0
		for i < len(payload) && payload[i] == 0x42 {
			i++
		}
		if i > 0 && i < len(payload) && payload[i] == 0x69 {
			sample, err := decodeProto(payload[i+1:])
			if err == nil && sample.Version > 0 {
				return sample, true
			}
		}
	}
	return Sample{}, false
}
func readVarint(data []byte) (uint64, int, error) {
	var value uint64
	for i, b := range data {
		if i >= 10 || i == 9 && b > 1 {
			return 0, 0, errFormat
		}
		value |= uint64(b&127) << uint(7*i)
		if b < 128 {
			return value, i + 1, nil
		}
	}
	return 0, 0, errFormat
}
func decodeProto(data []byte) (Sample, error) {
	var sample Sample
	for len(data) > 0 {
		tag, n, err := readVarint(data)
		if err != nil || tag < 8 {
			return sample, errFormat
		}
		data = data[n:]
		field, wire := int(tag>>3), tag&7
		var number uint64
		var real float64
		switch wire {
		case 0:
			number, n, err = readVarint(data)
			if err != nil {
				return sample, err
			}
			data = data[n:]
		case 1:
			if len(data) < 8 {
				return sample, errFormat
			}
			real = math.Float64frombits(binary.LittleEndian.Uint64(data))
			data = data[8:]
		case 5:
			if len(data) < 4 {
				return sample, errFormat
			}
			real = float64(math.Float32frombits(binary.LittleEndian.Uint32(data)))
			data = data[4:]
		case 2:
			var length uint64
			length, n, err = readVarint(data)
			if err != nil || length > uint64(len(data)-n) {
				return sample, errFormat
			}
			data = data[n+int(length):]
		default:
			return sample, errFormat
		}
		if math.IsNaN(real) || math.IsInf(real, 0) {
			return sample, errFormat
		}
		expected := uint64(0)
		if field >= 4 && field <= 6 {
			expected = 5
		}
		if field >= 11 && field <= 16 {
			expected = 1
		}
		if field <= 16 && wire != expected {
			return sample, errFormat
		}
		switch field {
		case 1:
			sample.Version = number
		case 2:
			sample.Gear = number
		case 3:
			sample.Frame = number
		case 4:
			sample.Speed = real
		case 5:
			sample.Accelerator = real
		case 6:
			sample.Steering = real
		case 7:
			sample.Left = number != 0
		case 8:
			sample.Right = number != 0
		case 9:
			sample.Brake = number != 0
		case 10:
			sample.Autopilot = number
		case 11:
			sample.Latitude = real
		case 12:
			sample.Longitude = real
		case 13:
			sample.Heading = real
		case 14:
			sample.AccelX = real
		case 15:
			sample.AccelY = real
		case 16:
			sample.AccelZ = real
		}
	}
	return sample, nil
}

// Common MP4 edits trim the media start or insert an initial empty interval.
// Nonlinear edits / playback rates are rejected rather than reporting wrong time.
func editTimeline(track, movie []byte, scale uint32) (float64, float64, error) {
	edit := child(child(track, "edts"), "elst")
	if len(edit) == 0 {
		return 0, -1, nil
	}
	movieHeader := child(movie, "mvhd")
	at := 12
	if len(movieHeader) > 0 && movieHeader[0] == 1 {
		at = 20
	}
	if len(movieHeader) < at+4 || binary.BigEndian.Uint32(movieHeader[at:]) == 0 || len(edit) < 8 {
		return 0, 0, errFormat
	}
	movieScale := float64(binary.BigEndian.Uint32(movieHeader[at:]))
	width := 12
	if edit[0] == 1 {
		width = 20
	} else if edit[0] != 0 {
		return 0, 0, errFormat
	}
	data, n, err := entries(edit, width)
	if err != nil || n < 1 || n > 2 {
		return 0, 0, errFormat
	}
	offset, total := 0.0, 0.0
	found := false
	for i := 0; i < n; i++ {
		entry := data[i*width:]
		duration := uint64(binary.BigEndian.Uint32(entry))
		media := int64(int32(binary.BigEndian.Uint32(entry[4:])))
		rate := binary.BigEndian.Uint32(entry[8:])
		if width == 20 {
			duration = binary.BigEndian.Uint64(entry)
			media = int64(binary.BigEndian.Uint64(entry[8:]))
			rate = binary.BigEndian.Uint32(entry[16:])
		}
		if rate != 1<<16 {
			return 0, 0, errFormat
		}
		seconds := float64(duration) / movieScale
		if media == -1 && !found {
			offset += seconds
		} else if media >= 0 && !found {
			offset -= float64(media) / float64(scale)
			found = true
		} else {
			return 0, 0, errFormat
		}
		total += seconds
	}
	if !found {
		return 0, 0, errFormat
	}
	return offset, total, nil
}

package archive

import (
	"bytes"
	"errors"
	"sync"
	"testing"
)

// memFile is an io.WriterAt that records what was written where.
type memFile struct {
	mu   sync.Mutex
	data []byte
	fail error
}

func (m *memFile) WriteAt(p []byte, off int64) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return 0, m.fail
	}
	if end := int(off) + len(p); end > len(m.data) {
		m.data = append(m.data, make([]byte, end-len(m.data))...)
	}
	copy(m.data[off:], p)
	return len(p), nil
}

func TestParallelCopy(t *testing.T) {
	src := make([]byte, 1000)
	for i := range src {
		src[i] = byte(i * 7)
	}
	for _, tc := range []struct{ workers, chunk int }{{1, 64}, {4, 64}, {4, 1000}, {8, 300}, {3, 4096}} {
		dst := &memFile{}
		if err := parallelCopy(dst, bytes.NewReader(src), int64(len(src)), tc.workers, tc.chunk); err != nil {
			t.Fatalf("%+v: %v", tc, err)
		}
		if !bytes.Equal(dst.data, src) {
			t.Errorf("%+v: copy differs", tc)
		}
	}
}

func TestParallelCopyStopsOnError(t *testing.T) {
	dst := &memFile{fail: errors.New("connection reset by peer")}
	err := parallelCopy(dst, bytes.NewReader(make([]byte, 10000)), 10000, 4, 100)
	if err == nil || err.Error() != "connection reset by peer" {
		t.Errorf("err = %v", err)
	}
}

func TestParallelCopyShortSource(t *testing.T) {
	// the file shrank since it was listed: an error, not a silent short copy
	err := parallelCopy(&memFile{}, bytes.NewReader(make([]byte, 100)), 1000, 2, 300)
	if err == nil {
		t.Error("want an error")
	}
}

func TestParseDialect(t *testing.T) {
	for in, want := range map[string]uint16{"": 0, "2.1": 0x0210, "3.1.1": 0x0311} {
		if got, err := ParseDialect(in); err != nil || got != want {
			t.Errorf("%q = %#x, %v", in, got, err)
		}
	}
	if _, err := ParseDialect("4.0"); err == nil {
		t.Error("want an error")
	}
}

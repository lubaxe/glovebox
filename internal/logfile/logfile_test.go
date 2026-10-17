package logfile

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFile(t *testing.T) {
	l := &File{Max: 100}
	// before Open, writes go nowhere and don't fail
	if n, err := l.Write([]byte("early\n")); n != 6 || err != nil {
		t.Fatalf("n=%d err=%v", n, err)
	}
	path := filepath.Join(t.TempDir(), "glovebox.log")
	if err := l.Open(path); err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	line := strings.Repeat("x", 39) + "\n" // 40 bytes
	for i := 0; i < 3; i++ {
		l.Write([]byte(line))
	}
	// the third line would pass 100 bytes, so the first two moved to .1
	cur, _ := os.ReadFile(path)
	old, _ := os.ReadFile(path + ".1")
	if len(cur) != 40 || len(old) != 80 {
		t.Errorf("current %d bytes, old %d bytes", len(cur), len(old))
	}
}

func TestFanout(t *testing.T) {
	var a, b bytes.Buffer
	log := slog.New(Fanout{
		slog.NewTextHandler(&a, &slog.HandlerOptions{Level: slog.LevelInfo}),
		slog.NewTextHandler(&b, &slog.HandlerOptions{Level: slog.LevelDebug}),
	}).With("svc", "glovebox")
	log.Debug("detail")
	log.Info("hello")
	if strings.Contains(a.String(), "detail") || !strings.Contains(a.String(), "msg=hello svc=glovebox") {
		t.Errorf("a = %q", a.String())
	}
	if !strings.Contains(b.String(), "detail") || !strings.Contains(b.String(), "hello") {
		t.Errorf("b = %q", b.String())
	}
}

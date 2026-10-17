package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ByteSize is a size in bytes, written in the config as e.g. "40G" or
// "512M" (powers of 1024).
type ByteSize int64

// UnmarshalText parses sizes like "40G", "512M", "1T" or "1048576".
func (b *ByteSize) UnmarshalText(text []byte) error {
	s := strings.TrimSpace(strings.ToUpper(string(text)))
	s = strings.TrimSuffix(strings.TrimSuffix(s, "B"), "I") // allow GB, GiB
	mult := int64(1)
	if n := len(s); n > 0 {
		switch s[n-1] {
		case 'K':
			mult = 1 << 10
		case 'M':
			mult = 1 << 20
		case 'G':
			mult = 1 << 30
		case 'T':
			mult = 1 << 40
		}
		if mult != 1 {
			s = s[:n-1]
		}
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 {
		return fmt.Errorf("invalid size %q: use e.g. 40G or 512M", text)
	}
	*b = ByteSize(v * float64(mult))
	return nil
}

// MarshalText writes the size in the largest whole unit.
func (b ByteSize) MarshalText() ([]byte, error) {
	for _, u := range []struct {
		suffix string
		size   int64
	}{{"T", 1 << 40}, {"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10}} {
		if b != 0 && int64(b)%u.size == 0 {
			return []byte(fmt.Sprintf("%d%s", int64(b)/u.size, u.suffix)), nil
		}
	}
	return []byte(strconv.FormatInt(int64(b), 10)), nil
}

// Duration is a time.Duration written in the config as e.g. "58m" or "1h30m".
type Duration time.Duration

// UnmarshalText parses durations in Go syntax.
func (d *Duration) UnmarshalText(text []byte) error {
	v, err := time.ParseDuration(strings.TrimSpace(string(text)))
	if err != nil {
		return fmt.Errorf("invalid duration %q: use e.g. 58m or 1h", text)
	}
	*d = Duration(v)
	return nil
}

// MarshalText writes the duration in Go syntax.
func (d Duration) MarshalText() ([]byte, error) {
	return []byte(time.Duration(d).String()), nil
}

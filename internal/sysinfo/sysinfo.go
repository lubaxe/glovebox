// Package sysinfo reports basic health of the Pi: uptime, memory, disk,
// temperature and Wi-Fi signal.
package sysinfo

import (
	"bufio"
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"time"
)

// Info is a snapshot of the system's state.
type Info struct {
	Hostname   string        `json:"hostname"`
	Model      string        `json:"model,omitempty"`
	Uptime     time.Duration `json:"-"`
	UptimeSec  float64       `json:"uptime_seconds"`
	Load1      float64       `json:"load1"`
	MemTotal   uint64        `json:"mem_total_bytes"`
	MemAvail   uint64        `json:"mem_available_bytes"`
	CPUTempC   *float64      `json:"cpu_temp_c,omitempty"`
	WiFiSignal *int          `json:"wifi_signal_dbm,omitempty"`
	RootDisk   *Disk         `json:"root_disk,omitempty"`
}

// Disk is the usage of one filesystem.
type Disk struct {
	Path  string `json:"path"`
	Total uint64 `json:"total_bytes"`
	Free  uint64 `json:"free_bytes"`
}

// Reader collects Info. Root is the filesystem /proc and /sys are read from,
// so tests can substitute a fake one.
type Reader struct {
	Root      fs.FS
	Hostname  func() (string, error)
	DiskUsage func(path string) (*Disk, error)
}

// NewReader returns a Reader for the running system.
func NewReader() *Reader {
	return &Reader{Root: os.DirFS("/"), Hostname: os.Hostname, DiskUsage: diskUsage}
}

// Read collects the current Info. Missing optional values (temperature,
// Wi-Fi, model) are left empty rather than failing.
func (r *Reader) Read() (Info, error) {
	var info Info
	var err error

	if info.Hostname, err = r.Hostname(); err != nil {
		return info, fmt.Errorf("hostname: %w", err)
	}
	if info.Uptime, err = r.uptime(); err != nil {
		return info, err
	}
	info.UptimeSec = info.Uptime.Seconds()
	if info.Load1, err = r.load1(); err != nil {
		return info, err
	}
	if info.MemTotal, info.MemAvail, err = r.memory(); err != nil {
		return info, err
	}
	if model, err := fs.ReadFile(r.Root, "proc/device-tree/model"); err == nil {
		info.Model = string(bytes.TrimRight(model, "\x00\n"))
	}
	info.CPUTempC = r.cpuTemp()
	info.WiFiSignal = r.wifiSignal("wlan0")
	if r.DiskUsage != nil {
		info.RootDisk, _ = r.DiskUsage("/")
	}
	return info, nil
}

func (r *Reader) uptime() (time.Duration, error) {
	data, err := fs.ReadFile(r.Root, "proc/uptime")
	if err != nil {
		return 0, fmt.Errorf("uptime: %w", err)
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0, fmt.Errorf("uptime: empty /proc/uptime")
	}
	secs, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, fmt.Errorf("uptime: %w", err)
	}
	return time.Duration(secs * float64(time.Second)), nil
}

func (r *Reader) load1() (float64, error) {
	data, err := fs.ReadFile(r.Root, "proc/loadavg")
	if err != nil {
		return 0, fmt.Errorf("loadavg: %w", err)
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0, fmt.Errorf("loadavg: empty /proc/loadavg")
	}
	return strconv.ParseFloat(fields[0], 64)
}

// memory returns MemTotal and MemAvailable in bytes.
func (r *Reader) memory() (total, avail uint64, err error) {
	f, err := r.Root.Open("proc/meminfo")
	if err != nil {
		return 0, 0, fmt.Errorf("meminfo: %w", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		// e.g. "MemAvailable:     279432 kB"
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		kb, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			total = kb * 1024
		case "MemAvailable:":
			avail = kb * 1024
		}
	}
	if err := sc.Err(); err != nil {
		return 0, 0, fmt.Errorf("meminfo: %w", err)
	}
	if total == 0 {
		return 0, 0, fmt.Errorf("meminfo: no MemTotal")
	}
	return total, avail, nil
}

func (r *Reader) cpuTemp() *float64 {
	data, err := fs.ReadFile(r.Root, "sys/class/thermal/thermal_zone0/temp")
	if err != nil {
		return nil
	}
	milli, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return nil
	}
	c := float64(milli) / 1000
	return &c
}

// wifiSignal reads the signal level of iface from /proc/net/wireless.
func (r *Reader) wifiSignal(iface string) *int {
	data, err := fs.ReadFile(r.Root, "proc/net/wireless")
	if err != nil {
		return nil
	}
	for _, line := range strings.Split(string(data), "\n") {
		// e.g. " wlan0: 0000   54.  -56.  -256        0      0      0      0     11        0"
		name, rest, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok || name != iface {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) < 3 {
			return nil
		}
		dbm, err := strconv.Atoi(strings.TrimSuffix(fields[2], "."))
		if err != nil {
			return nil
		}
		return &dbm
	}
	return nil
}

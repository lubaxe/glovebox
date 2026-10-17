package sysinfo

import (
	"testing"
	"testing/fstest"
	"time"
)

// Values taken from the dev Pi (Pi Zero 2 W, trixie).
var piFS = fstest.MapFS{
	"proc/uptime":                          {Data: []byte("4523.81 16012.33\n")},
	"proc/loadavg":                         {Data: []byte("0.37 0.32 0.13 1/142 1234\n")},
	"proc/meminfo":                         {Data: []byte("MemTotal:         425072 kB\nMemFree:          185000 kB\nMemAvailable:     279432 kB\nBuffers:           10000 kB\n")},
	"proc/device-tree/model":               {Data: []byte("Raspberry Pi Zero 2 W Rev 1.0\x00")},
	"sys/class/thermal/thermal_zone0/temp": {Data: []byte("41856\n")},
	"proc/net/wireless": {Data: []byte("Inter-| sta-|   Quality        |   Discarded packets               | Missed | WE\n" +
		" face | tus | link level noise |  nwid  crypt   frag  retry   misc | beacon | 22\n" +
		" wlan0: 0000   54.  -56.  -256        0      0      0      0     11        0\n")},
}

func reader(root fstest.MapFS) *Reader {
	return &Reader{
		Root:     root,
		Hostname: func() (string, error) { return "glovebox", nil },
		DiskUsage: func(path string) (*Disk, error) {
			return &Disk{Path: path, Total: 100, Free: 40}, nil
		},
	}
}

func TestRead(t *testing.T) {
	info, err := reader(piFS).Read()
	if err != nil {
		t.Fatal(err)
	}
	if info.Hostname != "glovebox" {
		t.Errorf("Hostname = %q", info.Hostname)
	}
	if info.Model != "Raspberry Pi Zero 2 W Rev 1.0" {
		t.Errorf("Model = %q", info.Model)
	}
	if want := 4523810 * time.Millisecond; info.Uptime != want {
		t.Errorf("Uptime = %v, want %v", info.Uptime, want)
	}
	if info.Load1 != 0.37 {
		t.Errorf("Load1 = %v", info.Load1)
	}
	if info.MemTotal != 425072*1024 || info.MemAvail != 279432*1024 {
		t.Errorf("MemTotal/MemAvail = %d/%d", info.MemTotal, info.MemAvail)
	}
	if info.CPUTempC == nil || *info.CPUTempC != 41.856 {
		t.Errorf("CPUTempC = %v", info.CPUTempC)
	}
	if info.WiFiSignal == nil || *info.WiFiSignal != -56 {
		t.Errorf("WiFiSignal = %v", info.WiFiSignal)
	}
	if info.RootDisk == nil || info.RootDisk.Free != 40 {
		t.Errorf("RootDisk = %+v", info.RootDisk)
	}
}

func TestReadWithoutOptionalFiles(t *testing.T) {
	minimal := fstest.MapFS{
		"proc/uptime":  piFS["proc/uptime"],
		"proc/loadavg": piFS["proc/loadavg"],
		"proc/meminfo": piFS["proc/meminfo"],
	}
	info, err := reader(minimal).Read()
	if err != nil {
		t.Fatal(err)
	}
	if info.Model != "" || info.CPUTempC != nil || info.WiFiSignal != nil {
		t.Errorf("optional values should be empty: %+v", info)
	}
}

func TestReadFailsWithoutProc(t *testing.T) {
	if _, err := reader(fstest.MapFS{}).Read(); err == nil {
		t.Error("err = nil, want an error")
	}
}

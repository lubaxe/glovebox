//go:build linux

// Command smbbench measures SMB upload speed from the Pi to the archive
// share, to tune archiving. It uploads data from memory (so the SD card
// isn't measured), reports throughput and CPU time, and can write a CPU
// profile.
//
//	sudo ./smbbench -server 192.168.1.10 -share TeslaCam -mb 64 -dialect 311 -cpuprofile cpu.out
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"runtime/pprof"
	"syscall"
	"time"

	"github.com/hirochachacha/go-smb2"
	"github.com/lubaxe/glovebox/internal/archive"
)

func main() {
	server := flag.String("server", "", "SMB server")
	share := flag.String("share", "TeslaCam", "share")
	creds := flag.String("creds", "/etc/glovebox/smb-credentials", "credentials file")
	mb := flag.Int("mb", 64, "megabytes to upload")
	dialect := flag.Uint("dialect", 0, "SMB dialect: 202, 210, 300, 302, 311 (0 = negotiate highest)")
	chunk := flag.Int("chunk", 0, "write chunk size in KiB (0 = io.Copy, which uses File.ReadFrom)")
	cpuprofile := flag.String("cpuprofile", "", "write a CPU profile to this file")
	parallel := flag.Int("parallel", 1, "concurrent WriteAt calls (chunk defaults to 1024K)")
	flag.Parse()

	user, pass, domain, err := archive.ReadCredentials(*creds)
	if err != nil {
		log.Fatal(err)
	}
	data := make([]byte, *mb<<20)
	if _, err := rand.Read(data); err != nil {
		log.Fatal(err)
	}

	// dialect revision numbers from MS-SMB2 (go-smb2 keeps its constants internal)
	d := map[uint]uint16{202: 0x0202, 210: 0x0210, 300: 0x0300, 302: 0x0302, 311: 0x0311}
	neg := smb2.Negotiator{}
	if *dialect != 0 {
		v, ok := d[*dialect]
		if !ok {
			log.Fatalf("unknown dialect %d", *dialect)
		}
		neg.SpecifiedDialect = v
	}

	conn, err := net.Dial("tcp", net.JoinHostPort(*server, "445"))
	if err != nil {
		log.Fatal(err)
	}
	dialer := &smb2.Dialer{Negotiator: neg, Initiator: &smb2.NTLMInitiator{User: user, Password: pass, Domain: domain}}
	sess, err := dialer.DialContext(context.Background(), conn)
	if err != nil {
		log.Fatal(err)
	}
	defer sess.Logoff()
	fs, err := sess.Mount(*share)
	if err != nil {
		log.Fatal(err)
	}
	defer fs.Umount()

	const dir = ".glovebox-bench"
	_ = fs.MkdirAll(dir, 0o755)
	name := dir + "/bench.bin"
	f, err := fs.Create(name)
	if err != nil {
		log.Fatal(err)
	}

	if *cpuprofile != "" {
		pf, err := os.Create(*cpuprofile)
		if err != nil {
			log.Fatal(err)
		}
		if err := pprof.StartCPUProfile(pf); err != nil {
			log.Fatal(err)
		}
		defer pprof.StopCPUProfile()
	}
	cpu0 := cpuTime()
	start := time.Now()
	if *parallel > 1 {
		err = writeParallel(f, data, *parallel, max(*chunk, 1024)<<10)
	} else if *chunk == 0 {
		_, err = io.Copy(f, bytes.NewReader(data))
	} else {
		for off := 0; off < len(data) && err == nil; off += *chunk << 10 {
			end := min(off+*chunk<<10, len(data))
			_, err = f.Write(data[off:end])
		}
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	elapsed := time.Since(start)
	cpu := cpuTime() - cpu0
	if err != nil {
		log.Fatal(err)
	}
	_ = fs.Remove(name)
	_ = fs.Remove(dir)

	fmt.Printf("dialect=%d chunk=%dK parallel=%d: %d MB in %.1fs = %.2f MB/s, CPU %.1fs (%.0f%% of one core)\n",
		*dialect, *chunk, *parallel, *mb, elapsed.Seconds(), float64(*mb)/elapsed.Seconds(), cpu.Seconds(), 100*cpu.Seconds()/elapsed.Seconds())
}

// writeParallel writes data with n WriteAt calls in flight, so the next
// request is on the wire while the server handles the previous one.
func writeParallel(f *smb2.File, data []byte, n, chunk int) error {
	offsets := make(chan int)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			for off := range offsets {
				end := min(off+chunk, len(data))
				if _, err := f.WriteAt(data[off:end], int64(off)); err != nil {
					errs <- err
					for range offsets {
					}
					return
				}
			}
			errs <- nil
		}()
	}
	for off := 0; off < len(data); off += chunk {
		offsets <- off
	}
	close(offsets)
	var first error
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil && first == nil {
			first = err
		}
	}
	return first
}

func cpuTime() time.Duration {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

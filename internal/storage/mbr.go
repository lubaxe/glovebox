package storage

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
)

const (
	sectorSize = 512
	// firstSector is where the partition starts: 1 MiB in, like fdisk/sfdisk.
	firstSector = 2048
	// partTypeExFAT is the MBR type for exFAT/NTFS ("HPFS/NTFS/exFAT").
	partTypeExFAT = 0x07
)

// mbr returns a master boot record with one partition of type partType that
// starts at 1 MiB and fills a disk of diskSize bytes.
func mbr(diskSize int64, partType byte) ([]byte, error) {
	sectors := diskSize / sectorSize
	if sectors <= firstSector {
		return nil, fmt.Errorf("disk size %d is too small", diskSize)
	}
	count := sectors - firstSector
	if count > 0xFFFFFFFF {
		return nil, fmt.Errorf("disk size %d is too big for an MBR partition table", diskSize)
	}

	b := make([]byte, sectorSize)
	// disk signature, so tools can tell images apart
	if _, err := io.ReadFull(rand.Reader, b[440:444]); err != nil {
		return nil, err
	}
	e := b[446:462]                        // first partition entry
	e[0] = 0x00                            // not bootable
	copy(e[1:4], []byte{0xFE, 0xFF, 0xFF}) // CHS start: "use LBA"
	e[4] = partType
	copy(e[5:8], []byte{0xFE, 0xFF, 0xFF}) // CHS end: "use LBA"
	binary.LittleEndian.PutUint32(e[8:12], firstSector)
	binary.LittleEndian.PutUint32(e[12:16], uint32(count))
	b[510], b[511] = 0x55, 0xAA
	return b, nil
}

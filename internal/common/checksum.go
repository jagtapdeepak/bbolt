package common

import (
	"encoding/binary"
	"hash/crc32"
)

// PageChecksumSize is the CRC-32 stored in the last bytes of a page span.
const PageChecksumSize = 4

var pageCRCTable = crc32.MakeTable(crc32.Castagnoli)

// Checksum returns the CRC-32 Castagnoli of buf excluding the trailing
// PageChecksumSize bytes (header and payload are both hashed). It does not
// inspect page type.
func Checksum(buf []byte) uint32 {
	if len(buf) < PageChecksumSize {
		return 0
	}
	return crc32.Checksum(buf[:len(buf)-PageChecksumSize], pageCRCTable)
}

// SetChecksum writes Checksum(buf) into the last PageChecksumSize bytes of buf.
func SetChecksum(buf []byte) {
	if len(buf) < PageChecksumSize {
		return
	}
	binary.LittleEndian.PutUint32(buf[len(buf)-PageChecksumSize:], Checksum(buf))
}

// VerifyChecksum reports whether the trailing checksum matches Checksum(buf).
func VerifyChecksum(buf []byte) bool {
	if len(buf) < PageChecksumSize {
		return false
	}
	got := binary.LittleEndian.Uint32(buf[len(buf)-PageChecksumSize:])
	return got == Checksum(buf)
}

// PagesForChecksum is how many pages are needed so payload bytes plus a
// trailing checksum fit in the span.
func PagesForChecksum(payload, pageSize int) int {
	if pageSize <= PageChecksumSize {
		return 1
	}
	n := (payload + PageChecksumSize + pageSize - 1) / pageSize
	if n < 1 {
		return 1
	}
	return n
}

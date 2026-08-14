package common

import (
	"encoding/binary"
	"testing"
)

func TestChecksum_CoversHeaderAndPayload(t *testing.T) {
	buf := make([]byte, 64)
	for i := range buf {
		buf[i] = byte(i)
	}
	SetChecksum(buf)
	if !VerifyChecksum(buf) {
		t.Fatal("checksum should match after SetChecksum")
	}

	buf[0]++
	if VerifyChecksum(buf) {
		t.Fatal("header change should fail verify")
	}
	buf[0]--

	buf[int(PageHeaderSize)]++
	if VerifyChecksum(buf) {
		t.Fatal("payload change should fail verify")
	}
}

func TestPagesForChecksum(t *testing.T) {
	const ps = 4096
	if n := PagesForChecksum(ps-PageChecksumSize, ps); n != 1 {
		t.Fatalf("got %d want 1", n)
	}
	if n := PagesForChecksum(ps-PageChecksumSize+1, ps); n != 2 {
		t.Fatalf("got %d want 2", n)
	}
}

func TestChecksum_StoredLittleEndian(t *testing.T) {
	buf := make([]byte, 32)
	SetChecksum(buf)
	want := Checksum(buf)
	got := binary.LittleEndian.Uint32(buf[len(buf)-PageChecksumSize:])
	if got != want {
		t.Fatalf("got %#x want %#x", got, want)
	}
}

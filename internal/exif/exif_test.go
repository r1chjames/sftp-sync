package exif

import (
	"encoding/binary"
	"os"
	"strings"
	"testing"
	"time"
)

// buildJPEGWithEXIF constructs a minimal JPEG with an embedded EXIF segment
// containing the given DateTimeOriginal string.
//
// Structure:
//
//	SOI (FF D8)
//	APP1 (FF E1) + length + "Exif\0\0"
//	  TIFF header (II or MM + 0x002A)
//	  IFD (1 entry: DateTimeOriginal tag 0x9003 → offset to ASCII string)
//	EOF (FF D9)
func buildJPEGWithEXIF(dateTime string) []byte {
	var buf []byte

	// JPEG SOI marker
	buf = append(buf, 0xFF, 0xD8)

	// APP1 marker + length placeholder
	app1Start := len(buf)
	buf = append(buf, 0xFF, 0xE1, 0x00, 0x00) // length filled later
	buf = append(buf, []byte("Exif\x00\x00")...)

	// TIFF header (little-endian, "II")
	tiffStart := len(buf)
	buf = append(buf, 0x49, 0x49, 0x2A, 0x00) // II + 0x002A
	// Offset to IFD0 (8 bytes from TIFF start = offset of IFD0 itself, which
	// immediately follows the 4-byte offset field).
	// Actually: TIFF header is 8 bytes total. IFD0 starts at tiffStart+8.
	buf = append(buf, 0x08, 0x00, 0x00, 0x00)

	// IFD0: count (1 entry), then the entry, then next IFD offset (0)
	ifdStart := len(buf)
	buf = append(buf, 0x01, 0x00) // 1 entry

	// --- IFD entry: DateTimeOriginal (tag 0x9003) ---
	// Tag: 0x9003 (little-endian)
	buf = append(buf, 0x03, 0x90)
	// Type: ASCII (2)
	buf = append(buf, 0x02, 0x00)
	// Count: length of dateTime string + null terminator
	count := uint32(len(dateTime) + 1)
	var countBytes [4]byte
	binary.LittleEndian.PutUint32(countBytes[:], count)
	buf = append(buf, countBytes[:]...)
	// Value/offset: we place the string after the IFD (after the 4-byte next-IFD pointer)
	// IFD currently ends at `ifdStart + 2 + 12 + 4 = ifdStart + 18`
	// But the string offset is relative to TIFF start, so:
	// stringOffsetRel = (ifdStart + 2 + 12 + 4) - tiffStart
	stringOffset := uint32((ifdStart + 18) - tiffStart)
	var offsetBytes [4]byte
	binary.LittleEndian.PutUint32(offsetBytes[:], stringOffset)
	buf = append(buf, offsetBytes[:]...)

	// Next IFD offset: 0 (end)
	buf = append(buf, 0x00, 0x00, 0x00, 0x00)

	// DateTimeOriginal string (null-terminated ASCII)
	buf = append(buf, []byte(dateTime)...)
	buf = append(buf, 0x00)

	// Fill in APP1 length. JPEG spec: the length value includes the 2-byte
	// length field itself but NOT the 0xFF 0xE1 marker bytes.
	app1Len := len(buf) - app1Start - 2
	buf[app1Start+2] = byte(app1Len >> 8)
	buf[app1Start+3] = byte(app1Len & 0xFF)

	// JPEG EOI marker
	buf = append(buf, 0xFF, 0xD9)

	return buf
}

// minimalJPEG returns a bare JPEG without any EXIF segment.
func minimalJPEG() []byte {
	return []byte{
		0xFF, 0xD8, // SOI
		0xFF, 0xDB, 0x00, 0x43, 0x00, // DQT marker + payload (dummy)
		0xFF, 0xD9, // EOI
	}
}

func TestDate_ValidEXIF(t *testing.T) {
	dateTime := "2024:06:15 14:30:00"
	data := buildJPEGWithEXIF(dateTime)

	f, err := os.CreateTemp("", "exif-test-*.jpg")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())

	if _, err := f.Write(data); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()

	d, err := Date(f.Name())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// EXIF DateTimeOriginal has no timezone — goexif returns local time.
	// Compare field-by-field rather than exact instant.
	if y, m, day := d.Date(); y != 2024 || m != time.June || day != 15 {
		t.Fatalf("Date = %v, want 2024-06-15", d)
	}
	if h, min, s := d.Clock(); h != 14 || min != 30 || s != 0 {
		t.Fatalf("Date = %v, want 14:30:00", d)
	}
}

func TestDate_NoEXIF(t *testing.T) {
	data := minimalJPEG()

	f, err := os.CreateTemp("", "exif-test-noexif-*.jpg")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())

	if _, err := f.Write(data); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()

	_, err = Date(f.Name())
	if err == nil {
		t.Fatal("expected error for JPEG without EXIF, got nil")
	}
}

func TestDate_NonImageFile(t *testing.T) {
	f, err := os.CreateTemp("", "exif-test-text-*.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())

	f.WriteString("not an image at all")
	f.Close()

	_, err = Date(f.Name())
	if err == nil {
		t.Fatal("expected error for non-image file, got nil")
	}
}

func TestDate_FileNotFound(t *testing.T) {
	_, err := Date("/path/does/not/exist.jpg")
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
	if !strings.Contains(err.Error(), "no such file") && !strings.Contains(err.Error(), "cannot find") {
		t.Fatalf("expected file-not-found error, got: %v", err)
	}
}

package verify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSHA256StreamMatchesTheKnownDigest(t *testing.T) {
	// The canonical digest of "abc" is a fixed published value, so this checks
	// the wiring rather than restating the implementation.
	want := "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"

	got, err := SHA256Stream(context.Background(), strings.NewReader("abc"))
	if err != nil {
		t.Fatalf("SHA256Stream() error = %v", err)
	}
	if got != want {
		t.Fatalf("digest = %q, want %q", got, want)
	}
}

func TestSHA256StreamHandlesEmptyAndLargeInput(t *testing.T) {
	empty, err := SHA256Stream(context.Background(), strings.NewReader(""))
	if err != nil {
		t.Fatalf("SHA256Stream(empty) error = %v", err)
	}
	// The digest of no bytes at all.
	if want := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"; empty != want {
		t.Fatalf("empty digest = %q, want %q", empty, want)
	}

	// Larger than one chunk, so the read loop iterates.
	data := strings.Repeat("photo", chunkSize)
	want := sha256.Sum256([]byte(data))
	got, err := SHA256Stream(context.Background(), strings.NewReader(data))
	if err != nil {
		t.Fatalf("SHA256Stream(large) error = %v", err)
	}
	if got != hex.EncodeToString(want[:]) {
		t.Fatalf("large digest = %q, want %q", got, hex.EncodeToString(want[:]))
	}
}

// TestSHA256StreamCancellation covers the requirement that hashing respects
// cancellation rather than reading a whole file to completion.
func TestSHA256StreamCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := SHA256Stream(ctx, strings.NewReader("abc"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

// TestSHA256StreamCancellationMidStream cancels after the first chunk is read,
// so the check has to happen inside the loop and not only before it.
func TestSHA256StreamCancellationMidStream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	reader := &cancellingReader{
		data:   make([]byte, 4*chunkSize),
		cancel: cancel,
	}

	_, err := SHA256Stream(ctx, reader)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	// The data needs four reads, so stopping after one proves the loop checked
	// the context between chunks rather than only before starting.
	if reader.reads != 1 {
		t.Fatalf("reads = %d, want the loop to stop after the first chunk", reader.reads)
	}
}

type cancellingReader struct {
	data   []byte
	offset int
	reads  int
	cancel func()
}

func (r *cancellingReader) Read(p []byte) (int, error) {
	if r.offset >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.offset:])
	r.offset += n
	r.reads++
	// Cancel as soon as the first chunk has been handed over, so the next loop
	// iteration must notice.
	if r.reads == 1 {
		r.cancel()
	}
	return n, nil
}

func TestSHA256StreamPropagatesReadErrors(t *testing.T) {
	wantErr := errors.New("connection reset")
	_, err := SHA256Stream(context.Background(), &failingReader{err: wantErr})

	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want it to wrap %v", err, wantErr)
	}
}

type failingReader struct{ err error }

func (r *failingReader) Read([]byte) (int, error) { return 0, r.err }

func TestSHA256File(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "photo.jpg")
	if err := os.WriteFile(path, []byte("abc"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := SHA256File(context.Background(), path)
	if err != nil {
		t.Fatalf("SHA256File() error = %v", err)
	}
	if want := "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"; got != want {
		t.Fatalf("digest = %q, want %q", got, want)
	}

	// A different file must produce a different digest, or the check is useless.
	other := filepath.Join(dir, "other.jpg")
	if err := os.WriteFile(other, []byte("abd"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	otherDigest, err := SHA256File(context.Background(), other)
	if err != nil {
		t.Fatalf("SHA256File(other) error = %v", err)
	}
	if otherDigest == got {
		t.Fatal("different contents produced the same digest")
	}
}

func TestSHA256FileMissingAndCancelled(t *testing.T) {
	if _, err := SHA256File(context.Background(), filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("SHA256File() = nil, want an error for a missing file")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "photo.jpg")
	if err := os.WriteFile(path, []byte("abc"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := SHA256File(ctx, path); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

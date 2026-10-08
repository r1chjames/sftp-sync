package sftp

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

func TestCopyWithProgressReportsCumulativeBytes(t *testing.T) {
	payload := bytes.Repeat([]byte("photo"), 200) // 1000 bytes
	var dst bytes.Buffer
	var calls []int64

	n, err := copyWithProgress(&dst, iotest.OneByteReader(bytes.NewReader(payload)), func(copied int64) error {
		calls = append(calls, copied)
		return nil
	})
	if err != nil {
		t.Fatalf("copyWithProgress: %v", err)
	}
	if n != int64(len(payload)) || dst.Len() != len(payload) {
		t.Fatalf("copied n=%d dst=%d, want %d", n, dst.Len(), len(payload))
	}
	if !bytes.Equal(dst.Bytes(), payload) {
		t.Fatal("copied bytes differ from source")
	}
	if len(calls) != len(payload) {
		t.Fatalf("progress calls = %d, want %d", len(calls), len(payload))
	}
	if calls[len(calls)-1] != int64(len(payload)) {
		t.Fatalf("final progress = %d, want %d", calls[len(calls)-1], len(payload))
	}
	for i := 1; i < len(calls); i++ {
		if calls[i] != calls[i-1]+1 {
			t.Fatalf("progress not monotonic at step %d: %d then %d", i, calls[i-1], calls[i])
		}
	}
}

func TestCopyWithProgressWithoutCallback(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 64)
	var dst bytes.Buffer

	n, err := copyWithProgress(&dst, bytes.NewReader(payload), nil)
	if err != nil {
		t.Fatalf("copyWithProgress: %v", err)
	}
	if n != int64(len(payload)) || dst.Len() != len(payload) {
		t.Fatalf("copied n=%d dst=%d, want %d", n, dst.Len(), len(payload))
	}
}

func TestCopyWithProgressStopsOnCallbackError(t *testing.T) {
	sentinel := errors.New("status update failed")
	payload := bytes.Repeat([]byte("y"), 50)
	var dst bytes.Buffer

	_, err := copyWithProgress(&dst, iotest.OneByteReader(bytes.NewReader(payload)), func(copied int64) error {
		if copied >= 10 {
			return sentinel
		}
		return nil
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want %v", err, sentinel)
	}
	if dst.Len() != 10 {
		t.Fatalf("copied %d bytes before abort, want 10", dst.Len())
	}
}

type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }

func TestCopyWithProgressPropagatesReadError(t *testing.T) {
	sentinel := errors.New("connection reset")
	src := io.MultiReader(strings.NewReader("12345"), failingReader{err: sentinel})
	var dst bytes.Buffer

	n, err := copyWithProgress(&dst, src, nil)
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want %v", err, sentinel)
	}
	if n != 5 || dst.Len() != 5 {
		t.Fatalf("copied n=%d dst=%d, want 5", n, dst.Len())
	}
}

// Package verify computes content digests used to check that a downloaded or
// previously existing file really is the file the server is offering.
package verify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
)

// chunkSize is the buffer used while hashing. A megabyte keeps the per-chunk
// cancellation check frequent enough to feel immediate on a slow transfer
// without turning the transfer into many tiny reads.
const chunkSize = 1 << 20

// SHA256Stream hashes everything read from r and returns the digest in lower-case
// hex.
//
// Cancellation is checked between chunks, so a long hash of a large file stops
// when the context is cancelled instead of running to completion. Hashing a
// multi-gigabyte video just to abandon the result would be wasted work, and on a
// shutdown it would delay the stop.
func SHA256Stream(ctx context.Context, r io.Reader) (string, error) {
	hash := sha256.New()
	buf := make([]byte, chunkSize)

	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}

		n, readErr := r.Read(buf)
		if n > 0 {
			if _, err := hash.Write(buf[:n]); err != nil {
				return "", fmt.Errorf("hashing: %w", err)
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			return "", fmt.Errorf("reading: %w", readErr)
		}
	}

	return hex.EncodeToString(hash.Sum(nil)), nil
}

// SHA256File hashes a local file.
func SHA256File(ctx context.Context, path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	defer file.Close()

	digest, err := SHA256Stream(ctx, file)
	if err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}
	return digest, nil
}

package syncer

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"

	"github.com/r1chjames/sftp-sync/internal/config"
	sftpclient "github.com/r1chjames/sftp-sync/internal/sftp"
	"github.com/r1chjames/sftp-sync/internal/state"
	"github.com/r1chjames/sftp-sync/internal/verify"
)

// errChecksumMismatch marks a transfer whose bytes did not survive it. It is
// retryable: a corrupted transfer is exactly the kind of failure another attempt
// can fix, unlike a permission problem.
var errChecksumMismatch = errors.New("checksum mismatch")

// adoptable reports whether the local file at localPath can be taken as the
// synced copy of a remote file, and returns the digest to record when one was
// computed.
//
// The size check is what stops a wrong, empty, or truncated local file from
// being recorded as a synced photo just because its path exists. It costs one
// stat and needs nothing from the server.
//
// In sha256 mode the contents are compared as well, which does need the server:
// the remote file is streamed to hash it, so hash mode spends a remote read per
// adopted file. That is the price of not trusting a size, and it is why the
// default mode does not hash.
func (s *Syncer) adoptable(ctx context.Context, remote sftpclient.RemoteFile, localPath string) (bool, string, error) {
	info, err := os.Stat(localPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Nothing local to adopt; the file is downloaded normally.
			return false, "", nil
		}
		return false, "", fmt.Errorf("stat %s: %w", localPath, err)
	}
	if info.IsDir() {
		// A directory in the way is the collision policy's problem, not a file
		// to adopt.
		return false, "", nil
	}

	if info.Size() != remote.Size {
		log.Printf("not adopting %s: local file is %d bytes, remote file is %d",
			localPath, info.Size(), remote.Size)
		return false, "", nil
	}

	if s.cfg.Sync.Verify != config.VerifySHA256 {
		return true, "", nil
	}

	remoteDigest, err := s.hashRemote(ctx, remote.Path)
	if err != nil {
		// Unverified, so not adopted. Whatever is wrong with the remote read
		// will surface again on the download path, with the file's own outcome.
		return false, "", fmt.Errorf("hash remote %s: %w", remote.Path, err)
	}

	localDigest, err := verify.SHA256File(ctx, localPath)
	if err != nil {
		return false, "", fmt.Errorf("hash local %s: %w", localPath, err)
	}

	if localDigest != remoteDigest {
		log.Printf("not adopting %s: contents differ from the remote file", localPath)
		return false, "", nil
	}

	return true, localDigest, nil
}

// verifyDownload checks a staged download against the remote file and returns the
// digest of both, which are equal when it returns without an error.
//
// A mismatch is a failed transfer and is retried by the caller's retry loop. A
// remote read that cannot be performed is not: discarding a file that was
// transferred successfully, because the server denies a second read of it, would
// throw away good work and repeat itself forever. That case is logged and the
// file is recorded without a digest, which is also what every other mode records.
func (s *Syncer) verifyDownload(ctx context.Context, remotePath, stagedPath string) (string, error) {
	localDigest, err := verify.SHA256File(ctx, stagedPath)
	if err != nil {
		return "", err
	}

	remoteDigest, err := s.hashRemote(ctx, remotePath)
	if err != nil {
		log.Printf("warning: could not verify %s against the remote file, keeping it unverified: %v",
			remotePath, err)
		return "", nil
	}

	if localDigest != remoteDigest {
		return "", fmt.Errorf("%w: %s hashed to %s but the remote file hashed to %s",
			errChecksumMismatch, remotePath, localDigest, remoteDigest)
	}

	return localDigest, nil
}

// entryFor builds the manifest entry for a downloaded file, including the digest
// only when verification produced one.
func entryFor(remote sftpclient.RemoteFile, localPath, digest string) state.Entry {
	return state.Entry{
		MTime:     remote.MTime,
		Size:      remote.Size,
		LocalPath: localPath,
		SHA256:    digest,
	}
}

package syncer

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/r1chjames/sftp-sync/internal/humanize"
	sftpclient "github.com/r1chjames/sftp-sync/internal/sftp"
)

// stagingPrefix is the name prefix of the temp files a download writes before it
// is moved into place. It comes from the package that creates them, so the
// cleanup and the creation cannot disagree about it.
const stagingPrefix = sftpclient.StagingPrefix

// stagingMaxAge is how old a staging file must be before startup will remove it.
//
// The threshold is deliberately generous. A staging file is created before the
// transfer and removed after it, so anything older than a day belongs to a run
// that is not coming back; anything younger might be a download in progress in
// this process or in another one sharing the directory, and deleting it would
// corrupt that transfer.
const stagingMaxAge = 24 * time.Hour

// pruneStaging removes staging files left behind by an interrupted run.
//
// It is called once when a job starts, before any download, so nothing it deletes
// can be in use by this job. It removes only regular files whose names match the
// staging pattern and whose modification time is older than stagingMaxAge, so a
// normal photo can never be deleted, however it is named, and a fresh temp file
// from another process is left alone.
func (s *Syncer) pruneStaging(dir string, now time.Time) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			log.Printf("warning: could not look for stale staging files in %s: %v", dir, err)
		}
		return 0
	}

	removed := 0
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, stagingPrefix) {
			continue
		}
		// ReadDir does not follow symlinks, so a symlink is not IsRegular and is
		// skipped: a link named like a staging file must not be followed.
		if !entry.Type().IsRegular() {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}
		age := now.Sub(info.ModTime())
		if age < stagingMaxAge {
			continue
		}

		path := filepath.Join(dir, name)
		if err := os.Remove(path); err != nil {
			log.Printf("warning: could not remove stale staging file %s: %v", path, err)
			continue
		}
		log.Printf("removed stale staging file %s (%s old)", path, age.Round(time.Hour))
		removed++
	}

	return removed
}

// diskSpaceMargin is the free space required beyond the batch itself, so a batch
// that just fits does not leave the filesystem full to the last byte.
const diskSpaceMargin = 64 << 20

// spaceError compares the free space on a filesystem with what a batch needs.
//
// It is separate from the query so the comparison can be tested, and it is a
// conservative check: the batch's total size is compared against free space, even
// though some of those bytes may already be on disk from a previous version of the
// file. It is a warning sign, not a promise.
func spaceError(dir string, available uint64, needed int64) error {
	if needed <= 0 {
		return nil
	}
	if available < uint64(needed)+diskSpaceMargin {
		return fmt.Errorf("not enough free space in %s: %s free, batch needs %s plus %s of margin",
			dir, unsignedBytes(available), humanize.Bytes(needed), humanize.Bytes(diskSpaceMargin))
	}
	return nil
}

// checkSpace reports an error when the local filesystem cannot hold the batch.
//
// A filesystem that cannot be queried is not a failure: the platform may not
// support it, and the download itself will fail with a clear error if space runs
// out. Refusing to start because space could not be measured would be worse than
// trying.
func (s *Syncer) checkSpace(dir string, needed int64) error {
	available, ok := availableBytes(dir)
	if !ok {
		log.Printf("warning: could not check free space in %s; continuing", dir)
		return nil
	}
	return spaceError(dir, available, needed)
}

// unsignedBytes renders a free-space count, which is unsigned, through the CLI's
// byte formatter, which takes a signed value.
func unsignedBytes(n uint64) string {
	if n > math.MaxInt64 {
		n = math.MaxInt64
	}
	return humanize.Bytes(int64(n))
}

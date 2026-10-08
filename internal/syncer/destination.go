package syncer

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/r1chjames/sftp-sync/internal/config"
	"github.com/r1chjames/sftp-sync/internal/state"
)

// maxRenameAttempts bounds the suffix search, so a directory full of conflicting
// names cannot make a sync spin forever.
const maxRenameAttempts = 1000

// batchDestinations decides which local path each downloaded file occupies, and
// tracks the ones already spoken for.
//
// Resolution is a check followed by a rename, so the check and the claim that
// follows must be atomic: two workers resolving the same desired path must not
// both be told it is free. Every candidate is tested and claimed under one lock
// before the caller is allowed to use it, which removes the check-then-rename
// window between workers entirely.
type batchDestinations struct {
	mu sync.Mutex
	// claimed maps a local path to the remote path holding it for this batch.
	claimed map[string]string
	// recorded maps a local path recorded in the manifest to its remote path,
	// so a destination belonging to a file that is not in this batch still
	// counts as occupied.
	recorded map[string]string
	// byRemote is the reverse index, so looking up what a remote path was last
	// written to does not scan the whole manifest per file.
	byRemote map[string]string
	// exhausted is set when the suffix search ran out of attempts, which is
	// worth saying once rather than per file.
	exhausted bool
	// maxAttempts bounds the suffix search. It is a field so tests can shrink the
	// budget instead of creating a thousand conflicting files.
	maxAttempts int
}

func newBatchDestinations(entries map[string]state.Entry) *batchDestinations {
	recorded := make(map[string]string, len(entries))
	byRemote := make(map[string]string, len(entries))
	for remotePath, entry := range entries {
		if entry.LocalPath == "" {
			continue
		}
		recorded[entry.LocalPath] = remotePath
		byRemote[remotePath] = entry.LocalPath
	}
	return &batchDestinations{
		claimed:     make(map[string]string),
		recorded:    recorded,
		byRemote:    byRemote,
		maxAttempts: maxRenameAttempts,
	}
}

// resolve returns the local path a remote file should occupy, applying the
// configured collision policy.
//
// A path already recorded for this remote file always wins, so a file that was
// renamed once is updated in place on the next sync instead of collecting
// another suffix.
//
// skipped reports that the policy chose to leave an existing file alone, in
// which case the caller has nothing to write.
func (d *batchDestinations) resolve(policy, remotePath, desired string) (path string, skipped bool, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if recorded := d.recordedFor(remotePath); recorded != "" {
		// Only another remote file's claim in this batch can take it away. The
		// file already at this path is the one this remote file wrote last time,
		// so it is not a collision, and the manifest may legitimately map two
		// remote paths here, in which case the first to ask keeps it.
		reason := d.claimedByOther(recorded, remotePath)
		if reason == "" {
			d.claimed[recorded] = remotePath
			return recorded, false, nil
		}
		log.Printf("warning: %s was mapped to %s, which is %s; choosing a new destination",
			remotePath, recorded, reason)
	}

	switch policy {
	case config.CollisionError:
		if reason := d.unavailableTo(desired, remotePath); reason != "" {
			return "", false, fmt.Errorf("destination %s is %s", desired, reason)
		}
		d.claimed[desired] = remotePath
		return desired, false, nil

	case config.CollisionSkip:
		if reason := d.unavailableTo(desired, remotePath); reason != "" {
			log.Printf("skipping %s: destination %s is %s", remotePath, desired, reason)
			return "", true, nil
		}
		d.claimed[desired] = remotePath
		return desired, false, nil

	default:
		limit := d.maxAttempts
		if limit <= 0 {
			limit = maxRenameAttempts
		}
		for attempt := 1; attempt <= limit; attempt++ {
			candidate := suffixed(desired, attempt)
			if reason := d.unavailableTo(candidate, remotePath); reason != "" {
				continue
			}
			d.claimed[candidate] = remotePath
			if attempt > 1 {
				log.Printf("renamed %s to %s: %s is taken", remotePath, candidate, desired)
			}
			return candidate, false, nil
		}
		if !d.exhausted {
			d.exhausted = true
			log.Printf("warning: no free destination for %s after %d attempts", desired, limit)
		}
		return "", false, fmt.Errorf("no free destination for %s after %d attempts", desired, limit)
	}
}

// recordedFor returns the local path recorded for a remote path, if any.
// Callers must hold d.mu.
func (d *batchDestinations) recordedFor(remotePath string) string {
	return d.byRemote[remotePath]
}

// claimedByOther reports that another remote file already holds this path in
// this batch, which is the one conflict no policy may resolve by writing.
// Callers must hold d.mu.
func (d *batchDestinations) claimedByOther(candidate, remotePath string) string {
	if owner, ok := d.claimed[candidate]; ok && owner != remotePath {
		return fmt.Sprintf("already the destination of %s", owner)
	}
	return ""
}

// unavailableTo describes why a fresh candidate cannot be used, or returns ""
// when it is free. Callers must hold d.mu.
func (d *batchDestinations) unavailableTo(candidate, remotePath string) string {
	if reason := d.claimedByOther(candidate, remotePath); reason != "" {
		return reason
	}
	if owner, ok := d.recorded[candidate]; ok && owner != remotePath {
		return fmt.Sprintf("already the destination of %s", owner)
	}
	if _, err := os.Lstat(candidate); err == nil {
		return "already occupied by a local file"
	}
	return ""
}

// suffixed returns the attempt-th candidate for a destination: the desired path
// itself for the first attempt, then IMG_0001-2.JPG, IMG_0001-3.JPG, and so on.
func suffixed(desired string, attempt int) string {
	if attempt <= 1 {
		return desired
	}
	ext := filepath.Ext(desired)
	return fmt.Sprintf("%s-%d%s", strings.TrimSuffix(desired, ext), attempt, ext)
}

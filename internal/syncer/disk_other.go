//go:build !linux && !darwin

package syncer

// availableBytes always reports that the free space is unknown, so the space
// check is skipped rather than reporting a space problem it cannot substantiate.
func availableBytes(string) (uint64, bool) {
	return 0, false
}

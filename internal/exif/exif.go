package exif

import (
	"os"
	"time"

	"github.com/rwcarlsen/goexif/exif"
)

// Date extracts the DateTimeOriginal from an image file's EXIF metadata.
// Returns a zero time and an error if EXIF data is absent or unreadable.
func Date(path string) (time.Time, error) {
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}, err
	}
	defer f.Close()

	x, err := exif.Decode(f)
	if err != nil {
		return time.Time{}, err
	}

	return x.DateTime()
}

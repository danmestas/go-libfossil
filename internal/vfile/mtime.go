package vfile

import (
	"fmt"
	"time"

	"github.com/danmestas/go-libfossil/simio"
)

// RecordableMTime is the vfile.mtime a scan or a write stores for a file
// whose mtime is mtime, in fossil's unit: whole seconds since 1970.
//
// This is the one deliberate difference from fossil. Fossil's
// vfile_check_signature (src/vfile.c) stores the mtime as it is and later
// skips hashing while size and mtime match, so it misses an edit that keeps
// the size within the second it recorded. Here an mtime not older than now's
// second is stored as 0, which makes the next scan, fossil's or ours, hash
// the file once; a stored 0 is never trusted.
func RecordableMTime(mtime time.Time, now int64) int64 {
	secs := mtime.Unix()
	if secs >= now {
		return 0
	}
	return secs
}

// WrittenMTime returns the vfile.mtime to record for the file at fullPath in
// env's storage, statting it right after it was written, as fossil's
// vfile_to_disk does. now is read before the stat, so a write after the stat
// cannot slip past the guard with an mtime equal to the one recorded.
func WrittenMTime(env *simio.Env, fullPath string) (int64, error) {
	if env == nil {
		panic("vfile.WrittenMTime: nil env")
	}
	now := env.Clock.Now().Unix()
	info, err := env.Storage.Stat(fullPath)
	if err != nil {
		return 0, fmt.Errorf("stat %s: %w", fullPath, err)
	}
	return RecordableMTime(info.ModTime(), now), nil
}

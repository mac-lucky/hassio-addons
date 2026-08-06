package recon

import (
	"errors"
	"io/fs"
	"os"
)

// pausePath is the flag file recording "the unattended loop is off":
// present means paused. Its own file rather than a state.json field, since
// every state.json write is a load-modify-save under opLock and a pause
// would either need that lock or race the in-flight save. A var so tests
// can point it at a temp directory.
var pausePath = "/data/paused"

// pausedFileBody is the paused flag file's contents. Nothing parses it -
// the file's presence is the flag - it just beats a zero-length file.
const pausedFileBody = "1\n"

// readPausedFile reports whether the flag file is there. Regular files
// only - a directory could never be cleared by writePausedFile, so it
// would read as paused forever. Any stat error reads as not paused.
func readPausedFile() bool {
	info, err := os.Stat(pausePath)
	return err == nil && info.Mode().IsRegular()
}

// writePausedFile records paused on disk, creating or removing the flag
// file. An already-absent file is not an error; it is what resume asks for.
func writePausedFile(paused bool) error {
	if !paused {
		if err := os.Remove(pausePath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	return os.WriteFile(pausePath, []byte(pausedFileBody), 0o600)
}

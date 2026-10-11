package gitsync

import (
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync/atomic"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/ignorekeys"
)

// userIgnoredKeys is the ignore_keys option compiled: the key paths of
// each file that has entries, by config-relative path. nil when the option
// is empty. Set once in main before any goroutine starts, like
// userExcluded, and atomic for the same reason.
var userIgnoredKeys atomic.Pointer[map[string][]ignorekeys.KeyPath]

// Bounds on the ignore_keys option, exclude_paths' numbers.
const (
	maxIgnoreKeyEntries  = 100
	maxIgnoreKeyEntryLen = 256
)

// SetIgnoredKeys validates the ignore_keys option and installs it. Each
// entry is "<file>:<dotted.key.path>" (ignorekeys.ParseEntry), and its file
// must be one this agent syncs at all: a secrets file, an excluded path or
// a secret-shaped name is refused rather than silently never matching. The
// error names the first bad entry by its position and says why, without
// quoting it; main treats it as fatal, like a bad exclude_paths entry.
// Call it after SetUserExclusions, which it checks against.
func SetIgnoredKeys(entries []string) error {
	if len(entries) > maxIgnoreKeyEntries {
		return fmt.Errorf("ignore_keys: %d entries, at most %d are allowed", len(entries), maxIgnoreKeyEntries)
	}
	byFile := make(map[string][]ignorekeys.KeyPath, len(entries))
	for i, e := range entries {
		entry, err := parseIgnoreKeyEntry(e)
		if err != nil {
			return fmt.Errorf("ignore_keys entry %d: %w", i+1, err)
		}
		if !slices.ContainsFunc(byFile[entry.File], func(k ignorekeys.KeyPath) bool { return slices.Equal(k, entry.Key) }) {
			byFile[entry.File] = append(byFile[entry.File], entry.Key)
		}
	}
	if len(byFile) == 0 {
		userIgnoredKeys.Store(nil)
		return nil
	}
	userIgnoredKeys.Store(&byFile)
	return nil
}

// parseIgnoreKeyEntry is one entry's syntax plus this package's view of
// whether its file is synced at all.
func parseIgnoreKeyEntry(e string) (ignorekeys.Entry, error) {
	if len(e) > maxIgnoreKeyEntryLen {
		return ignorekeys.Entry{}, fmt.Errorf("is longer than %d characters", maxIgnoreKeyEntryLen)
	}
	entry, err := ignorekeys.ParseEntry(e)
	if err != nil {
		return ignorekeys.Entry{}, err
	}
	switch {
	case IsSecretsFile(entry.File):
		return ignorekeys.Entry{}, errors.New("names a secrets file, which is never synced anyway")
	case Excluded(entry.File) || matchesSecretPattern(entry.File):
		return ignorekeys.Entry{}, errors.New("names a file that is excluded from syncing, so it has no keys to ignore")
	}
	return entry, nil
}

// IgnoredKeyFiles lists the files ignore_keys names, sorted: what main
// logs at startup instead of the entries themselves.
func IgnoredKeyFiles() []string {
	byFile := userIgnoredKeys.Load()
	if byFile == nil {
		return nil
	}
	files := make([]string, 0, len(*byFile))
	for f := range *byFile {
		files = append(files, f)
	}
	slices.Sort(files)
	return files
}

// IgnoredKeys returns the ignore_keys key paths for p, a repo/config-
// relative path as git lists it, or nil when p has none. Entries are exact
// paths, so the match is exact too.
func IgnoredKeys(p string) []ignorekeys.KeyPath {
	byFile := userIgnoredKeys.Load()
	if byFile == nil {
		return nil
	}
	return (*byFile)[p]
}

// sameIgnoringKeys reports whether two copies of p say the same thing:
// byte for byte, unless p has ignore_keys entries, when they are compared
// as YAML with those keys set aside (ignorekeys.Equal). A copy that does
// not parse falls back to the byte compare, with a warning naming the file
// and never its content.
func sameIgnoringKeys(p string, a, b []byte) bool {
	if keys := IgnoredKeys(p); len(keys) > 0 {
		equal, err := ignorekeys.Equal(keys, a, b)
		if err == nil {
			return equal
		}
		slog.Warn("gitsync: ignore_keys not applied, comparing the file byte for byte", "path", p, "reason", err)
	}
	return string(a) == string(b)
}

// withRepoValuesAt is what a write to git puts at p in place of live's
// content: for a file with ignore_keys entries, live's content with each
// ignored key carrying the repository's value, or removed where the
// repository has none (ignorekeys.Overlay), so a value only the app wrote
// never reaches git and never trips the literal-secret check. repo is nil
// when the repository does not hold p, and a repository copy that cannot
// be read that way counts as holding none of the keys.
//
// An error means the keys could not be set aside in live's copy - it does
// not parse as one YAML document, or an ignored key is ambiguous or
// anchored there - and the caller must not write it at all: the secret
// check only catches secret-shaped keys, and an ignored mqtt.user or
// mqtt.server would go to git in the clear.
func withRepoValuesAt(p string, live, repo []byte) ([]byte, error) {
	keys := IgnoredKeys(p)
	if len(keys) == 0 {
		return live, nil
	}
	out, err := ignorekeys.Overlay(keys, live, repo)
	if errors.Is(err, ignorekeys.ErrSource) {
		slog.Warn("gitsync: the repository copy cannot supply the ignored keys, removing them instead", "path", p, "reason", err)
		out, err = ignorekeys.Overlay(keys, live, nil)
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ignoreKeysHeldBack is the HeldBack for a file whose ignore_keys could
// not be set aside (withRepoValuesAt's error): named by its ignored key
// paths, with why in words. err only ever carries ignorekeys' own fixed
// phrases, never content.
func ignoreKeysHeldBack(p string, err error) HeldBack {
	keys := IgnoredKeys(p)
	names := make([]string, len(keys))
	for i, k := range keys {
		names[i] = k.String()
	}
	cause := ignorekeys.ErrNotYAML
	for _, e := range []error{ignorekeys.ErrAmbiguous, ignorekeys.ErrAnchored} {
		if errors.Is(err, e) {
			cause = e
		}
	}
	return HeldBack{Path: p, Keys: names, Reason: "ignore_keys could not be set aside: the file " + cause.Error()}
}

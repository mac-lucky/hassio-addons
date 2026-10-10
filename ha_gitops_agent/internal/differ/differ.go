// Package differ compares a checked-out repo tree against live config. It
// only describes what would change - it writes to neither side.
package differ

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/fsx"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/gitsync"
	"github.com/pmezard/go-difflib/difflib"
)

// Truncation caps applied to generated unified diffs so a single huge file
// can't blow up status payloads or the web UI.
const (
	maxDiffLines        = 400
	maxDiffBytes        = 40 * 1024
	truncationMarker    = "\n... diff truncated\n"
	binarySniffBytes    = 8000 // leading bytes sniffed for a NUL byte to decide "binary"
	largeFileChunkBytes = 1024 * 1024
)

// largeFileThresholdBytes: at or above this size, either side, a file is
// compared in chunks (largeFilesEqual) rather than read in - Compute holds
// both copies at once and would otherwise be OOM-killed. A var so tests
// can lower it.
var largeFileThresholdBytes int64 = 4 * 1024 * 1024

// kindRank sorts adds/updates before deletes; ties are broken by path.
func kindRank(kind string) int {
	switch kind {
	case "add", "update":
		return 0
	case "delete":
		return 1
	default:
		return 2
	}
}

// Change is one file-level difference between the repo and the live
// config.
type Change struct {
	// Path is relative to /config (and equally, relative to the repo
	// root), e.g. "automations.yaml" or "packages/demo.yaml".
	Path string
	// Kind is "add" (tracked, missing from config), "update" (both, content
	// differs), or "delete" (applied before, no longer tracked).
	Kind string
	// DiffText is a unified diff for text files, or a one-line summary for
	// binary content and files above largeFileThresholdBytes.
	DiffText string
}

// Compute compares tracked repo files against live config, one Change per
// differing path: "add", "update", or "delete" (only prevManifest paths
// can be deleted). gitsync.Excluded is skipped in both directions, so a
// secrets file or an exclude_paths entry is never planned - not even the
// delete of one an older version applied and recorded in the manifest.
//
// skippedContainment names paths refused for a security reason - a
// symlink or an escape past the root.
func Compute(repoRoot, configRoot string, tracked, prevManifest []string) (changes []Change, skippedContainment []string) {
	trackedSet := make(map[string]bool, len(tracked))
	for _, p := range tracked {
		trackedSet[p] = true
	}

	// Resolved once per call: every read below is confined to these two
	// roots (see isRegularFile).
	repoRootReal := fsx.Realpath(repoRoot)
	configRootReal := fsx.Realpath(configRoot)

	for _, p := range tracked {
		if gitsync.Excluded(p) {
			continue
		}
		c, ok, suspicious := diffTrackedPath(repoRoot, configRoot, repoRootReal, configRootReal, p)
		if ok {
			changes = append(changes, c)
		}
		if suspicious {
			skippedContainment = append(skippedContainment, p)
		}
	}

	for _, p := range prevManifest {
		if trackedSet[p] || gitsync.Excluded(p) {
			continue
		}
		c, ok, suspicious := diffDeletedPath(configRoot, configRootReal, p)
		if ok {
			changes = append(changes, c)
		}
		if suspicious {
			skippedContainment = append(skippedContainment, p)
		}
	}

	sort.SliceStable(changes, func(i, j int) bool {
		ri, rj := kindRank(changes[i].Kind), kindRank(changes[j].Kind)
		if ri != rj {
			return ri < rj
		}
		return changes[i].Path < changes[j].Path
	})
	sort.Strings(skippedContainment)
	return changes, skippedContainment
}

// diffTrackedPath produces one "add" or "update" Change, or nothing when
// the content matches or the path could not be compared - one unreadable
// file must never abort the diff. suspicious means a security refusal (see
// isRegularFile).
func diffTrackedPath(repoRoot, configRoot, repoRootReal, configRootReal, p string) (change Change, ok, suspicious bool) {
	repoPath := filepath.Join(repoRoot, p)
	configPath := filepath.Join(configRoot, p)

	if regular, susp := isRegularFile(repoPath, repoRootReal); !regular {
		slog.Warn("tracked path missing, not a regular file, or escapes the repo root via a symlink", "path", p)
		return Change{}, false, susp
	}

	// Any stat error, not only ENOENT, takes the "add" branch - matching
	// Python's os.path.exists(), which swallows them all.
	if _, err := os.Stat(configPath); err != nil {
		repoSize, sizeErr := fileSize(repoPath)
		if sizeErr != nil {
			slog.Warn("skipping while diffing", "path", p, "error", sizeErr)
			return Change{}, false, false
		}
		if repoSize > largeFileThresholdBytes {
			return Change{Path: p, Kind: "add", DiffText: largeFileSummary(0, repoSize)}, true, false
		}
		repoBytes := readFile(repoPath)
		if repoBytes == nil {
			return Change{}, false, false
		}
		return Change{Path: p, Kind: "add", DiffText: diffTextFor(nil, repoBytes, p)}, true, false
	}

	if regular, susp := isRegularFile(configPath, configRootReal); !regular {
		slog.Warn("config path is missing, not a regular file, or escapes the config root via a symlink", "path", p)
		return Change{}, false, susp
	}

	repoSize, err := fileSize(repoPath)
	if err != nil {
		slog.Warn("skipping while diffing", "path", p, "error", err)
		return Change{}, false, false
	}
	configSize, err := fileSize(configPath)
	if err != nil {
		slog.Warn("skipping while diffing", "path", p, "error", err)
		return Change{}, false, false
	}

	if repoSize > largeFileThresholdBytes || configSize > largeFileThresholdBytes {
		if largeFilesEqual(repoPath, configPath) {
			return Change{}, false, false
		}
		return Change{Path: p, Kind: "update", DiffText: largeFileSummary(configSize, repoSize)}, true, false
	}

	repoBytes := readFile(repoPath)
	configBytes := readFile(configPath)
	if repoBytes == nil || configBytes == nil {
		return Change{}, false, false
	}
	if bytes.Equal(repoBytes, configBytes) {
		return Change{}, false, false
	}
	return Change{Path: p, Kind: "update", DiffText: diffTextFor(configBytes, repoBytes, p)}, true, false
}

// diffTextFor builds a Change's DiffText, routing it through the masking
// pass when either side holds a literal value under a secret-shaped key,
// so that no secret is ever published.
func diffTextFor(beforeBytes, afterBytes []byte, path string) string {
	if !holdsSecrets(path, beforeBytes, afterBytes) {
		return makeDiff(beforeBytes, afterBytes, path)
	}
	return maskedDiff(beforeBytes, afterBytes, path)
}

// diffDeletedPath produces a "delete" Change for an untracked prevManifest
// path still present under configRoot. A delete diff quotes the LIVE file
// in full, so secret-bearing content is masked on the way out.
func diffDeletedPath(configRoot, configRootReal, p string) (change Change, ok, suspicious bool) {
	configPath := filepath.Join(configRoot, p)
	if regular, susp := isRegularFile(configPath, configRootReal); !regular {
		return Change{}, false, susp
	}

	configSize, err := fileSize(configPath)
	if err != nil {
		slog.Warn("skipping delete check", "path", p, "error", err)
		return Change{}, false, false
	}
	if configSize > largeFileThresholdBytes {
		return Change{Path: p, Kind: "delete", DiffText: largeFileSummary(configSize, 0)}, true, false
	}

	configBytes := readFile(configPath)
	if configBytes == nil {
		return Change{}, false, false
	}
	return Change{Path: p, Kind: "delete", DiffText: diffTextFor(configBytes, nil, p)}, true, false
}

// isRegularFile Lstats path (no symlink following) and requires its
// resolved location to stay inside rootReal - the leaf check alone misses
// a symlinked PARENT. A tracked symlink would otherwise publish any
// readable file's plaintext through Change.DiffText and /status.json.
//
// A stat failure is ok=false, suspicious=false; suspicious=true means the
// path exists but was refused, which recon surfaces as an event.
func isRegularFile(path, rootReal string) (ok, suspicious bool) {
	info, err := os.Lstat(path)
	if err != nil {
		return false, false
	}
	if !info.Mode().IsRegular() {
		return false, true
	}
	pathReal := fsx.Realpath(path)
	if pathReal == rootReal || strings.HasPrefix(pathReal, rootReal+string(filepath.Separator)) {
		return true, false
	}
	return false, true
}

// fileSize stats path for its size.
func fileSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// readFile reads a file whole, returning nil and a warning on any I/O
// failure. A var so tests can prove the large-file gate never calls it.
var readFile = defaultReadFile

func defaultReadFile(path string) []byte {
	data, err := os.ReadFile(path) // #nosec G304 -- path is always joined from repoRoot/configRoot with a git-tracked or previously-applied relative path, both caller-controlled inputs
	if err != nil {
		slog.Warn("could not read file", "path", path, "error", err)
		return nil
	}
	return data
}

// largeFilesEqual compares two files in lockstep largeFileChunkBytes
// chunks, never loading either whole. Unequal lengths, and either file
// failing to open, compare unequal - the safe default.
func largeFilesEqual(pathA, pathB string) bool {
	fa, err := os.Open(pathA) // #nosec G304 -- see defaultReadFile
	if err != nil {
		return false
	}
	defer func() { _ = fa.Close() }()
	fb, err := os.Open(pathB) // #nosec G304 -- see defaultReadFile
	if err != nil {
		return false
	}
	defer func() { _ = fb.Close() }()

	bufA := make([]byte, largeFileChunkBytes)
	bufB := make([]byte, largeFileChunkBytes)
	for {
		nA, errA := io.ReadFull(fa, bufA)
		nB, errB := io.ReadFull(fb, bufB)
		if nA != nB || !bytes.Equal(bufA[:nA], bufB[:nB]) {
			return false
		}

		doneA := errA == io.EOF || errA == io.ErrUnexpectedEOF
		doneB := errB == io.EOF || errB == io.ErrUnexpectedEOF
		if errA != nil && !doneA {
			return false
		}
		if errB != nil && !doneB {
			return false
		}
		if doneA || doneB {
			return doneA == doneB
		}
	}
}

// largeFileSummary is the one-line stand-in for a file compared without
// being read in, styled like makeDiff's binary-file summary.
func largeFileSummary(beforeSize, afterSize int64) string {
	return fmt.Sprintf("large file changed, %d -> %d bytes", beforeSize, afterSize)
}

// looksBinary treats a NUL byte in the first few KB as a binary marker,
// matching the heuristic git itself uses.
func looksBinary(data []byte) bool {
	n := len(data)
	if n > binarySniffBytes {
		n = binarySniffBytes
	}
	return bytes.IndexByte(data[:n], 0) >= 0
}

// makeDiff builds a unified diff (truncated if huge) between two file
// contents, or a one-line summary if either side is binary or not UTF-8.
// Line-diffing is go-difflib, the same engine internal/registries uses.
func makeDiff(beforeBytes, afterBytes []byte, path string) string {
	if looksBinary(beforeBytes) || looksBinary(afterBytes) || !utf8.Valid(beforeBytes) || !utf8.Valid(afterBytes) {
		return fmt.Sprintf("binary file changed, %d -> %d bytes", len(beforeBytes), len(afterBytes))
	}

	beforeLines := splitLinesKeepEnds(string(beforeBytes))
	afterLines := splitLinesKeepEnds(string(afterBytes))

	diff := difflib.UnifiedDiff{
		A:        beforeLines,
		B:        afterLines,
		FromFile: "config/" + path,
		ToFile:   "repo/" + path,
		Context:  3,
	}
	text, err := difflib.GetUnifiedDiffString(diff)
	if err != nil {
		// Only reachable if SequenceMatcher setup fails, which plain
		// []string inputs cannot cause.
		return ""
	}
	if text == "" {
		return ""
	}
	return truncate(splitLinesKeepEnds(text))
}

// truncate caps a unified diff at maxDiffLines lines and maxDiffBytes
// bytes, appending truncationMarker if either cap was hit.
func truncate(diffLines []string) string {
	truncated := false

	if len(diffLines) > maxDiffLines {
		diffLines = diffLines[:maxDiffLines]
		truncated = true
	}

	text := strings.Join(diffLines, "")
	encoded := []byte(text)
	if len(encoded) > maxDiffBytes {
		text = truncateValidUTF8(encoded, maxDiffBytes)
		truncated = true
	}

	if truncated {
		text += truncationMarker
	}
	return text
}

// truncateValidUTF8 cuts b to at most n bytes, then trims back to the last
// full rune boundary rather than leaving a replacement character.
func truncateValidUTF8(b []byte, n int) string {
	if n > len(b) {
		n = len(b)
	}
	b = b[:n]
	for len(b) > 0 && !utf8.Valid(b) {
		b = b[:len(b)-1]
	}
	return string(b)
}

// splitLinesKeepEnds splits s into lines, each keeping its trailing "\n"
// and none added to the last. Only "\n" counts as a boundary, which is all
// the YAML config this package diffs ever uses.
func splitLinesKeepEnds(s string) []string {
	if s == "" {
		return nil
	}
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i+1])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

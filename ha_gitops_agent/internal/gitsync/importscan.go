package gitsync

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/humanize"
)

// ImportLimits bounds what ScanLive accepts. A struct so an option could be
// wired in later - see DefaultImportLimits for why there is none today.
type ImportLimits struct {
	// MaxFiles caps how many importable files the tree may hold.
	MaxFiles int
	// MaxTotalBytes caps their combined size.
	MaxTotalBytes int64
	// MaxFileBytes caps any single file's size. Checked first: a huge blob
	// has a precise culprit worth naming.
	MaxFileBytes int64
	// MaxEntries caps how many directory entries the walk visits at all. A
	// backstop for a config root that is a mount point for something
	// enormous, which would otherwise walk unbounded to report nothing.
	MaxEntries int
}

// DefaultImportLimits are the limits every caller uses, sized against a
// real install: 6789 importable files and about 135 MB once .storage/, the
// recorder database, backups/, deps/ and tts/ are gone, 5127 of them
// custom_components/. An earlier 5000-file limit refused that install.
//
// Constants rather than add-on options, since their only right setting is
// "big enough for a sane config, small enough that a mistake fails"; the
// fix for hitting one is to move the offending directory out of the config
// root. .gitignore is NOT that fix - these are measured on the live
// filesystem before git is involved.
func DefaultImportLimits() ImportLimits {
	return ImportLimits{
		MaxFiles:      25000,
		MaxTotalBytes: 400 << 20, // 400 MiB
		MaxFileBytes:  25 << 20,  // 25 MiB
		MaxEntries:    200000,
	}
}

// maxImportOffenders bounds how many culprits an ImportTooLargeError names.
const maxImportOffenders = 10

// ImportPlan is everything ScanLive found: the files an import would
// capture, plus a per-reason account of what it passed over, so the UI can
// say "3 secret-shaped files were skipped".
type ImportPlan struct {
	// Files are repo-relative, forward-slash separated and sorted - what
	// TrackedFiles returns and Excluded/guardDriftPath expect.
	Files []string
	// TotalBytes is their combined size.
	TotalBytes int64

	// SkippedExcluded counts ExcludedPatterns hits. A pruned directory
	// counts once, not once per file underneath it.
	SkippedExcluded int
	// SkippedSecret counts SecretPatterns hits.
	SkippedSecret int
	// SkippedNonRegular counts symlinks, sockets, fifos and devices.
	SkippedNonRegular int
	// SkippedUnreadable counts entries that vanished or could not be stat'd
	// mid-walk - ordinary on a live tree, not an error.
	SkippedUnreadable int
}

// ImportOffender is one culprit in an ImportTooLargeError: a file (Path,
// Bytes), or a directory (trailing slash, Files too) for the count breach.
type ImportOffender struct {
	Path  string
	Bytes int64
	Files int
}

// ImportTooLargeError is returned when a live tree blows one of
// ImportLimits, naming what did it: "the import is too large" alone sends
// someone hunting through a config directory by hand.
type ImportTooLargeError struct {
	// Reason is which limit broke: "single file", "file count",
	// "total size" or "entry count".
	Reason string
	Limit  int64
	Actual int64
	// Offenders are the largest contributors, biggest first, capped at
	// maxImportOffenders. Empty for "entry count", which aborts the walk
	// before any accounting is meaningful.
	Offenders []ImportOffender
}

func (e *ImportTooLargeError) Error() string {
	var b strings.Builder
	b.WriteString("gitsync: import: refusing to import: ")
	switch e.Reason {
	case "file count", "entry count":
		fmt.Fprintf(&b, "%d %s exceeds the %d limit", e.Actual, pluralFiles(e.Reason), e.Limit)
	default:
		fmt.Fprintf(&b, "%s %s exceeds the %s limit", e.Reason, humanize.Bytes(e.Actual), humanize.Bytes(e.Limit))
	}
	if len(e.Offenders) > 0 {
		if e.Reason == "file count" {
			b.WriteString("; largest directories: ")
		} else {
			b.WriteString("; largest: ")
		}
		for i, o := range e.Offenders {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(o.Path)
			if o.Files > 0 {
				fmt.Fprintf(&b, " (%d files, %s)", o.Files, humanize.Bytes(o.Bytes))
			} else {
				fmt.Fprintf(&b, " (%s)", humanize.Bytes(o.Bytes))
			}
		}
	}
	if e.Reason == "entry count" {
		b.WriteString("; is the config directory a mount point for something else?")
	} else {
		// Deliberately does NOT suggest .gitignore: the limits are measured
		// before git is involved, so ignoring the offender changes nothing.
		b.WriteString("; move it out of the config directory and try again (.gitignore does not help here - the size check runs before git sees the files)")
	}
	return b.String()
}

// pluralFiles names the unit a count-based Reason is counting.
func pluralFiles(reason string) string {
	if reason == "entry count" {
		return "directory entries"
	}
	return "files"
}

// errTooManyEntries aborts a walk past ImportLimits.MaxEntries. A sentinel,
// so fs.WalkDir's error plumbing carries something cheap and the real error
// is built once at the top.
var errTooManyEntries = errors.New("too many entries")

// ScanLive walks configRoot and returns every file an import would capture:
// regular files whose repo-relative path is neither Excluded nor
// secret-shaped, refusing the whole result if it breaks limits.
//
// No context parameter: pure filesystem work bounded by limits.MaxEntries,
// run under the caller's own lock. Limits are enforced after the walk and
// BEFORE any git, so an oversized tree leaves no half-made branch; a breach
// returns the zero ImportPlan, never a partial one that would look complete.
func ScanLive(configRoot string, limits ImportLimits) (ImportPlan, error) {
	// Checked up front: WalkDir reports a failure on the ROOT through the
	// same callback, and the tolerant handling below would turn "the config
	// directory is not there" into a successful scan that found nothing.
	info, err := os.Stat(configRoot)
	if err != nil {
		return ImportPlan{}, fmt.Errorf("gitsync: import: cannot read the config directory %s: %w", configRoot, err)
	}
	if !info.IsDir() {
		return ImportPlan{}, fmt.Errorf("gitsync: import: the config directory %s is not a directory", configRoot)
	}

	var plan ImportPlan
	var offenders []ImportOffender
	dirFiles := map[string]int{}
	dirBytes := map[string]int64{}
	entries := 0

	err = filepath.WalkDir(configRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable subtree is skipped whole rather than failing
			// the scan: it must not make importing the rest impossible.
			plan.SkippedUnreadable++
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		entries++
		if limits.MaxEntries > 0 && entries > limits.MaxEntries {
			return errTooManyEntries
		}

		rel, relErr := filepath.Rel(configRoot, path)
		if relErr != nil {
			plan.SkippedUnreadable++
			return nil
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}

		if d.IsDir() {
			// Pruning whole subtrees is what keeps .storage/ and backups/
			// free, and counts each once toward SkippedExcluded. Excluded
			// already matches by path segment, so no trailing-slash fixup.
			if Excluded(rel) {
				plan.SkippedExcluded++
				return fs.SkipDir
			}
			if secretShapedDisallowed(rel) {
				plan.SkippedSecret++
				return fs.SkipDir
			}
			return nil
		}

		// d.Type() has Lstat semantics and WalkDir never follows symlinks,
		// so a symlinked file lands here as ModeSymlink and a symlinked
		// directory as a non-dir. Refusing both is the point: os.Stat would
		// hand us the target's bytes, so a "notes.yaml" pointing at
		// /data/options.json would be committed verbatim to a public repo.
		if !d.Type().IsRegular() {
			plan.SkippedNonRegular++
			return nil
		}

		if Excluded(rel) {
			plan.SkippedExcluded++
			return nil
		}
		// matchesSecretPattern, which this wraps, does no path.Clean and no
		// backslash conversion; safe only because rel came from
		// filepath.Rel plus ToSlash and is already clean.
		if secretShapedDisallowed(rel) {
			plan.SkippedSecret++
			return nil
		}

		info, infoErr := d.Info()
		if infoErr != nil {
			plan.SkippedUnreadable++
			return nil
		}

		// Belt and braces: a path the scan accepts must never be one
		// staging then rejects fatally. WalkDir not following symlinks
		// should already make this unreachable.
		if _, guardErr := guardDriftPath(configRoot, rel); guardErr != nil {
			plan.SkippedNonRegular++
			return nil
		}

		size := info.Size()
		if limits.MaxFileBytes > 0 && size > limits.MaxFileBytes {
			offenders = append(offenders, ImportOffender{Path: rel, Bytes: size})
		}
		plan.Files = append(plan.Files, rel)
		plan.TotalBytes += size

		top := rel
		if i := strings.IndexByte(rel, '/'); i >= 0 {
			top = rel[:i] + "/"
		}
		dirFiles[top]++
		dirBytes[top] += size
		return nil
	})

	switch {
	case errors.Is(err, errTooManyEntries):
		return ImportPlan{}, &ImportTooLargeError{
			Reason: "entry count",
			Limit:  int64(limits.MaxEntries),
			Actual: int64(entries),
		}
	case err != nil:
		return ImportPlan{}, fmt.Errorf("gitsync: import: scanning %s: %w", configRoot, err)
	}

	// Most specific breach first, so the message names a file where it can.
	if len(offenders) > 0 {
		sortOffenders(offenders)
		biggest := offenders[0]
		return ImportPlan{}, &ImportTooLargeError{
			Reason:    "single file",
			Limit:     limits.MaxFileBytes,
			Actual:    biggest.Bytes,
			Offenders: capOffenders(offenders),
		}
	}
	if limits.MaxFiles > 0 && len(plan.Files) > limits.MaxFiles {
		return ImportPlan{}, &ImportTooLargeError{
			Reason: "file count",
			Limit:  int64(limits.MaxFiles),
			Actual: int64(len(plan.Files)),
			// Directories, not files: naming ten files when several
			// thousand tripped a COUNT limit is nothing to act on.
			Offenders: capOffenders(dirOffenders(dirFiles, dirBytes)),
		}
	}
	if limits.MaxTotalBytes > 0 && plan.TotalBytes > limits.MaxTotalBytes {
		return ImportPlan{}, &ImportTooLargeError{
			Reason:    "total size",
			Limit:     limits.MaxTotalBytes,
			Actual:    plan.TotalBytes,
			Offenders: capOffenders(dirOffenders(dirFiles, dirBytes)),
		}
	}

	sort.Strings(plan.Files)
	return plan, nil
}

// dirOffenders turns the per-top-level-directory tallies into offenders,
// largest first.
func dirOffenders(files map[string]int, bytes map[string]int64) []ImportOffender {
	out := make([]ImportOffender, 0, len(files))
	for p, n := range files {
		out = append(out, ImportOffender{Path: p, Files: n, Bytes: bytes[p]})
	}
	sortOffenders(out)
	return out
}

// sortOffenders orders by size descending, then path, so error messages are
// deterministic.
func sortOffenders(o []ImportOffender) {
	sort.SliceStable(o, func(i, j int) bool {
		if o[i].Bytes != o[j].Bytes {
			return o[i].Bytes > o[j].Bytes
		}
		return o[i].Path < o[j].Path
	})
}

// capOffenders truncates to maxImportOffenders.
func capOffenders(o []ImportOffender) []ImportOffender {
	if len(o) > maxImportOffenders {
		return o[:maxImportOffenders]
	}
	return o
}

package gitsync

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// GitignoreFile is the repository-root .gitignore Import seeds when the
// repository does not already have one.
const GitignoreFile = ".gitignore"

// DefaultGitignore is that seed: everything in it is written by Home
// Assistant, an add-on or HACS rather than by the user, and costs a diff
// every time the machine touches it. A seed, not policy - written once into
// a repository with no .gitignore and never rewritten, so deleting a line
// and re-importing starts managing that path. ExcludedPatterns is the other
// half of the split and refuses unconditionally: unsafe, not merely noisy.
const DefaultGitignore = `# Seeded by the GitOps Agent on the first import, and never rewritten.
# Everything below is written by Home Assistant, an add-on, or HACS rather
# than by you. Delete a line and import again to start managing it.

# Integrations, frontend cards and AppDaemon apps installed through HACS.
# HACS updates these in place, so tracking them means a several-thousand-file
# diff per update, and an apply would roll an update back to the committed
# copy.
custom_components/
www/community/

# zigbee2mqtt runtime state: the device database, the network state file and
# the coordinator backup are all rewritten by the add-on while it runs.
zigbee2mqtt/state.json
zigbee2mqtt/database.db*
zigbee2mqtt/coordinator_backup.json

# Node-RED credentials and per-instance settings.
node-red/flows_cred.json
node-red/.config.*.json

# AppDaemon compiles its dashboards into this directory at startup.
appdaemon/compiled/

# Editor and OS leftovers.
*.bak
.DS_Store
`

// ensureGitignore writes DefaultGitignore at the repository root when
// nothing is there yet, and reports whether it wrote. Create-only, unlike
// ensureSopsConfig: .gitignore is a starting point the user edits, and
// rewriting it would silently re-ignore what they un-ignored. Guarded and
// unlinked first, since a tracked .gitignore may be a symlink.
func (g *GitSync) ensureGitignore() (bool, error) {
	full, err := guardDriftPath(g.Workdir, GitignoreFile)
	if err != nil {
		return false, err
	}
	// Lstat, not Stat: a dangling symlink is still something at this path.
	if _, err := os.Lstat(full); err == nil {
		return false, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("checking %s: %w", GitignoreFile, err)
	}
	if err := os.WriteFile(full, []byte(DefaultGitignore), 0o644); err != nil { // #nosec G306 -- path rules only, nothing secret
		return false, fmt.Errorf("writing %s: %w", GitignoreFile, err)
	}
	return true, nil
}

// copyIgnoresFromLive copies the config's own .gitignore files into the
// worktree, at any depth, before anything is filtered: seeding an empty
// branch is the one import with none of them there yet, and ESPHome's alone
// excludes 898 generated C++ files on a real install.
//
// The live copy overwrites the repository's deliberately - the import
// copies it over anyway and the bulk "git add" applies that content, so
// filtering by anything else would disagree with what gets staged. A
// repo-only .gitignore is not in the scan and keeps governing on its own.
func (g *GitSync) copyIgnoresFromLive(ctx context.Context, files []string, configRoot string) error {
	for _, p := range gitignorePaths(files) {
		// Repeated from stageImport's own loop, because this pass runs
		// BEFORE it and writes into the worktree.
		if err := refuseUnsyncablePath(p); err != nil {
			return err
		}
		// The main loop's own copy path, so the two cannot disagree about
		// symlinks or encryption. Idempotent; the loop still does the
		// counting.
		if _, err := g.copyLiveIntoWorkdir(ctx, configRoot, p); err != nil {
			return err
		}
	}
	return nil
}

// gitignorePaths picks the ignore files out of a scanned file list; the
// import and the preview have to agree on what counts as one.
func gitignorePaths(files []string) []string {
	var out []string
	for _, p := range files {
		if path.Base(p) == GitignoreFile {
			out = append(out, p)
		}
	}
	return out
}

// checkIgnoreArgvBudget caps the bytes of pathname in one "git
// check-ignore" argv. By size, not file count, because the kernel limits
// bytes; 128 KiB is well under the smallest ARG_MAX this runs on and still
// fits a whole real config in one or two subprocesses.
const checkIgnoreArgvBudget = 128 << 10

// ignoredSet asks git which of files the ignore rules in dir match; an
// empty dir means the worktree. check-ignore says nothing about
// already-tracked paths, matching "git add", so a managed file stays
// managed even if a later .gitignore edit would have covered it.
func (g *GitSync) ignoredSet(ctx context.Context, dir string, files []string) (map[string]struct{}, error) {
	ignored := make(map[string]struct{}, len(files))
	for start := 0; start < len(files); {
		end, budget := start, checkIgnoreArgvBudget
		for end < len(files) && (end == start || budget >= len(files[end])) {
			budget -= len(files[end])
			end++
		}
		// "--" so a pathname is never parsed as an option. No "-z": git
		// rejects it here, hence the un-C-quoting below.
		args := append([]string{"check-ignore", "--"}, files[start:end]...)
		start = end
		result, err := g.runGitRaw(ctx, args, dir, nil, 0)
		if err != nil {
			return nil, err
		}
		switch result.ExitCode {
		case 0: // at least one path in this batch is ignored
		case 1: // none are, and there is nothing on stdout to read
			continue
		default:
			return nil, newCommandError("git check-ignore failed (exit %d): %s",
				result.ExitCode, g.redactCredentials(strings.TrimSpace(result.Stderr)))
		}
		for _, line := range strings.Split(result.Stdout, "\n") {
			if line = strings.TrimSuffix(line, "\r"); line != "" {
				ignored[unquoteGitPath(line)] = struct{}{}
			}
		}
	}
	return ignored, nil
}

// filterIgnored drops the paths the ignore rules in dir match. Import gets
// this at staging anyway, but only after every file is read, encrypted and
// written; asking up front also makes the imported-file tally count what
// was committed rather than what was copied.
func (g *GitSync) filterIgnored(ctx context.Context, dir string, files []string) ([]string, error) {
	if len(files) == 0 {
		return files, nil
	}
	ignored, err := g.ignoredSet(ctx, dir, files)
	if err != nil {
		return nil, err
	}
	if len(ignored) == 0 {
		return files, nil
	}
	// max(): the subtraction cannot go negative, but make() panics rather
	// than clamps if it ever did.
	kept := make([]string, 0, max(len(files)-len(ignored), 0))
	for _, p := range files {
		if _, skip := ignored[p]; !skip {
			kept = append(kept, p)
		}
	}
	return kept, nil
}

// unquoteGitPath undoes the C-style quoting git applies to a pathname
// holding a byte outside printable ASCII ("caf\303\251/x.yaml", newline,
// tab, quote, backslash); unquoted paths pass through. One that does not
// parse is returned as-is, so at worst a file is imported that should not
// be, rather than a different path being silently excluded.
func unquoteGitPath(s string) string {
	if len(s) < 2 || s[0] != '"' || s[len(s)-1] != '"' {
		return s
	}
	// Go string-literal escapes are a superset of git's here, octal byte
	// escapes included.
	if unquoted, err := strconv.Unquote(s); err == nil {
		return unquoted
	}
	return s
}

// PreviewIgnored returns the subset of files an import would commit - those
// no .gitignore matches - and their total size. .gitignore rather than
// ExcludedPatterns decides that: 5860 files scanned, 191 committed on the
// install this was built against.
//
// It runs against a throwaway repository, because writing the live
// .gitignore files into the worktree would make the next reconcile compare
// a live file against a copy of itself. Built in the import's order: the
// repository's own files, the live ones over them, DefaultGitignore only if
// that leaves the root bare.
func (g *GitSync) PreviewIgnored(ctx context.Context, configRoot string, files []string) (kept []string, keptBytes int64, err error) {
	tmp, err := os.MkdirTemp("", "gitops-preview-")
	if err != nil {
		return nil, 0, fmt.Errorf("preview: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	if _, err := g.runGitRaw(ctx, []string{"init", "-q"}, tmp, nil, 0); err != nil {
		return nil, 0, err
	}

	for _, p := range g.ignorePathsAtHead(ctx) {
		content, showErr := g.runGitRaw(ctx, []string{"show", "HEAD:" + p}, "", nil, 0)
		if showErr != nil || content.ExitCode != 0 {
			continue
		}
		if err := writeUnderRoot(tmp, p, []byte(content.Stdout)); err != nil {
			return nil, 0, err
		}
	}
	for _, p := range gitignorePaths(files) {
		live, readErr := os.ReadFile(filepath.Join(configRoot, filepath.FromSlash(p))) // #nosec G304 -- p comes from ScanLive, already confined under configRoot
		if readErr != nil {
			continue
		}
		if err := writeUnderRoot(tmp, p, live); err != nil {
			return nil, 0, err
		}
	}
	if _, statErr := os.Lstat(filepath.Join(tmp, GitignoreFile)); errors.Is(statErr, fs.ErrNotExist) {
		if err := writeUnderRoot(tmp, GitignoreFile, []byte(DefaultGitignore)); err != nil {
			return nil, 0, err
		}
	}

	kept, err = g.filterIgnored(ctx, tmp, files)
	if err != nil {
		return nil, 0, err
	}
	// Sizing the kept files rather than subtracting the ignored ones: 191
	// stat calls instead of 5669, from one source taken at one moment.
	for _, p := range kept {
		if info, statErr := os.Lstat(filepath.Join(configRoot, filepath.FromSlash(p))); statErr == nil {
			keptBytes += info.Size()
		}
	}
	return kept, keptBytes, nil
}

// ignorePathsAtHead lists the .gitignore files the repository itself
// tracks. Best effort: a repository with no commits yet - what a first
// import is for - has no HEAD, and no repository-side rules to honor.
func (g *GitSync) ignorePathsAtHead(ctx context.Context) []string {
	tracked, err := g.TrackedFilesRaw(ctx, "HEAD")
	if err != nil {
		return nil
	}
	return gitignorePaths(tracked)
}

// writeUnderRoot writes one .gitignore into the throwaway tree. rel comes
// from git's index or ScanLive, so it is already repo-relative and clean;
// the containment check keeps that true by construction.
func writeUnderRoot(root, rel string, content []byte) error {
	// filepath.IsLocal rejects absolute paths, ones climbing out with "..",
	// and the Windows reserved names, so nothing that survives it escapes
	// root once joined. root is a MkdirTemp path this call owns, holding
	// only plain directories, so no symlink can redirect a cleaned path.
	local := filepath.FromSlash(rel)
	if !filepath.IsLocal(local) {
		return fmt.Errorf("preview: refusing to write outside the preview tree: %s", rel)
	}
	full := filepath.Join(root, local)
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		return fmt.Errorf("preview: %w", err)
	}
	if err := os.WriteFile(full, content, 0o600); err != nil { // #nosec G703 -- rel is filepath.IsLocal, so full stays under the MkdirTemp root this function owns
		return fmt.Errorf("preview: %w", err)
	}
	return nil
}

// refuseUnsyncablePath is the import path's refusal for a scanned file that
// is excluded outright, or secret-shaped in a way encryption does not
// cover. Named so its three call sites cannot drift apart.
func refuseUnsyncablePath(p string) error {
	if Excluded(p) || secretShapedDisallowed(p) {
		return fmt.Errorf("refusing to touch excluded/secret-shaped path: %s", p)
	}
	return nil
}

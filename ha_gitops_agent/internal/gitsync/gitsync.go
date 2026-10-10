// Package gitsync keeps a clone of the config repository outside /config,
// which must never become a git working tree; differ and applier read the
// clone's checked-out tree to reconcile into /config.
package gitsync

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/execx"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/options"
)

// DefaultWorkdir is the local clone location GitSync uses by default.
// /data persists across restarts and upgrades (a Supervisor-managed volume).
const DefaultWorkdir = "/data/repo"

// DefaultNetworkTimeout bounds clone and fetch (see GitSync.NetworkTimeout).
// A full clone of a config repository with media under www/ over a slow
// uplink routinely outlasts DefaultGitTimeout, and a clone that is killed
// every time never completes at all.
const DefaultNetworkTimeout = 10 * time.Minute

// DefaultGitTimeout bounds every git subprocess call this package makes.
const DefaultGitTimeout = 60 * time.Second

// secretsFileNames are the files Home Assistant and its add-ons read
// secrets from: secrets.yaml (or .yml) for Home Assistant and ESPHome,
// secret.yaml (or .yml) for Zigbee2MQTT. Something other than this agent
// renders them, so they are never synced in either direction, at any
// depth, and a tracked one is a hard stop (they are SecretPatterns too).
var secretsFileNames = []string{"secrets.yaml", "secrets.yml", "secret.yaml", "secret.yml"}

// IsSecretsFile reports whether p's basename is one of secretsFileNames,
// ignoring case. Excluded consults it on top of the exact entries below,
// so "Secrets.YAML" is never synced either.
func IsSecretsFile(p string) bool {
	base := path.Base(strings.ReplaceAll(p, `\`, "/"))
	for _, name := range secretsFileNames {
		if strings.EqualFold(base, name) {
			return true
		}
	}
	return false
}

// ExcludedPatterns are paths never synced in either direction, by differ or
// applier, and never deleted. The entry syntax is documented on Excluded.
// The exclude_paths option adds to them (SetUserExclusions).
var ExcludedPatterns = []string{
	".storage/",
	".cloud/",
	"secrets.yaml",
	"secrets.yml",
	"secret.yaml",
	"secret.yml",
	// The sops config versions before 0.9.0 maintained: repository-side
	// tooling, never written into /homeassistant.
	".sops.yaml",
	"*.db",
	// SQLite's sidecars (.db-wal, .db-shm) and anything else suffixing a
	// database file: "*.db-*" alone missed "zigbee2mqtt/database.db.backup".
	"*.db-*",
	"*.db.*",
	"*.log",
	"*.log.*",
	".ssh/",
	"deps/",
	"backups/",
	"tts/",
	".git/",
	// Python bytecode: regenerated on every restart and HACS update, 595 of
	// them on the install this was found on.
	"__pycache__/",
	"*.pyc",
	"*.pyo",
	// Home Assistant's own scratch space (currently the pip wheel cache).
	".cache/",
	// Machine-written identity and run state: .HA_VERSION changes per core
	// update, .uuid is per-install, .ha_run.lock is process-lifetime.
	".HA_VERSION",
	".uuid",
	".ha_run.lock",
	// Written by Home Assistant, not the user: ip_bans.yaml grows an entry
	// per failed login, known_devices.yaml (and its .bak) per device seen.
	"ip_bans.yaml",
	"known_devices.yaml*",
	// ESPHome build caches and Device Builder runtime state, which includes
	// binary key material (.device-builder-peer-link-key.bin).
	".esphome/",
	".device-builder*",
	// Registry manifests (see the registries package): agent INPUT, never
	// copied into /homeassistant. Root-anchored so a nested, unrelated
	// gitops/ elsewhere in the repo still syncs.
	"/gitops/",
	// Home Assistant's uploaded-image store. Root-anchored because
	// "www/image/" is just as likely to be a user's own folder of pictures.
	"/image/",
}

// exclusions is a list of entries with each one's kind decided once rather
// than re-derived per call: Excluded runs per file per scan, upwards of
// 7000 times on a real config.
type exclusions struct {
	dirs      map[string]struct{} // "dir/"  - any segment
	dirGlobs  []string            // "dir*/" - any segment, globbed
	rootDirs  map[string]struct{} // "/dir/" - first segment only
	pathDirs  []pathDir           // "a/b/", "/a*/" - a directory at that path from the root
	exact     map[string]struct{} // plain entries: the basename or the full path
	globs     []string            // "*.db" - the basename, globbed
	pathGlobs []string            // "a/meter-*", "/notes.yaml" - the full path, globbed
}

// pathDir is one root-relative directory entry and how many segments it
// spans, so a path is matched on exactly that many leading segments.
type pathDir struct {
	pattern  string
	segments int
}

var excluded = compileExclusions(ExcludedPatterns)

// userExcluded is the exclude_paths option compiled, nil when it is empty.
// Set once in main before any goroutine starts; atomic so a test can swap
// it under -race.
var userExcluded atomic.Pointer[exclusions]

func compileExclusions(entries []string) exclusions {
	m := exclusions{
		dirs:     make(map[string]struct{}, len(entries)),
		rootDirs: make(map[string]struct{}, len(entries)),
		exact:    make(map[string]struct{}, len(entries)),
	}
	for _, e := range entries {
		anchored := strings.HasPrefix(e, "/")
		dir := strings.HasSuffix(e, "/")
		body := strings.Trim(e, "/")
		nested := strings.Contains(body, "/")
		glob := strings.ContainsAny(body, "*?[")
		switch {
		case dir && !nested && !anchored && !glob:
			m.dirs[body] = struct{}{}
		case dir && !nested && !anchored:
			m.dirGlobs = append(m.dirGlobs, body)
		case dir && !nested && !glob:
			m.rootDirs[body] = struct{}{}
		case dir:
			m.pathDirs = append(m.pathDirs, pathDir{pattern: body, segments: strings.Count(body, "/") + 1})
		case !anchored && !nested && !glob:
			m.exact[body] = struct{}{}
		case !anchored && !nested:
			m.globs = append(m.globs, body)
		default:
			m.pathGlobs = append(m.pathGlobs, body)
		}
	}
	return m
}

// match reports whether normalized, a cleaned forward-slash path, hits any
// entry in m.
func (m *exclusions) match(normalized string) bool {
	// IndexByte rather than strings.Split: the slice Split returns was the
	// only allocation left on this per-file path.
	first, basename := normalized, normalized
	if i := strings.IndexByte(normalized, '/'); i >= 0 {
		first = normalized[:i]
		basename = normalized[strings.LastIndexByte(normalized, '/')+1:]
	}
	if _, ok := m.rootDirs[first]; ok {
		return true
	}
	for rest := normalized; rest != ""; {
		seg := rest
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			seg, rest = rest[:i], rest[i+1:]
		} else {
			rest = ""
		}
		if _, ok := m.dirs[seg]; ok {
			return true
		}
		for _, entry := range m.dirGlobs {
			if ok, _ := path.Match(entry, seg); ok {
				return true
			}
		}
	}
	for _, d := range m.pathDirs {
		if lead, ok := leadingSegments(normalized, d.segments); ok {
			if hit, _ := path.Match(d.pattern, lead); hit {
				return true
			}
		}
	}
	if _, ok := m.exact[basename]; ok {
		return true
	}
	if _, ok := m.exact[normalized]; ok {
		return true
	}
	for _, entry := range m.globs {
		if ok, _ := path.Match(entry, basename); ok {
			return true
		}
	}
	for _, entry := range m.pathGlobs {
		if ok, _ := path.Match(entry, normalized); ok {
			return true
		}
	}
	return false
}

// leadingSegments returns the first n segments of p, or false when p has
// fewer.
func leadingSegments(p string, n int) (string, bool) {
	end := 0
	for k := 0; k < n; k++ {
		if k > 0 {
			end++ // past the '/' the previous round stopped at
		}
		j := strings.IndexByte(p[end:], '/')
		if j < 0 {
			if k == n-1 {
				return p, true
			}
			return "", false
		}
		end += j
	}
	return p[:end], true
}

// SecretPatterns are filename patterns that make GuardSecretsAt refuse to
// sync at all. Stricter than ExcludedPatterns, which only skips a path: a
// secret-shaped file being tracked in git is itself the problem.
var SecretPatterns = []string{
	"secrets.yaml",
	// Both spellings, since HA accepts both. Without this, an import pushes
	// a live secrets.yml in the clear.
	"secrets.yml",
	// Zigbee2MQTT's secrets file, in both spellings.
	"secret.yaml",
	"secret.yml",
	"*.pem",
	"*.key",
	"id_rsa*",
	"id_ed25519*",
	// Glob, not the literal ".env": ".env.local" and ".env.production" were
	// sailing past, and Import made this a filter on what gets pushed.
	".env*",
}

// Excluded reports whether p, a repo/config-relative path (forward- or
// backslash separated), matches an ExcludedPatterns or exclude_paths
// entry, or is a secrets file (IsSecretsFile). Entry syntax:
//
//   - "dir/" matches any segment, basename included, at any depth.
//   - "/dir/" matches only a path whose FIRST segment is that name.
//   - "a/b/" (a slash inside) is the directory at that path from the root,
//     and everything under it.
//   - "*.db" globs the basename only.
//   - A plain entry matches the basename or the full path exactly.
//   - "a/meter-*" or "/notes.yaml" (a slash inside, or a leading one)
//     matches the full path from the root, globbed.
//
// "*", "?" and "[...]" glob within one segment, as path.Match does. The
// built-in entries use only the first, second, fourth and fifth forms.
func Excluded(p string) bool {
	normalized := strings.TrimLeft(strings.ReplaceAll(p, "\\", "/"), "/")
	// Clean first, or "sub/../gitops/foo.yaml" dodges the root-anchored
	// gitops/ guard. A path still climbing above the root afterwards is
	// excluded outright (fail closed).
	normalized = path.Clean(normalized)
	if normalized == ".." || strings.HasPrefix(normalized, "../") {
		return true
	}
	if IsSecretsFile(normalized) || excluded.match(normalized) {
		return true
	}
	if user := userExcluded.Load(); user != nil && user.match(normalized) {
		return true
	}
	return false
}

// UserExcluded reports whether p matches an exclude_paths entry alone. The
// stash a rollback restores from predates the option, so RollbackFrom asks
// this to keep its hands off what the user has since declared someone
// else's.
func UserExcluded(p string) bool {
	user := userExcluded.Load()
	if user == nil {
		return false
	}
	normalized := path.Clean(strings.TrimLeft(strings.ReplaceAll(p, "\\", "/"), "/"))
	return user.match(normalized)
}

// Bounds on the exclude_paths option, generous for a list typed by hand.
const (
	maxExcludeEntries   = 100
	maxExcludeEntryLen  = 256
	excludeEntryExample = "wmbusmeters/etc/wmbusmeters.d/"
)

// SetUserExclusions validates the exclude_paths option and installs it
// beside ExcludedPatterns. The error names the first bad entry and why;
// main treats it as fatal, since syncing with an exclusion silently
// dropped would write or delete exactly what the user asked to protect.
func SetUserExclusions(entries []string) error {
	if len(entries) > maxExcludeEntries {
		return fmt.Errorf("exclude_paths: %d entries, at most %d are allowed", len(entries), maxExcludeEntries)
	}
	for i, e := range entries {
		if err := validateExcludeEntry(e); err != nil {
			return fmt.Errorf("exclude_paths entry %d (%q): %w", i+1, e, err)
		}
	}
	if len(entries) == 0 {
		userExcluded.Store(nil)
		return nil
	}
	compiled := compileExclusions(entries)
	userExcluded.Store(&compiled)
	return nil
}

// validateExcludeEntry checks one exclude_paths entry against the syntax
// Excluded documents.
func validateExcludeEntry(e string) error {
	switch {
	case strings.TrimSpace(e) == "":
		return errors.New("is empty")
	case strings.TrimSpace(e) != e:
		return errors.New("has leading or trailing spaces")
	case len(e) > maxExcludeEntryLen:
		return fmt.Errorf("is longer than %d characters", maxExcludeEntryLen)
	case strings.ContainsRune(e, '\\'):
		return errors.New("uses a backslash; separate directories with /")
	case strings.Contains(e, "**"):
		return fmt.Errorf("uses **, which is not supported; a trailing / covers a whole directory, e.g. %s", excludeEntryExample)
	}
	for _, r := range e {
		if r < ' ' || r == 0x7f {
			return errors.New("contains a control character")
		}
	}
	body := strings.Trim(e, "/")
	if body == "" {
		return errors.New("names the whole config directory")
	}
	for _, seg := range strings.Split(body, "/") {
		switch seg {
		case "":
			return errors.New("has an empty path segment (//)")
		case ".", "..":
			return fmt.Errorf("has a %q segment; write the path from the config directory, e.g. %s", seg, excludeEntryExample)
		}
	}
	if _, err := path.Match(body, ""); err != nil {
		return errors.New("is not a valid pattern (an unclosed [ ?)")
	}
	return nil
}

// matchesSecretPattern checks p against SecretPatterns case-insensitively,
// against both the basename and the full relative path. No path.Clean and
// no backslash conversion, unlike Excluded.
func matchesSecretPattern(p string) bool {
	normalized := strings.ToLower(strings.TrimLeft(p, "/"))
	basename := normalized
	if idx := strings.LastIndex(normalized, "/"); idx >= 0 {
		basename = normalized[idx+1:]
	}
	for _, pattern := range SecretPatterns {
		pattern = strings.ToLower(pattern)
		if ok, _ := path.Match(pattern, basename); ok {
			return true
		}
		if ok, _ := path.Match(pattern, normalized); ok {
			return true
		}
	}
	return false
}

// SecretsTrackedError is returned by GuardSecretsAt when tracked files match
// SecretPatterns. A hard stop: no clone, fetch or apply until the offender
// is out of the tracked tree.
type SecretsTrackedError struct {
	// Files is the sorted, offending subset of GuardSecretsAt's input.
	Files []string
}

// Error is just the file list: callers (recon.ReconcileNow) supply their own
// "refusing to sync: secrets tracked in repository: " prefix.
func (e *SecretsTrackedError) Error() string {
	return strings.Join(e.Files, ", ")
}

// CommandError is returned when a git subprocess fails. Message is always
// pre-redacted of the configured git token (see redactCredentials), so it
// is safe to log or show in the status UI as-is.
type CommandError struct {
	Message string
}

func (e *CommandError) Error() string { return e.Message }

func newCommandError(format string, args ...any) *CommandError {
	return &CommandError{Message: fmt.Sprintf(format, args...)}
}

// ErrRemoteBranchMissing reports that opts.Branch is not on the remote: an
// unseeded repository or a mistyped branch, not a failure. Fetch wraps it
// with the branch name; recon matches it with errors.Is.
var ErrRemoteBranchMissing = errors.New("the tracked branch does not exist on the remote yet")

// redactCredentials strips every secret this configuration can put in
// front of git - the raw token, the base64 "user:token" blob some git/curl
// failure paths echo back verbatim, and a password embedded in repo_url's
// userinfo, which git quotes back in its own errors - from all git output
// before it is logged or turned into a CommandError.
func (g *GitSync) redactCredentials(text string) string {
	text = execx.Redact(text, g.Opts.GitToken)
	text = execx.Redact(text, g.basicAuthBlob())
	return execx.Redact(text, g.repoURLPassword())
}

// repoURLPassword is the password in Opts.RepoURL's userinfo, or "".
// credentialEnv never sends it anywhere, but the URL itself goes on git's
// command line, and git repeats it in errors.
func (g *GitSync) repoURLPassword() string {
	parsed, err := url.Parse(g.Opts.RepoURL)
	if err != nil || parsed.User == nil {
		return ""
	}
	password, _ := parsed.User.Password()
	return password
}

// basicAuthBlob returns the base64 "user:token" blob for the Basic
// Authorization header, or "" if no token is configured. Shared by
// credentialEnv, which sends it, and redactCredentials, which scrubs it.
func (g *GitSync) basicAuthBlob() string {
	if g.Opts.GitToken == "" {
		return ""
	}
	username := g.Opts.GitUsername
	if username == "" {
		username = "x-access-token"
	}
	return base64.StdEncoding.EncodeToString([]byte(username + ":" + g.Opts.GitToken))
}

// GitSync owns the local clone of the config repository. Opts is the
// add-on's current options (RepoURL, Branch, GitUsername, GitToken);
// Workdir is what it clones into and checks out from. Not safe for
// concurrent use.
type GitSync struct {
	Opts    options.Options
	Workdir string

	// Runner executes every git subprocess. Tests inject a fake to inspect
	// the exact argv/env a call used without invoking a real git binary.
	Runner Runner

	// Timeout bounds every git subprocess call. Defaults to
	// DefaultGitTimeout when zero.
	Timeout time.Duration

	// NetworkTimeout bounds clone and fetch instead, whose cost is a
	// transfer - the whole history, for a first clone - rather than local
	// work. New sets DefaultNetworkTimeout; zero falls back to Timeout.
	NetworkTimeout time.Duration

	// gitConfigGlobal is a per-workdir git config file, so core.autocrlf
	// and safe.directory never touch the real ~/.gitconfig.
	gitConfigGlobal string
}

// New builds a GitSync for opts, cloning into (and checking out from)
// workdir.
func New(opts options.Options, workdir string) *GitSync {
	return &GitSync{
		Opts:            opts,
		Workdir:         workdir,
		Runner:          execx.CommandRunner{},
		Timeout:         DefaultGitTimeout,
		NetworkTimeout:  DefaultNetworkTimeout,
		gitConfigGlobal: strings.TrimRight(absPath(workdir), "/") + ".gitconfig",
	}
}

// EnsureClone clones opts.RepoURL into Workdir unless it is already a clone
// of it; a clone whose origin no longer matches is wiped and re-cloned.
// The clone URL stays credential-free, so origin never carries a token and
// nothing secret is written under Workdir/.git; auth reaches git through
// the process environment of that one call (see credentialEnv).
func (g *GitSync) EnsureClone(ctx context.Context) error {
	if err := g.guardReadArgs(); err != nil {
		return err
	}
	gitDir := filepath.Join(g.Workdir, ".git")
	if info, err := os.Stat(gitDir); err == nil && info.IsDir() {
		origin, _ := g.currentOrigin(ctx)
		if origin == g.Opts.RepoURL {
			return nil
		}
		slog.Info("gitsync: origin changed, re-cloning",
			"from", execx.RedactURL(origin), "to", execx.RedactURL(g.Opts.RepoURL), "workdir", g.Workdir)
		if err := os.RemoveAll(g.Workdir); err != nil {
			return fmt.Errorf("gitsync: removing old clone: %w", err)
		}
	}

	parent := filepath.Dir(absPath(g.Workdir))
	if err := os.MkdirAll(parent, 0o750); err != nil {
		return fmt.Errorf("gitsync: creating parent dir: %w", err)
	}

	// Cloned beside Workdir and renamed into place only once complete. A
	// clone killed partway - its timeout SIGKILLs the process group, so git
	// cannot clean up, and neither can a container stop - used to leave a
	// .git with the right origin and no history, which the check above
	// accepted as a finished clone from then on.
	partial := absPath(g.Workdir) + ".partial"
	if err := os.RemoveAll(partial); err != nil {
		return fmt.Errorf("gitsync: removing an unfinished clone: %w", err)
	}
	if _, err := g.runGitWith(ctx, []string{"clone", "--no-checkout", "--origin", "origin", g.Opts.RepoURL, partial}, parent, g.networkEnv(), g.NetworkTimeout); err != nil {
		_ = os.RemoveAll(partial)
		return err
	}
	if _, err := g.runGit(ctx, []string{"config", "core.autocrlf", "false"}, partial, nil); err != nil {
		_ = os.RemoveAll(partial)
		return err
	}
	// Whatever is left at Workdir has no usable .git (see above).
	if err := os.RemoveAll(g.Workdir); err != nil {
		return fmt.Errorf("gitsync: removing an unusable clone: %w", err)
	}
	if err := os.Rename(partial, g.Workdir); err != nil {
		_ = os.RemoveAll(partial)
		return fmt.Errorf("gitsync: moving the finished clone into place: %w", err)
	}
	if _, err := g.runGit(ctx, []string{"config", "--global", "--add", "safe.directory", absPath(g.Workdir)}, g.Workdir, nil); err != nil {
		return err
	}
	slog.Info("gitsync: cloned", "repo_url", execx.RedactURL(g.Opts.RepoURL), "workdir", g.Workdir)
	return nil
}

// Fetch fetches opts.Branch and returns its SHA. Fast-forward only, and
// from opts.RepoURL directly rather than the named origin, so a changed
// RepoURL takes effect without re-running EnsureClone. Credentials ride in
// the environment of this one call (GIT_CONFIG_COUNT/KEY/VALUE), never in
// argv - readable via /proc - and never written to origin or any config
// file on disk.
func (g *GitSync) Fetch(ctx context.Context) (string, error) {
	if err := g.guardReadArgs(); err != nil {
		return "", err
	}
	if err := g.fetchWithRetry(ctx); err != nil {
		// Exit 128 covers a missing branch and every auth or network failure
		// alike, so an exit code decides rather than git's prose. Only a
		// definite absence becomes the sentinel: a probe that itself fails,
		// or that finds the branch, leaves the fetch's own error alone. The
		// probe runs only here, so the happy path still costs two calls.
		if has, probeErr := g.RemoteHasBranch(ctx); probeErr == nil && !has {
			return "", fmt.Errorf("%w: %s", ErrRemoteBranchMissing, g.Opts.Branch)
		}
		return "", err
	}
	result, err := g.runGit(ctx, []string{"rev-parse", "FETCH_HEAD"}, g.Workdir, nil)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(result.Stdout), nil
}

// fetchRetryDelays are the waits before each retry of a fetch that failed
// transiently; one retry per entry. A var so tests can shorten them.
var fetchRetryDelays = []time.Duration{2 * time.Second, 8 * time.Second}

// fetchWithRetry is the fetch itself, retried on an error that says the
// forge or the network hiccuped rather than that anything is wrong: a
// forge restarting behind its proxy answers 502 for a few seconds, and
// every such blip used to put the agent in the error state and drop the
// plan waiting for review until the next interval.
func (g *GitSync) fetchWithRetry(ctx context.Context) error {
	args := []string{"fetch", "--quiet", g.Opts.RepoURL, g.Opts.Branch}
	for attempt := 0; ; attempt++ {
		_, err := g.runGitWith(ctx, args, g.Workdir, g.networkEnv(), g.NetworkTimeout)
		if err == nil || attempt >= len(fetchRetryDelays) || !IsTransientFetchError(err) {
			return err
		}
		slog.Info("gitsync: fetch failed transiently, retrying", "attempt", attempt+1, "error", err)
		select {
		case <-ctx.Done():
			return err
		case <-time.After(fetchRetryDelays[attempt]):
		}
	}
}

// transientFetchMarkers are the git and curl messages of a failure worth
// retrying. Matched on git's own text, which LC_ALL=C keeps stable. Our
// own "timed out after" is deliberately absent: a fetch that ran out its
// whole budget would not finish any faster the second time.
var transientFetchMarkers = []string{
	"The requested URL returned error: 5",
	"RPC failed; HTTP 5",
	"Could not resolve host",
	"Temporary failure in name resolution",
	"Failed to connect",
	"Connection refused",
	"Connection reset",
	"Connection timed out",
	"Operation timed out",
	"Operation too slow",
	"early EOF",
	"unexpected disconnect",
}

// IsTransientFetchError reports whether err reads as the forge or the
// network being briefly unavailable rather than anything being wrong with
// the repository, the credentials or the request. Exported for recon,
// which rides out a short outage instead of reporting it as an error.
func IsTransientFetchError(err error) bool {
	msg := err.Error()
	for _, marker := range transientFetchMarkers {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// RemoteHasBranch reports whether opts.Branch exists on the remote. Exit 2
// alone means "no such branch"; every other non-zero (128 for an
// unreachable host or an expired token) is an error, or an auth failure
// would read as an empty repository.
func (g *GitSync) RemoteHasBranch(ctx context.Context) (bool, error) {
	result, err := g.runGitRaw(ctx, []string{"ls-remote", "--exit-code", "--heads", g.Opts.RepoURL, g.Opts.Branch}, "", g.credentialEnv(), 0)
	if err != nil {
		return false, err
	}
	switch result.ExitCode {
	case 0:
		return true, nil
	case 2:
		return false, nil
	}
	reason := g.redactCredentials(strings.TrimSpace(result.Stderr))
	if reason == "" {
		reason = g.redactCredentials(strings.TrimSpace(result.Stdout))
	}
	return false, newCommandError("git ls-remote failed (exit %d): %s", result.ExitCode, reason)
}

// networkEnv is credentialEnv plus a stall limit for the two transfers that
// get NetworkTimeout. That budget is sized for a slow but MOVING transfer;
// without a low-speed limit git waits out all of it on a forge that accepts
// the connection and then sends nothing, holding the operation lock - and
// with it Apply and Roll Back - for the whole ten minutes. Under 1 KiB/s
// for 30 seconds aborts instead, and reads as transient (see
// transientFetchMarkers).
func (g *GitSync) networkEnv() []string {
	return append(g.credentialEnv(), "GIT_HTTP_LOW_SPEED_LIMIT=1024", "GIT_HTTP_LOW_SPEED_TIME=30")
}

// credentialEnv returns the extra environment Fetch adds to authenticate as
// opts.GitUsername/opts.GitToken, or nil if no token is configured.
func (g *GitSync) credentialEnv() []string {
	basicAuth := g.basicAuthBlob()
	if basicAuth == "" {
		return nil
	}
	return []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.extraheader",
		"GIT_CONFIG_VALUE_0=Authorization: Basic " + basicAuth,
	}
}

// remoteHelperURL matches git's "<transport>::<address>" remote-helper
// syntax, whose ext:: transport runs an arbitrary command.
var remoteHelperURL = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9+.-]*::`)

// guardReadArgs validates the two option values that ride git's command
// line on the READ path - clone and fetch. guardWriteBranch already covers
// the push paths, and its own doc comment names this exact hole: Fetch
// takes the branch as a trailing positional, where a leading "-" parses as
// an option, and the repo URL rides argv the same way. An "ext::" URL is
// command execution outright. Both values are operator-set options, so
// this is a guard rail rather than a trust boundary - but the check is two
// string tests, and the write path already refuses the same shapes.
func (g *GitSync) guardReadArgs() error {
	if strings.HasPrefix(g.Opts.RepoURL, "-") {
		return fmt.Errorf("gitsync: refusing a repo_url starting with '-'")
	}
	if remoteHelperURL.MatchString(g.Opts.RepoURL) {
		return fmt.Errorf("gitsync: refusing the git remote-helper URL syntax in repo_url (%q transport)",
			strings.SplitN(g.Opts.RepoURL, "::", 2)[0])
	}
	if strings.HasPrefix(g.Opts.Branch, "-") {
		return fmt.Errorf("gitsync: refusing a branch name starting with '-': %s", g.Opts.Branch)
	}
	return nil
}

// guardWriteBranch validates opts.Branch as a PUSH TARGET (Import,
// RecordFile); op doubles as the verb in the first refusal. check-ref-format
// catches ":" and friends by exit code alone but exits 0 on
// "refs/heads/--mirror", so a leading "-" is refused separately: Fetch takes
// the branch as a trailing positional, where git parses it as an option.
func (g *GitSync) guardWriteBranch(ctx context.Context, op string) error {
	if g.Opts.Branch == "" {
		return fmt.Errorf("gitsync: %s: no branch configured to %s onto", op, op)
	}
	if strings.HasPrefix(g.Opts.Branch, "-") {
		return fmt.Errorf("gitsync: %s: refusing to use a branch name starting with '-': %s", op, g.Opts.Branch)
	}
	ok, err := g.runGitStatus(ctx, []string{"check-ref-format", "refs/heads/" + g.Opts.Branch}, "", nil)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("gitsync: %s: refusing to push to malformed branch name: %s", op, g.Opts.Branch)
	}
	return nil
}

// Checkout does a forced detached checkout of sha in Workdir, then git
// clean -fdx, so TrackedFiles and on-disk content agree.
func (g *GitSync) Checkout(ctx context.Context, sha string) error {
	if _, err := g.runGit(ctx, []string{"checkout", "--detach", "--force", sha}, g.Workdir, nil); err != nil {
		return err
	}
	if _, err := g.runGit(ctx, []string{"clean", "-fdx"}, g.Workdir, nil); err != nil {
		return err
	}
	return nil
}

// CurrentSHA returns the SHA checked out in Workdir, or "" if there is no
// clone yet or HEAD does not resolve (e.g. after a --no-checkout clone).
func (g *GitSync) CurrentSHA(ctx context.Context) string {
	info, err := os.Stat(filepath.Join(g.Workdir, ".git"))
	if err != nil || !info.IsDir() {
		return ""
	}
	result, err := g.runGit(ctx, []string{"rev-parse", "--verify", "-q", "HEAD"}, g.Workdir, nil)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(result.Stdout)
}

// TrackedFilesRaw returns every repo-relative path git tracks at sha,
// unfiltered - the only view safe to pass to GuardSecretsAt, since
// TrackedFiles has already dropped secrets.yaml and .ssh/ as excluded.
func (g *GitSync) TrackedFilesRaw(ctx context.Context, sha string) ([]string, error) {
	// -z, so paths arrive NUL-separated and verbatim. Without it git
	// C-quotes any non-ASCII path ("caf\303\251/secrets.yaml") and both
	// IsSecretsFile and Excluded stop matching it.
	result, err := g.runGit(ctx, []string{"ls-tree", "-r", "-z", "--name-only", sha}, g.Workdir, nil)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, line := range strings.Split(result.Stdout, "\x00") {
		if line != "" {
			files = append(files, line)
		}
	}
	return files, nil
}

// TrackedFiles is TrackedFilesRaw with ExcludedPatterns hits filtered out,
// so downstream diff/apply packages never see them.
func (g *GitSync) TrackedFiles(ctx context.Context, sha string) ([]string, error) {
	raw, err := g.TrackedFilesRaw(ctx, sha)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, p := range raw {
		if !Excluded(p) {
			files = append(files, p)
		}
	}
	return files, nil
}

// GuardSecretsAt errors if the tree at sha must not be synced at all - call
// it with TrackedFilesRaw's output before any checkout or diff:
//
//   - a tracked path matching SecretPatterns is a *SecretsTrackedError;
//   - a tracked file carrying SOPS metadata is a *SopsTrackedError, since
//     this version no longer decrypts and would otherwise apply ciphertext
//     as config (see sops.go).
//
// Both at once come back joined (errors.Join), each still reachable with
// errors.As. A secrets file that is also SOPS-encrypted is reported as
// SOPS only, since that error says how to get it out. Blobs are read
// through the object database, so nothing reaches disk first.
func (g *GitSync) GuardSecretsAt(ctx context.Context, sha string, files []string) error {
	sopsFiles, err := g.sopsFilesAt(ctx, sha, files)
	if err != nil {
		return err
	}
	isSops := make(map[string]bool, len(sopsFiles))
	for _, f := range sopsFiles {
		isSops[f] = true
	}
	var offenders []string
	for _, f := range files {
		if matchesSecretPattern(f) && !isSops[f] {
			offenders = append(offenders, f)
		}
	}
	var errs []error
	if len(offenders) > 0 {
		sort.Strings(offenders)
		errs = append(errs, &SecretsTrackedError{Files: offenders})
	}
	if len(sopsFiles) > 0 {
		errs = append(errs, &SopsTrackedError{Files: sopsFiles})
	}
	switch len(errs) {
	case 0:
		return nil
	case 1:
		return errs[0]
	}
	return errors.Join(errs...)
}

// currentOrigin returns the URL configured for the origin remote, or "" if
// it cannot be determined (no remote, corrupt repo).
func (g *GitSync) currentOrigin(ctx context.Context) (string, error) {
	result, err := g.runGit(ctx, []string{"remote", "get-url", "origin"}, g.Workdir, nil)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(result.Stdout), nil
}

// gitEnv builds one git call's environment: the process environment minus
// inherited GIT_TRACE*/GIT_CURL_VERBOSE (stripDebugEnv), with
// GIT_CONFIG_GLOBAL isolated per workdir, GIT_TERMINAL_PROMPT=0 so a bad
// credential fails fast instead of hanging, plus any call-specific extras.
func (g *GitSync) gitEnv(extra []string) []string {
	env := stripDebugEnv(os.Environ())
	env = setEnv(env, "GIT_CONFIG_GLOBAL", g.gitConfigGlobal)
	env = setEnv(env, "GIT_TERMINAL_PROMPT", "0")
	// A fixed English locale, because this package and commitback.go match
	// substrings out of git's own output, which any other LC_ALL/LANG (an
	// observed condition, not a theoretical one) translates away.
	env = setEnv(env, "LC_ALL", "C")
	env = setEnv(env, "LANGUAGE", "")
	for _, kv := range extra {
		key, value, _ := strings.Cut(kv, "=")
		env = setEnv(env, key, value)
	}
	return env
}

// stripDebugEnv returns env with every GIT_TRACE* and GIT_CURL_VERBOSE
// entry removed: either one makes git dump the Authorization header
// carrying our credential to stderr, past anything redactCredentials could
// strip afterward. Stripped before this package adds its own entries.
func stripDebugEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		if key == "GIT_CURL_VERBOSE" || strings.HasPrefix(key, "GIT_TRACE") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// setEnv returns env with key set to value, replacing any existing entry.
// A plain append is unsafe: on POSIX the FIRST occurrence of a key wins.
func setEnv(env []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			continue
		}
		out = append(out, kv)
	}
	return append(out, key+"="+value)
}

// runGit runs "git args..." (never through a shell), raising a
// *CommandError with the token redacted on a non-zero exit or a timeout.
// dir defaults to Workdir; extraEnv is merged on top of gitEnv's defaults.
func (g *GitSync) runGit(ctx context.Context, args []string, dir string, extraEnv []string) (RunResult, error) {
	return g.runGitWith(ctx, args, dir, extraEnv, 0)
}

// runGitWith is runGit with an explicit timeout, for the calls whose cost
// scales with the config tree rather than with anything this package
// controls - failing at 60s AFTER staging a whole import is the worst
// outcome. timeout <= 0 means the normal budget.
func (g *GitSync) runGitWith(ctx context.Context, args []string, dir string, extraEnv []string, timeout time.Duration) (RunResult, error) {
	result, err := g.runGitRaw(ctx, args, dir, extraEnv, timeout)
	if err != nil {
		return RunResult{}, err
	}
	if result.ExitCode != 0 {
		// "git commit" with nothing staged explains itself on STDOUT, not
		// stderr; the fallback fires only when stderr is empty, so no other
		// call site's message changes.
		reason := g.redactCredentials(strings.TrimSpace(result.Stderr))
		if reason == "" {
			reason = g.redactCredentials(strings.TrimSpace(result.Stdout))
		}
		return RunResult{}, newCommandError("git %s failed (exit %d): %s", args[0], result.ExitCode, reason)
	}
	return result, nil
}

// runGitRaw hands back git's own result, treating a non-zero exit as data:
// only a launch failure or a timeout comes back as an error. runGitWith and
// runGitStatus are thin wrappers over it.
func (g *GitSync) runGitRaw(ctx context.Context, args []string, dir string, extraEnv []string, timeout time.Duration) (RunResult, error) {
	if dir == "" {
		dir = g.Workdir
	}
	if timeout <= 0 {
		timeout = g.Timeout
	}
	if timeout <= 0 {
		timeout = DefaultGitTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	env := g.gitEnv(extraEnv)
	fullArgs := append([]string{"git"}, args...)
	result, err := g.Runner.Run(runCtx, dir, env, fullArgs...)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return RunResult{}, newCommandError("git %s timed out after %s", args[0], timeout)
		}
		return RunResult{}, newCommandError("git %s failed to run: %s", args[0], g.redactCredentials(err.Error()))
	}
	return result, nil
}

// runGitStatus runs a git command whose non-zero exit is an ANSWER and
// reports whether it exited zero: "ls-remote --exit-code" (2 = no such
// branch on the remote) and "diff --cached --quiet" (1 = something is
// staged), read as exit codes so neither depends on git's prose.
func (g *GitSync) runGitStatus(ctx context.Context, args []string, dir string, extraEnv []string) (bool, error) {
	result, err := g.runGitRaw(ctx, args, dir, extraEnv, 0)
	if err != nil {
		return false, err
	}
	return result.ExitCode == 0, nil
}

// absPath returns p as an absolute path, falling back to p unchanged if
// filepath.Abs fails - every call site treats this as best-effort hygiene.
func absPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return abs
}

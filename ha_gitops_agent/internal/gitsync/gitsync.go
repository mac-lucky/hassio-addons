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
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/execx"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/options"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/sopscrypt"
)

// DefaultWorkdir is the local clone location GitSync uses by default.
// /data persists across restarts and upgrades (a Supervisor-managed volume).
const DefaultWorkdir = "/data/repo"

// DefaultGitTimeout bounds every git subprocess call this package makes.
const DefaultGitTimeout = 60 * time.Second

// encryptionOn is the process-wide SOPS+age switch: see SetEncryptionEnabled.
var encryptionOn atomic.Bool

// SetEncryptionEnabled turns SOPS+age handling on or off package-wide -
// Excluded, secretShapedDisallowed and the import scan take no *GitSync.
// Set once in main, before any goroutine starts.
func SetEncryptionEnabled(on bool) { encryptionOn.Store(on) }

// EncryptionEnabled reports whether SOPS+age handling is on.
func EncryptionEnabled() bool { return encryptionOn.Load() }

// secretsFileEntry is the one ExcludedPatterns entry encryption switches
// off: encrypted before it enters the worktree, secrets.yaml is syncable.
const secretsFileEntry = "secrets.yaml"

// ExcludedPatterns are paths never synced in either direction, by differ or
// applier. The entry syntax is documented on Excluded; every entry is
// unconditional except secretsFileEntry.
var ExcludedPatterns = []string{
	".storage/",
	".cloud/",
	secretsFileEntry,
	// The sops config the agent maintains (see ensureSopsConfig):
	// repository-side tooling, never written into /homeassistant.
	sopscrypt.ConfigFile,
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

// exclusions is ExcludedPatterns with each entry's kind decided once rather
// than re-derived per call: Excluded runs per file per scan, upwards of
// 7000 times on a real config.
type exclusions struct {
	dirs     map[string]struct{} // "dir/"  - any segment
	rootDirs map[string]struct{} // "/dir/" - first segment only
	exact    map[string]struct{} // plain entries
	globs    []string            // entries holding "*"
}

var excluded = compileExclusions(ExcludedPatterns)

func compileExclusions(entries []string) exclusions {
	m := exclusions{
		dirs:     make(map[string]struct{}, len(entries)),
		rootDirs: make(map[string]struct{}, len(entries)),
		exact:    make(map[string]struct{}, len(entries)),
	}
	for _, e := range entries {
		switch {
		case strings.HasPrefix(e, "/"):
			m.rootDirs[strings.Trim(e, "/")] = struct{}{}
		case strings.HasSuffix(e, "/"):
			m.dirs[strings.TrimSuffix(e, "/")] = struct{}{}
		case strings.Contains(e, "*"):
			m.globs = append(m.globs, e)
		default:
			m.exact[e] = struct{}{}
		}
	}
	return m
}

// exactHit reports an exact-entry match, applying the one conditional
// entry here so the table stays immutable and shared across goroutines.
func (m exclusions) exactHit(name string, syncSecretsFile bool) bool {
	if _, ok := m.exact[name]; !ok {
		return false
	}
	return !syncSecretsFile || name != secretsFileEntry
}

// SecretPatterns are filename patterns that make GuardSecretsAt refuse to
// sync at all. Stricter than ExcludedPatterns, which only skips a path: a
// secret-shaped file being tracked in git is itself the problem.
var SecretPatterns = []string{
	"secrets.yaml",
	// Both spellings, since HA accepts both and sopscrypt.IsSecretsFile
	// treats them as one. Without this, an import with encryption off
	// pushes a live secrets.yml in the clear.
	"secrets.yml",
	"*.pem",
	"*.key",
	"id_rsa*",
	"id_ed25519*",
	// Glob, not the literal ".env": ".env.local" and ".env.production" were
	// sailing past, and Import made this a filter on what gets pushed.
	".env*",
}

// Excluded reports whether p, a repo/config-relative path (forward- or
// backslash separated), matches an ExcludedPatterns entry:
//
//   - "dir/" matches any segment, basename included, at any depth.
//   - "/dir/" matches only a path whose FIRST segment is that name.
//   - "*.db" globs the basename only.
//   - A plain entry matches the basename or the full path exactly.
//
// With SetEncryptionEnabled(true), secrets.yaml is NOT excluded: it is
// ciphertext by the time it reaches the worktree. Every other entry is
// runtime state and stays excluded unconditionally.
func Excluded(p string) bool {
	normalized := strings.TrimLeft(strings.ReplaceAll(p, "\\", "/"), "/")
	// Clean first, or "sub/../gitops/foo.yaml" dodges the root-anchored
	// gitops/ guard. A path still climbing above the root afterwards is
	// excluded outright (fail closed).
	normalized = path.Clean(normalized)
	if normalized == ".." || strings.HasPrefix(normalized, "../") {
		return true
	}

	// IndexByte rather than strings.Split: the slice Split returns was the
	// only allocation left on this per-file path.
	first, basename := normalized, normalized
	if i := strings.IndexByte(normalized, '/'); i >= 0 {
		first = normalized[:i]
		basename = normalized[strings.LastIndexByte(normalized, '/')+1:]
	}
	if _, ok := excluded.rootDirs[first]; ok {
		return true
	}
	for rest := normalized; rest != ""; {
		seg := rest
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			seg, rest = rest[:i], rest[i+1:]
		} else {
			rest = ""
		}
		if _, ok := excluded.dirs[seg]; ok {
			return true
		}
	}

	// Exact before glob, so a path already caught by a directory entry
	// (".ssh/secrets.yaml") stays excluded whatever the encryption setting.
	syncSecretsFile := EncryptionEnabled()
	if excluded.exactHit(basename, syncSecretsFile) || excluded.exactHit(normalized, syncSecretsFile) {
		return true
	}
	for _, entry := range excluded.globs {
		if ok, _ := path.Match(entry, basename); ok {
			return true
		}
	}
	return false
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

// secretShapedDisallowed is matchesSecretPattern, except a secrets.yaml or
// .yml passes when encryption is on (it reaches the worktree as ciphertext).
// Nothing else in SecretPatterns ever passes; where the question is "secret-
// shaped at all" rather than "may this be handled", use the raw check.
func secretShapedDisallowed(p string) bool {
	if !matchesSecretPattern(p) {
		return false
	}
	return !EncryptionEnabled() || !sopscrypt.IsSecretsFile(p)
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

	// Crypter encrypts secrets into the worktree and back out. nil (no age
	// key) is fine: every call site is the nil-safe g.Crypter.Enabled().
	// Set alongside SetEncryptionEnabled - same option, checked together.
	Crypter *sopscrypt.Crypter

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
		gitConfigGlobal: strings.TrimRight(absPath(workdir), "/") + ".gitconfig",
	}
}

// EnsureClone clones opts.RepoURL into Workdir unless it is already a clone
// of it; a clone whose origin no longer matches is wiped and re-cloned.
// The clone URL stays credential-free, so origin never carries a token and
// nothing secret is written under Workdir/.git; auth reaches git through
// the process environment of that one call (see credentialEnv).
func (g *GitSync) EnsureClone(ctx context.Context) error {
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

	if _, err := g.runGit(ctx, []string{"clone", "--no-checkout", "--origin", "origin", g.Opts.RepoURL, g.Workdir}, parent, g.credentialEnv()); err != nil {
		return err
	}
	if _, err := g.runGit(ctx, []string{"config", "core.autocrlf", "false"}, g.Workdir, nil); err != nil {
		return err
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
	if _, err := g.runGit(ctx, []string{"fetch", "--quiet", g.Opts.RepoURL, g.Opts.Branch}, g.Workdir, g.credentialEnv()); err != nil {
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

// GuardSecretsAt errors if any entry in files is a secret-shaped path in
// the tree at sha - call it with TrackedFilesRaw's output before any diff or
// apply. With encryption on, a secrets.yaml/.yml passes only if its blob
// really is sops-encrypted, read via "git show" BEFORE any checkout so a
// plaintext secret never reaches disk; anything else still fails outright.
//
// Plaintext-with-a-key and encrypted-without-one return plain errors rather
// than *SecretsTrackedError: the fixes differ ("get this out of git" versus
// "encrypt it" versus "set age_key"), so the types must too.
func (g *GitSync) GuardSecretsAt(ctx context.Context, sha string, files []string) error {
	encrypting := EncryptionEnabled() && g.Crypter.Enabled()
	var offenders []string
	for _, f := range files {
		if !matchesSecretPattern(f) {
			continue
		}
		if encrypting && sopscrypt.IsSecretsFile(f) {
			encrypted, err := g.blobIsEncrypted(ctx, sha, f)
			if err != nil {
				return err
			}
			if encrypted {
				continue
			}
			return fmt.Errorf(
				"tracked %s is not SOPS-encrypted - re-run Import to encrypt it, or encrypt it with sops locally", f)
		}
		// Refused either way, but an encrypted secrets.yaml means a missing
		// age_key rather than a leak. Best-effort, since the blob read only
		// changes the wording: a failure leaves the original refusal.
		if sopscrypt.IsSecretsFile(f) {
			if encrypted, err := g.blobIsEncrypted(ctx, sha, f); err == nil && encrypted {
				return fmt.Errorf(
					"tracked %s is SOPS-encrypted but no age_key is configured - set the age_key option to the key it was encrypted to", f)
			}
		}
		offenders = append(offenders, f)
	}
	if len(offenders) == 0 {
		return nil
	}
	sort.Strings(offenders)
	return &SecretsTrackedError{Files: offenders}
}

// blobIsEncrypted reports whether the blob at path in sha is a sops
// document. Read via "git show" because the guard runs before Checkout,
// which is the point: a plaintext secret is detected without hitting disk,
// and only the boolean leaves this function.
func (g *GitSync) blobIsEncrypted(ctx context.Context, sha, path string) (bool, error) {
	result, err := g.runGit(ctx, []string{"show", sha + ":" + path}, "", nil)
	if err != nil {
		return false, err
	}
	return sopscrypt.IsEncrypted([]byte(result.Stdout)), nil
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

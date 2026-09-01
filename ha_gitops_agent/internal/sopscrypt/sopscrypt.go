// Package sopscrypt encrypts and decrypts Home Assistant config files with
// sops and an age key. Load-bearing constraints:
//
//   - Values, not files: only values under secret-shaped keys become
//     ENC[...], except secrets.yaml which is encrypted whole. Only YAML,
//     JSON and dotenv qualify (see Format); sops blobs anything else.
//   - The private key never reaches argv: encrypting needs only the public
//     recipient, decrypting passes the identity as SOPS_AGE_KEY in that one
//     subprocess env, and any inherited age key is stripped first.
//   - Every call carries its rules as flags and runs from an empty
//     directory outside the worktree, so a .sops.yaml committed to the
//     repository gets no vote (see runDir).
//   - Leaf package: internal/gitsync imports this, so it may import only
//     stdlib-only leaves of its own (internal/execx, see exec.go).
//
// age only validates the identity and derives its public recipient; the
// sops binary does all the crypto.
package sopscrypt

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"filippo.io/age"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/execx"
)

// DefaultTimeout bounds every sops subprocess call this package makes.
// Generous on purpose: being wrong the other way half-encrypts a worktree.
const DefaultTimeout = 60 * time.Second

// ConfigFile is the name of the sops config the agent manages at the
// repository root (see SopsConfig).
const ConfigFile = ".sops.yaml"

// Crypter holds one age identity and runs sops against it. The zero value
// and a nil receiver both mean "encryption is off" (see Enabled).
type Crypter struct {
	// identity is the full "AGE-SECRET-KEY-1..." string: never logged,
	// never in argv, redacted out of anything a failed sops call reports.
	identity string
	// recipient is the derived "age1..." public key, the only half
	// encryption needs.
	recipient string

	// Runner executes every sops subprocess, defaulting to a real
	// exec.CommandContext; tests inject a fake to inspect argv/env.
	Runner Runner

	// Timeout bounds every sops subprocess call. Defaults to
	// DefaultTimeout when zero.
	Timeout time.Duration
}

// New returns a Crypter encrypting to the recipient derived from ageKey (an
// X25519 "AGE-SECRET-KEY-1..." identity, surrounding whitespace trimmed).
// A malformed key is an error, not silently-disabled encryption.
func New(ageKey string) (*Crypter, error) {
	key := strings.TrimSpace(ageKey)
	if key == "" {
		return nil, errors.New("sopscrypt: no age key configured")
	}
	identity, err := age.ParseX25519Identity(key)
	if err != nil {
		// Redacted anyway: this is the one place the raw key is in hand.
		return nil, fmt.Errorf("sopscrypt: invalid age key: %s", execx.Redact(err.Error(), key))
	}
	return &Crypter{
		identity:  key,
		recipient: identity.Recipient().String(),
		Runner:    execx.CommandRunner{},
		Timeout:   DefaultTimeout,
	}, nil
}

// Recipient returns the public "age1..." recipient this Crypter encrypts
// to, or "" when encryption is off. Safe on a nil receiver.
func (c *Crypter) Recipient() string {
	if c == nil {
		return ""
	}
	return c.recipient
}

// Enabled reports whether this Crypter can actually encrypt. Safe on a nil
// receiver and on the zero value, both meaning "no age key configured".
func (c *Crypter) Enabled() bool {
	return c != nil && c.identity != "" && c.recipient != ""
}

// EncryptFileInPlace rewrites absPath as a sops document encrypted to this
// Crypter's recipient. The repository-relative relPath decides how much:
// secrets.yaml (or .yml) whole, anything else only under keys matching
// SecretKeyRegex. No key material is passed - encrypting needs only the
// public recipient.
//
// Fails if absPath is already encrypted (sops refuses a top-level "sops"
// entry) or if its format cannot be resolved: sops infers its store from
// the extension, a dotenv file has none, and the BINARY store it falls back
// to encrypts the whole file as one opaque blob (see Format).
func (c *Crypter) EncryptFileInPlace(ctx context.Context, absPath, relPath string) error {
	if !c.Enabled() {
		return errors.New("sopscrypt: encryption is not enabled")
	}
	format, err := c.formatToEncrypt(absPath, relPath)
	if err != nil {
		return err
	}
	args := []string{"sops", "encrypt", "--in-place", "--age", c.recipient}
	if !IsSecretsFile(relPath) {
		args = append(args, "--encrypted-regex", SecretKeyRegex)
	}
	args = append(args, storeFlags(format)...)
	args = append(args, absPath)

	dir, err := runDir()
	if err != nil {
		return err
	}
	result, err := c.run(ctx, args, dir, nil)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("sopscrypt: encrypting %s failed (exit %d): %s", relPath, result.ExitCode, c.failureReason(result))
	}
	return nil
}

// formatToEncrypt resolves the store sops must be told to use for relPath,
// reading absPath only when the extension does not settle it - so it stays
// callable for a path that does not exist yet. An unreadable file is
// refused rather than falling back to sops's whole-file binary store.
func (c *Crypter) formatToEncrypt(absPath, relPath string) (Format, error) {
	if format := formatFromPath(relPath); format != FormatNone {
		return format, nil
	}
	data, err := os.ReadFile(absPath) // #nosec G304 -- caller-guarded path inside the worktree
	if err != nil {
		return FormatNone, fmt.Errorf(
			"sopscrypt: refusing to encrypt %s: its format is not settled by its name and it could not be read to confirm: %v", relPath, err)
	}
	format := FormatFor(relPath, data)
	if format == FormatNone {
		return FormatNone, fmt.Errorf(
			"sopscrypt: refusing to encrypt %s: it is not YAML, JSON or dotenv, and SOPS would encrypt the whole file as one opaque blob", relPath)
	}
	return format, nil
}

// DecryptFile returns the plaintext of the sops document at absPath. The
// identity reaches sops only through this one subprocess's SOPS_AGE_KEY,
// with any inherited age key dropped first. The returned bytes are
// plaintext secrets: never log them or write them outside /homeassistant.
func (c *Crypter) DecryptFile(ctx context.Context, absPath string) ([]byte, error) {
	if !c.Enabled() {
		return nil, errors.New("sopscrypt: encryption is not enabled")
	}
	args := []string{"sops", "decrypt"}
	// Checked here so no path to sops can skip it: the document's own
	// metadata picks the backend, and it is repository content. A read
	// failure is an error, not a fall-through - running sops anyway would
	// skip exactly this guard against repository-declared kms/vault
	// backends, on nothing more than a transient EACCES.
	data, err := os.ReadFile(absPath) // #nosec G304 -- caller-guarded path inside the worktree
	if err != nil {
		return nil, fmt.Errorf("sopscrypt: refusing to decrypt %s: could not read it to check its key source: %v",
			filepath.Base(absPath), err)
	}
	if source := UnsupportedKeySource(data); source != "" {
		return nil, fmt.Errorf("sopscrypt: refusing to decrypt %s: it declares a %s master key, and this agent decrypts only with its configured age key",
			filepath.Base(absPath), source)
	}
	// Named on the way out too, mirroring EncryptFileInPlace: sops
	// infers its store from the extension case-sensitively, and the
	// wrong store fails outright ("no binary data found in tree").
	// The ciphertext settles what the path cannot, since a dotenv
	// document has no extension.
	format := formatFromPath(absPath)
	if format == FormatNone && isDotenvCiphertext(data) {
		format = FormatDotenv
	}
	args = append(args, storeFlags(format)...)
	args = append(args, absPath)

	dir, err := runDir()
	if err != nil {
		return nil, err
	}
	result, err := c.run(ctx, args, dir, []string{"SOPS_AGE_KEY=" + c.identity})
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("sopscrypt: decrypting %s failed (exit %d): %s", filepath.Base(absPath), result.ExitCode, c.failureReason(result))
	}
	return []byte(result.Stdout), nil
}

// storeFlags names the sops store for format, or nothing for FormatNone.
// Passed on EVERY call because sops infers the store from the extension
// CASE-SENSITIVELY: "config.JSON" reaches the BINARY store, which blobs the
// whole file, ignores --encrypted-regex, and still round-trips cleanly.
func storeFlags(format Format) []string {
	if format == FormatNone {
		return nil
	}
	store := string(format)
	return []string{"--input-type", store, "--output-type", store}
}

// Probe runs "sops --version" to check the binary is present and
// executable. Called once at startup, so a missing sops or an AppArmor
// denial surfaces there rather than mid-import with plaintext already in
// the worktree. --disable-version-check stops sops querying GitHub for a
// newer release, which would make this need outbound internet.
func (c *Crypter) Probe(ctx context.Context) error {
	if !c.Enabled() {
		return errors.New("sopscrypt: encryption is not enabled")
	}
	result, err := c.run(ctx, []string{"sops", "--version", "--disable-version-check"}, "", nil)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("sopscrypt: sops --version failed (exit %d): %s", result.ExitCode, c.failureReason(result))
	}
	return nil
}

// SopsConfig renders the .sops.yaml the agent maintains at the repository
// root, kept in lockstep with EncryptFileInPlace's flags so sops run by
// hand in a clone treats files the same way.
//
// dotenv deliberately gets no rule: nothing in a .sops.yaml can set the
// input type, so a path rule broad enough to match one would make "sops
// encrypt meter-0001" succeed by binary-encrypting the whole file. With no
// rule it fails cleanly; DOCS.md carries the command that works.
func (c *Crypter) SopsConfig() []byte {
	return []byte(`# Managed by GitOps Agent (ha-gitops-agent). creation_rules are regenerated
# automatically; manual edits to them will be overwritten.
creation_rules:
  - path_regex: (^|/)secrets\.ya?ml$
    age: ` + c.Recipient() + `
  - path_regex: \.ya?ml$
    encrypted_regex: '` + SecretKeyRegex + `'
    age: ` + c.Recipient() + `
  - path_regex: \.json$
    encrypted_regex: '` + SecretKeyRegex + `'
    age: ` + c.Recipient() + `
`)
}

// run executes one sops invocation under a bounded timeout; a non-zero exit
// is data, only a launch failure or a timeout comes back as an error.
func (c *Crypter) run(ctx context.Context, args []string, dir string, extraEnv []string) (RunResult, error) {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	runner := c.Runner
	if runner == nil {
		runner = execx.CommandRunner{}
	}
	result, err := runner.Run(runCtx, dir, append(baseEnv(), extraEnv...), args...)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return RunResult{}, fmt.Errorf("sopscrypt: sops %s timed out after %s", args[1], timeout)
		}
		return RunResult{}, fmt.Errorf("sopscrypt: sops %s failed to run: %s", args[1], execx.Redact(err.Error(), c.identity))
	}
	return result, nil
}

// failureReason strips the identity out of sops's output, preferring stderr
// but falling back to stdout, where the "already encrypted" refusal lands.
func (c *Crypter) failureReason(result RunResult) string {
	reason := strings.TrimSpace(result.Stderr)
	if reason == "" {
		reason = strings.TrimSpace(result.Stdout)
	}
	return execx.Redact(reason, c.identity)
}

// runDir is the empty agent-owned directory every sops subprocess runs in,
// never the worktree - a security boundary: sops walks UP for a .sops.yaml,
// and a planted "unencrypted_regex: '.*'" makes it exit 0 with a valid sops
// block over plaintext values, which IsEncrypted, GuardSecretsAt and the
// masked diff would all accept.
//
// --config is no alternative: sops rejects a config with no matching
// creation rule. A MkdirTemp failure is FATAL to the call, never a
// fallback to the temp dir itself: sops's upward search from /tmp reaches
// / - exactly where a planted config would sit - so running there defeats
// the boundary this directory exists to hold.
func runDir() (string, error) {
	sopsRunDirOnce.Do(func() {
		sopsRunDir, sopsRunDirErr = os.MkdirTemp("", "sopscrypt-")
	})
	if sopsRunDirErr != nil {
		return "", fmt.Errorf("sopscrypt: could not create the sops run directory: %w", sopsRunDirErr)
	}
	return sopsRunDir, nil
}

var (
	sopsRunDirOnce sync.Once
	sopsRunDir     string
	sopsRunDirErr  error
)

// baseEnv is the process environment minus any inherited age key, which
// also keeps DecryptFile's own SOPS_AGE_KEY from landing second: on POSIX
// the FIRST occurrence of a duplicated key wins.
func baseEnv() []string {
	env := os.Environ()
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		if key == "SOPS_AGE_KEY" || key == "SOPS_AGE_KEY_FILE" {
			continue
		}
		out = append(out, kv)
	}
	return out
}

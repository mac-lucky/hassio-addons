package gitsync

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/sopscrypt"
)

// Every test here flips the package-level encryption switch, so none may
// call t.Parallel and every one must restore it - enableEncryption does both.

// An age key for this file alone, only ever seen by sopscrypt.New's
// validation: fakeSops stands in for the binary.
const (
	testAgeIdentity  = "AGE-SECRET-KEY-1QUUCUYTP2443EWJWQKK6LCAAUGS09XXGDHLVQV82Z2Y6200NDGAQJ8SUFT"
	testAgeRecipient = "age1jddwquuv7ck0fsyxhrly7jcvm2xtkhf4efk9sz9pptphcympw3psv4q4dz"
)

// fakeCipherPrefix is the line fakeSops hides a file's plaintext behind.
const fakeCipherPrefix = "fakedata: "

// fakeSopsMeta makes fakeSops's output satisfy sopscrypt.IsEncrypted.
const fakeSopsMeta = "sops:\n    mac: FAKEMAC\n    version: 3.13.2\n"

// fakeSops stands in for the binary: an in-place "encrypt" that base64-hides
// content behind a sops-shaped envelope, and a "decrypt" that gives it back.
// Reversible because encryptedCopyIsCurrent decrypts and compares to live.
type fakeSops struct {
	calls [][]string
	// encrypts counts in-place encryptions: "did this import rewrite it?".
	encrypts int
	// decryptRewrite stands in for the real binary re-emitting a document
	// from its own parse, which is why currency cannot compare raw bytes.
	decryptRewrite func(string) string
}

func (f *fakeSops) Run(_ context.Context, _ string, _ []string, args ...string) (sopscrypt.RunResult, error) {
	f.calls = append(f.calls, append([]string(nil), args...))
	path := args[len(args)-1]
	data, err := os.ReadFile(path) // #nosec G304 -- test fake, path comes from the code under test's own t.TempDir() tree
	if err != nil {
		return sopscrypt.RunResult{Stderr: err.Error(), ExitCode: 1}, nil
	}

	switch args[1] {
	case "encrypt":
		if sopscrypt.IsEncrypted(data) {
			// The real binary refuses a file that already carries a
			// top-level "sops" entry (exit 203, on stdout).
			return sopscrypt.RunResult{Stdout: "file already contains a top-level entry called 'sops'", ExitCode: 203}, nil
		}
		payload := fakeCipherPrefix + base64.StdEncoding.EncodeToString(data) + "\n" + fakeSopsMeta
		if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
			return sopscrypt.RunResult{Stderr: err.Error(), ExitCode: 1}, nil
		}
		f.encrypts++
		return sopscrypt.RunResult{}, nil
	case "decrypt":
		plaintext, ok := fakeDecrypt(data)
		if !ok {
			return sopscrypt.RunResult{Stderr: "not a fake-encrypted file", ExitCode: 1}, nil
		}
		out := string(plaintext)
		if f.decryptRewrite != nil {
			out = f.decryptRewrite(out)
		}
		return sopscrypt.RunResult{Stdout: out}, nil
	}
	return sopscrypt.RunResult{Stderr: "unexpected subcommand " + args[1], ExitCode: 2}, nil
}

// fakeEncrypt is fakeSops's transformation, for seeding a repository with
// an already-encrypted file.
func fakeEncrypt(plaintext string) string {
	return fakeCipherPrefix + base64.StdEncoding.EncodeToString([]byte(plaintext)) + "\n" + fakeSopsMeta
}

func fakeDecrypt(data []byte) ([]byte, bool) {
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(line, fakeCipherPrefix); ok {
			plaintext, err := base64.StdEncoding.DecodeString(rest)
			if err != nil {
				return nil, false
			}
			return plaintext, true
		}
	}
	return nil, false
}

// enableEncryption turns the package switch on for one test and gives gs a
// Crypter backed by fakeSops.
func enableEncryption(t *testing.T, gs *GitSync) *fakeSops {
	t.Helper()
	SetEncryptionEnabled(true)
	t.Cleanup(func() { SetEncryptionEnabled(false) })

	crypter, err := sopscrypt.New(testAgeIdentity)
	if err != nil {
		t.Fatalf("sopscrypt.New: %v", err)
	}
	fake := &fakeSops{}
	crypter.Runner = fake
	if gs != nil {
		gs.Crypter = crypter
	}
	return fake
}

// --- the switch -----------------------------------------------------------

// Encryption opens exactly one door, secrets.yaml, and leaves every other
// excluded or secret-shaped path as shut as it was.
func TestEncryptionSwitchFlipsOnlyTheSecretsFileRules(t *testing.T) {
	unchangedExcluded := []string{
		".storage/core.entity_registry", ".cloud/x", ".ssh/id_rsa", "home-assistant_v2.db",
		"home-assistant.log", "deps/lib.py", "backups/x.tar", "tts/x.mp3", "gitops/registries.yaml",
		// Excluded matches the sops config by basename at any depth, which
		// keeps a tracked packages/.sops.yaml out of /homeassistant.
		sopscrypt.ConfigFile, "packages/" + sopscrypt.ConfigFile,
	}
	unchangedSecretShaped := []string{
		"certs/fullchain.pem", "certs/privkey.key", "id_rsa", "sub/id_ed25519", ".env", ".env.local",
	}

	if !Excluded("secrets.yaml") {
		t.Error("Excluded(secrets.yaml) = false with encryption off, want true")
	}
	if !secretShapedDisallowed("secrets.yaml") {
		t.Error("secretShapedDisallowed(secrets.yaml) = false with encryption off, want true")
	}
	assertUnchangedRules(t, unchangedExcluded, unchangedSecretShaped)

	enableEncryption(t, nil)

	if Excluded("secrets.yaml") {
		t.Error("Excluded(secrets.yaml) = true with encryption on, want false: it has to be importable and diffable")
	}
	for _, p := range []string{"secrets.yaml", "SECRETS.YAML", "config/secrets.yaml"} {
		if secretShapedDisallowed(p) {
			t.Errorf("secretShapedDisallowed(%q) = true with encryption on, want false", p)
		}
		// Still raw secret-shaped, which is how GuardSecretsAt knows the
		// blob is worth inspecting.
		if !matchesSecretPattern(p) {
			t.Errorf("matchesSecretPattern(%q) = false, want true regardless of the switch", p)
		}
	}
	assertUnchangedRules(t, unchangedExcluded, unchangedSecretShaped)
}

func assertUnchangedRules(t *testing.T, excluded, secretShaped []string) {
	t.Helper()
	for _, p := range excluded {
		if !Excluded(p) {
			t.Errorf("Excluded(%q) = false, want true regardless of the encryption switch", p)
		}
	}
	for _, p := range secretShaped {
		if !secretShapedDisallowed(p) {
			t.Errorf("secretShapedDisallowed(%q) = false, want true regardless of the encryption switch", p)
		}
	}
}

// --- GuardSecretsAt -------------------------------------------------------

func TestGuardSecretsAtAcceptsAnEncryptedSecretsFile(t *testing.T) {
	tmp := t.TempDir()
	bare, work := makeRemote(t, tmp, "remote")
	commitFile(t, work, "automations.yaml", "- id: demo\n", "commit")
	commitFile(t, work, "secrets.yaml", fakeEncrypt("mqtt_password: hunter2\n"), "commit")

	gs := New(makeOpts("file://"+bare), filepath.Join(tmp, "clone"))
	enableEncryption(t, gs)
	ctx := context.Background()
	if err := gs.EnsureClone(ctx); err != nil {
		t.Fatalf("EnsureClone: %v", err)
	}
	sha, err := gs.Fetch(ctx)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	raw, err := gs.TrackedFilesRaw(ctx, sha)
	if err != nil {
		t.Fatalf("TrackedFilesRaw: %v", err)
	}
	if !contains(raw, "secrets.yaml") {
		t.Fatal("raw tracked files missing secrets.yaml")
	}

	if err := gs.GuardSecretsAt(ctx, sha, raw); err != nil {
		t.Errorf("GuardSecretsAt() error = %v, want nil for an encrypted secrets.yaml", err)
	}
	// It must reach the diff too: encrypted, it is an ordinary syncable file.
	tracked, err := gs.TrackedFiles(ctx, sha)
	if err != nil {
		t.Fatalf("TrackedFiles: %v", err)
	}
	if !contains(tracked, "secrets.yaml") {
		t.Errorf("TrackedFiles = %v, want secrets.yaml included with encryption on", tracked)
	}
}

// The guard reads the blob from the object database, catching this before
// any checkout puts the plaintext on disk.
func TestGuardSecretsAtRefusesPlaintextSecretsFile(t *testing.T) {
	tmp := t.TempDir()
	bare, work := makeRemote(t, tmp, "remote")
	commitFile(t, work, "secrets.yaml", "mqtt_password: hunter2\n", "commit")

	workdir := filepath.Join(tmp, "clone")
	gs := New(makeOpts("file://"+bare), workdir)
	enableEncryption(t, gs)
	ctx := context.Background()
	if err := gs.EnsureClone(ctx); err != nil {
		t.Fatalf("EnsureClone: %v", err)
	}
	sha, err := gs.Fetch(ctx)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	raw, err := gs.TrackedFilesRaw(ctx, sha)
	if err != nil {
		t.Fatalf("TrackedFilesRaw: %v", err)
	}

	err = gs.GuardSecretsAt(ctx, sha, raw)
	if err == nil {
		t.Fatal("GuardSecretsAt() error = nil, want a refusal for a plaintext secrets.yaml")
	}
	want := "tracked secrets.yaml is not SOPS-encrypted - re-run Import to encrypt it, or encrypt it with sops locally"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
	// A different fix from "get this file out of git", so a different type.
	var tracked *SecretsTrackedError
	if errors.As(err, &tracked) {
		t.Error("error is a *SecretsTrackedError; the two conditions need different advice")
	}
	// Nothing was checked out on the way to that answer.
	if _, err := os.Stat(filepath.Join(workdir, "secrets.yaml")); err == nil {
		t.Error("the plaintext secret was written to disk; the guard must run before any checkout")
	}
}

// Raw key material has no key-by-key structure to encrypt selectively, so
// it stays refused either way.
func TestGuardSecretsAtRefusesOtherSecretShapedPathsEitherWay(t *testing.T) {
	tmp := t.TempDir()
	bare, work := makeRemote(t, tmp, "remote")
	// Not ".env": a global gitignore for it would fail the fixture setup.
	commitFile(t, work, "sub/id_ed25519", "-----BEGIN OPENSSH PRIVATE KEY-----\n", "commit")
	commitFile(t, work, "certs/privkey.pem", "-----BEGIN PRIVATE KEY-----\n", "commit")

	gs := New(makeOpts("file://"+bare), filepath.Join(tmp, "clone"))
	ctx := context.Background()
	if err := gs.EnsureClone(ctx); err != nil {
		t.Fatalf("EnsureClone: %v", err)
	}
	sha, err := gs.Fetch(ctx)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	raw, err := gs.TrackedFilesRaw(ctx, sha)
	if err != nil {
		t.Fatalf("TrackedFilesRaw: %v", err)
	}

	var target *SecretsTrackedError
	if err := gs.GuardSecretsAt(ctx, sha, raw); !errors.As(err, &target) {
		t.Fatalf("GuardSecretsAt() with encryption off = %v, want *SecretsTrackedError", err)
	}

	enableEncryption(t, gs)
	if err := gs.GuardSecretsAt(ctx, sha, raw); !errors.As(err, &target) {
		t.Fatalf("GuardSecretsAt() with encryption on = %v, want *SecretsTrackedError", err)
	}
	if len(target.Files) != 2 {
		t.Errorf("offenders = %v, want both the ssh key and the pem", target.Files)
	}
}

// --- import ---------------------------------------------------------------

func TestImportEncryptsSecretsFileAndWritesSopsConfig(t *testing.T) {
	tmp := t.TempDir()
	bare, work := makeRemote(t, tmp, "repo")
	commitFile(t, work, "README.md", "readme\n", "init")

	configRoot := filepath.Join(tmp, "config")
	writeLiveText(t, configRoot, "configuration.yaml", "homeassistant:\n  name: Home\n")
	writeLiveText(t, configRoot, "secrets.yaml", "mqtt_password: hunter2\n")

	gs := importGitSync(t, bare, filepath.Join(tmp, "workdir"))
	fake := enableEncryption(t, gs)

	res, err := gs.Import(context.Background(), configRoot, generousLimits(), fixedImportTime)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res.Files != 2 {
		t.Errorf("Files = %d, want 2 (secrets.yaml is importable once it is encrypted)", res.Files)
	}

	pushed, ok := showAtRef(t, bare, "main", "secrets.yaml")
	if !ok {
		t.Fatal("secrets.yaml missing from the tracked branch")
	}
	if strings.Contains(pushed, "hunter2") {
		t.Errorf("pushed secrets.yaml = %q, the plaintext secret reached the remote", pushed)
	}
	if !sopscrypt.IsEncrypted([]byte(pushed)) {
		t.Errorf("pushed secrets.yaml = %q, want sops-shaped ciphertext", pushed)
	}

	// The managed .sops.yaml rides along so a human who clones the repo can
	// decrypt and re-encrypt with a plain "sops <file>".
	config, ok := showAtRef(t, bare, "main", sopscrypt.ConfigFile)
	if !ok {
		t.Fatalf("%s missing from the tracked branch", sopscrypt.ConfigFile)
	}
	if !strings.Contains(config, testAgeRecipient) {
		t.Errorf("%s = %q, want the age recipient", sopscrypt.ConfigFile, config)
	}
	if strings.Contains(config, testAgeIdentity) {
		t.Fatalf("%s carries the private key", sopscrypt.ConfigFile)
	}

	// A file with no secret-shaped key stays readable: values-only
	// encryption is what keeps a config repo reviewable.
	plain, ok := showAtRef(t, bare, "main", "configuration.yaml")
	if !ok || plain != "homeassistant:\n  name: Home\n" {
		t.Errorf("configuration.yaml = %q (ok=%v), want it committed unchanged", plain, ok)
	}
	if fake.encrypts != 1 {
		t.Errorf("encrypt calls = %d, want exactly 1 (only secrets.yaml needed it)", fake.encrypts)
	}
}

// The default: with no age key configured, nothing changes.
func TestImportWithEncryptionOffStillPrunesSecretsFile(t *testing.T) {
	tmp := t.TempDir()
	bare, work := makeRemote(t, tmp, "repo")
	commitFile(t, work, "README.md", "readme\n", "init")

	configRoot := filepath.Join(tmp, "config")
	writeLiveText(t, configRoot, "configuration.yaml", "homeassistant:\n  name: Home\n")
	writeLiveText(t, configRoot, "secrets.yaml", "mqtt_password: hunter2\n")

	gs := importGitSync(t, bare, filepath.Join(tmp, "workdir"))
	res, err := gs.Import(context.Background(), configRoot, generousLimits(), fixedImportTime)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res.Files != 1 {
		t.Errorf("Files = %d, want 1 (secrets.yaml is not importable without an age key)", res.Files)
	}
	if _, ok := showAtRef(t, bare, "main", "secrets.yaml"); ok {
		t.Error("secrets.yaml reached the remote with encryption off")
	}
	if _, ok := showAtRef(t, bare, "main", sopscrypt.ConfigFile); ok {
		t.Errorf("%s was written with no age key configured", sopscrypt.ConfigFile)
	}
}

// HA accepts secrets.yml as readily as secrets.yaml and sopscrypt treats
// them as one file, so the gate in front of them must agree both ways.
func TestImportHandlesTheYmlSpellingOfSecretsToo(t *testing.T) {
	tmp := t.TempDir()
	bare, work := makeRemote(t, tmp, "repo")
	commitFile(t, work, "README.md", "readme\n", "init")

	configRoot := filepath.Join(tmp, "config")
	writeLiveText(t, configRoot, "configuration.yaml", "homeassistant:\n  name: Home\n")
	writeLiveText(t, configRoot, "secrets.yml", "mqtt_password: hunter2\n")

	// Encryption off: it must not reach the remote at all.
	gs := importGitSync(t, bare, filepath.Join(tmp, "workdir-plain"))
	res, err := gs.Import(context.Background(), configRoot, generousLimits(), fixedImportTime)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res.Files != 1 {
		t.Errorf("Files = %d, want 1 (secrets.yml is not importable without an age key)", res.Files)
	}
	if pushed, ok := showAtRef(t, bare, "main", "secrets.yml"); ok {
		t.Errorf("secrets.yml reached the remote with encryption off: %q", pushed)
	}

	// Encryption on: it is imported, and only as ciphertext.
	gs = importGitSync(t, bare, filepath.Join(tmp, "workdir-encrypted"))
	fake := enableEncryption(t, gs)

	res, err = gs.Import(context.Background(), configRoot, generousLimits(), fixedImportTime)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res.Files != 2 {
		t.Errorf("Files = %d, want 2 (secrets.yml joins the import once it is encrypted)", res.Files)
	}
	pushed, ok := showAtRef(t, bare, "main", "secrets.yml")
	if !ok {
		t.Fatal("secrets.yml missing from the tracked branch with encryption on")
	}
	if strings.Contains(pushed, "hunter2") {
		t.Errorf("pushed secrets.yml = %q, the plaintext secret reached the remote", pushed)
	}
	if !sopscrypt.IsEncrypted([]byte(pushed)) {
		t.Errorf("pushed secrets.yml = %q, want sops-shaped ciphertext", pushed)
	}
	if fake.encrypts != 1 {
		t.Errorf("encrypt calls = %d, want exactly 1 (only secrets.yml needed it)", fake.encrypts)
	}
}

// The unit-level half of the test above, including staying raw
// secret-shaped so GuardSecretsAt still inspects the blob.
func TestSecretsYmlIsSecretShapedOnBothSidesOfTheSwitch(t *testing.T) {
	for _, p := range []string{"secrets.yml", "SECRETS.YML", "config/secrets.yml"} {
		if !matchesSecretPattern(p) {
			t.Errorf("matchesSecretPattern(%q) = false, want true regardless of the switch", p)
		}
		if !secretShapedDisallowed(p) {
			t.Errorf("secretShapedDisallowed(%q) = false with encryption off, want true", p)
		}
	}

	enableEncryption(t, nil)

	for _, p := range []string{"secrets.yml", "SECRETS.YML", "config/secrets.yml"} {
		if secretShapedDisallowed(p) {
			t.Errorf("secretShapedDisallowed(%q) = true with encryption on, want false", p)
		}
		if !matchesSecretPattern(p) {
			t.Errorf("matchesSecretPattern(%q) = false, want true regardless of the switch", p)
		}
	}
}

// sops ciphertext is nondeterministic, so blind re-encryption would commit
// endlessly without changing anything.
func TestImportOfUnchangedEncryptedSecretsMakesNoCommit(t *testing.T) {
	tmp := t.TempDir()
	bare, work := makeRemote(t, tmp, "repo")
	commitFile(t, work, "README.md", "readme\n", "init")

	configRoot := filepath.Join(tmp, "config")
	writeLiveText(t, configRoot, "secrets.yaml", "mqtt_password: hunter2\n")

	gs := importGitSync(t, bare, filepath.Join(tmp, "workdir"))
	fake := enableEncryption(t, gs)

	first, err := gs.Import(context.Background(), configRoot, generousLimits(), fixedImportTime)
	if err != nil {
		t.Fatalf("first Import: %v", err)
	}
	if fake.encrypts != 1 {
		t.Fatalf("encrypt calls after the first import = %d, want 1", fake.encrypts)
	}

	_, err = gs.Import(context.Background(), configRoot, generousLimits(), fixedImportTime)
	if err == nil {
		t.Fatal("second Import() error = nil, want the ordinary nothing-to-import refusal")
	}
	if !strings.Contains(err.Error(), "nothing to import") {
		t.Errorf("second Import error = %v, want the nothing-to-import refusal", err)
	}
	if fake.encrypts != 1 {
		t.Errorf("encrypt calls after the second import = %d, want 1: unchanged content must not be re-encrypted", fake.encrypts)
	}

	// The tracked branch still points at the first import's commit.
	head := headSHA(t, bare, "main")
	if head != first.CommitSHA {
		t.Errorf("main = %s, want it unmoved at %s", head, first.CommitSHA)
	}
}

// The other half: the skip must not swallow a real change.
func TestImportOfChangedSecretsFileStillCommits(t *testing.T) {
	tmp := t.TempDir()
	bare, work := makeRemote(t, tmp, "repo")
	commitFile(t, work, "README.md", "readme\n", "init")

	configRoot := filepath.Join(tmp, "config")
	writeLiveText(t, configRoot, "secrets.yaml", "mqtt_password: hunter2\n")

	gs := importGitSync(t, bare, filepath.Join(tmp, "workdir"))
	fake := enableEncryption(t, gs)
	if _, err := gs.Import(context.Background(), configRoot, generousLimits(), fixedImportTime); err != nil {
		t.Fatalf("first Import: %v", err)
	}

	writeLiveText(t, configRoot, "secrets.yaml", "mqtt_password: rotated\n")
	if _, err := gs.Import(context.Background(), configRoot, generousLimits(), fixedImportTime.Add(time.Minute)); err != nil {
		t.Fatalf("second Import: %v", err)
	}
	if fake.encrypts != 2 {
		t.Errorf("encrypt calls = %d, want 2: a changed secret must be re-encrypted", fake.encrypts)
	}

	pushed, ok := showAtRef(t, bare, "main", "secrets.yaml")
	if !ok {
		t.Fatal("secrets.yaml missing from the tracked branch")
	}
	plaintext, ok := fakeDecrypt([]byte(pushed))
	if !ok {
		t.Fatalf("pushed secrets.yaml = %q, want fake-encrypted content", pushed)
	}
	if string(plaintext) != "mqtt_password: rotated\n" {
		t.Errorf("decrypted secrets.yaml = %q, want the rotated secret", plaintext)
	}
}

// sops rewrites a "!secret foo" node into an encrypted string and the tag
// never comes back, so a file mixing the two has no safe answer.
func TestImportRefusesInlineSecretAlongsideCustomTag(t *testing.T) {
	tmp := t.TempDir()
	bare, work := makeRemote(t, tmp, "repo")
	commitFile(t, work, "README.md", "readme\n", "init")

	configRoot := filepath.Join(tmp, "config")
	writeLiveText(t, configRoot, "configuration.yaml",
		"mqtt:\n  password: hunter2\nautomation: !include automations.yaml\n")

	gs := importGitSync(t, bare, filepath.Join(tmp, "workdir"))
	enableEncryption(t, gs)

	_, err := gs.Import(context.Background(), configRoot, generousLimits(), fixedImportTime)
	if err == nil {
		t.Fatal("Import() error = nil, want a refusal")
	}
	for _, want := range []string{"configuration.yaml", "secrets.yaml", "!secret"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to mention %q", err, want)
		}
	}
	if _, ok := showAtRef(t, bare, "main", "configuration.yaml"); ok {
		t.Error("the offending file reached the remote")
	}
}

// --- commit-back ----------------------------------------------------------

// Checked against the bare remote, not anything GitSync reports about
// itself.
func TestCommitBackEncryptsDriftedSecretsFile(t *testing.T) {
	tmp := t.TempDir()
	bare, work := makeRemote(t, tmp, "remote")
	commitFile(t, work, "secrets.yaml", fakeEncrypt("mqtt_password: old\n"), "commit")

	workdir := filepath.Join(tmp, "clone")
	gs := New(makeOpts("file://"+bare), workdir)
	fake := enableEncryption(t, gs)
	ctx := context.Background()
	if err := gs.EnsureClone(ctx); err != nil {
		t.Fatalf("EnsureClone: %v", err)
	}
	sha, err := gs.Fetch(ctx)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if err := gs.Checkout(ctx, sha); err != nil {
		t.Fatalf("Checkout: %v", err)
	}

	configRoot := filepath.Join(tmp, "homeassistant")
	writeLiveText(t, configRoot, "secrets.yaml", "mqtt_password: hunter2\n")

	branch, err := gs.CommitBack(ctx, []DriftFile{{Path: "secrets.yaml", Kind: "update"}}, configRoot, sha, fixedDriftTime)
	if err != nil {
		t.Fatalf("CommitBack: %v", err)
	}
	if fake.encrypts != 1 {
		t.Errorf("encrypt calls = %d, want 1", fake.encrypts)
	}

	pushed, ok := showAtRef(t, bare, branch, "secrets.yaml")
	if !ok {
		t.Fatalf("pushed branch %q does not contain secrets.yaml", branch)
	}
	if strings.Contains(pushed, "hunter2") {
		t.Errorf("pushed secrets.yaml = %q, the plaintext secret reached the remote", pushed)
	}
	if !sopscrypt.IsEncrypted([]byte(pushed)) {
		t.Errorf("pushed secrets.yaml = %q, want sops-shaped ciphertext", pushed)
	}
	plaintext, ok := fakeDecrypt([]byte(pushed))
	if !ok || string(plaintext) != "mqtt_password: hunter2\n" {
		t.Errorf("decrypted = %q (ok=%v), want the drifted live secret", plaintext, ok)
	}

	if config, ok := showAtRef(t, bare, branch, sopscrypt.ConfigFile); !ok {
		t.Errorf("%s missing from the drift branch", sopscrypt.ConfigFile)
	} else if !strings.Contains(config, testAgeRecipient) {
		t.Errorf("%s = %q, want the age recipient", sopscrypt.ConfigFile, config)
	}
}

// The same file is just as unencryptable arriving as drift.
func TestCommitBackRefusesInlineSecretAlongsideCustomTag(t *testing.T) {
	tmp := t.TempDir()
	bare, work := makeRemote(t, tmp, "remote")
	commitFile(t, work, "configuration.yaml", "homeassistant:\n  name: Home\n", "commit")

	workdir := filepath.Join(tmp, "clone")
	gs := New(makeOpts("file://"+bare), workdir)
	enableEncryption(t, gs)
	ctx := context.Background()
	if err := gs.EnsureClone(ctx); err != nil {
		t.Fatalf("EnsureClone: %v", err)
	}
	sha, err := gs.Fetch(ctx)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if err := gs.Checkout(ctx, sha); err != nil {
		t.Fatalf("Checkout: %v", err)
	}

	configRoot := filepath.Join(tmp, "homeassistant")
	writeLiveText(t, configRoot, "configuration.yaml",
		"mqtt:\n  password: hunter2\nautomation: !include automations.yaml\n")

	_, err = gs.CommitBack(ctx, []DriftFile{{Path: "configuration.yaml", Kind: "update"}}, configRoot, sha, fixedDriftTime)
	if err == nil {
		t.Fatal("CommitBack() error = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "configuration.yaml") {
		t.Errorf("error = %v, want it to name the offending file", err)
	}
	for _, b := range listRemoteBranches(t, bare) {
		if b != "main" {
			t.Errorf("remote gained branch %q; nothing should have been pushed", b)
		}
	}
}

// --- scan -----------------------------------------------------------------

func TestScanLiveIncludesSecretsFileOnlyWithEncryptionOn(t *testing.T) {
	root := t.TempDir()
	writeLive(t, root, "configuration.yaml", 5)
	writeLive(t, root, "secrets.yaml", 5)
	writeLive(t, root, ".env", 5)
	writeLive(t, root, "certs/fullchain.pem", 5)

	plan, err := ScanLive(root, generousLimits())
	if err != nil {
		t.Fatalf("ScanLive: %v", err)
	}
	if contains(plan.Files, "secrets.yaml") {
		t.Errorf("Files = %v, want secrets.yaml skipped with encryption off", plan.Files)
	}
	// SkippedExcluded, not SkippedSecret: the walker tests Excluded first
	// and secrets.yaml is on both lists.
	if plan.SkippedExcluded != 1 {
		t.Errorf("SkippedExcluded = %d, want 1 (secrets.yaml)", plan.SkippedExcluded)
	}
	if plan.SkippedSecret != 2 {
		t.Errorf("SkippedSecret = %d, want 2 (.env and the pem)", plan.SkippedSecret)
	}

	enableEncryption(t, nil)

	plan, err = ScanLive(root, generousLimits())
	if err != nil {
		t.Fatalf("ScanLive: %v", err)
	}
	if !contains(plan.Files, "secrets.yaml") {
		t.Errorf("Files = %v, want secrets.yaml included with encryption on", plan.Files)
	}
	if plan.SkippedExcluded != 0 {
		t.Errorf("SkippedExcluded = %d, want 0: secrets.yaml is no longer excluded", plan.SkippedExcluded)
	}
	// The two that stay refused either way.
	if plan.SkippedSecret != 2 {
		t.Errorf("SkippedSecret = %d, want 2 (.env and the pem, never secrets.yaml)", plan.SkippedSecret)
	}
	if contains(plan.Files, ".env") || contains(plan.Files, "certs/fullchain.pem") {
		t.Errorf("Files = %v, want raw key material still refused", plan.Files)
	}
}

// With only the switch set, secrets.yaml is importable with nothing to
// encrypt it: a loud failure, not a quiet plaintext push.
func TestImportFailsClosedWhenTheSwitchIsOnWithoutAKey(t *testing.T) {
	tmp := t.TempDir()
	bare, work := makeRemote(t, tmp, "repo")
	commitFile(t, work, "README.md", "readme\n", "init")

	configRoot := filepath.Join(tmp, "config")
	writeLiveText(t, configRoot, "secrets.yaml", "mqtt_password: hunter2\n")

	gs := importGitSync(t, bare, filepath.Join(tmp, "workdir"))
	SetEncryptionEnabled(true)
	t.Cleanup(func() { SetEncryptionEnabled(false) })
	// Deliberately no gs.Crypter.

	_, err := gs.Import(context.Background(), configRoot, generousLimits(), fixedImportTime)
	if err == nil {
		t.Fatal("Import() error = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "no age key is loaded") {
		t.Errorf("error = %v, want it to name the missing age key", err)
	}
	if _, ok := showAtRef(t, bare, "main", "secrets.yaml"); ok {
		t.Error("a plaintext secrets.yaml reached the remote")
	}
}

// --- .sops.yaml -----------------------------------------------------------

// stageDrift stages the file whenever it is written, so an unconditional
// rewrite would put a no-op .sops.yaml in every drift commit.
func TestEnsureSopsConfigOnlyRewritesWhenItHasTo(t *testing.T) {
	gs := New(makeOpts("file:///unused"), t.TempDir())

	written, err := gs.ensureSopsConfig()
	if err != nil {
		t.Fatalf("ensureSopsConfig with no age key: %v", err)
	}
	if written {
		t.Error("ensureSopsConfig() wrote a file with no age key configured")
	}

	enableEncryption(t, gs)
	full := filepath.Join(gs.Workdir, sopscrypt.ConfigFile)

	written, err = gs.ensureSopsConfig()
	if err != nil {
		t.Fatalf("ensureSopsConfig: %v", err)
	}
	if !written {
		t.Fatal("ensureSopsConfig() = false on a worktree with no .sops.yaml, want true")
	}
	first, err := os.ReadFile(full) // #nosec G304 -- t.TempDir() fixture path
	if err != nil {
		t.Fatal(err)
	}

	written, err = gs.ensureSopsConfig()
	if err != nil {
		t.Fatalf("second ensureSopsConfig: %v", err)
	}
	if written {
		t.Error("ensureSopsConfig() rewrote an already-correct .sops.yaml")
	}

	if err := os.WriteFile(full, []byte("creation_rules: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	written, err = gs.ensureSopsConfig()
	if err != nil {
		t.Fatalf("third ensureSopsConfig: %v", err)
	}
	if !written {
		t.Error("ensureSopsConfig() left a stale .sops.yaml in place")
	}
	after, err := os.ReadFile(full) // #nosec G304 -- t.TempDir() fixture path
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(first) {
		t.Errorf(".sops.yaml = %q, want it restored to %q", after, first)
	}
}

// writeLiveText is writeLive with real content rather than filler.
func writeLiveText(t *testing.T, root, rel, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		t.Fatalf("MkdirAll %s: %v", full, err)
	}
	if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile %s: %v", full, err)
	}
}

// headSHA reads a ref's commit straight out of the bare remote.
func headSHA(t *testing.T, bare, ref string) string {
	t.Helper()
	cmd := exec.Command("git", "--git-dir="+bare, "rev-parse", ref) // #nosec G204 -- fixed "git" binary; args are test-controlled fixture values
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git rev-parse %s: %v", ref, err)
	}
	return strings.TrimSpace(string(out))
}

// --- review: regressions found by review ---------------------------------

// A tracked .sops.yaml can be a symlink and survives checkout, so an
// unguarded write through it overwrites whatever it points at unnoticed.
func TestEnsureSopsConfigRefusesToWriteThroughASymlink(t *testing.T) {
	tmp := t.TempDir()
	workdir := filepath.Join(tmp, "workdir")
	if err := os.MkdirAll(workdir, 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	outside := filepath.Join(tmp, "outside.yaml")
	const sentinel = "homeassistant:\n  name: Home\n"
	if err := os.WriteFile(outside, []byte(sentinel), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(workdir, sopscrypt.ConfigFile)); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	gs := &GitSync{Workdir: workdir}
	enableEncryption(t, gs)

	if _, err := gs.ensureSopsConfig(); err == nil {
		t.Error("ensureSopsConfig() = nil error, want a refusal to write through a symlink escaping the worktree")
	}
	after, err := os.ReadFile(outside) // #nosec G304 -- test fixture under t.TempDir()
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(after) != sentinel {
		t.Errorf("the file outside the worktree was overwritten:\n%s", after)
	}
}

// A link resolving back inside the worktree is not an escape, but the
// managed config must still end up a real file.
func TestEnsureSopsConfigReplacesAnInBoundsSymlink(t *testing.T) {
	tmp := t.TempDir()
	workdir := filepath.Join(tmp, "workdir")
	if err := os.MkdirAll(workdir, 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	target := filepath.Join(workdir, "innocent.yaml")
	if err := os.WriteFile(target, []byte("keep: me\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(workdir, sopscrypt.ConfigFile)); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	gs := &GitSync{Workdir: workdir}
	enableEncryption(t, gs)

	if _, err := gs.ensureSopsConfig(); err != nil {
		t.Fatalf("ensureSopsConfig: %v", err)
	}
	kept, err := os.ReadFile(target) // #nosec G304 -- test fixture under t.TempDir()
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(kept) != "keep: me\n" {
		t.Errorf("wrote through the symlink, clobbering %s:\n%s", target, kept)
	}
}

// Both paths reach the same guard via copyLiveIntoWorkdir today; this
// catches a refactor that gives commit-back its own copy path.
func TestCommitBackFailsClosedWhenTheSwitchIsOnWithoutAKey(t *testing.T) {
	tmp := t.TempDir()
	bare, work := makeRemote(t, tmp, "remote")
	commitFile(t, work, "secrets.yaml", "mqtt_password: old\n", "init")

	configRoot := filepath.Join(tmp, "config")
	writeLiveText(t, configRoot, "secrets.yaml", "mqtt_password: NEWSECRET\n")

	gs := New(makeOpts("file://"+bare), filepath.Join(tmp, "clone"))
	ctx := context.Background()
	if err := gs.EnsureClone(ctx); err != nil {
		t.Fatalf("EnsureClone: %v", err)
	}
	sha, err := gs.Fetch(ctx)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if err := gs.Checkout(ctx, sha); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	SetEncryptionEnabled(true)
	t.Cleanup(func() { SetEncryptionEnabled(false) })
	// Deliberately no gs.Crypter: the half-wired shape.

	files := []DriftFile{{Path: "secrets.yaml", Kind: "update"}}
	_, err = gs.CommitBack(ctx, files, configRoot, sha, fixedDriftTime)
	if err == nil {
		t.Fatal("CommitBack() = nil error, want a refusal: encryption is on with no key to encrypt with")
	}
	if !strings.Contains(err.Error(), "age key") {
		t.Errorf("error = %v, want it to name the missing age key", err)
	}
	for _, branch := range listRemoteBranches(t, bare) {
		if strings.HasPrefix(branch, "gitops/drift-") {
			t.Fatalf("a drift branch was pushed anyway: %s", branch)
		}
	}
}

// sops re-emits from its own parse - quotes dropped, empties written as
// null - so a byte-exact currency check would rewrite on every import.
func TestImportSkipsReencryptingWhenSopsOnlyReformatted(t *testing.T) {
	tmp := t.TempDir()
	bare, work := makeRemote(t, tmp, "repo")
	commitFile(t, work, "README.md", "readme\n", "init")

	// Quoted value and an empty one: the two shapes sops normalizes.
	const live = "http_password: \"abc#123\"\nunused:\n"
	configRoot := filepath.Join(tmp, "config")
	writeLiveText(t, configRoot, "secrets.yaml", live)

	gs := importGitSync(t, bare, filepath.Join(tmp, "workdir"))
	fake := enableEncryption(t, gs)
	first, err := gs.Import(context.Background(), configRoot, generousLimits(), fixedImportTime)
	if err != nil {
		t.Fatalf("first Import: %v", err)
	}
	firstEncrypts, firstSHA := fake.encrypts, first.CommitSHA

	// Nothing changed live; the fake returns a normalized document on
	// decrypt, standing in for sops's re-serialization.
	fake.decryptRewrite = func(plaintext string) string {
		return strings.ReplaceAll(strings.ReplaceAll(plaintext, `"abc#123"`, "abc#123"), "unused:\n", "unused: null\n")
	}

	_, err = gs.Import(context.Background(), configRoot, generousLimits(), fixedImportTime.Add(time.Minute))
	if err == nil {
		t.Fatal("second Import() error = nil, want the nothing-to-import refusal")
	}
	if !strings.Contains(err.Error(), "nothing to import") {
		t.Errorf("second Import error = %v, want the nothing-to-import refusal", err)
	}
	if fake.encrypts != firstEncrypts {
		t.Errorf("encrypt calls = %d, want %d: the file was re-encrypted over formatting sops itself introduced", fake.encrypts, firstEncrypts)
	}
	if head := headSHA(t, bare, "main"); head != firstSHA {
		t.Errorf("main = %s, want it unmoved at %s: a commit that changes nothing was pushed", head, firstSHA)
	}
}

// --- VM e2e: found on real hardware, 2026-08-04 ---------------------------

// A real config hit thirteen such ESPHome files at once; failing on the
// first means thirteen rescan rounds for a list the scan already had.
func TestImportReportsEveryRefusalNotJustTheFirst(t *testing.T) {
	tmp := t.TempDir()
	bare, work := makeRemote(t, tmp, "repo")
	commitFile(t, work, "README.md", "readme\n", "init")

	configRoot := filepath.Join(tmp, "config")
	// A literal secret beside a !secret reference: the ESPHome shape.
	for _, name := range []string{"esphome/one.yaml", "esphome/two.yaml", "esphome/three.yaml"} {
		writeLiveText(t, configRoot, name, "api:\n  encryption:\n    key: LITERALSECRET\nwifi:\n  password: !secret wifi_password\n")
	}
	writeLiveText(t, configRoot, "configuration.yaml", "homeassistant:\n  name: Home\n")

	gs := importGitSync(t, bare, filepath.Join(tmp, "workdir"))
	enableEncryption(t, gs)

	_, err := gs.Import(context.Background(), configRoot, generousLimits(), fixedImportTime)
	if err == nil {
		t.Fatal("Import() = nil error, want a refusal naming the files that cannot be encrypted")
	}
	for _, name := range []string{"esphome/one.yaml", "esphome/two.yaml", "esphome/three.yaml"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error does not name %s, so the user would find it only on the next run:\n%v", name, err)
		}
	}
	// Nothing staged: a partial import reads as a complete snapshot while
	// missing the files that hold secrets.
	if _, ok := showAtRef(t, bare, "main", "configuration.yaml"); ok {
		t.Error("a partial import was pushed despite refused files")
	}
}

// "pin" and "*_pin" used to match, so a board full of GPIO assignments
// read as secret-bearing - most of the matches on the live config.
func TestGPIOPinsAreNotSecrets(t *testing.T) {
	content := "sensor:\n  - platform: dht\n    pin: GPIO4\n    cs_pin: GPIO5\n    i2s_bclk_pin: GPIO6\n"

	need, refusal := sopscrypt.NeedsEncryption("esphome/board.yaml", []byte(content))
	if need {
		t.Error("a config of GPIO pin assignments was treated as secret-bearing")
	}
	if refusal != "" {
		t.Errorf("refusal = %q, want none for ordinary hardware wiring", refusal)
	}
}

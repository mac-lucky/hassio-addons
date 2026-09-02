package recon

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/sopscrypt"
)

// testAgeIdentity exists only to satisfy sopscrypt.New's validation -
// fakeSops stands in for the binary, so nothing is really encrypted.
const testAgeIdentity = "AGE-SECRET-KEY-1QUUCUYTP2443EWJWQKK6LCAAUGS09XXGDHLVQV82Z2Y6200NDGAQJ8SUFT"

// encryptedFixture is sops-shaped enough for sopscrypt.IsEncrypted, which
// requires a key source next to the mac and version.
const encryptedFixture = "mqtt_password: ENC[AES256_GCM,data:xx]\nsops:\n    age:\n        - recipient: age1test\n          enc: x\n    mac: FAKEMAC\n    version: 3.13.2\n"

// fakeSops records every sops argv and answers with canned plaintext,
// so these tests never spawn a real process.
type fakeSops struct {
	calls   [][]string
	stdout  string
	stderr  string
	exit    int
	decrypt string
}

func (f *fakeSops) Run(_ context.Context, _ string, _ []string, args ...string) (sopscrypt.RunResult, error) {
	f.calls = append(f.calls, append([]string(nil), args...))
	if f.exit != 0 {
		return sopscrypt.RunResult{Stdout: f.stdout, Stderr: f.stderr, ExitCode: f.exit}, nil
	}
	return sopscrypt.RunResult{Stdout: f.decrypt}, nil
}

func newFakeCrypter(t *testing.T, fake *fakeSops) *sopscrypt.Crypter {
	t.Helper()
	crypter, err := sopscrypt.New(testAgeIdentity)
	if err != nil {
		t.Fatalf("sopscrypt.New: %v", err)
	}
	crypter.Runner = fake
	return crypter
}

// A passthrough transform here would remove differ.Compute's outright
// refusal of encrypted repository content.
func TestRepoDecryptTransformIsNilWithoutAKey(t *testing.T) {
	if transform := repoDecryptTransform(nil, "/data/repo"); transform != nil {
		t.Error("repoDecryptTransform(nil, ...) != nil, want nil so encrypted content stays a hard failure")
	}
	if applierRepoTransform(nil) != nil {
		t.Error("applierRepoTransform(nil) != nil, want nil so the applier copies as-is")
	}
}

func TestRepoDecryptTransformPassesPlaintextThrough(t *testing.T) {
	fake := &fakeSops{}
	transform := repoDecryptTransform(newFakeCrypter(t, fake), "/data/repo")

	in := []byte("homeassistant:\n  name: Home\n")
	out, encrypted, err := transform("configuration.yaml", in)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	if string(out) != string(in) {
		t.Errorf("out = %q, want the input unchanged", out)
	}
	if encrypted {
		t.Error("encrypted = true for plaintext, want false")
	}
	if len(fake.calls) != 0 {
		t.Errorf("sops calls = %v, want none for a file that is not ciphertext", fake.calls)
	}
}

// The transform gets a repo-relative path but sops works on a file, so
// the absolute path has to be rebuilt from the caller's root.
func TestRepoDecryptTransformDecryptsFromTheRepoRoot(t *testing.T) {
	fake := &fakeSops{decrypt: "mqtt_password: hunter2\n"}
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "secrets.yaml"), encryptedFixture)
	transform := repoDecryptTransform(newFakeCrypter(t, fake), root)

	out, encrypted, err := transform("secrets.yaml", []byte(encryptedFixture))
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	if string(out) != "mqtt_password: hunter2\n" {
		t.Errorf("out = %q, want the decrypted plaintext", out)
	}
	if !encrypted {
		t.Error("encrypted = false for ciphertext, want true - the diff for this file has to be masked")
	}
	if len(fake.calls) != 1 {
		t.Fatalf("sops calls = %d, want 1", len(fake.calls))
	}
	call := fake.calls[0]
	if want := filepath.Join(root, "secrets.yaml"); call[len(call)-1] != want {
		t.Errorf("sops argv = %q, want it to end in %q", call, want)
	}
}

// Reporting a failed decrypt as unchanged content would let the applier
// write ENC[...] into the live config.
func TestRepoDecryptTransformSurfacesFailures(t *testing.T) {
	fake := &fakeSops{exit: 1, stderr: "no key could decrypt the data"}
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "secrets.yaml"), encryptedFixture)
	transform := repoDecryptTransform(newFakeCrypter(t, fake), root)

	if _, _, err := transform("secrets.yaml", []byte(encryptedFixture)); err == nil {
		t.Fatal("transform() error = nil, want the decrypt failure")
	} else if !strings.Contains(err.Error(), "no key could decrypt") {
		t.Errorf("error = %v, want sops's own explanation", err)
	}
}

// The applier writes plaintext either way; only the differ, which decides
// how much of the diff to publish, needs the flag.
func TestApplierRepoTransformDropsTheEncryptedFlag(t *testing.T) {
	fake := &fakeSops{decrypt: "mqtt_password: hunter2\n"}
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "secrets.yaml"), encryptedFixture)
	transform := applierRepoTransform(repoDecryptTransform(newFakeCrypter(t, fake), root))

	out, err := transform("secrets.yaml", []byte(encryptedFixture))
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	if string(out) != "mqtt_password: hunter2\n" {
		t.Errorf("out = %q, want the decrypted plaintext", out)
	}
}

// --- review: the wiring itself was untested ------------------------------

// The transforms were covered; the wiring that reaches them was not.
func TestNewInstallsTheDecryptTransformInBothSeams(t *testing.T) {
	r := New(baseOpts(), Deps{Crypter: newFakeCrypter(t, &fakeSops{})})

	d, ok := r.differ.(realDiffer)
	if !ok {
		t.Fatalf("differ is %T, want realDiffer", r.differ)
	}
	if d.transform == nil {
		t.Error("differ has no decrypt transform: encrypted repository content would be compared as ciphertext")
	}
	a, ok := r.applier.(*realApplier)
	if !ok {
		t.Fatalf("applier is %T, want *realApplier", r.applier)
	}
	if a.cfg.TransformRepoFile == nil {
		t.Error("applier has no decrypt transform: ciphertext would be written into the config directory")
	}
}

// Nil is deliberate: it keeps differ.Compute's "encrypted content and no
// age_key" refusal reachable.
func TestNewLeavesTheTransformsNilWithoutAKey(t *testing.T) {
	r := New(baseOpts(), Deps{})

	if d, ok := r.differ.(realDiffer); ok && d.transform != nil {
		t.Error("differ has a decrypt transform with no age key configured")
	}
	if a, ok := r.applier.(*realApplier); ok && a.cfg.TransformRepoFile != nil {
		t.Error("applier has a decrypt transform with no age key configured")
	}
}

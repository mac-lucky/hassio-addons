package fsx

import (
	"os"
	"path/filepath"
	"testing"
)

// realTempDir is t.TempDir() with its symlinks resolved: on macOS /var
// is a symlink, so the raw temp dir is not a valid expected value.
func realTempDir(t *testing.T) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolving the temp dir: %v", err)
	}
	return resolved
}

// TestRealpathResolvesAnExistingSymlink is the plain case, the one
// filepath.EvalSymlinks already handles on its own.
func TestRealpathResolvesAnExistingSymlink(t *testing.T) {
	root := realTempDir(t)
	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatalf("writing the target file: %v", err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("creating the symlink: %v", err)
	}

	if got := Realpath(link); got != target {
		t.Errorf("Realpath(%q) = %q, want %q", link, got, target)
	}
}

// TestRealpathKeepsANonexistentTail is why this exists:
// filepath.EvalSymlinks fails outright on a path that does not exist,
// and an add's destination never does.
func TestRealpathKeepsANonexistentTail(t *testing.T) {
	root := realTempDir(t)
	want := filepath.Join(root, "not-written-yet.yaml")

	if got := Realpath(want); got != want {
		t.Errorf("Realpath(%q) = %q, want it returned unchanged", want, got)
	}
}

// TestRealpathResolvesASymlinkedParentOfANonexistentTail is the case
// every containment guard depends on: a directory symlink out of the
// guarded root with nothing written through it yet.
func TestRealpathResolvesASymlinkedParentOfANonexistentTail(t *testing.T) {
	root := realTempDir(t)
	outside := filepath.Join(root, "outside")
	if err := os.Mkdir(outside, 0o750); err != nil {
		t.Fatalf("creating the outside dir: %v", err)
	}
	inside := filepath.Join(root, "inside")
	if err := os.Mkdir(inside, 0o750); err != nil {
		t.Fatalf("creating the inside dir: %v", err)
	}
	link := filepath.Join(inside, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("creating the directory symlink: %v", err)
	}

	got := Realpath(filepath.Join(link, "secrets.yaml"))
	want := filepath.Join(outside, "secrets.yaml")
	if got != want {
		t.Errorf("Realpath() = %q, want %q - a symlinked parent must resolve even when the tail does not exist", got, want)
	}
}

// TestRealpathResolvesEveryMissingLevel: several tail levels can be
// absent, and resolution still has to reach the symlink above them.
func TestRealpathResolvesEveryMissingLevel(t *testing.T) {
	root := realTempDir(t)
	real := filepath.Join(root, "real")
	if err := os.Mkdir(real, 0o750); err != nil {
		t.Fatalf("creating the real dir: %v", err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("creating the directory symlink: %v", err)
	}

	got := Realpath(filepath.Join(link, "packages", "new", "demo.yaml"))
	want := filepath.Join(real, "packages", "new", "demo.yaml")
	if got != want {
		t.Errorf("Realpath() = %q, want %q", got, want)
	}
}

// TestRealpathNormalizesWithoutResolving covers the filepath.Clean half:
// nothing to resolve, just the cleaned string.
func TestRealpathNormalizesWithoutResolving(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"/no/such/place/./here/../file.yaml", "/no/such/place/file.yaml"},
		{"relative.yaml", "relative.yaml"},
		{"./relative.yaml", "relative.yaml"},
		{"/", "/"},
	}
	for _, tc := range cases {
		if got := Realpath(tc.in); got != tc.want {
			t.Errorf("Realpath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

package gitsync

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
)

// recordedRun captures one Runner.Run invocation.
type recordedRun struct {
	dir  string
	env  []string
	args []string
}

// fakeRunner records every call and returns canned success, so no real
// git process is ever spawned.
type fakeRunner struct {
	calls []recordedRun
}

func (f *fakeRunner) Run(_ context.Context, dir string, env []string, args ...string) (RunResult, error) {
	f.calls = append(f.calls, recordedRun{
		dir:  dir,
		env:  append([]string(nil), env...),
		args: append([]string(nil), args...),
	})
	if len(args) >= 2 && args[1] == "rev-parse" {
		return RunResult{Stdout: strings.Repeat("a", 40) + "\n"}, nil
	}
	return RunResult{}, nil
}

// The token reaches git only through the fetch env
// (GIT_CONFIG_COUNT/KEY_0/VALUE_0).
func TestFetchCredentialNeverInArgvOnlyInEnv(t *testing.T) {
	token := "ghp_TESTTOKEN123"
	opts := makeOpts("https://git.example.invalid/repo.git")
	opts.GitUsername = "agent"
	opts.GitToken = token

	gs := New(opts, "/unused/workdir")
	fr := &fakeRunner{}
	gs.Runner = fr

	sha, err := gs.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if sha == "" {
		t.Fatal("Fetch() returned an empty sha")
	}
	if len(fr.calls) != 2 {
		t.Fatalf("len(calls) = %d, want 2 (fetch, rev-parse)", len(fr.calls))
	}

	for _, call := range fr.calls {
		for _, a := range call.args {
			if strings.Contains(a, token) {
				t.Fatalf("token leaked into argv: %v", call.args)
			}
		}
	}

	fetchCall := fr.calls[0]
	if fetchCall.args[1] != "fetch" {
		t.Fatalf("calls[0].args = %v, want a fetch invocation", fetchCall.args)
	}

	var authValue string
	var hasCount, hasKey bool
	for _, kv := range fetchCall.env {
		switch {
		case kv == "GIT_CONFIG_COUNT=1":
			hasCount = true
		case kv == "GIT_CONFIG_KEY_0=http.extraheader":
			hasKey = true
		case strings.HasPrefix(kv, "GIT_CONFIG_VALUE_0="):
			authValue = strings.TrimPrefix(kv, "GIT_CONFIG_VALUE_0=")
		}
	}
	if !hasCount || !hasKey {
		t.Errorf("fetch env missing GIT_CONFIG_COUNT/KEY_0: %v", fetchCall.env)
	}
	if authValue == "" {
		t.Fatal("fetch env missing GIT_CONFIG_VALUE_0")
	}
	wantB64 := base64.StdEncoding.EncodeToString([]byte("agent:" + token))
	if authValue != "Authorization: Basic "+wantB64 {
		t.Errorf("GIT_CONFIG_VALUE_0 = %q, want %q", authValue, "Authorization: Basic "+wantB64)
	}

	// The credential env is scoped to the fetch, not to any later call.
	revParseCall := fr.calls[1]
	for _, kv := range revParseCall.env {
		if strings.HasPrefix(kv, "GIT_CONFIG_COUNT") || strings.HasPrefix(kv, "GIT_CONFIG_KEY_0") || strings.HasPrefix(kv, "GIT_CONFIG_VALUE_0") {
			t.Errorf("non-fetch call %v carries credential env: %v", revParseCall.args, revParseCall.env)
		}
	}
}

// The clone predates every fetch, so an unauthenticated one leaves a
// private repo unusable from a cold start. Its argv stays token-free.
func TestEnsureCloneSendsCredentialInEnv(t *testing.T) {
	token := "ghp_CLONETOKEN456"
	opts := makeOpts("https://git.example.invalid/repo.git")
	opts.GitUsername = "agent"
	opts.GitToken = token

	gs := New(opts, t.TempDir()+"/repo")
	fr := &fakeRunner{}
	gs.Runner = fr

	if err := gs.EnsureClone(context.Background()); err != nil {
		t.Fatalf("EnsureClone: %v", err)
	}
	if len(fr.calls) == 0 {
		t.Fatal("EnsureClone ran no git command")
	}

	cloneCall := fr.calls[0]
	if cloneCall.args[1] != "clone" {
		t.Fatalf("calls[0].args = %v, want a clone invocation", cloneCall.args)
	}
	for _, a := range cloneCall.args {
		if strings.Contains(a, token) {
			t.Fatalf("token leaked into clone argv: %v", cloneCall.args)
		}
	}

	wantValue := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("agent:"+token))
	var gotValue string
	var hasCount, hasKey bool
	for _, kv := range cloneCall.env {
		switch {
		case kv == "GIT_CONFIG_COUNT=1":
			hasCount = true
		case kv == "GIT_CONFIG_KEY_0=http.extraheader":
			hasKey = true
		case strings.HasPrefix(kv, "GIT_CONFIG_VALUE_0="):
			gotValue = strings.TrimPrefix(kv, "GIT_CONFIG_VALUE_0=")
		}
	}
	if !hasCount || !hasKey {
		t.Errorf("clone env missing GIT_CONFIG_COUNT/KEY_0: %v", cloneCall.env)
	}
	if gotValue != wantValue {
		t.Errorf("GIT_CONFIG_VALUE_0 = %q, want %q", gotValue, wantValue)
	}

	// Only the clone carries it; the config calls that follow reach no remote.
	for _, call := range fr.calls[1:] {
		for _, kv := range call.env {
			if strings.HasPrefix(kv, "GIT_CONFIG_COUNT") {
				t.Errorf("post-clone call %v carries credential env: %v", call.args, call.env)
			}
		}
	}
}

func TestFetchNoTokenOmitsCredentialEnv(t *testing.T) {
	gs := New(makeOpts("https://git.example.invalid/repo.git"), "/unused/workdir")
	fr := &fakeRunner{}
	gs.Runner = fr

	if _, err := gs.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	for _, call := range fr.calls {
		for _, kv := range call.env {
			if strings.HasPrefix(kv, "GIT_CONFIG_COUNT") {
				t.Errorf("call %v carries GIT_CONFIG_COUNT with no token configured: %v", call.args, call.env)
			}
		}
	}
}

// git/curl echo the header value back verbatim, so the base64
// "user:token" blob has to be scrubbed as well as the raw token.
func TestRedactCredentialsStripsBase64Blob(t *testing.T) {
	token := "ghp_TESTTOKEN123"
	opts := makeOpts("https://git.example.invalid/repo.git")
	opts.GitUsername = "agent"
	opts.GitToken = token
	gs := New(opts, "/unused/workdir")

	blob := base64.StdEncoding.EncodeToString([]byte("agent:" + token))
	text := "fatal: could not read Response code, HTTP header Authorization: Basic " + blob + " rejected"

	redacted := gs.redactCredentials(text)
	if strings.Contains(redacted, blob) {
		t.Errorf("redactCredentials() = %q, still contains base64 credential blob", redacted)
	}
	if strings.Contains(redacted, token) {
		t.Errorf("redactCredentials() = %q, still contains raw token", redacted)
	}
	if !strings.Contains(redacted, "***REDACTED***") {
		t.Errorf("redactCredentials() = %q, want a ***REDACTED*** marker", redacted)
	}
}

func TestRedactCredentialsNoTokenIsNoop(t *testing.T) {
	gs := New(makeOpts("https://git.example.invalid/repo.git"), "/unused/workdir")
	text := "fatal: repository not found"
	if got := gs.redactCredentials(text); got != text {
		t.Errorf("redactCredentials() = %q, want unchanged %q", got, text)
	}
}

// An inherited GIT_TRACE*/GIT_CURL_VERBOSE makes git dump the
// Authorization header to stderr, bypassing redactCredentials.
func TestGitEnvStripsInheritedDebugVars(t *testing.T) {
	t.Setenv("GIT_TRACE", "1")
	t.Setenv("GIT_TRACE_CURL", "2")
	t.Setenv("GIT_TRACE_CURL_NO_DATA", "1")
	t.Setenv("GIT_CURL_VERBOSE", "1")

	gs := New(makeOpts("https://git.example.invalid/repo.git"), "/unused/workdir")
	env := gs.gitEnv(nil)

	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		if key == "GIT_CURL_VERBOSE" || strings.HasPrefix(key, "GIT_TRACE") {
			t.Errorf("gitEnv() leaked inherited debug var: %q", kv)
		}
	}
}

func TestGitEnvKeepsOwnConfigAfterStrippingDebugVars(t *testing.T) {
	t.Setenv("GIT_TRACE", "1")

	gs := New(makeOpts("https://git.example.invalid/repo.git"), "/unused/workdir")
	env := gs.gitEnv(nil)

	var hasPrompt bool
	for _, kv := range env {
		if kv == "GIT_TERMINAL_PROMPT=0" {
			hasPrompt = true
		}
	}
	if !hasPrompt {
		t.Errorf("gitEnv() = %v, want GIT_TERMINAL_PROMPT=0", env)
	}
}

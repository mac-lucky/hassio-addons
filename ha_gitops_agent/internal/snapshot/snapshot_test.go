package snapshot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// jsonResponse builds a scripted *http.Response with a JSON body.
func jsonResponse(t *testing.T, status int, body any) *http.Response {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal fixture response: %v", err)
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewReader(data))}
}

// fakeClient scripts or records every HTTP verb this package uses; no
// network access.
type fakeClient struct {
	postResponse *http.Response
	getResponse  *http.Response
	postErr      error
	getErr       error

	// onPost, when set, sees the outgoing request before the scripted
	// response - the only way to inspect its context deadline.
	onPost func(*http.Request)

	postedURLs []string
	getURLs    []string
	deleted    []string
}

func (f *fakeClient) Do(req *http.Request) (*http.Response, error) {
	switch req.Method {
	case http.MethodPost:
		f.postedURLs = append(f.postedURLs, req.URL.String())
		if f.onPost != nil {
			f.onPost(req)
		}
		if f.postErr != nil {
			return nil, f.postErr
		}
		return f.postResponse, nil
	case http.MethodGet:
		f.getURLs = append(f.getURLs, req.URL.String())
		if f.getErr != nil {
			return nil, f.getErr
		}
		return f.getResponse, nil
	case http.MethodDelete:
		f.deleted = append(f.deleted, req.URL.String())
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader([]byte("{}")))}, nil
	default:
		return nil, errors.New("fakeClient: unexpected method " + req.Method)
	}
}

func TestPreApplyBackupReturnsSlugOnSuccess(t *testing.T) {
	t.Setenv("SUPERVISOR_TOKEN", "test-token")
	client := &fakeClient{
		postResponse: jsonResponse(t, 200, map[string]any{"result": "ok", "data": map[string]any{"slug": "abc123"}}),
	}

	slug, err := PreApplyBackup(context.Background(), client)
	if err != nil {
		t.Fatalf("PreApplyBackup() error = %v, want nil", err)
	}
	if slug != "abc123" {
		t.Errorf("PreApplyBackup() = %q, want %q", slug, "abc123")
	}
	if len(client.postedURLs) != 1 || client.postedURLs[0] != "http://supervisor/backups/new/partial" {
		t.Errorf("postedURLs = %v, want [http://supervisor/backups/new/partial]", client.postedURLs)
	}
}

func TestPreApplyBackupReturnsEmptyOnHTTPError(t *testing.T) {
	t.Setenv("SUPERVISOR_TOKEN", "test-token")
	client := &fakeClient{postResponse: jsonResponse(t, 503, map[string]any{})}

	slug, err := PreApplyBackup(context.Background(), client)
	if slug != "" {
		t.Errorf("PreApplyBackup() = %q, want empty", slug)
	}
	if err == nil {
		t.Fatal("PreApplyBackup() error = nil, want an error naming the status")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("PreApplyBackup() error = %q, want it to name the 503", err)
	}
}

func TestPreApplyBackupReturnsEmptyOnTransportError(t *testing.T) {
	t.Setenv("SUPERVISOR_TOKEN", "test-token")
	transportErr := errors.New("connection refused")
	client := &fakeClient{postErr: transportErr}

	slug, err := PreApplyBackup(context.Background(), client)
	if slug != "" {
		t.Errorf("PreApplyBackup() = %q, want empty", slug)
	}
	if !errors.Is(err, transportErr) {
		t.Errorf("PreApplyBackup() error = %v, want it to wrap %v", err, transportErr)
	}
}

// A large install times out here, so the error has to name the deadline:
// "context deadline exceeded" alone tells a user nothing.
func TestPreApplyBackupErrorNamesTheBackupTimeout(t *testing.T) {
	t.Setenv("SUPERVISOR_TOKEN", "test-token")
	prev := BackupTimeout
	BackupTimeout = 90 * time.Second
	t.Cleanup(func() { BackupTimeout = prev })

	client := &fakeClient{postErr: context.DeadlineExceeded}

	_, err := PreApplyBackup(context.Background(), client)
	if err == nil {
		t.Fatal("PreApplyBackup() error = nil, want a timeout error")
	}
	if !strings.Contains(err.Error(), "1m30s") {
		t.Errorf("PreApplyBackup() error = %q, want it to name the 1m30s BackupTimeout", err)
	}
}

// PreApplyBackup must use BackupTimeout, not Prune's much shorter
// RequestTimeout; nothing else observes the difference.
func TestPreApplyBackupUsesBackupTimeoutNotRequestTimeout(t *testing.T) {
	t.Setenv("SUPERVISOR_TOKEN", "test-token")
	prevBackup, prevRequest := BackupTimeout, RequestTimeout
	BackupTimeout = time.Hour
	RequestTimeout = time.Nanosecond
	t.Cleanup(func() { BackupTimeout, RequestTimeout = prevBackup, prevRequest })

	var deadline time.Time
	client := &fakeClient{
		postResponse: jsonResponse(t, 200, map[string]any{"result": "ok", "data": map[string]any{"slug": "abc123"}}),
		onPost:       func(req *http.Request) { deadline, _ = req.Context().Deadline() },
	}

	if _, err := PreApplyBackup(context.Background(), client); err != nil {
		t.Fatalf("PreApplyBackup() error = %v, want nil", err)
	}
	if remaining := time.Until(deadline); remaining < 30*time.Minute {
		t.Errorf("request deadline is %v away, want ~1h (BackupTimeout), not RequestTimeout", remaining)
	}
}

func TestPreApplyBackupReturnsEmptyWhenSlugMissing(t *testing.T) {
	t.Setenv("SUPERVISOR_TOKEN", "test-token")
	client := &fakeClient{postResponse: jsonResponse(t, 200, map[string]any{"result": "ok", "data": map[string]any{}})}

	slug, err := PreApplyBackup(context.Background(), client)
	if slug != "" {
		t.Errorf("PreApplyBackup() = %q, want empty", slug)
	}
	if err == nil {
		t.Error("PreApplyBackup() error = nil, want an error for the missing slug")
	}
}

func TestPreApplyBackupReturnsEmptyWithoutSupervisorToken(t *testing.T) {
	t.Setenv("SUPERVISOR_TOKEN", "")
	client := &fakeClient{}

	slug, err := PreApplyBackup(context.Background(), client)
	if slug != "" {
		t.Errorf("PreApplyBackup() = %q, want empty", slug)
	}
	if err == nil {
		t.Error("PreApplyBackup() error = nil, want an error for the missing token")
	}
	if len(client.postedURLs) != 0 {
		t.Errorf("postedURLs = %v, want none (no request without a token)", client.postedURLs)
	}
}

func backupsResponse(t *testing.T, backups []map[string]any) *http.Response {
	t.Helper()
	return jsonResponse(t, 200, map[string]any{"result": "ok", "data": map[string]any{"backups": backups}})
}

func TestPruneKeepsNewestAndDeletesOnlyPrefixed(t *testing.T) {
	t.Setenv("SUPERVISOR_TOKEN", "test-token")
	backups := []map[string]any{
		{"slug": "keep1", "name": "gitops-agent pre-apply 2026-01-05T00:00:00Z", "date": "2026-01-05"},
		{"slug": "keep2", "name": "gitops-agent pre-apply 2026-01-04T00:00:00Z", "date": "2026-01-04"},
		{"slug": "keep3", "name": "gitops-agent pre-apply 2026-01-03T00:00:00Z", "date": "2026-01-03"},
		{"slug": "old1", "name": "gitops-agent pre-apply 2026-01-02T00:00:00Z", "date": "2026-01-02"},
		{"slug": "old2", "name": "gitops-agent pre-apply 2026-01-01T00:00:00Z", "date": "2026-01-01"},
		{"slug": "manual1", "name": "manual backup before update", "date": "2026-01-06"},
		{"slug": "other-addon", "name": "some other addon's backup", "date": "2026-01-06"},
	}
	client := &fakeClient{getResponse: backupsResponse(t, backups)}

	Prune(3, client)

	want := map[string]bool{
		"http://supervisor/backups/old1": true,
		"http://supervisor/backups/old2": true,
	}
	if len(client.deleted) != len(want) {
		t.Fatalf("deleted = %v, want %v", client.deleted, want)
	}
	for _, url := range client.deleted {
		if !want[url] {
			t.Errorf("unexpected delete: %s", url)
		}
	}
}

func TestPruneWithFewerThanKeepDeletesNothing(t *testing.T) {
	t.Setenv("SUPERVISOR_TOKEN", "test-token")
	backups := []map[string]any{
		{"slug": "a", "name": "gitops-agent pre-apply 2026-01-02T00:00:00Z", "date": "2026-01-02"},
		{"slug": "b", "name": "gitops-agent pre-apply 2026-01-01T00:00:00Z", "date": "2026-01-01"},
	}
	client := &fakeClient{getResponse: backupsResponse(t, backups)}

	Prune(5, client)

	if len(client.deleted) != 0 {
		t.Errorf("deleted = %v, want none", client.deleted)
	}
}

func TestPruneNeverDeletesUnprefixedEvenIfItWouldBeEvicted(t *testing.T) {
	t.Setenv("SUPERVISOR_TOKEN", "test-token")
	backups := []map[string]any{
		{"slug": "not-ours", "name": "totally unrelated backup", "date": "2026-01-09"},
	}
	client := &fakeClient{getResponse: backupsResponse(t, backups)}

	Prune(0, client)

	if len(client.deleted) != 0 {
		t.Errorf("deleted = %v, want none", client.deleted)
	}
}

func TestPruneHandlesListHTTPErrorWithoutPanicking(t *testing.T) {
	t.Setenv("SUPERVISOR_TOKEN", "test-token")
	client := &fakeClient{getResponse: jsonResponse(t, 503, map[string]any{})}

	Prune(3, client) // must not panic

	if len(client.deleted) != 0 {
		t.Errorf("deleted = %v, want none", client.deleted)
	}
}

func TestPruneHandlesTransportErrorWithoutPanicking(t *testing.T) {
	t.Setenv("SUPERVISOR_TOKEN", "test-token")
	client := &fakeClient{getErr: errors.New("connection refused")}

	Prune(3, client) // must not panic

	if len(client.deleted) != 0 {
		t.Errorf("deleted = %v, want none", client.deleted)
	}
}

func TestPruneReturnsEarlyWithoutSupervisorToken(t *testing.T) {
	t.Setenv("SUPERVISOR_TOKEN", "")
	client := &fakeClient{getResponse: backupsResponse(t, nil)}

	Prune(3, client)

	if len(client.getURLs) != 0 {
		t.Errorf("getURLs = %v, want none (no request without a token)", client.getURLs)
	}
}

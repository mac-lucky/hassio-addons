package statusd

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/options"
)

// recordedCall captures one Do() invocation.
type recordedCall struct {
	url         string
	body        map[string]any
	authz       string
	hasDeadline bool
}

// fakeClient records every call and returns a scripted response or error.
type fakeClient struct {
	status int
	err    error
	calls  []recordedCall
}

func (f *fakeClient) Do(req *http.Request) (*http.Response, error) {
	var body map[string]any
	if req.Body != nil {
		raw, _ := io.ReadAll(req.Body)
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &body); err != nil {
				panic(err)
			}
		}
	}
	_, hasDeadline := req.Context().Deadline()
	f.calls = append(f.calls, recordedCall{
		url:         req.URL.String(),
		body:        body,
		authz:       req.Header.Get("Authorization"),
		hasDeadline: hasDeadline,
	})
	if f.err != nil {
		return nil, f.err
	}
	status := f.status
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader("")),
	}, nil
}

func TestPushValidStatePostsExpectedBodyAndAuthHeader(t *testing.T) {
	t.Setenv("SUPERVISOR_TOKEN", "test-token-123")
	client := &fakeClient{}

	ok, err := Push("drift_pending", map[string]any{"last_sha": "abc1234", "pending_changes": 2}, client)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if !ok {
		t.Fatal("Push() = false, want true")
	}
	if len(client.calls) != 1 {
		t.Fatalf("len(calls) = %d, want 1", len(client.calls))
	}

	call := client.calls[0]
	if !strings.HasSuffix(call.url, "/core/api/states/sensor.gitops_agent_status") {
		t.Errorf("url = %q, want suffix /core/api/states/sensor.gitops_agent_status", call.url)
	}
	if call.authz != "Bearer test-token-123" {
		t.Errorf("Authorization = %q, want %q", call.authz, "Bearer test-token-123")
	}
	if !call.hasDeadline {
		t.Error("request context has no deadline, want a bounded timeout")
	}
	if call.body["state"] != "drift_pending" {
		t.Errorf("body.state = %v, want drift_pending", call.body["state"])
	}
	attrs, ok := call.body["attributes"].(map[string]any)
	if !ok {
		t.Fatalf("body.attributes = %T, want map[string]any", call.body["attributes"])
	}
	if attrs["last_sha"] != "abc1234" {
		t.Errorf("attributes.last_sha = %v, want abc1234", attrs["last_sha"])
	}
	if attrs["pending_changes"] != float64(2) {
		t.Errorf("attributes.pending_changes = %v, want 2", attrs["pending_changes"])
	}
	if attrs["friendly_name"] != "GitOps Agent Status" {
		t.Errorf("attributes.friendly_name = %v, want %q", attrs["friendly_name"], "GitOps Agent Status")
	}
	if attrs["icon"] != "mdi:source-branch" {
		t.Errorf("attributes.icon = %v, want %q", attrs["icon"], "mdi:source-branch")
	}
}

func TestPushInvalidStateReturnsErrorAndMakesNoCall(t *testing.T) {
	t.Setenv("SUPERVISOR_TOKEN", "test-token-123")
	client := &fakeClient{}

	_, err := Push("not_a_real_state", map[string]any{}, client)
	if err == nil {
		t.Fatal("Push() error = nil, want an error")
	}
	if len(client.calls) != 0 {
		t.Errorf("len(calls) = %d, want 0", len(client.calls))
	}
}

func TestPushHTTPFailureReturnsFalseAndNoError(t *testing.T) {
	t.Setenv("SUPERVISOR_TOKEN", "test-token-123")
	client := &fakeClient{err: errors.New("connection refused")}

	ok, err := Push("error", map[string]any{"error": "boom"}, client)
	if err != nil {
		t.Fatalf("Push() error = %v, want nil", err)
	}
	if ok {
		t.Error("Push() = true, want false")
	}
}

func TestPushNon2xxResponseReturnsFalseAndNoError(t *testing.T) {
	t.Setenv("SUPERVISOR_TOKEN", "test-token-123")
	client := &fakeClient{status: http.StatusInternalServerError}

	ok, err := Push("in_sync", map[string]any{}, client)
	if err != nil {
		t.Fatalf("Push() error = %v, want nil", err)
	}
	if ok {
		t.Error("Push() = true, want false")
	}
}

func TestPushMissingSupervisorTokenReturnsFalseAndMakesNoCall(t *testing.T) {
	t.Setenv("SUPERVISOR_TOKEN", "")
	client := &fakeClient{}

	ok, err := Push("in_sync", map[string]any{}, client)
	if err != nil {
		t.Fatalf("Push() error = %v, want nil", err)
	}
	if ok {
		t.Error("Push() = true, want false")
	}
	if len(client.calls) != 0 {
		t.Errorf("len(calls) = %d, want 0", len(client.calls))
	}
}

// Pins options.Supervisor to the URL the docs quote.
func TestSupervisorBaseURL(t *testing.T) {
	if options.Supervisor != "http://supervisor" {
		t.Errorf("options.Supervisor = %q, want %q", options.Supervisor, "http://supervisor")
	}
}

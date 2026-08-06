package regapply

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// --- extra fakes -----------------------------------------------------------
//
// fakeAddonHTTP (addonopts_test.go) always emits a valid JSON envelope, so
// it cannot express the three cases here: a specific Supervisor message, a
// non-JSON body, and a call that never answers.

type addonUpdateDoerFunc func(*http.Request) (*http.Response, error)

func (f addonUpdateDoerFunc) Do(req *http.Request) (*http.Response, error) { return f(req) }

func addonUpdateRawResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}

// blockingAddonUpdateDoer waits for the request context to be cancelled
// and reports that, the way net/http's client does on a deadline.
func blockingAddonUpdateDoer() addonUpdateDoerFunc {
	return func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	}
}

// --- FetchAddonUpdateInfo: the shapes Supervisor's ok/error envelope covers --

func TestFetchAddonUpdateInfo(t *testing.T) {
	tests := []struct {
		name          string
		status        int
		data          map[string]any
		want          AddonUpdateInfo
		wantNotInsErr bool
		wantErrParts  []string
	}{
		{
			name:   "update available",
			status: 200,
			data: map[string]any{
				"name": "ESPHome Device Builder", "version": "2025.7.1",
				"version_latest": "2025.8.0", "update_available": true,
			},
			want: AddonUpdateInfo{
				Slug: "a0d7b954_esphome", Name: "ESPHome Device Builder",
				Version: "2025.7.1", VersionLatest: "2025.8.0", UpdateAvailable: true,
			},
		},
		{
			name:   "already up to date",
			status: 200,
			data: map[string]any{
				"name": "ESPHome Device Builder", "version": "2025.8.0",
				"version_latest": "2025.8.0", "update_available": false,
			},
			want: AddonUpdateInfo{
				Slug: "a0d7b954_esphome", Name: "ESPHome Device Builder",
				Version: "2025.8.0", VersionLatest: "2025.8.0", UpdateAvailable: false,
			},
		},
		{
			name:   "null version means the add-on is not installed",
			status: 200,
			data: map[string]any{
				"name": "ESPHome Device Builder", "version": nil,
				"version_latest": "2025.8.0", "update_available": false,
			},
			wantNotInsErr: true,
		},
		{
			name:   "explicit installed false means the add-on is not installed",
			status: 200,
			data: map[string]any{
				"name": "ESPHome Device Builder", "installed": false,
				"version": "2025.8.0", "version_latest": "2025.8.0",
			},
			wantNotInsErr: true,
		},
		{
			name:          "404 means the add-on is not installed",
			status:        404,
			wantNotInsErr: true,
		},
		{
			name:         "500 surfaces the status and supervisor's own message",
			status:       500,
			wantErrParts: []string{"a0d7b954_esphome", "info returned HTTP 500", "boom"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setSupervisorToken(t)
			client := newFakeAddonHTTP()
			client.queueResponse("GET", "/addons/a0d7b954_esphome/info", tt.status, tt.data)

			got, err := FetchAddonUpdateInfo(context.Background(), client, "a0d7b954_esphome")

			switch {
			case tt.wantNotInsErr:
				if !errors.Is(err, ErrAddonNotInstalled) {
					t.Fatalf("err = %v, want errors.Is(err, ErrAddonNotInstalled)", err)
				}
				if !strings.Contains(err.Error(), "a0d7b954_esphome") {
					t.Errorf("err = %q, want the slug named in it", err)
				}
			case len(tt.wantErrParts) > 0:
				if err == nil {
					t.Fatalf("err = nil, want a failure")
				}
				if errors.Is(err, ErrAddonNotInstalled) {
					t.Errorf("err = %v, want a plain failure, not ErrAddonNotInstalled", err)
				}
				for _, part := range tt.wantErrParts {
					if !strings.Contains(err.Error(), part) {
						t.Errorf("err = %q, want it to mention %q", err, part)
					}
				}
			default:
				if err != nil {
					t.Fatalf("err = %v", err)
				}
				if got != tt.want {
					t.Errorf("got %+v, want %+v", got, tt.want)
				}
			}
		})
	}
}

// --- FetchAddonUpdateInfo: bodies the shared harness cannot produce ---------

func TestFetchAddonUpdateInfoRawBodies(t *testing.T) {
	tests := []struct {
		name          string
		status        int
		body          string
		wantNotInsErr bool
		wantErrParts  []string
	}{
		{
			// Why the message match exists: the store routes answer an
			// unknown slug with a 400 rather than a 404.
			name:          "400 saying the add-on does not exist",
			status:        400,
			body:          `{"result":"error","message":"Addon ghost_addon does not exist"}`,
			wantNotInsErr: true,
		},
		{
			name:          "400 saying the add-on is not installed",
			status:        400,
			body:          `{"result":"error","message":"Addon ghost_addon is not installed"}`,
			wantNotInsErr: true,
		},
		{
			name:          "the match is case-insensitive",
			status:        400,
			body:          `{"result":"error","message":"Addon ghost_addon Does Not Exist"}`,
			wantNotInsErr: true,
		},
		{
			// The defensive half of notInstalledMarkers: a fork or a
			// rewording degrades to "not installed", not an HTTP failure.
			name:          "400 using wording only a fork would emit",
			status:        400,
			body:          `{"result":"error","message":"Unknown addon ghost_addon"}`,
			wantNotInsErr: true,
		},
		{
			// A 400 alone is not a not-installed answer, or the caller
			// renders "not installed" for a working add-on.
			name:         "400 saying something else is a real failure",
			status:       400,
			body:         `{"result":"error","message":"Invalid request body"}`,
			wantErrParts: []string{"info returned HTTP 400", "Invalid request body"},
		},
		{
			// A shape Supervisor never produces (proxy, stub, moved route),
			// not a missing add-on - so not a manifest problem to report.
			name:         "a 200 whose envelope carries no data object",
			status:       200,
			body:         `{"result":"ok"}`,
			wantErrParts: []string{"a0d7b954_esphome", "info response carried no data object"},
		},
		{
			name:         "a 200 whose data is explicitly null",
			status:       200,
			body:         `{"result":"ok","data":null}`,
			wantErrParts: []string{"info response carried no data object"},
		},
		{
			name:         "invalid JSON on a 200",
			status:       200,
			body:         `{"result":"ok","data":{"version":`,
			wantErrParts: []string{"a0d7b954_esphome", "info returned invalid JSON"},
		},
		{
			name:         "a non-JSON body on a 200",
			status:       200,
			body:         "502 Bad Gateway",
			wantErrParts: []string{"info returned invalid JSON"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setSupervisorToken(t)
			client := addonUpdateDoerFunc(func(_ *http.Request) (*http.Response, error) {
				return addonUpdateRawResponse(tt.status, tt.body), nil
			})

			_, err := FetchAddonUpdateInfo(context.Background(), client, "a0d7b954_esphome")

			if err == nil {
				t.Fatalf("err = nil, want a failure")
			}
			if tt.wantNotInsErr {
				if !errors.Is(err, ErrAddonNotInstalled) {
					t.Fatalf("err = %v, want errors.Is(err, ErrAddonNotInstalled)", err)
				}
				return
			}
			if errors.Is(err, ErrAddonNotInstalled) {
				t.Errorf("err = %v, want a plain failure, not ErrAddonNotInstalled", err)
			}
			for _, part := range tt.wantErrParts {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("err = %q, want it to mention %q", err, part)
				}
			}
		})
	}
}

func TestFetchAddonUpdateInfoRequestShape(t *testing.T) {
	setSupervisorToken(t)
	var seen *http.Request
	var seenDeadline time.Time
	var seenHasDeadline bool
	client := addonUpdateDoerFunc(func(req *http.Request) (*http.Response, error) {
		seen = req
		// Read it in flight: FetchAddonUpdateInfo cancels the context on exit.
		seenDeadline, seenHasDeadline = req.Context().Deadline()
		return addonUpdateRawResponse(200, `{"result":"ok","data":{"version":"1.0","version_latest":"1.0"}}`), nil
	})

	if _, err := FetchAddonUpdateInfo(context.Background(), client, "a0d7b954_esphome"); err != nil {
		t.Fatalf("err = %v", err)
	}

	if seen == nil {
		t.Fatal("no request was made")
	}
	if seen.Method != http.MethodGet {
		t.Errorf("method = %q, want GET", seen.Method)
	}
	if want := "http://supervisor/addons/a0d7b954_esphome/info"; seen.URL.String() != want {
		t.Errorf("url = %q, want %q", seen.URL, want)
	}
	if got := seen.Header.Get("Authorization"); got != "Bearer test-token" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer test-token")
	}
	// addonUpdateInfoTimeout is a const no test can shrink to watch it fire,
	// so assert the deadline reached the request context instead.
	deadline, ok := seenDeadline, seenHasDeadline
	if !ok {
		t.Fatal("the request context carried no deadline, want addonUpdateInfoTimeout applied")
	}
	if remaining := time.Until(deadline); remaining <= 0 || remaining > addonUpdateInfoTimeout {
		t.Errorf("deadline is %s out, want it within addonUpdateInfoTimeout (%s)", remaining, addonUpdateInfoTimeout)
	}
}

func TestFetchAddonUpdateInfoMissingTokenErrors(t *testing.T) {
	t.Setenv("SUPERVISOR_TOKEN", "")
	client := newFakeAddonHTTP()

	_, err := FetchAddonUpdateInfo(context.Background(), client, "a0d7b954_esphome")

	if err == nil {
		t.Fatal("err = nil, want a failure")
	}
	if len(client.calls) != 0 {
		t.Errorf("calls = %+v, want none - there is no token to authenticate them with", client.calls)
	}
}

func TestFetchAddonUpdateInfoRespectsTheCallersDeadline(t *testing.T) {
	setSupervisorToken(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err := FetchAddonUpdateInfo(ctx, blockingAddonUpdateDoer(), "a0d7b954_esphome")

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want errors.Is(err, context.DeadlineExceeded)", err)
	}
}

// --- UpdateAddon ------------------------------------------------------------

func TestUpdateAddon(t *testing.T) {
	tests := []struct {
		name         string
		status       int
		wantErrParts []string
	}{
		{name: "200 is a success", status: 200},
		{
			name:         "400 surfaces the status and supervisor's own message",
			status:       400,
			wantErrParts: []string{"a0d7b954_esphome", "update returned HTTP 400", "boom"},
		},
		{
			name:         "500 surfaces the status and supervisor's own message",
			status:       500,
			wantErrParts: []string{"update returned HTTP 500", "boom"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setSupervisorToken(t)
			client := newFakeAddonHTTP()
			client.queueResponse("POST", "/store/addons/a0d7b954_esphome/update", tt.status, nil)

			err := UpdateAddon(context.Background(), client, "a0d7b954_esphome")

			if len(tt.wantErrParts) == 0 {
				if err != nil {
					t.Fatalf("err = %v", err)
				}
			} else {
				if err == nil {
					t.Fatalf("err = nil, want a failure")
				}
				for _, part := range tt.wantErrParts {
					if !strings.Contains(err.Error(), part) {
						t.Errorf("err = %q, want it to mention %q", err, part)
					}
				}
			}
			// Count all calls, not just the store route: the deprecated
			// POST /addons/<slug>/update must not go out alongside it.
			if len(client.calls) != 1 {
				t.Fatalf("calls = %+v, want exactly 1", client.calls)
			}
			if calls := client.callsFor("POST", "/store/addons/a0d7b954_esphome/update"); len(calls) != 1 {
				t.Errorf("calls = %+v, want the one call to be POST to the store update route", client.calls)
			}
		})
	}
}

func TestUpdateAddonRequestShape(t *testing.T) {
	setSupervisorToken(t)
	var seen *http.Request
	var seenBody []byte
	client := addonUpdateDoerFunc(func(req *http.Request) (*http.Response, error) {
		seen = req
		if req.Body != nil {
			seenBody, _ = io.ReadAll(req.Body)
		}
		return addonUpdateRawResponse(200, `{"result":"ok","data":{}}`), nil
	})

	if err := UpdateAddon(context.Background(), client, "a0d7b954_esphome"); err != nil {
		t.Fatalf("err = %v", err)
	}

	if seen == nil {
		t.Fatal("no request was made")
	}
	if seen.Method != http.MethodPost {
		t.Errorf("method = %q, want POST", seen.Method)
	}
	if want := "http://supervisor/store/addons/a0d7b954_esphome/update"; seen.URL.String() != want {
		t.Errorf("url = %q, want %q", seen.URL, want)
	}
	if got := seen.Header.Get("Authorization"); got != "Bearer test-token" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer test-token")
	}
	if got := seen.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	// backup:true is what makes Supervisor take the partial backup that is
	// the only way back off a bad update.
	if want := `{"backup":true}`; string(seenBody) != want {
		t.Errorf("body = %q, want %q", seenBody, want)
	}
}

func TestUpdateAddonMissingTokenErrors(t *testing.T) {
	t.Setenv("SUPERVISOR_TOKEN", "")
	client := newFakeAddonHTTP()

	err := UpdateAddon(context.Background(), client, "a0d7b954_esphome")

	if err == nil {
		t.Fatal("err = nil, want a failure")
	}
	if len(client.calls) != 0 {
		t.Errorf("calls = %+v, want none - there is no token to authenticate them with", client.calls)
	}
}

// Also proves addonUpdateTimeout is the deadline applied: at the
// 30-minute default, a client that never answers would hang the suite.
func TestUpdateAddonRespectsItsOwnTimeout(t *testing.T) {
	setSupervisorToken(t)
	prev := addonUpdateTimeout
	addonUpdateTimeout = 20 * time.Millisecond
	defer func() { addonUpdateTimeout = prev }()

	err := UpdateAddon(context.Background(), blockingAddonUpdateDoer(), "a0d7b954_esphome")

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want errors.Is(err, context.DeadlineExceeded)", err)
	}
}

func TestUpdateAddonRespectsTheCallersCancellation(t *testing.T) {
	setSupervisorToken(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := UpdateAddon(ctx, blockingAddonUpdateDoer(), "a0d7b954_esphome")

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want errors.Is(err, context.Canceled)", err)
	}
}

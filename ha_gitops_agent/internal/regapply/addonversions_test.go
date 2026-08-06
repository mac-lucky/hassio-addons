package regapply

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/options"
)

// addonEntry builds one element of GET /addons' "addons" array. version
// is any so a test can hand it a real null.
func addonEntry(slug, name string, version any) map[string]any {
	return map[string]any{"slug": slug, "name": name, "version": version}
}

func TestFetchInstalledAddons(t *testing.T) {
	tests := []struct {
		name         string
		status       int
		data         map[string]any
		want         []InstalledAddon
		wantErrParts []string
	}{
		{
			name:   "installed add-ons come back in supervisor's order",
			status: 200,
			data: map[string]any{"addons": []any{
				addonEntry("a0d7b954_esphome", "ESPHome Device Builder", "2025.8.0"),
				addonEntry("core_samba", "Samba share", "12.3.2"),
			}},
			want: []InstalledAddon{
				{Slug: "a0d7b954_esphome", Name: "ESPHome Device Builder", Version: "2025.8.0"},
				{Slug: "core_samba", Name: "Samba share", Version: "12.3.2"},
			},
		},
		{
			name:   "a null version is a store entry, not an install",
			status: 200,
			data: map[string]any{"addons": []any{
				addonEntry("a0d7b954_esphome", "ESPHome Device Builder", "2025.8.0"),
				addonEntry("core_mariadb", "MariaDB", nil),
			}},
			want: []InstalledAddon{
				{Slug: "a0d7b954_esphome", Name: "ESPHome Device Builder", Version: "2025.8.0"},
			},
		},
		{
			name:   "an empty version is dropped the same way a null one is",
			status: 200,
			data: map[string]any{"addons": []any{
				addonEntry("core_mariadb", "MariaDB", ""),
			}},
			want: []InstalledAddon{},
		},
		{
			name:   "an entry with no slug cannot be recorded and is dropped",
			status: 200,
			data: map[string]any{"addons": []any{
				addonEntry("", "Nameless", "1.0.0"),
				addonEntry("core_samba", "Samba share", "12.3.2"),
			}},
			want: []InstalledAddon{{Slug: "core_samba", Name: "Samba share", Version: "12.3.2"}},
		},
		{
			name:   "an add-on with no display name still records its version",
			status: 200,
			data:   map[string]any{"addons": []any{addonEntry("local_thing", "", "0.1.0")}},
			want:   []InstalledAddon{{Slug: "local_thing", Version: "0.1.0"}},
		},
		{
			name:   "an empty list is reported as one, not as an error",
			status: 200,
			data:   map[string]any{"addons": []any{}},
			want:   []InstalledAddon{},
		},
		{
			name:         "500 surfaces the status and supervisor's own message",
			status:       500,
			wantErrParts: []string{"add-on list returned HTTP 500", "boom"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setSupervisorToken(t)
			client := newFakeAddonHTTP()
			client.queueResponse("GET", "/addons", tt.status, tt.data)

			got, err := FetchInstalledAddons(context.Background(), client)

			if len(tt.wantErrParts) > 0 {
				if err == nil {
					t.Fatalf("FetchInstalledAddons() error = nil, want an error")
				}
				for _, part := range tt.wantErrParts {
					if !strings.Contains(err.Error(), part) {
						t.Errorf("error = %q, want it to contain %q", err.Error(), part)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("FetchInstalledAddons() error = %v, want nil", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("FetchInstalledAddons() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// A 200 with no data object must fail, not read as "nothing installed" -
// the agent is itself an add-on, and that would blank the recorded file.
func TestFetchInstalledAddonsRejectsAResponseWithNoDataObject(t *testing.T) {
	setSupervisorToken(t)
	client := addonUpdateDoerFunc(func(*http.Request) (*http.Response, error) {
		return addonUpdateRawResponse(200, `{"result":"ok"}`), nil
	})
	if _, err := FetchInstalledAddons(context.Background(), client); err == nil ||
		!strings.Contains(err.Error(), "carried no data object") {
		t.Errorf("error = %v, want it to report the missing data object", err)
	}
}

func TestFetchInstalledAddonsReportsInvalidJSON(t *testing.T) {
	setSupervisorToken(t)
	client := addonUpdateDoerFunc(func(*http.Request) (*http.Response, error) {
		return addonUpdateRawResponse(200, "not json at all"), nil
	})
	if _, err := FetchInstalledAddons(context.Background(), client); err == nil ||
		!strings.Contains(err.Error(), "invalid JSON") {
		t.Errorf("error = %v, want it to report invalid JSON", err)
	}
}

func TestFetchInstalledAddonsSendsTheBearerToken(t *testing.T) {
	setSupervisorToken(t)
	var auth string
	client := addonUpdateDoerFunc(func(req *http.Request) (*http.Response, error) {
		auth = req.Header.Get("Authorization")
		return addonUpdateRawResponse(200, `{"result":"ok","data":{"addons":[]}}`), nil
	})
	if _, err := FetchInstalledAddons(context.Background(), client); err != nil {
		t.Fatalf("FetchInstalledAddons: %v", err)
	}
	if want := "Bearer test-token"; auth != want {
		t.Errorf("Authorization = %q, want %q", auth, want)
	}
}

func TestFetchInstalledAddonsRequiresASupervisorToken(t *testing.T) {
	t.Setenv("SUPERVISOR_TOKEN", "")
	if _, err := FetchInstalledAddons(context.Background(), newFakeAddonHTTP()); !errors.Is(err, options.ErrMissingSupervisorToken) {
		t.Errorf("error = %v, want ErrMissingSupervisorToken", err)
	}
}

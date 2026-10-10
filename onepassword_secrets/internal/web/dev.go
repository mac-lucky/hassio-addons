//go:build dev

package web

import (
	"net/http"
	"os"
	"time"

	"github.com/mac-lucky/hassio-addons/onepassword_secrets/internal/connectd"
	"github.com/mac-lucky/hassio-addons/onepassword_secrets/internal/engine"
	"github.com/mac-lucky/hassio-addons/onepassword_secrets/internal/opconnect"
	"github.com/mac-lucky/hassio-addons/onepassword_secrets/internal/state"
)

// Previews, served with OPS_DEV=1 and -tags dev as /?preview=<name>.
var previews = []string{"firstrun", "starting", "healthy", "attention", "restart", "error", "dryrun"}

func devPreview(r *http.Request) (string, bool) {
	if os.Getenv(DevEnvVar) != "1" {
		return "", false
	}
	name := r.URL.Query().Get("preview")
	for _, p := range previews {
		if p == name {
			return name, true
		}
	}
	return "", false
}

func devKeyHistory(key string) []state.Entry {
	now := time.Now().UTC()
	return []state.Entry{
		{Time: now.Add(-3 * time.Hour), Kind: state.KindRefresh, Title: "Reloaded template", Keys: []string{key}},
		{Time: now.Add(-3*time.Hour - time.Minute), Kind: state.KindChange, Title: "Updated 1 key", Detail: "in secrets.yaml", Keys: []string{key}},
	}
}

func devStatus(name string) engine.Status {
	now := time.Now().UTC()
	day := 24 * time.Hour
	s := engine.Status{
		Version:    "dev",
		State:      engine.StateHealthy,
		Headline:   "In sync",
		Detail:     "23 keys from 1Password in 3 files",
		Embedded:   true,
		LastSync:   now.Add(-12 * time.Second),
		NextSync:   now.Add(48 * time.Second),
		LastChange: now.Add(-3 * time.Hour),
		Token:      engine.TokenInfo{Known: true, ExpiresAt: now.Add(312 * day), DaysLeft: 312},
		Connect: engine.ConnectInfo{
			Reachable: true, Synced: true, ServerVersion: "1.8.3",
			Dependencies: []opconnect.Dependency{
				{Service: "sqlite", Status: "ACTIVE", Message: "Connected to /data/connect/.op/data/1password.sqlite"},
				{Service: "account_data", Status: "AVAILABLE", Message: "Account data is available"},
				{Service: "sync", Status: "ACTIVE"},
			},
			Processes: []connectd.ProcState{
				{Name: connectd.Sync, Running: true, PID: 31, StartedAt: now.Add(-50 * time.Hour)},
				{Name: connectd.API, Running: true, PID: 32, StartedAt: now.Add(-50 * time.Hour), Restarts: 1},
			},
		},
		Vaults: []engine.VaultInfo{{Name: "homeassistant", ID: "v1", Items: 14, Discover: true}},
		Files: []engine.FileInfo{
			{Path: "secrets.yaml", Exists: true, Keys: 23, Managed: 23},
			{Path: "esphome/secrets.yaml", Exists: true, Keys: 17, Managed: 17},
			{Path: "zigbee2mqtt/secret.yaml", Exists: true, Keys: 2, Managed: 2},
		},
	}
	row := func(key, item, st string, rotated time.Time, interval string, uses ...engine.Use) engine.SecretRow {
		r := engine.SecretRow{
			Key: key, State: st, Source: "homeassistant > " + item + " > " + key,
			VaultName: "homeassistant", ItemTitle: item, FieldLabel: key,
			Ref: "op://homeassistant/" + item + "/" + key, Files: []string{"secrets.yaml"},
			UsedBy: uses, ChangedAt: rotated, Rotation: "r3-manual", Interval: interval, RotatedAt: rotated,
		}
		if d := durOf(interval); d > 0 && !rotated.IsZero() {
			r.Due = rotated.Add(d)
			r.Overdue = now.After(r.Due)
			r.DueSoon = !r.Overdue && r.Due.Sub(now) < 14*day
			r.RotationPct = min(100, int(100*now.Sub(rotated)/d))
		}
		return r
	}
	ha := func(domain, file string, line int) engine.Use {
		return engine.Use{Kind: "ha", Label: domain, Domain: domain, File: file, Line: line}
	}
	s.Secrets = []engine.SecretRow{
		row("gitops_git_token", "homeassistant gitops-agent", engine.RowSynced, now.Add(-6*day), "365d", engine.Use{Kind: "addon", Label: "GitOps Agent", File: "git_token"}),
		row("gitops_webhook_secret", "homeassistant gitops-agent", engine.RowSynced, now.Add(-6*day), "365d", engine.Use{Kind: "addon", Label: "GitOps Agent", File: "webhook_secret"}),
		row("google_assistant_service_account_json", "homeassistant google", engine.RowSynced, now.Add(-200*day), "365d", ha("google_assistant", "configuration.yaml", 88)),
		row("mqtt_password", "homeassistant mqtt", engine.RowSynced, now.Add(-30*day), "365d", engine.Use{Kind: "addon", Label: "Mosquitto broker", File: "logins[0].password"}, engine.Use{Kind: "file", Label: "zigbee2mqtt", File: "zigbee2mqtt/configuration.yaml", Line: 22}),
		row("network_key_json", "homeassistant zigbee", engine.RowSynced, now.Add(-400*day), "on-compromise", engine.Use{Kind: "file", Label: "zigbee2mqtt", File: "zigbee2mqtt/configuration.yaml", Line: 31}),
		row("wifi_password", "esphome wifi", engine.RowSynced, now.Add(-90*day), "365d", engine.Use{Kind: "esphome", Label: "plant-care", File: "esphome/plant-care.yaml", Line: 12}, engine.Use{Kind: "esphome", Label: "epaper-xiao", File: "esphome/epaper-xiao.yaml", Line: 9}),
		row("xiaomi_fan_token", "homeassistant xiaomi-fan", engine.RowSynced, now.Add(-370*day), "365d", ha("xiaomi_miio", "configuration.yaml", 41)),
		row("wmbus_warmwater_key", "homeassistant wmbus", engine.RowSynced, now.Add(-355*day), "365d", engine.Use{Kind: "addon", Label: "Wmbusmeters", File: "meters[0].key"}),
	}
	s.Secrets[3].Files = []string{"secrets.yaml", "zigbee2mqtt/secret.yaml"}
	s.Secrets[5].Files = []string{"secrets.yaml", "esphome/secrets.yaml"}
	s.Secrets[2].Structured = true
	s.Secrets[4].Structured = true
	for i := range 15 {
		s.Secrets = append(s.Secrets, row("esp_device_"+string(rune('a'+i))+"_api_key", "esphome devices", engine.RowSynced, now.Add(-time.Duration(20+i*9)*day), "365d", engine.Use{Kind: "esphome", Label: "device-" + string(rune('a'+i)), File: "esphome/device.yaml", Line: 5}))
	}
	s.Activity = []state.Entry{
		{Time: now.Add(-3 * time.Hour), Kind: state.KindRefresh, Title: "Reloaded template", Keys: []string{"xiaomi_fan_token"}},
		{Time: now.Add(-3*time.Hour - time.Minute), Kind: state.KindChange, Title: "Updated 1 key", Detail: "in secrets.yaml", Keys: []string{"xiaomi_fan_token"}},
		{Time: now.Add(-26 * time.Hour), Kind: state.KindRefresh, Title: "Restarted Mosquitto broker", Keys: []string{"mqtt_password"}},
		{Time: now.Add(-26*time.Hour - 2*time.Minute), Kind: state.KindChange, Title: "Updated 1 key", Detail: "in secrets.yaml, zigbee2mqtt/secret.yaml", Keys: []string{"mqtt_password"}},
	}
	counts(&s)

	switch name {
	case "firstrun":
		return engine.Status{
			Version: "dev", State: engine.StateUnconfigured, Headline: "Not set up yet",
			Detail: "Add the Connect credentials and token in the add-on's Configuration tab.", Embedded: true,
			Setup: []engine.SetupStep{
				{Label: "Connect credentials set", Detail: "connect_credentials: the 1password-credentials.json of this Connect server"},
				{Label: "Access token set", Detail: "connect_token: a token for this server with read access to the vaults"},
				{Label: "Connect answers"}, {Label: "Vaults synced from 1Password"}, {Label: "Token accepted"},
				{Label: "Vault visible: homeassistant"}, {Label: "Secrets found in 1Password", Detail: "0 keys"},
			},
		}
	case "starting":
		return engine.Status{
			Version: "dev", State: engine.StateStarting, Headline: "Waiting for Connect's first sync",
			Detail: "Account data is not available because synchronization has not yet started", Embedded: true,
			Connect: engine.ConnectInfo{Reachable: true, ServerVersion: "1.8.3"},
			Setup: []engine.SetupStep{
				{Label: "Connect credentials set", Done: true}, {Label: "Access token set", Done: true},
				{Label: "Connect answers", Done: true, Detail: "1.8.3"}, {Label: "Vaults synced from 1Password"},
				{Label: "Token accepted"}, {Label: "Vault visible: homeassistant"}, {Label: "Secrets found in 1Password", Detail: "0 keys"},
			},
		}
	case "attention":
		s.Secrets = append(s.Secrets,
			engine.SecretRow{Key: "xiaomi_vacuum_token", State: engine.RowUnmanaged, Files: []string{"secrets.yaml"}, UsedBy: []engine.Use{ha("xiaomi_miio", "configuration.yaml", 52)}},
			engine.SecretRow{Key: "old_cloud_password", State: engine.RowUnmanaged, Files: []string{"secrets.yaml"}},
		)
		s.Secrets[6].Overdue = true
		s.Problems = []engine.Problem{
			{Severity: "warning", Key: "xiaomi_fan_token", Title: "xiaomi_fan_token is due for rotation", Detail: "Rotated " + now.Add(-370*day).Format("2006-01-02") + ", interval 365d.", Fix: "Rotate it at its issuer, update the item and its rotated_at; this add-on applies the new value by itself."},
			{Severity: "info", Key: "xiaomi_vacuum_token", Title: "xiaomi_vacuum_token is not in 1Password yet", Detail: "It lives only in secrets.yaml.", Fix: "Add a field labelled xiaomi_vacuum_token to an item in the homeassistant vault; it is managed from the next sync."},
		}
		counts(&s)
		s.State, s.Headline, s.Detail = engine.StateAttention, "In sync, 1 warning", s.Problems[0].Title
	case "restart":
		s.RestartPending = []string{"xiaomi_miio"}
		s.Problems = []engine.Problem{{Severity: "warning", Title: "Home Assistant needs a restart", Detail: "New values are written, but xiaomi_miio reads them only at startup.", Fix: "Press Restart Home Assistant."}}
		counts(&s)
		s.State, s.Headline, s.Detail = engine.StateAttention, "In sync, 1 warning", s.Problems[0].Title
	case "error":
		s.Secrets = append(s.Secrets,
			engine.SecretRow{Key: "notify_api_key", State: engine.RowMissing, UsedBy: []engine.Use{ha("notify", "configuration.yaml", 120)}},
			engine.SecretRow{Key: "dup_token", State: engine.RowConflict},
		)
		s.Problems = []engine.Problem{
			{Severity: "error", Title: "Change rolled back", Detail: "the configuration check failed: Invalid config for xiaomi_miio at configuration.yaml, line 41", Fix: "The previous values are back in place. Fix the cause, then press Sync now."},
			{Severity: "error", Key: "notify_api_key", Title: "notify_api_key is missing", Detail: "Used by configuration.yaml:120, but it is in neither 1Password nor secrets.yaml.", Fix: "Add a field labelled notify_api_key to an item in the homeassistant vault."},
			{Severity: "error", Key: "dup_token", Title: "dup_token comes from more than one field", Detail: "label used by homeassistant > a > dup_token, homeassistant > b > dup_token", Fix: "Rename one of the fields: each key must come from exactly one field."},
		}
		counts(&s)
		s.Activity = append([]state.Entry{{Time: now.Add(-time.Minute), Kind: state.KindError, Title: "Rolled back: the configuration check failed", Detail: "Invalid config for xiaomi_miio", Keys: []string{"xiaomi_fan_token"}}}, s.Activity...)
		s.State, s.Headline, s.Detail = engine.StateError, "3 problems need attention", "Change rolled back"
	case "dryrun":
		s.DryRun = true
		s.Secrets[0].State = engine.RowPending
		s.Secrets[6].State = engine.RowPending
		s.Files[0].PendingChanged = []string{"gitops_git_token", "xiaomi_fan_token"}
		s.Files[2] = engine.FileInfo{Path: "zigbee2mqtt/secret.yaml", PendingAdded: []string{"mqtt_password", "network_key_json"}}
		s.Secrets = append(s.Secrets, engine.SecretRow{Key: "hand_written_key", State: engine.RowUnmanaged, Files: []string{"secrets.yaml"}})
		counts(&s)
		s.Detail = "Dry run: 4 keys would change"
	}
	return s
}

func durOf(interval string) time.Duration {
	switch interval {
	case "365d":
		return 365 * 24 * time.Hour
	case "7d":
		return 7 * 24 * time.Hour
	}
	return 0
}

func counts(s *engine.Status) {
	c := engine.Counts{}
	for _, r := range s.Secrets {
		c.Keys++
		switch r.State {
		case engine.RowSynced, engine.RowPending, engine.RowUnused:
			c.Managed++
		case engine.RowMissing:
			c.Missing++
		case engine.RowUnmanaged:
			c.Unmanaged++
		case engine.RowConflict:
			c.Conflicts++
		}
		if r.Overdue {
			c.Overdue++
		}
	}
	for _, p := range s.Problems {
		switch p.Severity {
		case "error":
			c.Errors++
		case "warning":
			c.Warnings++
		}
	}
	s.Counts = c
}

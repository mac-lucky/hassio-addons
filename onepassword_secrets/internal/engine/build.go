package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/mac-lucky/hassio-addons/onepassword_secrets/internal/discover"
	"github.com/mac-lucky/hassio-addons/onepassword_secrets/internal/options"
	"github.com/mac-lucky/hassio-addons/onepassword_secrets/internal/render"
	"github.com/mac-lucky/hassio-addons/onepassword_secrets/internal/state"
)

// Thresholds for the rotation and token warnings.
const (
	dueSoonWindow = 14 * 24 * time.Hour
	tokenWarnDays = 30
	// sensorRefresh re-publishes an unchanged sensor, so it survives a
	// Core restart (states pushed over the API are not restored).
	sensorRefresh = 10 * time.Minute
)

// buildStatus turns a finished cycle into the Status the UI shows.
func (e *Engine) buildStatus(s Status, c *cycle) Status {
	now := e.cfg.Now()
	opts := e.cfg.Options
	root := opts.SecretsFiles[0]
	s.RestartPending = append([]string(nil), e.st.RestartPending...)
	s.LastChange = e.st.LastWrite
	s.Vaults = c.vaults
	s.Problems = append(s.Problems, c.problems...)

	// Files, and which file holds which key.
	inFile := map[string][]string{}
	present := map[string]bool{}
	pending := map[string]bool{}
	for _, j := range c.jobs {
		fi := FileInfo{Path: j.path, Exists: j.existed}
		if j.err != nil {
			fi.Error = j.err.Error()
			s.Problems = append(s.Problems, Problem{Severity: "error", Title: "Cannot render " + j.path, Detail: j.err.Error(), Fix: "Fix the file (or the secrets_files option); nothing is written to it until then."})
			s.Files = append(s.Files, fi)
			continue
		}
		for _, key := range sortedKeysOf(j.plan.KeyErrors) {
			s.Problems = append(s.Problems, Problem{
				Severity: "error", Key: key,
				Title:  key + " could not be written to " + j.path,
				Detail: j.plan.KeyErrors[key] + "; the file keeps its previous value.",
				Fix:    "Correct the value in 1Password.",
			})
		}
		keys := map[string]bool{}
		if j.plan.Content != nil {
			if names, err := render.Keys(j.plan.Content); err == nil {
				for _, k := range names {
					keys[k] = true
				}
			}
		} else if names, err := render.Keys(j.current); err == nil {
			for _, k := range names {
				keys[k] = true
			}
		}
		if opts.DryRun {
			// The plan's content is what the file WOULD hold; the file
			// itself still has only the current keys.
			keys = map[string]bool{}
			if names, err := render.Keys(j.current); err == nil {
				for _, k := range names {
					keys[k] = true
				}
			}
			fi.PendingAdded = j.plan.Added
			fi.PendingChanged = j.plan.Changed
			fi.PendingRemoved = j.plan.Removed
			for _, l := range [][]string{j.plan.Added, j.plan.Changed, j.plan.Removed} {
				for _, k := range l {
					pending[k] = true
				}
			}
		}
		for k := range keys {
			inFile[k] = append(inFile[k], j.path)
			present[k] = true
		}
		fi.Keys = len(keys)
		fi.Managed = len(j.desired)
		fi.Unmanaged = j.plan.Unmanaged
		fi.Orphaned = j.plan.Orphaned
		fi.Exists = j.existed || (!opts.DryRun && j.plan.Content != nil)
		s.Files = append(s.Files, fi)
	}

	// Every key anyone knows about.
	all := map[string]bool{}
	for k := range c.catalog.Entries {
		all[k] = true
	}
	for k := range c.catalog.Conflicted {
		all[k] = true
	}
	referenced := map[string]bool{}
	for _, r := range c.routed {
		all[r.Key] = true
		referenced[r.Key] = true
	}
	for _, a := range c.addonRefs {
		all[a.Key] = true
		referenced[a.Key] = true
	}
	for k := range present {
		all[k] = true
	}

	for _, key := range sortedKeys(all) {
		row := SecretRow{Key: key, Files: inFile[key], UsedBy: usesOf(c.routed, c.addonRefs, key)}
		if ks, ok := e.st.Keys[key]; ok {
			row.ChangedAt = ks.ChangedAt
		}
		entry, inCatalog := c.catalog.Entries[key]
		switch {
		case c.catalog.Conflicted[key]:
			row.State = RowConflict
		case inCatalog && pending[key]:
			row.State = RowPending
		case inCatalog && !referenced[key]:
			row.State = RowUnused
		case inCatalog:
			row.State = RowSynced
		case present[key]:
			row.State = RowUnmanaged
		default:
			row.State = RowMissing
		}
		if inCatalog {
			row.Source = entry.Source()
			row.VaultName = entry.VaultName
			row.ItemTitle = entry.ItemTitle
			row.FieldLabel = entry.FieldLabel
			row.Ref = entry.Ref
			if row.Ref == "" {
				row.Ref = refFor(entry)
			}
			row.Structured = entry.Structured
			fillRotation(&row, entry.Meta, now)
		}
		s.Secrets = append(s.Secrets, row)
		e.rowProblems(&s, row, root)
	}

	for _, p := range c.catalog.Problems {
		sev := "error"
		fix := ""
		switch p.Kind {
		case "conflict":
			fix = "Rename one of the fields: each key must come from exactly one field."
		case "not_json":
			fix = "Put the JSON document in the field, or drop _json from the label."
		case "unresolved_reference":
			fix = "Correct the reference in the add-on's references option."
		case "empty_value":
			sev = "warning"
			fix = "Fill the field in 1Password, or delete it."
		}
		s.Problems = append(s.Problems, Problem{Severity: sev, Title: problemTitle(p), Detail: p.Detail, Fix: fix, Key: p.Key})
	}
	for _, w := range e.scan.Warnings {
		s.Problems = append(s.Problems, Problem{Severity: "info", Title: "Config file not fully scanned", Detail: w})
	}
	if s.Token.Known && s.Token.DaysLeft <= tokenWarnDays {
		sev := "warning"
		title := fmt.Sprintf("The Connect token expires in %d days", s.Token.DaysLeft)
		if s.Token.DaysLeft <= 0 {
			sev = "error"
			title = "The Connect token has expired"
		}
		s.Problems = append(s.Problems, Problem{Severity: sev, Title: title, Detail: "Expires " + s.Token.ExpiresAt.Format("2006-01-02") + ".", Fix: "Create a new token for this Connect server and put it in connect_token; then revoke the old one."})
	}
	if len(s.RestartPending) > 0 {
		s.Problems = append(s.Problems, Problem{Severity: "warning", Title: "Home Assistant needs a restart", Detail: "New values are written, but " + strings.Join(s.RestartPending, ", ") + " reads them only at startup.", Fix: "Press Restart Home Assistant."})
	}

	sort.SliceStable(s.Problems, func(i, j int) bool {
		return severityRank(s.Problems[i].Severity) < severityRank(s.Problems[j].Severity)
	})
	for _, row := range s.Secrets {
		s.Counts.Keys++
		switch row.State {
		case RowSynced, RowPending, RowUnused:
			s.Counts.Managed++
		case RowMissing:
			s.Counts.Missing++
		case RowUnmanaged:
			s.Counts.Unmanaged++
		case RowConflict:
			s.Counts.Conflicts++
		}
		if row.State == RowUnused {
			s.Counts.Unused++
		}
		if row.Overdue {
			s.Counts.Overdue++
		} else if row.DueSoon {
			s.Counts.DueSoon++
		}
	}
	for _, p := range s.Problems {
		switch p.Severity {
		case "error":
			s.Counts.Errors++
		case "warning":
			s.Counts.Warnings++
		}
	}

	switch {
	case s.Counts.Errors > 0:
		s.State = StateError
		s.Headline = plural(s.Counts.Errors, "problem needs attention", "problems need attention")
		s.Detail = s.Problems[0].Title
	case s.Counts.Warnings > 0:
		s.State = StateAttention
		s.Headline = "In sync, " + plural(s.Counts.Warnings, "warning", "warnings")
		s.Detail = s.Problems[0].Title
	default:
		s.State = StateHealthy
		s.Headline = "In sync"
		s.Detail = fmt.Sprintf("%s from 1Password in %s", plural(s.Counts.Managed, "key", "keys"), plural(len(opts.SecretsFiles), "file", "files"))
	}
	if opts.DryRun {
		if n := len(pending); n > 0 {
			s.Detail = fmt.Sprintf("Dry run: %s would change", plural(n, "key", "keys"))
		}
	}
	s.Setup = setupSteps(opts, s, c)
	return s
}

func (e *Engine) rowProblems(s *Status, row SecretRow, root string) {
	where := ""
	if len(row.UsedBy) > 0 {
		u := row.UsedBy[0]
		where = u.File
		if u.Line > 0 {
			where = fmt.Sprintf("%s:%d", u.File, u.Line)
		}
		if u.Kind == "addon" {
			where = u.Label + " (" + u.File + ")"
		}
		if len(row.UsedBy) > 1 {
			where += fmt.Sprintf(" and %d more", len(row.UsedBy)-1)
		}
	}
	vault := "homeassistant"
	if len(e.cfg.Options.Vaults) > 0 {
		vault = e.cfg.Options.Vaults[0]
	}
	switch row.State {
	case RowMissing:
		s.Problems = append(s.Problems, Problem{
			Severity: "error", Key: row.Key,
			Title:  row.Key + " is missing",
			Detail: "Used by " + where + ", but it is in neither 1Password nor " + root + ".",
			Fix:    fmt.Sprintf("Add a field labelled %s to an item in the %s vault.", row.Key, vault),
		})
	case RowUnmanaged:
		if len(row.UsedBy) > 0 {
			s.Problems = append(s.Problems, Problem{
				Severity: "info", Key: row.Key,
				Title:  row.Key + " is not in 1Password yet",
				Detail: "It lives only in " + strings.Join(row.Files, ", ") + ".",
				Fix:    fmt.Sprintf("Add a field labelled %s to an item in the %s vault; it is managed from the next sync.", row.Key, vault),
			})
		}
	}
	if row.Overdue {
		s.Problems = append(s.Problems, Problem{
			Severity: "warning", Key: row.Key,
			Title:  row.Key + " is due for rotation",
			Detail: fmt.Sprintf("Rotated %s, interval %s.", dateOr(row.RotatedAt, "never"), row.Interval),
			Fix:    "Rotate it at its issuer, update the item and its rotated_at; this add-on applies the new value by itself.",
		})
	}
}

func sortedKeysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func problemTitle(p discover.Problem) string {
	switch p.Kind {
	case "conflict":
		return p.Key + " comes from more than one field"
	case "not_json":
		return p.Key + " is not JSON"
	case "unresolved_reference":
		return "The reference for " + p.Key + " does not resolve"
	case "empty_value":
		return p.Key + " is empty"
	}
	return p.Key
}

func severityRank(s string) int {
	switch s {
	case "error":
		return 0
	case "warning":
		return 1
	}
	return 2
}

func refFor(e discover.Entry) string {
	parts := []string{e.VaultName, e.ItemTitle}
	if e.Section != "" {
		parts = append(parts, e.Section)
	}
	return "op://" + strings.Join(append(parts, e.FieldLabel), "/")
}

func fillRotation(row *SecretRow, m discover.Meta, now time.Time) {
	row.Rotation = m.Rotation
	row.Interval = m.Interval
	row.RotatedAt = m.RotatedAt
	row.Due = m.Due()
	if row.Due.IsZero() {
		return
	}
	row.Overdue = now.After(row.Due)
	row.DueSoon = !row.Overdue && row.Due.Sub(now) < dueSoonWindow
	if !m.RotatedAt.IsZero() {
		total := row.Due.Sub(m.RotatedAt)
		if total > 0 {
			pct := int(100 * now.Sub(m.RotatedAt) / total)
			row.RotationPct = min(max(pct, 0), 100)
		}
	}
}

func dateOr(t time.Time, fallback string) string {
	if t.IsZero() {
		return fallback
	}
	return t.Format("2006-01-02")
}

// setupSteps is the first-run checklist. c is nil before a cycle got far
// enough to know about vaults and files.
func setupSteps(opts options.Options, s Status, c *cycle) []SetupStep {
	steps := []SetupStep{}
	if opts.Embedded() {
		steps = append(steps, SetupStep{Label: "Connect credentials set", Done: opts.CredentialsSet, Detail: "connect_credentials: the 1password-credentials.json of this Connect server"})
	} else {
		steps = append(steps, SetupStep{Label: "Connect server set", Done: opts.ConnectURL != "", Detail: opts.ConnectURL})
	}
	steps = append(steps,
		SetupStep{Label: "Access token set", Done: opts.ConnectToken != "", Detail: "connect_token: a token for this server with read access to the vaults"},
		SetupStep{Label: "Connect answers", Done: s.Connect.Reachable, Detail: s.Connect.ServerVersion},
		SetupStep{Label: "Vaults synced from 1Password", Done: s.Connect.Synced},
	)
	tokenOK := c != nil
	steps = append(steps, SetupStep{Label: "Token accepted", Done: tokenOK})
	vaultsOK := c != nil && len(c.vaults) > 0
	if c != nil {
		for _, v := range c.vaults {
			if v.Missing {
				vaultsOK = false
			}
		}
	}
	steps = append(steps, SetupStep{Label: "Vault visible: " + strings.Join(opts.Vaults, ", "), Done: vaultsOK})
	keys := 0
	if c != nil {
		keys = len(c.catalog.Entries)
	}
	steps = append(steps, SetupStep{Label: "Secrets found in 1Password", Done: keys > 0, Detail: plural(keys, "key", "keys")})
	return steps
}

// sensorEntity is the status sensor this add-on pushes.
const sensorEntity = "sensor.onepassword_secrets"

// noticeOutsideRestart clears a pending restart once Home Assistant was
// restarted some other way (its UI, an update): states pushed over the API
// do not survive a Core restart, so our sensor missing means it happened.
func (e *Engine) noticeOutsideRestart(ctx context.Context) {
	if len(e.st.RestartPending) == 0 || e.publishedAt.IsZero() {
		return
	}
	if exists, err := e.ha.HasState(ctx, sensorEntity); err != nil || exists {
		return
	}
	e.clearPending()
	_ = e.ha.Dismiss(ctx, notifyRestart)
	e.record(state.KindRefresh, "Home Assistant was restarted; the new values are in use", "", nil)
}

// publish stores the new status, pushes the sensor, and raises or clears
// the error notification on a transition.
func (e *Engine) publish(ctx context.Context, s Status) {
	e.noticeOutsideRestart(ctx)
	s.RestartPending = append([]string(nil), e.st.RestartPending...)
	e.mu.Lock()
	s.Syncing = e.status.Syncing
	s.Phase = e.status.Phase
	if s.NextSync.IsZero() {
		s.NextSync = e.status.NextSync
	}
	e.status = s
	e.mu.Unlock()

	errText := ""
	if s.State == StateError {
		errText = s.Headline + ": " + s.Detail
	}
	if errText != e.lastErr {
		if errText != "" {
			e.record(state.KindError, s.Headline, s.Detail, nil)
			e.notify(ctx, notifyError, "1Password Secrets: "+s.Headline, s.Detail+"\n\nOpen the 1Password Secrets panel for details.")
		} else if e.lastErr != "" {
			_ = e.ha.Dismiss(ctx, notifyError)
			e.record(state.KindInfo, "Recovered", "", nil)
		}
		e.lastErr = errText
	}
	if s.Token.Known && s.Token.DaysLeft <= tokenWarnDays && !e.tokenNotified {
		e.tokenNotified = true
		e.notify(ctx, notifyToken, "1Password Secrets: Connect token expires soon",
			fmt.Sprintf("The Connect access token expires on %s. Create a new one for this server, put it in connect_token, then revoke the old one.", s.Token.ExpiresAt.Format("2006-01-02")))
	}

	attrs := map[string]any{
		"friendly_name":   "1Password Secrets",
		"icon":            "mdi:shield-key",
		"headline":        s.Headline,
		"managed_keys":    s.Counts.Managed,
		"missing_keys":    s.Counts.Missing,
		"unmanaged_keys":  s.Counts.Unmanaged,
		"rotation_due":    s.Counts.Overdue,
		"problems":        s.Counts.Errors + s.Counts.Warnings,
		"restart_pending": len(s.RestartPending) > 0,
		"dry_run":         s.DryRun,
	}
	if !s.LastChange.IsZero() {
		attrs["last_change"] = s.LastChange.UTC().Format(time.RFC3339)
	}
	if s.Token.Known {
		attrs["token_expires_in_days"] = s.Token.DaysLeft
	}
	sig := fmt.Sprint(s.State, attrs)
	if sig == e.lastPublished && e.cfg.Now().Sub(e.publishedAt) < sensorRefresh {
		return
	}
	if err := e.ha.SetState(ctx, sensorEntity, s.State, attrs); err == nil {
		e.lastPublished = sig
		e.publishedAt = e.cfg.Now()
	}
}

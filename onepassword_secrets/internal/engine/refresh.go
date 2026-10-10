package engine

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/mac-lucky/hassio-addons/onepassword_secrets/internal/ha"
	"github.com/mac-lucky/hassio-addons/onepassword_secrets/internal/refscan"
	"github.com/mac-lucky/hassio-addons/onepassword_secrets/internal/state"
)

// Notification IDs this add-on owns.
const (
	notifyError   = "onepassword_secrets_error"
	notifyRestart = "onepassword_secrets_restart"
	notifyDevices = "onepassword_secrets_devices"
	notifyToken   = "onepassword_secrets_token"
)

// RolledBackError reports a change that was written, failed to apply,
// and was rolled back.
type RolledBackError struct{ Reason string }

func (e *RolledBackError) Error() string { return "change rolled back: " + e.Reason }

// EventChanged is fired after a change is applied.
const EventChanged = "onepassword_secrets_changed"

// consumers is who has to act on a set of changed keys.
type consumers struct {
	// domains maps an integration to the changed keys its YAML uses.
	domains map[string][]string
	// addons maps a slug to the changed keys it uses.
	addons map[string][]string
	// devices are files outside Home Assistant's config (ESPHome device
	// YAML) that need a rebuild to pick a value up.
	devices map[string][]string
	// others are files no add-on or integration could be matched to.
	others map[string][]string
}

// consumersOf finds who uses the changed keys, per file: a reference is
// refreshed only when the key changed in the file it reads.
func (e *Engine) consumersOf(c *cycle, changed map[string][]string) consumers {
	targets := e.cfg.Options.SecretsFiles
	inFile := map[string]map[string]bool{}
	for f, keys := range changed {
		inFile[f] = map[string]bool{}
		for _, k := range keys {
			inFile[f][k] = true
		}
	}
	out := consumers{domains: map[string][]string{}, addons: map[string][]string{}, devices: map[string][]string{}, others: map[string][]string{}}
	add := func(m map[string][]string, k, key string) {
		if slices.Contains(m[k], key) {
			return
		}
		m[k] = append(m[k], key)
	}
	for _, r := range c.routed {
		target, _ := refscan.TargetFor(r, targets)
		if !inFile[target][r.Key] {
			continue
		}
		if r.Domain != "" {
			add(out.domains, r.Domain, r.Key)
			continue
		}
		top := strings.SplitN(r.File, "/", 2)[0]
		switch {
		case top == "esphome":
			add(out.devices, r.File, r.Key)
		case e.addonForDir(top) != "":
			add(out.addons, e.addonForDir(top), r.Key)
		default:
			add(out.others, r.File, r.Key)
		}
	}
	for _, a := range c.addonRefs {
		// Supervisor and the GitOps agent read the root file only.
		if inFile[targets[0]][a.Key] {
			add(out.addons, a.Slug, a.Key)
		}
	}
	return out
}

// addonForDir maps a top-level config directory to the add-on that reads
// it: "zigbee2mqtt" -> "45df7312_zigbee2mqtt". Exact slug or "<repo>_dir".
func (e *Engine) addonForDir(dir string) string {
	for _, a := range e.addons {
		if a.Slug == dir || strings.HasSuffix(a.Slug, "_"+dir) {
			return a.Slug
		}
	}
	return ""
}

func (e *Engine) addonName(slug string) string {
	for _, a := range e.addons {
		if a.Slug == slug {
			return a.Name
		}
	}
	return slug
}

func (e *Engine) addonRunning(slug string) bool {
	for _, a := range e.addons {
		if a.Slug == slug {
			return a.State == "started"
		}
	}
	return false
}

// safeErr renders an error from Home Assistant or Supervisor for the log,
// the panel and notifications: an HTTP error is its status code only,
// because Core's error text can quote configuration with secrets resolved.
func safeErr(err error) string {
	var se *ha.StatusError
	if errors.As(err, &se) {
		return fmt.Sprintf("%s returned HTTP %d", se.Path, se.Code)
	}
	return err.Error()
}

// safeReason keeps a check_config failure to its locations (ha's client
// already does; this holds for any HA implementation the engine gets).
func safeReason(reason string) string {
	if strings.HasPrefix(reason, "check_config ") {
		return reason
	}
	return ha.SafeCheckReason(reason)
}

// resumePending finishes a change a restart interrupted.
func (e *Engine) resumePending(ctx context.Context, c *cycle) error {
	p := e.st.Pending
	if p == nil {
		return nil
	}
	e.record(state.KindInfo, "Finishing an interrupted change", "", p.Keys())
	return e.refresh(ctx, c, p)
}

// refresh makes every consumer of the changed keys pick the new values up.
// Order: validate, reload what can reload, restart Core if needed (guarded,
// rolled back on failure), then restart add-ons once Supervisor's secrets
// cache has expired, then tell the owner about devices. Each finished stage
// is saved, so a restart of this add-on resumes where it stopped.
func (e *Engine) refresh(ctx context.Context, c *cycle, p *state.PendingRefresh) error {
	changed := p.Keys()
	who := e.consumersOf(c, p.Changed)
	var reloaded, restartedAddons []string
	coreRestarted := false

	if p.Stage != "addons" {
		var restartFor []string
		if len(who.domains) > 0 {
			e.setPhase("Validating the configuration")
			if ok, reason := e.ha.CheckConfig(ctx); !ok {
				reason = safeReason(reason)
				return e.fail2(ctx, p, "the configuration check failed: "+reason,
					"Home Assistant's configuration check failed with the new values, so the secrets files were restored.\n\n"+reason)
			}
			services, err := e.ha.Services(ctx)
			if err != nil {
				services = nil
			}
			domains := make([]string, 0, len(who.domains))
			for d := range who.domains {
				domains = append(domains, d)
			}
			sort.Strings(domains)
			for _, d := range domains {
				service := "reload"
				if d == "homeassistant" {
					service = "reload_core_config"
				}
				if services == nil || !services[d][service] {
					restartFor = append(restartFor, d)
					continue
				}
				e.setPhase("Reloading " + d)
				if err := e.ha.CallService(ctx, d, service, nil); err != nil {
					e.record(state.KindError, "Reloading "+d+" failed; it needs a restart", safeErr(err), who.domains[d])
					restartFor = append(restartFor, d)
					continue
				}
				reloaded = append(reloaded, d)
			}
			if len(reloaded) > 0 {
				e.record(state.KindRefresh, "Reloaded "+strings.Join(reloaded, ", "), "", changed)
			}
		}
		if len(restartFor) > 0 {
			if e.cfg.Options.CoreRestart == "auto" {
				if err := e.guardedRestart(ctx, p, restartFor); err != nil {
					return err
				}
				coreRestarted = true
			} else {
				e.addPending(restartFor)
				e.record(state.KindRestart, "Home Assistant needs a restart", "for "+strings.Join(restartFor, ", "), changed)
				e.notify(ctx, notifyRestart, "1Password Secrets: restart needed",
					"New secret values are written, but "+strings.Join(restartFor, ", ")+" only reads them at startup. Restart Home Assistant to apply them.")
			}
		}
		p.Stage = "addons"
		e.st.Pending = p
		if err := e.st.Save(e.cfg.StatePath); err != nil {
			e.record(state.KindError, "Saving state failed", err.Error(), nil)
		}
	}

	if len(who.addons) > 0 && e.cfg.Options.RestartAddons {
		if wait := e.cfg.SupervisorSecretsDelay - e.cfg.Now().Sub(p.At); wait > 0 {
			e.setPhase("Waiting for Supervisor to re-read secrets.yaml")
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
		}
		slugs := make([]string, 0, len(who.addons))
		for s := range who.addons {
			slugs = append(slugs, s)
		}
		sort.Strings(slugs)
		for _, slug := range slugs {
			if slug == e.cfg.SelfSlug {
				continue
			}
			if !e.addonRunning(slug) {
				e.record(state.KindInfo, e.addonName(slug)+" is not running; it reads the new value when it starts", "", who.addons[slug])
				continue
			}
			e.setPhase("Restarting " + e.addonName(slug))
			if err := e.ha.RestartAddon(ctx, slug); err != nil {
				e.record(state.KindError, "Restarting "+e.addonName(slug)+" failed", safeErr(err), who.addons[slug])
				continue
			}
			restartedAddons = append(restartedAddons, slug)
			e.record(state.KindRefresh, "Restarted "+e.addonName(slug), "", who.addons[slug])
		}
	} else if len(who.addons) > 0 {
		var names []string
		for s := range who.addons {
			names = append(names, e.addonName(s))
		}
		sort.Strings(names)
		e.record(state.KindRestart, "Add-ons need a restart: "+strings.Join(names, ", "), "restart_addons is off", changed)
	}

	if len(who.devices) > 0 {
		files := make([]string, 0, len(who.devices))
		for f := range who.devices {
			files = append(files, path.Base(f))
		}
		sort.Strings(files)
		e.record(state.KindRestart, "ESPHome devices need a new build", strings.Join(files, ", "), changed)
		e.notify(ctx, notifyDevices, "1Password Secrets: rebuild ESPHome devices",
			"These ESPHome devices use a secret that changed. Install them again from the ESPHome dashboard so they get the new value: "+strings.Join(files, ", ")+".")
	}
	for f, keys := range who.others {
		e.record(state.KindInfo, f+" uses a changed key", "Nothing known reads this file; restart whatever does.", keys)
	}

	e.dropPrevious(p.Written)
	e.st.Pending = nil
	if err := e.st.Save(e.cfg.StatePath); err != nil {
		e.record(state.KindError, "Saving state failed", err.Error(), nil)
	}
	e.setPhase("")
	files := make([]string, 0, len(p.Written))
	for _, w := range p.Written {
		files = append(files, w.Path)
	}
	e.fire(ctx, changed, files, reloaded, restartedAddons, coreRestarted)
	return nil
}

// fail2 rolls a change back, holds it, and says so.
func (e *Engine) fail2(ctx context.Context, p *state.PendingRefresh, reason, message string) error {
	e.rollback(p.Written, p.Before)
	e.holdBack(p.Signature, reason)
	e.record(state.KindError, "Rolled back: "+reason, "", p.Keys())
	e.notify(ctx, notifyError, "1Password Secrets: change rolled back", message)
	e.setPhase("")
	return &RolledBackError{Reason: reason}
}

// guardedRestart restarts Core after a config check, waits for it to go
// down and come back, and on failure restores the previous files and
// restarts again.
func (e *Engine) guardedRestart(ctx context.Context, p *state.PendingRefresh, reasons []string) error {
	keys := p.Keys()
	e.setPhase("Validating the configuration")
	if ok, reason := e.ha.CheckConfig(ctx); !ok {
		reason = safeReason(reason)
		return e.fail2(ctx, p, "the configuration check failed: "+reason,
			"Home Assistant's configuration check failed with the new values, so the secrets files were restored.\n\n"+reason)
	}
	e.setPhase("Restarting Home Assistant")
	e.record(state.KindRestart, "Restarting Home Assistant", "for "+strings.Join(reasons, ", "), keys)
	ok, why := e.restartAndWait(ctx)
	if ok {
		e.clearPending()
		e.record(state.KindRefresh, "Home Assistant is back", "", keys)
		return nil
	}
	e.rollback(p.Written, p.Before)
	e.holdBack(p.Signature, why)
	e.record(state.KindError, "Rolled back: "+why, "Restarting again with the previous values.", keys)
	recovered, why2 := e.restartAndWait(ctx)
	msg := "Home Assistant did not come back after a restart with the new secret values (" + why + "), so the previous secrets files were restored and it was restarted again."
	if !recovered {
		msg += " That restart failed too (" + why2 + "); check Home Assistant's log."
	}
	e.notify(ctx, notifyError, "1Password Secrets: restart failed, rolled back", msg)
	e.setPhase("")
	return &RolledBackError{Reason: why}
}

// restartAndWait restarts Core and reports whether it really went down and
// answered again. A restart Supervisor or Core refused (an HTTP error) is a
// failure; a dropped connection is not, as long as Core is seen down.
func (e *Engine) restartAndWait(ctx context.Context) (bool, string) {
	if err := e.ha.RestartCore(ctx); err != nil {
		var se *ha.StatusError
		if errors.As(err, &se) {
			return false, "Home Assistant refused the restart (" + safeErr(err) + ")"
		}
	}
	deadline := e.cfg.Now().Add(e.cfg.RestartDownTimeout)
	wentDown := false
	for e.cfg.Now().Before(deadline) {
		if !e.ha.Probe(ctx) {
			wentDown = true
			break
		}
		select {
		case <-ctx.Done():
			return false, "interrupted"
		case <-time.After(2 * time.Second):
		}
	}
	if !wentDown {
		return false, "Home Assistant did not restart"
	}
	if !e.ha.WaitHealthy(ctx, e.cfg.RestartUpTimeout) {
		return false, "Home Assistant did not come back in time"
	}
	return true, ""
}

// RestartCoreNow is the UI's "Restart Home Assistant" for a pending
// restart in notify mode. Guarded the same way, without a rollback (the
// files were written cycles ago).
func (e *Engine) RestartCoreNow(ctx context.Context) error {
	e.runMu.Lock()
	defer e.runMu.Unlock()
	e.setSyncing(true)
	defer e.setSyncing(false)
	e.setPhase("Validating the configuration")
	defer e.setPhase("")
	if ok, reason := e.ha.CheckConfig(ctx); !ok {
		reason = safeReason(reason)
		e.record(state.KindError, "Restart refused: the configuration check failed", reason, nil)
		return fmt.Errorf("configuration check failed: %s", reason)
	}
	e.setPhase("Restarting Home Assistant")
	e.record(state.KindRestart, "Restarting Home Assistant", "requested from the panel", nil)
	if ok, why := e.restartAndWait(ctx); !ok {
		e.record(state.KindError, why, "", nil)
		return errors.New(why)
	}
	e.clearPending()
	_ = e.ha.Dismiss(ctx, notifyRestart)
	e.record(state.KindRefresh, "Home Assistant is back", "", nil)
	return nil
}

// holdBack remembers a failed change so it is not retried every poll.
func (e *Engine) holdBack(signature, reason string) {
	e.st.HeldBack = signature
	e.st.HeldBackReason = reason
	if err := e.st.Save(e.cfg.StatePath); err != nil {
		e.record(state.KindError, "Saving state failed", err.Error(), nil)
	}
}

func (e *Engine) addPending(domains []string) {
	set := map[string]bool{}
	for _, d := range e.st.RestartPending {
		set[d] = true
	}
	for _, d := range domains {
		set[d] = true
	}
	e.st.RestartPending = sortedKeys(set)
	_ = e.st.Save(e.cfg.StatePath)
}

func (e *Engine) clearPending() {
	if len(e.st.RestartPending) == 0 {
		return
	}
	e.st.RestartPending = nil
	_ = e.st.Save(e.cfg.StatePath)
}

func (e *Engine) setPhase(phase string) {
	e.mu.Lock()
	if phase == "" {
		e.status.Phase = ""
	} else {
		e.status.Phase = phase
	}
	e.mu.Unlock()
}

func (e *Engine) notify(ctx context.Context, id, title, message string) {
	if err := e.ha.Notify(ctx, id, title, message); err != nil {
		e.record(state.KindError, "Could not create a notification", err.Error(), nil)
	}
}

// fire sends the change event. Keys and actions only, never a value.
func (e *Engine) fire(ctx context.Context, keys, files, reloaded, addons []string, core bool) {
	data := map[string]any{
		"keys":             keys,
		"files":            files,
		"reloaded":         nonNil(reloaded),
		"restarted_addons": nonNil(addons),
		"core_restarted":   core,
	}
	_ = e.ha.FireEvent(ctx, EventChanged, data)
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// routedRefs is exported for the status builder: who uses a key.
func usesOf(routed []refscan.Ref, addonRefs []refscan.AddonRef, key string) []Use {
	var out []Use
	for _, r := range routed {
		if r.Key != key {
			continue
		}
		u := Use{File: r.File, Line: r.Line, Domain: r.Domain}
		top := strings.SplitN(r.File, "/", 2)[0]
		switch {
		case r.Domain != "":
			u.Kind = "ha"
			u.Label = r.Domain
		case top == "esphome":
			u.Kind = "esphome"
			u.Label = strings.TrimSuffix(path.Base(r.File), path.Ext(r.File))
		default:
			u.Kind = "file"
			u.Label = top
		}
		out = append(out, u)
	}
	for _, a := range addonRefs {
		if a.Key == key {
			out = append(out, Use{Kind: "addon", Label: a.Name, File: a.Option})
		}
	}
	return out
}

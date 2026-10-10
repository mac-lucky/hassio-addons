package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mac-lucky/hassio-addons/onepassword_secrets/internal/discover"
	"github.com/mac-lucky/hassio-addons/onepassword_secrets/internal/fsx"
	"github.com/mac-lucky/hassio-addons/onepassword_secrets/internal/opconnect"
	"github.com/mac-lucky/hassio-addons/onepassword_secrets/internal/refscan"
	"github.com/mac-lucky/hassio-addons/onepassword_secrets/internal/render"
	"github.com/mac-lucky/hassio-addons/onepassword_secrets/internal/state"
)

// fileJob is one secrets file in one cycle.
type fileJob struct {
	path       string // config-relative
	abs        string
	current    []byte
	existed    bool
	mode       os.FileMode
	plan       render.Plan
	desired    map[string]bool
	keep       map[string]bool
	referenced map[string]bool
	err        error
}

// cycle is everything one sync worked out, for the status.
type cycle struct {
	vaults    []VaultInfo
	catalog   discover.Catalog
	routed    []refscan.Ref
	addonRefs []refscan.AddonRef
	jobs      []*fileJob
	problems  []Problem
}

// SyncOnce runs one full cycle.
func (e *Engine) SyncOnce(ctx context.Context) {
	e.runMu.Lock()
	defer e.runMu.Unlock()
	e.setSyncing(true)
	defer e.setSyncing(false)

	opts := e.cfg.Options
	next := e.baseStatus()
	if !opts.Configured() {
		next.State = StateUnconfigured
		next.Headline = "Not set up yet"
		next.Detail = "Add the Connect credentials and token in the add-on's Configuration tab."
		next.Setup = setupSteps(opts, next, nil)
		e.publish(ctx, next)
		return
	}

	health, err := e.connect.Health(ctx)
	if err != nil {
		if e.connectCrashLooping() {
			e.fail(ctx, next, connectStoppingHeadline, err, connectStoppingFix)
			return
		}
		if e.connectJustStarted() {
			// The API listens only once connect-sync has created its
			// database: a second or so after every start.
			next.State = StateStarting
			next.Headline = "Starting Connect"
			next.Detail = "The built-in Connect server is starting."
			e.keepLastView(&next)
			next.Setup = setupSteps(opts, next, nil)
			e.publish(ctx, next)
			return
		}
		e.fail(ctx, next, "Connect is not answering", err, "The embedded server may still be starting; its log lines are in the add-on log.")
		return
	}
	if !health.Synced() {
		// Connect starts syncing only after a request with a valid token
		// ("Make a request with a valid bearer token to initialize"), so
		// make one; its error is expected until the first sync is through.
		if _, err := e.connect.Vaults(ctx); err != nil && opconnect.IsUnauthorized(err) {
			e.fail(ctx, next, "Connect refused the access token", err, "The token does not belong to this Connect server, expired or was revoked. Create a new one for this server and put it in connect_token.")
			return
		}
		if h2, err := e.connect.Health(ctx); err == nil {
			health = h2
		}
	}
	next.Connect.Reachable = true
	next.Connect.ServerVersion = health.Version
	next.Connect.Dependencies = health.Dependencies
	next.Connect.Synced = health.Synced()
	if !next.Connect.Synced {
		// Once its database exists, connect-api answers /health with
		// connect-sync down, so a sync crash loop shows up here.
		if e.connectCrashLooping() {
			e.fail(ctx, next, connectStoppingHeadline, errors.New("a Connect process keeps exiting"), connectStoppingFix)
			return
		}
		dep := health.Dependency("account_data")
		next.State = StateStarting
		next.Headline = "Waiting for Connect's first sync"
		next.Detail = dep.Message
		if op := health.Dependency("1Password"); strings.EqualFold(op.Status, "ERROR") || strings.Contains(strings.ToLower(op.Message), "credentials") {
			next.State = StateError
			next.Headline = "Connect cannot reach 1Password"
			next.Detail = op.Message
		}
		next.Setup = setupSteps(opts, next, nil)
		e.publish(ctx, next)
		return
	}

	vaults, err := e.connect.Vaults(ctx)
	if err != nil {
		fix := ""
		if opconnect.IsUnauthorized(err) {
			fix = "Connect refused the access token. Create a new one for this server and put it in connect_token."
		}
		e.fail(ctx, next, "Could not list vaults", err, fix)
		return
	}

	c := &cycle{}
	fetched, err := e.fetch(ctx, vaults, c)
	if err != nil {
		e.fail(ctx, next, "Could not read items", err, "")
		return
	}
	refs := make([]discover.Reference, len(opts.References))
	for i, r := range opts.References {
		refs[i] = discover.Reference{Key: r.Key, Ref: r.Ref}
	}
	c.catalog = discover.Build(fetched, refs)

	if err := e.rescan(); err != nil {
		e.fail(ctx, next, "Could not scan the configuration", err, "")
		return
	}
	c.addonRefs = e.addonRefsCached(ctx)

	e.plan(c)

	var writeErr error
	if opts.DryRun {
		e.noteDryRun(c)
	} else {
		writeErr = e.resumePending(ctx, c)
		if writeErr == nil {
			writeErr = e.write(ctx, c)
		}
	}

	next = e.buildStatus(next, c)
	if writeErr != nil {
		next.State = StateError
		next.Headline = "Could not write the secrets files"
		next.Detail = writeErr.Error()
		var rb *RolledBackError
		if errors.As(writeErr, &rb) {
			next.Headline = "Change rolled back"
			next.Detail = rb.Reason
		}
		next.Problems = append([]Problem{{Severity: "error", Title: next.Headline, Detail: next.Detail, Fix: "The previous values are back in place. Fix the cause, then press Sync now."}}, next.Problems...)
	}
	e.publish(ctx, next)
}

const (
	connectStoppingHeadline = "Connect keeps stopping"
	connectStoppingFix      = "Its processes exit soon after they start. The add-on log has the reason, on the lines marked component=connect-api or component=connect-sync."
)

// connectCrashLooping reports an embedded Connect process that exited
// several times without staying up in between, the last time within the
// longest restart backoff, so the headline can tell a crash loop from a
// slow start.
func (e *Engine) connectCrashLooping() bool {
	if e.procs == nil {
		return false
	}
	now := e.cfg.Now()
	for _, p := range e.procs.Status() {
		if p.QuickExits >= 3 && now.Sub(p.LastExitAt) < 2*time.Minute {
			return true
		}
	}
	return false
}

// connectStartGrace is how long after a Connect process (re)starts an
// unanswered health check counts as starting, not as an error.
const connectStartGrace = time.Minute

// connectJustStarted reports an embedded Connect process started within
// connectStartGrace.
func (e *Engine) connectJustStarted() bool {
	if e.procs == nil {
		return false
	}
	now := e.cfg.Now()
	for _, p := range e.procs.Status() {
		if !p.StartedAt.IsZero() && now.Sub(p.StartedAt) < connectStartGrace {
			return true
		}
	}
	return false
}

// baseStatus is the part of Status that does not depend on a cycle.
func (e *Engine) baseStatus() Status {
	now := e.cfg.Now()
	e.mu.Lock()
	prev := e.status
	e.mu.Unlock()
	s := Status{
		Version:        e.cfg.Version,
		DryRun:         e.cfg.Options.DryRun,
		Embedded:       e.cfg.Options.Embedded(),
		Token:          tokenInfo(e.cfg.Options.ConnectToken, now),
		LastSync:       now,
		NextSync:       prev.NextSync,
		LastChange:     e.st.LastWrite,
		RestartPending: append([]string(nil), e.st.RestartPending...),
	}
	if !e.cfg.Options.Embedded() {
		s.Connect.URL = redactURL(e.cfg.Options.ConnectURL)
	}
	return s
}

// fail publishes a failed cycle. The files keep their last good content.
func (e *Engine) fail(ctx context.Context, next Status, headline string, err error, fix string) {
	next.State = StateError
	next.Headline = headline
	next.Detail = err.Error()
	next.Connect.Error = err.Error()
	if fix != "" {
		next.Problems = append(next.Problems, Problem{Severity: "error", Title: headline, Detail: err.Error(), Fix: fix})
	}
	e.keepLastView(&next)
	next.Setup = setupSteps(e.cfg.Options, next, nil)
	e.publish(ctx, next)
}

// keepLastView carries the last good view of the keys into next, so the
// page does not go blank while Connect restarts.
func (e *Engine) keepLastView(next *Status) {
	e.mu.Lock()
	next.Secrets = e.status.Secrets
	next.Files = e.status.Files
	next.Vaults = e.status.Vaults
	next.Counts = e.status.Counts
	e.mu.Unlock()
}

// fetch reads the vaults the options name (and the ones references point
// into), fetching a full item only when its version moved.
func (e *Engine) fetch(ctx context.Context, vaults []opconnect.Vault, c *cycle) ([]discover.Vault, error) {
	find := func(nameOrID string) (opconnect.Vault, bool) {
		for _, v := range vaults {
			if v.ID == nameOrID || v.Name == nameOrID {
				return v, true
			}
		}
		return opconnect.Vault{}, false
	}
	type want struct {
		vault    opconnect.Vault
		discover bool
		items    map[string]bool // for reference-only vaults: titles or IDs
	}
	wants := map[string]*want{}
	var order []string
	add := func(v opconnect.Vault) *want {
		if w, ok := wants[v.ID]; ok {
			return w
		}
		w := &want{vault: v, items: map[string]bool{}}
		wants[v.ID] = w
		order = append(order, v.ID)
		return w
	}
	for _, name := range e.cfg.Options.Vaults {
		v, ok := find(name)
		if !ok {
			c.vaults = append(c.vaults, VaultInfo{Name: name, Discover: true, Missing: true})
			c.problems = append(c.problems, Problem{
				Severity: "error",
				Title:    fmt.Sprintf("Vault %q is not readable", name),
				Detail:   "The access token cannot see a vault with this name or ID.",
				Fix:      "Grant the Connect server and token read access to it, or correct the vaults option.",
			})
			continue
		}
		add(v).discover = true
	}
	for _, r := range e.cfg.Options.References {
		p, err := discover.ParseRef(r.Ref)
		if err != nil {
			continue
		}
		if v, ok := find(p.Vault); ok {
			add(v).items[p.Item] = true
		}
	}

	seen := map[string]bool{}
	var out []discover.Vault
	for _, id := range order {
		w := wants[id]
		summaries, err := e.connect.Items(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("vault %s: %w", w.vault.Name, err)
		}
		dv := discover.Vault{Vault: w.vault, Discover: w.discover}
		for _, s := range summaries {
			if !w.discover && !w.items[s.Title] && !w.items[s.ID] {
				continue
			}
			seen[s.ID] = true
			if cached, ok := e.items[s.ID]; ok && cached.item.Version == s.Version && cached.item.VaultID == id {
				dv.Items = append(dv.Items, cached.item)
				continue
			}
			item, err := e.connect.Item(ctx, id, s.ID)
			if err != nil {
				if opconnect.IsNotFound(err) {
					continue // deleted between the list and the fetch
				}
				return nil, fmt.Errorf("vault %s, item %s: %w", w.vault.Name, s.Title, err)
			}
			if item.VaultID == "" {
				item.VaultID = id
			}
			e.items[s.ID] = cachedItem{item: item}
			dv.Items = append(dv.Items, item)
		}
		out = append(out, dv)
		c.vaults = append(c.vaults, VaultInfo{Name: w.vault.Name, ID: w.vault.ID, Items: len(dv.Items), Discover: w.discover})
	}
	for id := range e.items {
		if !seen[id] {
			delete(e.items, id)
		}
	}
	return out, nil
}

func (e *Engine) scanOptions() refscan.Options {
	targets := map[string]bool{}
	for _, f := range e.cfg.Options.SecretsFiles {
		targets[f] = true
	}
	return refscan.Options{Root: e.cfg.Root, Skip: func(rel string) bool { return targets[rel] }}
}

// rescan rescans the config tree when its fingerprint moved.
func (e *Engine) rescan() error {
	o := e.scanOptions()
	fp, err := refscan.Fingerprint(o)
	if err != nil {
		return err
	}
	if e.scanned && fp == e.scan.Fingerprint {
		return nil
	}
	res, err := refscan.Scan(o)
	if err != nil {
		return err
	}
	e.scan = res
	e.scanned = true
	return nil
}

// addonRefsCached re-reads other add-ons' options at most every
// AddonRefsTTL; a failure keeps the previous answer.
func (e *Engine) addonRefsCached(ctx context.Context) []refscan.AddonRef {
	if !e.addonRefsAt.IsZero() && e.cfg.Now().Sub(e.addonRefsAt) < e.cfg.AddonRefsTTL {
		return e.addonRefs
	}
	addons, err := e.ha.Addons(ctx)
	if err != nil {
		return e.addonRefs
	}
	var refs []refscan.AddonRef
	for _, a := range addons {
		if a.Slug == e.cfg.SelfSlug {
			continue
		}
		opts, err := e.ha.AddonOptions(ctx, a.Slug)
		if err != nil {
			continue
		}
		refs = append(refs, refscan.AddonOptionRefs(a.Slug, a.Name, opts)...)
	}
	e.addons = addons
	e.addonRefs = refs
	e.addonRefsAt = e.cfg.Now()
	e.addonsLoaded = true
	return refs
}

// ForgetAddonRefs makes the next cycle re-read add-on options.
func (e *Engine) ForgetAddonRefs() {
	e.runMu.Lock()
	e.addonRefsAt = time.Time{}
	e.runMu.Unlock()
}

// plan routes keys to files and renders every file.
func (e *Engine) plan(c *cycle) {
	targets := e.cfg.Options.SecretsFiles
	root := targets[0]
	referenced := map[string]map[string]bool{}
	for _, t := range targets {
		referenced[t] = map[string]bool{}
	}
	for _, r := range e.scan.Refs {
		t, ok := refscan.TargetFor(r, targets)
		if !ok {
			continue
		}
		c.routed = append(c.routed, r)
		referenced[t][r.Key] = true
	}
	for _, a := range c.addonRefs {
		referenced[root][a.Key] = true
	}

	pruneSafe := e.addonsLoaded
	for _, v := range c.vaults {
		if v.Missing {
			pruneSafe = false
		}
	}
	for _, file := range targets {
		job := &fileJob{path: file, abs: filepath.Join(e.cfg.Root, filepath.FromSlash(file)), referenced: referenced[file], desired: map[string]bool{}, keep: map[string]bool{}}
		c.jobs = append(c.jobs, job)
		job.current, job.existed, job.mode, job.err = e.readTarget(job.abs)
		if job.err != nil {
			continue
		}
		var desired []render.Desired
		for key, entry := range c.catalog.Entries {
			if file != root && !job.referenced[key] {
				continue
			}
			job.desired[key] = true
			desired = append(desired, render.Desired{Key: key, Value: entry.Value, Structured: entry.Structured})
		}
		sort.Slice(desired, func(i, j int) bool { return desired[i].Key < desired[j].Key })
		managed := e.st.ManagedSet(file)
		for key := range c.catalog.Conflicted {
			if managed[key] {
				job.keep[key] = true
			}
		}
		if !pruneSafe {
			// A vault that went missing (lost access, renamed) or add-on
			// options not read yet: a key absent from this cycle's view is
			// not known to be gone, so every written key stays.
			for key := range managed {
				if !job.desired[key] {
					job.keep[key] = true
				}
			}
		}
		if !job.existed && len(desired) == 0 {
			continue // nothing to put in a file that is not there
		}
		if !job.existed {
			if _, err := os.Stat(filepath.Dir(job.abs)); err != nil {
				job.err = fmt.Errorf("%s: its directory does not exist", file)
				continue
			}
		}
		job.plan, job.err = render.Render(render.Input{
			Current:           job.current,
			Desired:           desired,
			Keep:              job.keep,
			PreviouslyManaged: managed,
			Referenced:        job.referenced,
		})
	}
}

// readTarget reads a secrets file, refusing anything that is not a
// regular file inside the config directory (a symlink pointing out of it
// would make this add-on write wherever it pointed).
func (e *Engine) readTarget(abs string) (content []byte, existed bool, mode os.FileMode, err error) {
	root := fsx.Realpath(e.cfg.Root)
	real := fsx.Realpath(abs)
	if real != root && !strings.HasPrefix(real, root+string(filepath.Separator)) {
		return nil, false, 0, errors.New("resolves outside the config directory")
	}
	info, err := os.Lstat(abs)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, 0o600, nil
	}
	if err != nil {
		return nil, false, 0, err
	}
	if !info.Mode().IsRegular() {
		return nil, false, 0, errors.New("is not a regular file")
	}
	data, err := os.ReadFile(abs) // #nosec G304 -- contained above
	if err != nil {
		return nil, false, 0, err
	}
	return data, true, info.Mode().Perm(), nil
}

func (e *Engine) noteDryRun(c *cycle) {
	var parts []string
	var keys []string
	for _, j := range c.jobs {
		if j.err != nil || !j.plan.Touched() {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s: %d new, %d changed, %d removed", j.path, len(j.plan.Added), len(j.plan.Changed), len(j.plan.Removed)))
		keys = append(keys, j.plan.Added...)
		keys = append(keys, j.plan.Changed...)
		keys = append(keys, j.plan.Removed...)
	}
	sig := strings.Join(parts, "; ")
	if sig == e.lastDryRun {
		return
	}
	e.lastDryRun = sig
	if sig == "" {
		e.record(state.KindDryRun, "Dry run: the files already match 1Password", "", nil)
		return
	}
	e.record(state.KindDryRun, "Dry run: would update the secrets files", sig, dedupe(keys))
}

// write writes every file whose content moved, remembers what it wrote,
// and refreshes the consumers of changed keys. The change is recorded as
// pending in state.json before the first byte is written, so an interrupted
// refresh is resumed by the next cycle (resumePending).
func (e *Engine) write(ctx context.Context, c *cycle) error {
	now := e.cfg.Now().UTC()
	sig := e.changeSignature(c)
	if sig != "" && sig == e.st.HeldBack {
		c.problems = append(c.problems, Problem{
			Severity: "error",
			Title:    "Update held back",
			Detail:   "The last attempt to apply these values failed and was rolled back: " + e.st.HeldBackReason,
			Fix:      "Fix the cause and press Sync now, or change the value in 1Password.",
		})
		return nil
	}

	var todo []*fileJob
	changed := map[string][]string{}
	for _, j := range c.jobs {
		if j.err != nil || j.plan.Content == nil || bytes.Equal(j.plan.Content, j.current) {
			continue
		}
		todo = append(todo, j)
		if keys := touchedKeys(j.plan); len(keys) > 0 {
			changed[j.path] = keys
		}
	}

	before := e.st.Snap()
	pending := &state.PendingRefresh{Changed: changed, Stage: "all", Signature: sig, Before: before, At: now}
	for _, j := range todo {
		f := state.PendingFile{Path: j.path, Existed: j.existed}
		if j.existed {
			f.Previous = filepath.Join(e.cfg.PreviousDir, previousName(j.path))
		}
		pending.Written = append(pending.Written, f)
	}
	if len(changed) > 0 {
		e.st.Pending = pending
		if err := e.st.Save(e.cfg.StatePath); err != nil {
			e.st.Pending = nil
			return fmt.Errorf("saving state before the write: %w", err)
		}
	}

	var written []state.PendingFile
	for i, j := range todo {
		f := pending.Written[i]
		if j.existed {
			if err := os.MkdirAll(e.cfg.PreviousDir, 0o700); err != nil {
				e.rollback(written, before)
				return err
			}
			if err := fsx.WriteFileAtomic(f.Previous, j.current, 0o600); err != nil {
				e.rollback(written, before)
				return fmt.Errorf("keeping the previous %s: %w", j.path, err)
			}
		}
		mode := j.mode
		if mode == 0 {
			mode = 0o600
		}
		if err := fsx.WriteFileAtomicRandom(j.abs, j.plan.Content, mode); err != nil {
			e.rollback(append(written, f), before)
			return fmt.Errorf("writing %s: %w", j.path, err)
		}
		written = append(written, f)
	}

	// Remember what each file now holds from 1Password, and each value's
	// fingerprint, whether or not a byte moved (adoption).
	for _, j := range c.jobs {
		if j.err != nil || j.plan.Content == nil {
			continue
		}
		managed := map[string]bool{}
		for k := range j.desired {
			managed[k] = true
		}
		for k := range j.keep {
			managed[k] = true
		}
		for _, k := range j.plan.Orphaned {
			managed[k] = true
		}
		e.st.SetManaged(j.path, managed)
	}
	for key, entry := range c.catalog.Entries {
		fp := entry.Value.Fingerprint(e.key)
		prev, ok := e.st.Keys[key]
		if !ok || prev.FP != fp {
			changedAt := now
			if !ok && !entry.ItemUpdated.IsZero() {
				changedAt = entry.ItemUpdated.UTC()
			}
			e.st.Keys[key] = state.KeyState{FP: fp, ChangedAt: changedAt, Source: entry.Source()}
		} else if prev.Source != entry.Source() {
			prev.Source = entry.Source()
			e.st.Keys[key] = prev
		}
	}
	for key := range e.st.Keys {
		if _, ok := c.catalog.Entries[key]; !ok && !c.catalog.Conflicted[key] {
			delete(e.st.Keys, key)
		}
	}
	if len(written) > 0 {
		e.st.LastWrite = now
	}
	if err := e.st.Save(e.cfg.StatePath); err != nil {
		e.rollback(written, before)
		return fmt.Errorf("saving state: %w", err)
	}
	if len(written) == 0 {
		return nil
	}

	var files []string
	for _, w := range written {
		files = append(files, w.Path)
	}
	all := pending.Keys()
	if len(all) == 0 {
		e.record(state.KindInfo, "Rewrote "+strings.Join(files, ", "), "Formatting or the header only; no value moved.", nil)
		e.dropPrevious(written)
		return nil
	}
	e.record(state.KindChange, fmt.Sprintf("Updated %s", plural(len(all), "key", "keys")), "in "+strings.Join(files, ", "), all)
	return e.refresh(ctx, c, pending)
}

// touchedKeys are the keys a plan adds, changes or removes.
func touchedKeys(p render.Plan) []string {
	keys := append(append(append([]string{}, p.Added...), p.Changed...), p.Removed...)
	sort.Strings(keys)
	return keys
}

// previousName is the /data/previous file name for a config-relative path:
// a hash, so no two paths can share a copy.
func previousName(path string) string {
	sum := sha256.Sum256([]byte(path))
	return hex.EncodeToString(sum[:8])
}

// changeSignature identifies what this cycle would change: the keys and
// fingerprints of every value about to be written differently. A failed
// apply of the same signature is not retried every poll.
func (e *Engine) changeSignature(c *cycle) string {
	var parts []string
	for _, j := range c.jobs {
		if j.err != nil || !j.plan.Touched() {
			continue
		}
		for _, k := range touchedKeys(j.plan) {
			fp := ""
			if entry, ok := c.catalog.Entries[k]; ok {
				fp = entry.Value.Fingerprint(e.key)
			}
			parts = append(parts, j.path+":"+k+":"+fp)
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, "|")
}

// rollback restores the files, and the memory of them, as they were
// before the write. A copy whose restore failed is kept, so nothing is
// lost; the failure is recorded.
func (e *Engine) rollback(written []state.PendingFile, before state.Snapshot) {
	for _, w := range written {
		abs := filepath.Join(e.cfg.Root, filepath.FromSlash(w.Path))
		if !w.Existed {
			if err := os.Remove(abs); err != nil && !errors.Is(err, fs.ErrNotExist) {
				e.record(state.KindError, "Rollback of "+w.Path+" failed", err.Error(), nil)
			}
			continue
		}
		data, err := os.ReadFile(w.Previous) // #nosec G304 -- our own /data copy
		if err != nil {
			e.record(state.KindError, "Rollback of "+w.Path+" failed", "its previous copy is gone", nil)
			continue
		}
		mode := os.FileMode(0o600)
		if info, err := os.Stat(abs); err == nil {
			mode = info.Mode().Perm()
		}
		if err := fsx.WriteFileAtomicRandom(abs, data, mode); err != nil {
			e.record(state.KindError, "Rollback of "+w.Path+" failed", err.Error()+"; the previous copy stays in /data/previous", nil)
			continue
		}
		_ = os.Remove(w.Previous)
	}
	e.st.Restore(before)
	e.st.Pending = nil
	if err := e.st.Save(e.cfg.StatePath); err != nil {
		e.record(state.KindError, "Saving state after a rollback failed", err.Error(), nil)
	}
}

// dropPrevious deletes the plaintext copies once they are not needed.
func (e *Engine) dropPrevious(written []state.PendingFile) {
	for _, w := range written {
		if w.Previous != "" {
			_ = os.Remove(w.Previous)
		}
	}
}

// cleanPrevious removes copies no pending change refers to (left by a
// crash between a write and its cleanup).
func (e *Engine) cleanPrevious() {
	keep := map[string]bool{}
	if e.st.Pending != nil {
		for _, w := range e.st.Pending.Written {
			keep[filepath.Base(w.Previous)] = true
		}
	}
	entries, err := os.ReadDir(e.cfg.PreviousDir)
	if err != nil {
		return
	}
	for _, de := range entries {
		if !keep[de.Name()] {
			_ = os.Remove(filepath.Join(e.cfg.PreviousDir, de.Name()))
		}
	}
}

// redactURL hides a password in a URL's userinfo; the status reaches the
// panel and /status.json.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(unparsable URL)"
	}
	return u.Redacted()
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func dedupe(in []string) []string {
	set := map[string]bool{}
	for _, s := range in {
		set[s] = true
	}
	return sortedKeys(set)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

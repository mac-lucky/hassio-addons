package recon

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"sort"

	"go.yaml.in/yaml/v3"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/regapply"
)

// This file is the track_addon_versions option end to end: at the tail of
// every completed reconcile cycle, read the installed add-ons and write
// their versions into the repository.
//
// The mirror image of addonupdate.go, and unaware of it: that one CHANGES
// versions, this one only OBSERVES them whoever moved them (the HA UI,
// Supervisor's auto-update, this agent, a backup restore). Nothing here is
// planned, applied or read back - it is a record, not a manifest - which
// is why a failure never touches state/lastError. Not gated on dry_run
// either: dry_run governs changes to the BOX, and this writes to the
// repository, the same line import and commit_back draw.

// addonVersionsFile is where the record lives, relative to the repository
// root. Under gitops/ because it belongs to the agent, and gitops/ is
// root-anchored in gitsync.ExcludedPatterns - so the file-sync layer never
// copies it into /homeassistant nor reports it as drift.
const addonVersionsFile = "gitops/addon-versions.yaml"

// addonVersionsCommitMessage is every record commit's fixed message, like
// gitsync's DriftCommitMessage. Fixed rather than describing the change,
// which the diff already says.
const addonVersionsCommitMessage = "versions: record installed add-on versions"

// versionChangeEventCap is how many per-add-on change lines one record
// logs before collapsing the rest into a count - a core update pulling
// twenty add-ons is one event, not twenty in a 200-line log.
const versionChangeEventCap = 5

// addonVersionsHeader opens every rendered record. Part of the rendered
// bytes, so the byte-exact no-op check covers it too.
const addonVersionsHeader = `# Written by the GitOps Agent add-on: every add-on installed on this Home
# Assistant instance, and the version it is on.
#
# Machine-owned. The agent rewrites this file whenever a version changes,
# so any edit made here is overwritten on the next reconcile. It is a
# record of what IS installed, not a manifest of what should be: nothing
# in it is ever applied, and the agent never reads it back.
`

// recordAddonVersions writes the installed add-on versions into the
// repository, committing only when they differ from what is recorded
// (gitsync.RecordFile). Called from the tail of a completed cycle, already
// holding opLock. Every failure path is a warning and nothing more - the
// same isolation lastBackupError and commitDriftBack have, so a push that
// keeps failing cannot turn the dashboard red for an in-sync config.
func (r *Reconciler) recordAddonVersions(ctx context.Context) {
	addons, err := r.registryApplier.FetchInstalledAddons(ctx)
	if err != nil {
		r.noteVersionRecordFailure("could not read the installed add-on list", err)
		return
	}
	if len(addons) == 0 {
		// Never truthful - this agent is itself an installed add-on - so
		// an empty list is Supervisor having a moment. Recording it would
		// blank a real record and restore it next cycle. Process log only:
		// it self-corrects and is not the user's to act on.
		slog.Warn("recon: supervisor reported no installed add-ons, not recording versions")
		return
	}

	content, err := renderAddonVersions(addons)
	if err != nil {
		r.noteVersionRecordFailure("could not render the add-on version record", err)
		return
	}
	committed, err := r.git.RecordFile(ctx, addonVersionsFile, content, addonVersionsCommitMessage)
	if err != nil {
		r.noteVersionRecordFailure("could not record add-on versions", err)
		return
	}
	r.clearVersionRecordFailure()

	current := versionsBySlug(addons)
	r.mu.Lock()
	previous := r.recordedVersions
	// Set whether or not anything was committed: on success the repository
	// holds what was just rendered, so this is the next cycle's baseline.
	r.recordedVersions = current
	if committed {
		r.lastVersionRecordUTC = utcNowISO()
	}
	r.mu.Unlock()

	if !committed {
		return
	}
	r.logVersionChanges(previous, current)
	// No pushStatus: nothing the sensor carries changed. The activity log
	// is what the user sees; LastVersionRecordUTC rides along on Status
	// for /status.json, and is deliberately not a sensor attribute.
}

// noteVersionRecordFailure reports one failed record, logging an event
// only on the way INTO failure. The guard matters most here (see
// noteAddonCheckFailure): this runs every cycle, so a push that cannot
// succeed would fill the 200-entry log in hours. slog gets every one.
func (r *Reconciler) noteVersionRecordFailure(what string, err error) {
	var first bool
	r.withMu(func() {
		first = !r.versionRecordFailed
		r.versionRecordFailed = true
	})
	slog.Warn("recon: "+what, "error", err)
	if first {
		r.logEvent(fmt.Sprintf("warning: %s: %s", what, err.Error()))
	}
}

// clearVersionRecordFailure is the other half of that guard, logging only
// if the previous record failed. Worth an event because ordinary success
// is silent, so nothing else would say the failure had stopped.
func (r *Reconciler) clearVersionRecordFailure() {
	var recovered bool
	r.withMu(func() {
		recovered = r.versionRecordFailed
		r.versionRecordFailed = false
	})
	if recovered {
		r.logEvent("add-on version record recovered")
	}
}

// logVersionChanges writes one event per add-on whose version moved since
// this process's last record, capped at versionChangeEventCap. previous is
// nil until this process has recorded once; no baseline is recovered from
// the repository for that first cycle, since the file's diff already says
// what changed and reading the old blob would cost a git call per cycle.
func (r *Reconciler) logVersionChanges(previous, current map[string]string) {
	lines := versionChangeLines(previous, current)
	if previous == nil || len(lines) == 0 {
		// Either nothing to compare against, or the versions match and
		// something else in the file moved - a display name, or a hand
		// edit just overwritten. Both report what is recorded now.
		r.logEvent(fmt.Sprintf("recorded add-on versions (%d add-on(s))", len(current)))
		return
	}

	shown := lines
	if len(shown) > versionChangeEventCap {
		shown = shown[:versionChangeEventCap]
	}
	for _, line := range shown {
		r.logEvent("recorded version change: " + line)
	}
	if extra := len(lines) - len(shown); extra > 0 {
		r.logEvent(fmt.Sprintf("... and %d more add-on version change(s)", extra))
	}
}

// versionChangeLines describes how current differs from previous, one
// sorted line per add-on that moved, was installed, or went away.
func versionChangeLines(previous, current map[string]string) []string {
	slugs := make([]string, 0, len(current)+len(previous))
	for slug := range current {
		slugs = append(slugs, slug)
	}
	for slug := range previous {
		if _, still := current[slug]; !still {
			slugs = append(slugs, slug)
		}
	}
	sort.Strings(slugs)

	var lines []string
	for _, slug := range slugs {
		was, had := previous[slug]
		now, still := current[slug]
		switch {
		case had && still && was != now:
			lines = append(lines, fmt.Sprintf("%s %s -> %s", slug, was, now))
		case !had && still:
			lines = append(lines, fmt.Sprintf("%s added at %s", slug, now))
		case had && !still:
			lines = append(lines, fmt.Sprintf("%s removed (was %s)", slug, was))
		}
	}
	return lines
}

// versionsBySlug is one record's in-memory shape. Only the version, since
// that is all a change event reports - a display name moving is a
// rewritten file, not a version change.
func versionsBySlug(addons []regapply.InstalledAddon) map[string]string {
	out := make(map[string]string, len(addons))
	for _, a := range addons {
		if _, seen := out[a.Slug]; seen {
			continue
		}
		out[a.Slug] = a.Version
	}
	return out
}

// renderAddonVersions turns the installed list into the exact repository
// bytes: the header, then one entry per add-on sorted by slug.
//
// Determinism is required, not a nicety: gitsync.RecordFile decides
// whether to commit by comparing these bytes against the stored blob, so
// anything unordered would push a reshuffled copy every cycle. Hence
// sorted slugs, first-wins on a duplicate, and an explicit node tree
// rather than marshalling a Go map.
//
// Every scalar is tagged !!str so version "1.2" cannot read back as a
// float for a user's own tooling. That follows YAML 1.2's core schema, so
// legacy 1.1 booleans ("yes", "off") stay unquoted - nothing here reaches
// HA's 1.1 loader, where that would matter (see internal/sopscrypt).
func renderAddonVersions(addons []regapply.InstalledAddon) ([]byte, error) {
	bySlug := make(map[string]regapply.InstalledAddon, len(addons))
	slugs := make([]string, 0, len(addons))
	for _, a := range addons {
		if _, seen := bySlug[a.Slug]; seen {
			continue
		}
		bySlug[a.Slug] = a
		slugs = append(slugs, a.Slug)
	}
	sort.Strings(slugs)

	root := &yaml.Node{Kind: yaml.MappingNode}
	for _, slug := range slugs {
		addon := bySlug[slug]
		entry := &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
			yamlString("name"), yamlString(addon.Name),
			yamlString("version"), yamlString(addon.Version),
		}}
		root.Content = append(root.Content, yamlString(slug), entry)
	}

	var buf bytes.Buffer
	buf.WriteString(addonVersionsHeader)
	enc := yaml.NewEncoder(&buf)
	// Two spaces, matching the hand-written manifests under gitops/;
	// yaml.v3 would otherwise indent nested mappings by four.
	enc.SetIndent(2)
	if err := enc.Encode(root); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func yamlString(value string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
}

// maybeRecordAddonVersions is the reconcile cycle's hook: records when the
// option is on and there is a repository to record into. Split out so
// reconcileNow's call site is one line and the gating lives here.
func (r *Reconciler) maybeRecordAddonVersions(ctx context.Context) {
	if !r.opts.TrackAddonVersions || r.opts.RepoURL == "" {
		return
	}
	r.recordAddonVersions(ctx)
}

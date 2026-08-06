//go:build dev

package web

// The dev preview states, tagged to match dev.go (dev_stub_test.go covers
// the untagged side). The fake agent and helpers come from web_test.go.

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestDevPreviewParamRendersCannedStatus(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.configured = false
	handler := New(agent)

	rec := doRequest(t, handler, http.MethodGet, "/?preview=drift", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "scripts/vacuum_kitchen.yaml") {
		t.Error("body does not render the canned drift fixture")
	}
	if strings.Contains(body, "not configured yet") {
		t.Error("body must not fall through to the agent's first-run page")
	}
}

// Every row shape but the first needs a check to have already run, so the
// preview is the only way to see the four without a Supervisor.
func TestDevPreviewAddonUpdatesRendersEveryRowShape(t *testing.T) {
	devEnv(t)
	handler := New(newFakeAgent())

	rec := doRequest(t, handler, http.MethodGet, "/?preview=addon_updates", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`Add-on updates <span class="count">2</span>`,
		"up to date",
		"2026.7.3 -&gt; 2026.8.0",
		"update failed: supervisor request failed",
		"check failed: supervisor request failed with 502",
		"not installed",
		"refused: will not update self",
		// Six hours, the age past which the client marks a check stale.
		`data-stale-after="21600"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body does not render %q", want)
		}
	}

	// The fold boundary, and why the two "unknown" badges differ: a failed
	// CHECK may succeed next cycle, the two below need a person.
	rows := addonUpdateRows(t, body)
	if len(rows) != 4 {
		t.Fatalf("the main list previews %d rows, want 4", len(rows))
	}
	if !strings.Contains(rows[3], "MariaDB") {
		t.Errorf("the failed check does not preview above the fold: %s", rows[3])
	}
	folded := addonUpdateFoldedRows(t, body)
	if len(folded) != 2 {
		t.Fatalf("the fold previews %d rows, want 2", len(folded))
	}
	for i, want := range []string{"core_typo", "local_ha_gitops_agent"} {
		if !strings.Contains(folded[i], want) {
			t.Errorf("folded row %d is not %s: %s", i, want, folded[i])
		}
	}

	// Both age branches on one screen, which is why devAddonCheckedUTC is
	// not on the fixed date: one row fresh, the rest stale.
	if !strings.Contains(body, `data-utc="`+devAddonCheckedUTC+`" data-utc-rel`) {
		t.Error("no row previews a check made minutes ago")
	}
	if !strings.Contains(body, `data-utc="2026-08-03T14:12:07&#43;00:00" data-utc-rel`) {
		t.Error("no row previews a check old enough to be marked stale")
	}
}

// The transient half of the card, on screen only while a check is in
// flight: the disabled button and the line covering a long install.
func TestDevPreviewAddonCheckingDisablesTheButton(t *testing.T) {
	devEnv(t)
	handler := New(newFakeAgent())

	rec := doRequest(t, handler, http.MethodGet, "/?preview=addon_checking", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !buttonIsDisabled(hxButton(t, body, "addons/check")) {
		t.Error("the check button is not disabled while a check is running")
	}
	if !strings.Contains(body, "Checking for updates now") {
		t.Error("the card does not say a check is under way")
	}
	// The rows on screen during a check are the PREVIOUS check's, which is
	// what makes this state worth previewing rather than guessing at.
	if len(addonUpdateRows(t, body)) == 0 {
		t.Error("the preview blanks the card while checking; a real one keeps the last results")
	}
}

// A real install previews 191 files, so the fixture runs past
// inventoryMax - the only way to see the line that closes a capped list.
func TestDevPreviewImportPreviewCollapsesItsFileList(t *testing.T) {
	devEnv(t)
	handler := New(newFakeAgent())

	rec := doRequest(t, handler, http.MethodGet, "/?preview=import_preview", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	card := importPreviewCard(t, body)
	for _, want := range []string{
		`<ul class="inventory" tabindex="0">`,
		"... and 7 more, listed in full by status.json",
		// The size a real tree weighs, which is what the summary asks the
		// reader to agree to pushing.
		"17.1 MB",
	} {
		if !strings.Contains(card, want) {
			t.Errorf("the previewed card does not render %q", want)
		}
	}
	if !strings.Contains(hxButton(t, body, "import/preview/dismiss"), "Dismiss") {
		t.Error("the previewed card carries no Dismiss button")
	}
}

// The subentry "update" kind covers two unrelated outcomes, a reconfigure
// and a bookkeeping-only unmanage; both need the parent integration set up.
func TestDevPreviewDriftRendersSubentryRows(t *testing.T) {
	devEnv(t)
	handler := New(newFakeAgent())

	rec := doRequest(t, handler, http.MethodGet, "/?preview=drift", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"subentry:widget_kitchen",
		"a new subentry flow will be driven to create this subentry",
		"subentry:widget_hall",
		"a reconfigure flow will re-submit it",
		"stopped managing subentry &#39;widget_old_office&#39;",
		"no live integration entry for domain &#39;google&#39;",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body does not render %q", want)
		}
	}
}

func TestDevPreviewUnknownNameFallsBackToAgent(t *testing.T) {
	devEnv(t)
	handler := New(newFakeAgent())

	rec := doRequest(t, handler, http.MethodGet, "/?preview=nope", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "example.invalid") {
		t.Error("body does not render the agent's real status")
	}
}

// A production request (ingress proxy address, dev flag unset) carrying
// ?preview= must still render the agent's real status.
func TestDevPreviewIgnoredWithoutDevFlag(t *testing.T) {
	t.Setenv(DevEnvVar, "0")
	handler := New(newFakeAgent())

	req := httptest.NewRequest(http.MethodGet, "/?preview=drift", nil)
	req.RemoteAddr = ingressProxyAddr + ":12345"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "example.invalid") {
		t.Error("preview must be ignored when the dev flag is unset")
	}
}

// Without the name travelling with the poll, the live status would
// replace the canned one five seconds after it was asked for.
func TestDevPreviewKeepsPollingItself(t *testing.T) {
	devEnv(t)
	handler := New(newFakeAgent())

	page := doRequest(t, handler, http.MethodGet, "/?preview=applying", nil).Body.String()

	if !strings.Contains(page, "&amp;preview=applying") {
		t.Fatal("the preview does not carry its own name into the polling URL")
	}
	rec := doRequest(t, handler, http.MethodGet, "/fragment?h="+fragmentHashOf(t, page)+"&preview=applying", nil)

	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204 - the preview's first poll should agree with the page", rec.Code)
	}
	changed := doRequest(t, handler, http.MethodGet, "/fragment?preview=applying", nil)
	if !strings.Contains(changed.Body.String(), "pill-applying") {
		t.Error("polling with a preview name does not render the canned status")
	}
}

// Every row shape without a box that has rolled an apply back
// (devRunCatalogue).
func TestDevPreviewHistoryRendersEveryRowShape(t *testing.T) {
	devEnv(t)
	handler := New(newFakeAgent())

	rec := doRequest(t, handler, http.MethodGet, "/?preview=history", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"badge-reconcile",
		"badge-apply",
		"badge-rollback",
		"badge-import",
		"outcome-ok",
		"outcome-in_sync",
		"outcome-drift",
		"outcome-partial",
		"outcome-rolled_back",
		"outcome-error",
		`<span class="sha" title="">-</span>`, // the rollback row's absent SHA
		"3m 12s",                              // humanDuration's minutes branch
		"check_config failed",                 // a long error on its own row
	} {
		if !strings.Contains(body, want) {
			t.Errorf("preview does not render %q", want)
		}
	}
}

// runListOf is the run-history <ul> alone, for counting rows on a page
// whose activity feed is made of <li> too.
func runListOf(t *testing.T, body string) string {
	t.Helper()
	const open = `<ul class="runs" tabindex="0">`
	start := strings.Index(body, open)
	if start < 0 {
		t.Fatal("body renders no run list")
	}
	rest := body[start:]
	end := strings.Index(rest, "</ul>")
	if end < 0 {
		t.Fatal("body renders an unterminated run list")
	}
	return rest[:end]
}

// The page renders every row shape too, and more rows than the card it
// was reached from - which is the page's reason to exist.
func TestDevPreviewHistoryPageRendersEveryRowShape(t *testing.T) {
	devEnv(t)
	handler := New(newFakeAgent())

	rec := doRequest(t, handler, http.MethodGet, "/history?preview=history", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"<h1>Run history</h1>",
		"badge-reconcile",
		"badge-apply",
		"badge-rollback",
		"badge-import",
		"outcome-ok",
		"outcome-in_sync",
		"outcome-drift",
		"outcome-partial",
		"outcome-rolled_back",
		"outcome-error",
		`<span class="sha" title="">-</span>`, // the rollback row's absent SHA
		"3m 12s",                              // humanDuration's minutes branch
		"check_config failed",                 // a long error on its own row
	} {
		if !strings.Contains(body, want) {
			t.Errorf("preview does not render %q", want)
		}
	}

	card := doRequest(t, handler, http.MethodGet, "/?preview=history", nil).Body.String()
	// The page is run rows alone; the card's have to be counted inside
	// their own list, since the activity feed is made of <li> too.
	pageRows := strings.Count(body, "<li>")
	cardRows := strings.Count(runListOf(t, card), "<li>")
	if pageRows <= cardRows {
		t.Errorf("the page renders %d rows and the card %d - the preview cannot show what the page is for",
			pageRows, cardRows)
	}
	// The card's link promises a number; the page has to be holding it.
	if !strings.Contains(card, "all "+strconv.Itoa(pageRows)+" &rarr;") {
		t.Errorf("the card's link does not promise the %d rows the page renders", pageRows)
	}
}

// Without the name in both directions, following the link out of a
// preview - or back from it - lands on the live agent.
func TestDevPreviewHistoryLinksCarryThePreviewNameBothWays(t *testing.T) {
	devEnv(t)
	handler := New(newFakeAgent())

	body := doRequest(t, handler, http.MethodGet, "/?preview=history", nil).Body.String()

	if !strings.Contains(body, `href="history?preview=history"`) {
		t.Fatal("the card's link does not carry the preview name")
	}
	page := doRequest(t, handler, http.MethodGet, "/history?preview=history", nil).Body.String()
	if !strings.Contains(page, "badge-import") {
		t.Error("following the link does not render the canned history")
	}
	if !strings.Contains(page, `<a href="./?preview=history">&larr; Dashboard</a>`) {
		t.Error("the page's back link drops the preview name")
	}
}

// What the confirm quotes is composed at apply time, so the fixture has
// to carry one or the dialog only previews its fallback.
func TestDevPreviewDriftRollbackConfirmQuotesTheStash(t *testing.T) {
	devEnv(t)
	handler := New(newFakeAgent())

	body := doRequest(t, handler, http.MethodGet, "/?preview=drift", nil).Body.String()

	if !strings.Contains(body, "This puts 3 file(s) and registry objects back as they were before it") {
		t.Error("the drift preview's rollback confirm does not quote what the apply stashed")
	}
}

// The everyday previews carry a history too, so the card is not something
// only its own preview shows.
func TestDevPreviewDriftRendersRunHistory(t *testing.T) {
	devEnv(t)
	handler := New(newFakeAgent())

	body := doRequest(t, handler, http.MethodGet, "/?preview=drift", nil).Body.String()

	if !strings.Contains(body, `class="runs"`) {
		t.Error("the drift preview does not render the run history card")
	}
}

// No live-agent path in dev: an item only lands in the failure memory
// after a real flow failed. Both shapes - still declared, and orphaned.
func TestDevPreviewBlockedRendersBothShapesWithRetryButtons(t *testing.T) {
	devEnv(t)
	handler := New(newFakeAgent())

	rec := doRequest(t, handler, http.MethodGet, "/?preview=blocked", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`Recorded failures <span class="count">2</span>`,
		"workday_main",
		"invalid_auth",
		"widget_garage",
		`hx-vals="{&#34;key&#34;:&#34;integration:workday_main&#34;}"`,
		`hx-vals="{&#34;key&#34;:&#34;subentry:widget_garage&#34;}"`,
		// The still-declared half as the planner reports it: an error op
		// in the registry card, pointing at the same button.
		"previous attempt failed:",
		"press Retry on the dashboard",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("preview does not render %q", want)
		}
	}
	// A blocked item is never planned as a create; the refusal above
	// replaces it.
	if strings.Contains(body, "a new config-entry flow will be driven to create this integration") {
		t.Error("preview plans a create for an item its own failure memory blocks")
	}
}

// Every flag at once, the only way to see the chip row wrap: a real agent
// raises them one at a time and rarely.
func TestDevPreviewHealthRendersEveryChip(t *testing.T) {
	devEnv(t)
	handler := New(newFakeAgent())

	rec := doRequest(t, handler, http.MethodGet, "/?preview=health", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"history writes failing",
		"version record failing",
		"update check cannot resolve own slug",
		"update check failing: a0d7b954_esphome",
		"update check failing: core_samba",
		"hacs layer: HACS is not installed",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("preview does not render %q", want)
		}
	}
}

// The drift fixture carries the add-on op that makes the restart
// sentence appear.
func TestDevPreviewDriftConfirmNamesTheRestart(t *testing.T) {
	devEnv(t)
	handler := New(newFakeAgent())

	body := doRequest(t, handler, http.MethodGet, "/?preview=drift", nil).Body.String()

	if !strings.Contains(body, "This will restart add-on(s): core_configurator.") {
		t.Error("the drift preview's apply confirm does not name the add-on it restarts")
	}
}

// No live-agent path in dev either: a group only fills after a real apply
// took ownership. Every group at once, and one long enough to scroll.
func TestDevPreviewManagedRendersEveryGroup(t *testing.T) {
	devEnv(t)
	handler := New(newFakeAgent())

	rec := doRequest(t, handler, http.MethodGet, "/?preview=managed", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`Managed by this agent <span class="count">234</span>`,
		`<span class="path">files</span>`,
		`<span class="path">floors, areas, labels and helpers</span>`,
		`<span class="path">entities</span>`,
		`<span class="path">dashboards</span>`,
		`<span class="path">add-on options</span>`,
		`<span class="path">integrations</span>`,
		`<span class="path">subentries</span>`,
		`<span class="path">HACS integrations</span>`,
		"<code>automations.yaml</code>",
		"<code>input_boolean:guest_mode</code>",
		"<code>widget_kitchen</code>",
		"<code>anker_solix</code>",
		// One of the two HACS names above is still waiting for the
		// restart that loads it.
		`<span class="chip chip-neutral chip-restart">downloaded, not loaded yet: anker_solix &middot; restart Home Assistant, then set it up (or declare its entry in gitops/integrations.yaml)</span>`,
		// The file group runs past the render cap, the only way to see
		// the line that closes one.
		"... and 12 more, listed in full by status.json",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("preview does not render %q", want)
		}
	}
}

// The chip, banner and Resume button only exist once somebody has pressed
// Pause against a real agent.
func TestDevPreviewPausedRendersTheWholeControl(t *testing.T) {
	devEnv(t)
	handler := New(newFakeAgent())

	rec := doRequest(t, handler, http.MethodGet, "/?preview=paused", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`<span class="chip chip-neutral chip-paused">paused</span>`,
		`hx-post="resume"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("preview does not render %q", want)
		}
	}
	// Unwrapped, since these are sentences and the banner is hard-wrapped
	// across five source lines - see unwrapped in web_test.go.
	banner := unwrapped(body)
	for _, want := range []string{
		"Automatic checks are paused.",
		"Roll Back still work",
		"Check for updates on the add-on card",
		"the last check before the pause",
	} {
		if !strings.Contains(banner, want) {
			t.Errorf("preview does not render %q", want)
		}
	}
	// The fixture clears the countdown the way recon.SetPaused does.
	if strings.Contains(body, "next check by") {
		t.Error("the paused preview still counts down to a check that is not coming")
	}
}

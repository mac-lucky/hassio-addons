// Package web is the add-on's ingress web UI: status dashboard plus
// Check now / Apply / Rollback actions, served as htmx fragments. Ports
// app.web, gating access on Supervisor's fixed ingress proxy address.
//
// Every URL rendered here is RELATIVE (hx-post="apply", never a leading
// slash): the ingress proxy strips its own mount prefix, so a relative
// URL resolves against the mounted page with no server-side rewriting.
package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/applier"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/differ"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/execx"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/history"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/httpx"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/humanize"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/recon"
)

//go:embed templates/*.html
var templateFiles embed.FS

//go:embed static
var staticFiles embed.FS

// ingressProxyAddr is the address Supervisor's ingress proxy connects
// from; any other source is refused. Mirrors INGRESS_PROXY_ADDR.
const ingressProxyAddr = "172.30.32.2"

// DevEnvVar, set to "1", bypasses the ingress-address check in every
// build and arms dev.go's previews, which additionally need `-tags dev`
// to be compiled in at all (see dev_stub.go). Mirrors DEV_ENV_VAR.
const DevEnvVar = "GITOPS_DEV"

// Agent is what the running *recon.Reconciler must implement for the web
// UI to drive it. An interface, so this package depends on recon's
// behavior and not its concrete type; the assertion below turns signature
// drift into a build failure rather than a test that can rot.
type Agent interface {
	// Status returns the current status for display: sync state, last SHA,
	// pending count, last apply time, last error.
	Status() recon.Status
	// Busy reports whether an operation holds the agent's operation lock.
	// The routes' early-out probe: Status clones the full mirror set -
	// events, history, the managed inventory - under the same mutex the
	// running operation needs, and the start-wait polls it every 5ms.
	Busy() bool
	// ReconcileNow runs one fetch + diff cycle immediately.
	ReconcileNow(ctx context.Context) []differ.Change
	// ApplyNow applies the currently pending diff.
	ApplyNow(ctx context.Context, force bool) applier.Result
	// Rollback restores the last known-good state.
	Rollback(ctx context.Context) applier.Result
	// CommitDriftBack commits the pending file drift to a new throwaway
	// branch for review.
	CommitDriftBack(ctx context.Context) (string, error)
	// PreviewImport reports what an import would capture from the live
	// config tree, running no git command at all.
	PreviewImport(ctx context.Context) (recon.ImportPreview, error)
	// DismissImportPreview forgets the recorded preview, taking the card
	// off the page. Clears one field under a mutex: nothing to cancel and
	// nothing that can fail.
	DismissImportPreview()
	// ImportLive seeds the repository from the live config tree, pushing
	// one commit onto the tracked branch.
	ImportLive(ctx context.Context) (recon.ImportSummary, error)
	// RetryBlocked forgets one blocked item's recorded failure so the next
	// cycle plans it again. key is a recon.BlockedItem.Key.
	RetryBlocked(key string) error
	// HistoryAll is every run held, newest-first, for GET /history.
	// Status().History is the same list cut to what polling can afford.
	HistoryAll() []history.Record
	// SetPaused switches the unattended reconcile loop off or back on. The
	// error means only that the flag was not recorded for the next
	// restart; the agent is paused either way.
	SetPaused(paused bool) error
	// CheckAddonUpdates runs one check over every slug in
	// auto_update_addons, installing what it finds. Its results land in
	// Status().AddonUpdates and the activity feed, so it returns nothing.
	CheckAddonUpdates(ctx context.Context)
}

var _ Agent = (*recon.Reconciler)(nil)

// funcMap is the template.FuncMap every parsed template shares.
var funcMap = template.FuncMap{
	"addonUpdates":   addonUpdatesFunc,
	"callout":        calloutFunc,
	"diffLines":      diffLinesFunc,
	"humanBytes":     humanize.Bytes,
	"humanDuration":  humanize.Duration,
	"humanState":     humanState,
	"humanTime":      humanTime,
	"inventoryGroup": inventoryGroupFunc,
	"join":           strings.Join,
	"retryVals":      retryVals,
	"reverseEvents":  reverseEvents,
}

// retryVals is one Retry button's hx-vals payload, naming which recorded
// failure the pressed row belongs to. Marshalled rather than written in
// the template: htmx parses malformed JSON as null, which posts no key at
// all, and a key is a user-supplied string.
func retryVals(key string) string {
	encoded, err := json.Marshal(map[string]string{"key": key})
	if err != nil {
		// Unreachable: a map[string]string always marshals.
		slog.Warn("web: could not encode a retry key", "key", key, "error", err)
		return "{}"
	}
	return string(encoded)
}

// templates holds every *.html under templates/, parsed once and
// associated so each is addressable by base filename. _partials.html is
// the exception: it only contributes {{define}}d fragments.
var templates = template.Must(template.New("web").Funcs(funcMap).ParseFS(templateFiles, "templates/*.html"))

// humanState renders "drift_pending" as "drift pending".
func humanState(s string) string {
	return strings.ReplaceAll(s, "_", " ")
}

// humanTime renders an RFC3339 timestamp (always UTC here) as "Jan 2,
// 15:04"; templates keep the raw value in a title. Bad input passes back.
func humanTime(ts string) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ts
	}
	return t.Format("Jan 2, 15:04")
}

// reverseEvents returns events newest-first, without mutating the input.
func reverseEvents(events []recon.Event) []recon.Event {
	out := make([]recon.Event, len(events))
	for i, e := range events {
		out[len(events)-1-i] = e
	}
	return out
}

// diffLineView is one rendered line of a diff: Class is "diff-add",
// "diff-del", or "" (a context/header line).
type diffLineView struct {
	Class string
	Text  string
}

// diffLinesFunc splits a unified diff into (class, line) pairs so the
// template can colour +/- lines. Headers (+++/---) stay uncoloured.
func diffLinesFunc(diffText string) []diffLineView {
	if diffText == "" {
		return nil
	}
	lines := strings.Split(diffText, "\n")
	// strings.Split leaves a trailing "" for a trailing newline.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	out := make([]diffLineView, 0, len(lines))
	for _, line := range lines {
		class := ""
		switch {
		case strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++"):
			class = "diff-add"
		case strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---"):
			class = "diff-del"
		}
		out = append(out, diffLineView{Class: class, Text: line})
	}
	return out
}

// calloutView is one error/warning card, as {{define "callout"}} renders
// it. Variant ("error"/"warning") names the card and message classes;
// Hint is an optional paragraph only the failed-backup card uses.
type calloutView struct {
	Variant string
	Heading string
	Body    string
	Hint    string
}

// calloutFunc is the template's constructor for a calloutView, since a
// template cannot build a struct on its own.
func calloutFunc(variant, heading, body, hint string) calloutView {
	return calloutView{Variant: variant, Heading: heading, Body: body, Hint: hint}
}

// inventoryMax is how many names of one group the page renders; the rest
// are counted. Without a cap an idle dashboard re-renders and re-hashes
// every file each poll (536 KB of fragment for a 7423-file manifest).
// Capped in the view, not recon, so /status.json stays complete.
const inventoryMax = 200

// inventoryView is one group of the managed-inventory card. Names, never
// values (recon.ManagedInventory is its only source). Total is the real
// size, Items is capped at inventoryMax, More is what the cap left out.
type inventoryView struct {
	Label string
	Total int
	Items []string
	More  int
}

// inventoryGroupFunc is the template's constructor for an inventoryView,
// with the cap applied here so no call site can forget it.
func inventoryGroupFunc(label string, items []string) inventoryView {
	view := inventoryView{Label: label, Total: len(items), Items: items}
	if len(items) > inventoryMax {
		view.Items = items[:inventoryMax]
		view.More = len(items) - inventoryMax
	}
	return view
}

// addonUpdatesView splits the add-on update card in two: Rows can still
// move on their own, Folded cannot until the configuration changes.
// recon.AddonUpdateStatus.Actionable is the only thing deciding which.
// Split in the view, so /status.json keeps one flat list; both are
// rendered and counted.
//
// Both slices come from one walk, so both keep auto_update_addons' order.
// Load bearing: renderFragment hashes the bytes to answer a poll with
// 204, so an order that could differ between renders would re-swap the
// dashboard every five seconds. Never build either from a map.
type addonUpdatesView struct {
	Rows   []recon.AddonUpdateStatus
	Folded []recon.AddonUpdateStatus
}

// addonUpdatesFunc is the template's constructor for an addonUpdatesView,
// since a template cannot split a slice in two on its own.
func addonUpdatesFunc(updates []recon.AddonUpdateStatus) addonUpdatesView {
	var view addonUpdatesView
	for _, update := range updates {
		if update.Actionable() {
			view.Rows = append(view.Rows, update)
			continue
		}
		view.Folded = append(view.Folded, update)
	}
	return view
}

// safeStatus is agent.Status() with any credential stripped from RepoURL
// (execx.RedactURL, which fails closed) before it reaches the dashboard
// or /status.json.
func safeStatus(agent Agent) recon.Status {
	status := agent.Status()
	status.RepoURL = execx.RedactURL(status.RepoURL)
	return status
}

// pageData is what every template in templates/ renders against.
type pageData struct {
	Status recon.Status
	// AssetVer is assetVersion, appended to every static URL as ?v=. It is
	// what makes the immutable caching below safe.
	AssetVer string
	// FragmentHash identifies the fragment, and goes into _main.html's own
	// polling URL so an unchanged next poll can answer 204. See
	// renderFragment for how it avoids chasing its own tail.
	FragmentHash string
	// Preview is the dev.go preview name, or "" for a real render. The
	// polling URL carries it so the live status does not replace it.
	Preview string
}

// historyPageData is what history.html renders against. Its own type
// rather than pageData: this page polls nothing and renders no status, so
// it needs neither a fragment hash nor a recon.Status.
type historyPageData struct {
	// AssetVer is assetVersion, for the same ?v= cache-busting query.
	AssetVer string
	// Records is every run, newest-first, from Agent.HistoryAll.
	Records []history.Record
	// Preview is the dev.go preview name, or "" - carried by the link back
	// so following it does not land on the live agent.
	Preview string
}

// fragmentHashPlaceholder stands in for the fragment's own hash while it
// is hashed. Not hex, so it cannot be mistaken for a real hash.
const fragmentHashPlaceholder = "xxxxxxxxxxxxxxxx"

// fragmentHashLen is how many hex digits the polling URL carries. A
// collision costs one skipped refresh, which the next change corrects.
const fragmentHashLen = 16

// renderFragment renders _main.html for status, with the hash the poller
// identifies it by.
//
// The hash cannot cover the slot it lands in or it would never converge,
// so the fragment is rendered once with a placeholder, hashed as
// rendered, and only the FIRST placeholder is replaced - structurally the
// polling attribute on the outermost element, never status text.
//
// GET / comes through for the hash only; index.html then renders
// _main.html again to the same bytes, so the first poll answers 204.
func renderFragment(status recon.Status, preview string) (body []byte, hash string) {
	data := pageData{
		Status:       status,
		AssetVer:     assetVersion,
		FragmentHash: fragmentHashPlaceholder,
		Preview:      preview,
	}
	var buf bytes.Buffer
	if err := templates.ExecuteTemplate(&buf, "_main.html", data); err != nil {
		// Unreachable short of a template bug, and partial output is the
		// best there is to send.
		slog.Warn("web: template render failed", "template", "_main.html", "error", err)
	}
	sum := sha256.Sum256(buf.Bytes())
	hash = hex.EncodeToString(sum[:])[:fragmentHashLen]
	return bytes.Replace(buf.Bytes(), []byte(fragmentHashPlaceholder), []byte(hash), 1), hash
}

// requestStatus resolves what a read request renders against: the dev
// preview it named, or the agent's redacted status. The second return is
// the preview name, "" for a real render (see pageData.Preview).
func requestStatus(agent Agent, r *http.Request) (recon.Status, string) {
	if status, preview, ok := devPreviewStatus(r); ok {
		return status, preview
	}
	return safeStatus(agent), ""
}

// renderPage writes the whole dashboard document for status.
func renderPage(w http.ResponseWriter, status recon.Status, preview string) {
	_, hash := renderFragment(status, preview)
	writeHTMLHeaders(w)
	data := pageData{Status: status, AssetVer: assetVersion, FragmentHash: hash, Preview: preview}
	if err := templates.ExecuteTemplate(w, "index.html", data); err != nil {
		slog.Warn("web: template render failed", "template", "index.html", "error", err)
	}
}

// writeFragment answers with the fragment unconditionally - what the
// action routes send back, since a button press always wants the result.
func writeFragment(w http.ResponseWriter, status recon.Status, preview string) {
	body, _ := renderFragment(status, preview)
	writeHTMLHeaders(w)
	if _, err := w.Write(body); err != nil {
		slog.Warn("web: writing the fragment failed", "error", err)
	}
}

// writeHTMLHeaders sets the headers every rendered HTML response carries.
// This HTML names which ?v= to fetch, so it must revalidate every time or
// a cached copy would pin the previous release's stylesheet.
func writeHTMLHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
}

// assetVersion is a content hash over everything under static/, computed
// once at init. Appended to each static URL as ?v=, and served as ETag.
var assetVersion = computeAssetVersion()

// computeAssetVersion hashes every embedded static file, name as well as
// bytes so a rename moves the hash, in fs.WalkDir's lexical order. The
// NUL between name and contents keeps the concatenation unambiguous:
// without it ("a.css", "X") and ("a.cssX", "") would collide.
func computeAssetVersion() string {
	var blob []byte
	err := fs.WalkDir(staticFiles, "static", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		body, err := staticFiles.ReadFile(name)
		if err != nil {
			return err
		}
		blob = append(blob, name...)
		blob = append(blob, 0)
		blob = append(blob, body...)
		return nil
	})
	if err != nil {
		// An embed.FS cannot fail to walk on a build that compiled.
		panic(err)
	}
	sum := sha256.Sum256(blob)
	return hex.EncodeToString(sum[:])[:8]
}

// staticCacheControl keeps a static asset for a year without
// revalidating. Safe only because every URL carries ?v=assetVersion:
// editing a file changes the hash, the URL, and so misses the cache.
const staticCacheControl = "public, max-age=31536000, immutable"

// cacheStatic adds caching headers for requests naming a file that
// exists in fsys - a 404 or directory listing must not be cached a year.
//
// The ETag is set BEFORE next runs: net/http's file server reads it off
// the ResponseWriter to answer If-None-Match with a 304 itself. Nothing
// else can supply one, since embed.FS entries have a zero ModTime.
func cacheStatic(fsys fs.FS, next http.Handler) http.Handler {
	etag := `"` + assetVersion + `"`
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isRegularFile(fsys, r.URL.Path) {
			w.Header().Set("ETag", etag)
			w.Header().Set("Cache-Control", staticCacheControl)
		}
		next.ServeHTTP(w, r)
	})
}

// isRegularFile reports whether urlPath (already stripped of its /static/
// prefix) names a file in fsys.
func isRegularFile(fsys fs.FS, urlPath string) bool {
	name := strings.TrimPrefix(path.Clean(urlPath), "/")
	if !fs.ValidPath(name) {
		return false
	}
	info, err := fs.Stat(fsys, name)
	return err == nil && info.Mode().IsRegular()
}

// New builds the ingress web UI's http.Handler, bound to agent (the
// running *recon.Reconciler). Routes call back into it; this package
// holds no reconciliation state. Ready to serve on config.yaml's
// ingress_port.
func New(agent Agent) http.Handler {
	mux := http.NewServeMux()
	// One per handler, so two servers in a test never share a slot.
	tracker := &opTracker{}

	staticSub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		// The "static" directory is compile-time verified by the embed.
		panic(err)
	}
	mux.Handle("GET /static/", http.StripPrefix("/static/", cacheStatic(staticSub, http.FileServerFS(staticSub))))

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		status, preview := requestStatus(agent, r)
		renderPage(w, status, preview)
	})

	// GET /fragment is what the dashboard polls. It answers 204 when the
	// fragment is byte-for-byte the caller's, and htmx does not swap on a
	// 204 - so an idle dashboard never tears out an open diff.
	mux.HandleFunc("GET /fragment", func(w http.ResponseWriter, r *http.Request) {
		body, hash := renderFragment(requestStatus(agent, r))
		if hash == r.URL.Query().Get("h") {
			// No Content-Type: there is no content. Cache-Control still
			// applies - this is as short lived as the fragment.
			w.Header().Set("Cache-Control", "no-cache")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeHTMLHeaders(w)
		if _, err := w.Write(body); err != nil {
			slog.Warn("web: writing the fragment failed", "error", err)
		}
	})

	// GET /history is the whole run history on its own page: no htmx, no
	// polling, no hash - a snapshot a reload refreshes. The card stays
	// capped because it re-renders several times a minute; these render
	// once, from memory (Agent.HistoryAll), so 200 rows cost no disk read.
	mux.HandleFunc("GET /history", func(w http.ResponseWriter, r *http.Request) {
		// A seam of its own rather than the fixture's Status.History,
		// which is the CARD's cut - see devPreviewHistory.
		records, preview, ok := devPreviewHistory(r)
		if !ok {
			records = agent.HistoryAll()
		}
		writeHTMLHeaders(w)
		data := historyPageData{AssetVer: assetVersion, Records: records, Preview: preview}
		if err := templates.ExecuteTemplate(w, "history.html", data); err != nil {
			slog.Warn("web: template render failed", "template", "history.html", "error", err)
		}
	})

	mux.HandleFunc("POST /reconcile", opRoute(agent, tracker, "reconcile", func(ctx context.Context) error {
		agent.ReconcileNow(ctx)
		return nil
	}))

	mux.HandleFunc("POST /apply", opRoute(agent, tracker, "apply", func(ctx context.Context) error {
		agent.ApplyNow(ctx, true)
		return nil
	}))

	mux.HandleFunc("POST /rollback", opRoute(agent, tracker, "rollback", func(ctx context.Context) error {
		agent.Rollback(ctx)
		return nil
	}))

	mux.HandleFunc("POST /commitback", opRoute(agent, tracker, "commit drift back", func(ctx context.Context) error {
		_, err := agent.CommitDriftBack(ctx)
		return err
	}))

	mux.HandleFunc("POST /import/preview", opRoute(agent, tracker, "import preview", func(ctx context.Context) error {
		_, err := agent.PreviewImport(ctx)
		return err
	}))

	// Not an opRoute, for the pause pair's three reasons: nothing blocks,
	// nothing on the box changes, and it takes no opLock.
	//
	// A dismiss arriving mid-preview clears nothing, so the finishing
	// preview's card returns on the next poll. Dismissing again is free.
	mux.HandleFunc("POST /import/preview/dismiss", func(w http.ResponseWriter, r *http.Request) {
		agent.DismissImportPreview()
		writeFragment(w, safeStatus(agent), "")
	})

	mux.HandleFunc("POST /import", opRoute(agent, tracker, "import", func(ctx context.Context) error {
		_, err := agent.ImportLive(ctx)
		return err
	}))

	// The one action route carrying a parameter. Read here, not inside the
	// operation, because the body is the request's and the operation
	// outlives it (see opContext). The chained reconcile is what makes one
	// press useful: clearing the memory only decides what the NEXT cycle
	// plans, so the row would otherwise vanish for a whole interval.
	mux.HandleFunc("POST /retry", func(w http.ResponseWriter, r *http.Request) {
		// Bounded before anything reads it: an unrecognized key is echoed
		// into the 200-entry activity ring and the log, so an unbounded one
		// could evict the whole feed (and net/http alone allows 10MB of
		// form body). Real keys are "<layer>:<manifest id>", well under the
		// cap.
		r.Body = http.MaxBytesReader(w, r.Body, maxRetryBodyBytes)
		key := r.FormValue("key")
		if len(key) > maxRetryKeyLen {
			http.Error(w, "retry key too long", http.StatusBadRequest)
			return
		}
		opRoute(agent, tracker, "retry blocked item", func(ctx context.Context) error {
			if err := agent.RetryBlocked(key); err != nil {
				return err
			}
			agent.ReconcileNow(ctx)
			return nil
		})(w, r)
	})

	mux.HandleFunc("POST /addons/check", checkRoute(agent))

	// Not opRoute, and each of its three jobs is one this pair must not
	// have: no busy check (being usable mid-apply is the point), no
	// goroutine (SetPaused writes a 40-byte file), and no opLock - it is
	// not an operation on the config.
	mux.HandleFunc("POST /pause", pauseRoute(agent, true))
	mux.HandleFunc("POST /resume", pauseRoute(agent, false))

	mux.HandleFunc("GET /status.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(statusJSON{Status: safeStatus(agent), Operation: tracker.view()}); err != nil {
			slog.Warn("web: failed to encode status.json", "error", err)
		}
	})

	return requireIngress(requireSameOrigin(mux))
}

// opRoute builds one action route: refuse while an operation is running,
// otherwise start op in the background and answer at once with the
// fragment.
//
// Answering at once is the point - these routinely outrun the ingress
// server's 30-second write timeout (an apply waits up to 15 minutes on
// the pre-apply backup), so a handler waiting for its result would have
// the response cut off. Polling brings the result back instead.
//
// The Busy check is an early out, not the lock: recon takes
// opLock.TryLock and refuses on its own. op's error goes to the container
// log; the user reads refusals in the polled fragment's event log.
func opRoute(agent Agent, tracker *opTracker, name string, op func(context.Context) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !agent.Busy() {
			done := make(chan struct{})
			// opContext, not r.Context(): the operation outlives the
			// response, whose end cancels the request context.
			ctx := opContext(r)
			id := tracker.start(name)
			w.Header().Set(opIDHeader, strconv.FormatUint(id, 10))
			go func() {
				defer close(done)
				defer recoverOp(name)
				err := op(ctx)
				tracker.finish(id, err)
				if err != nil {
					slog.Warn("web: operation did not run", "op", name, "error", err)
				}
			}()
			awaitBusy(agent, done)
		} else {
			w.Header().Set(opRefusedHeader, "busy")
		}
		writeFragment(w, safeStatus(agent), "")
	}
}

// Headers an API caller reads off an action POST. The body is always the
// htmx fragment, so the correlation id rides here rather than in it.
const (
	opIDHeader      = "X-GitOps-Op-Id"
	opRefusedHeader = "X-GitOps-Op-Refused"
)

// Bounds on POST /retry's one parameter - see the route.
const (
	maxRetryBodyBytes = 4096
	maxRetryKeyLen    = 256
)

// opTracker is the most recent background operation a route started:
// which one, whether it is still running, and what it returned. One slot,
// not a queue - opRoute refuses a second while one runs, so there is never
// a second to hold. Built per New, never package state.
//
// It exists for the non-browser caller. A POST answers before its
// operation has necessarily started (see opRoute), so a script reading
// /status.json straight afterwards cannot tell "not started yet" from
// "finished": Busy is false in both, and true for an unrelated tick. The
// id handed back is what closes that gap.
type opTracker struct {
	mu          sync.Mutex
	id          uint64
	name        string
	running     bool
	err         string
	finishedUTC string
}

// opView is opTracker's snapshot, embedded in /status.json.
type opView struct {
	// ID is 0 until this process starts its first operation.
	ID      uint64 `json:"id"`
	Name    string `json:"name"`
	Running bool   `json:"running"`
	// Error is only meaningful once Running is false.
	Error       string `json:"error"`
	FinishedUTC string `json:"finished_utc"`
}

func (t *opTracker) start(name string) uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.id++
	t.name = name
	t.running = true
	t.err = ""
	t.finishedUTC = ""
	return t.id
}

// finish ignores a stale id, so a late panic recovery from a superseded
// operation cannot overwrite the current one's result.
func (t *opTracker) finish(id uint64, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.id != id {
		return
	}
	t.running = false
	t.finishedUTC = time.Now().UTC().Format(time.RFC3339)
	if err != nil {
		t.err = err.Error()
	}
}

func (t *opTracker) view() opView {
	t.mu.Lock()
	defer t.mu.Unlock()
	return opView{ID: t.id, Name: t.name, Running: t.running, Error: t.err, FinishedUTC: t.finishedUTC}
}

// statusJSON is recon.Status plus the web layer's own view of the
// operation a POST started - the one fact recon.Status cannot carry,
// because it describes the config and not this process's routes. Embedded,
// so every existing top-level key stays where it was.
type statusJSON struct {
	recon.Status
	Operation opView `json:"operation"`
}

// pauseRoute builds POST /pause or /resume: set the flag, answer with the
// fragment. The error is logged and dropped on purpose - SetPaused only
// fails at RECORDING the flag, never at applying it, so the page is
// accurate and answering with an error would claim the loop still runs.
func pauseRoute(agent Agent, paused bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := agent.SetPaused(paused); err != nil {
			slog.Warn("web: the pause flag was not recorded", "paused", paused, "error", err)
		}
		writeFragment(w, safeStatus(agent), "")
	}
}

// checkRoute builds POST /addons/check: start one check in the
// background, answer with the fragment. Read against opRoute's three jobs
// - it keeps one, drops one, swaps one:
//
//   - KEEPS the goroutine and recoverOp: a check spends up to
//     regapply.UpdateAddon's 30-minute budget per stale add-on.
//   - DROPS the Status().Busy early-out. Busy answers for opLock, but
//     this operation gates on checkLock, so it would refuse a press
//     during an unrelated apply. CheckAddonUpdates single-flights itself.
//   - SWAPS awaitBusy for the same wait on AddonCheckRunning; waiting on
//     Busy would burn the budget on a flag the check never sets.
//
// The swap gives up awaitBusy's non-cosmetic job, so recon.WaitIdle can
// report idle during the check's FETCH phase. Safe only here: that phase
// only reads from Supervisor, and the mutating phase takes opLock per
// add-on. Worst case is an update never started, which the next interval
// starts.
//
// What replaces the dropped Busy check is the SAME early-out against the
// right lock, and it is opRoute's own rationale rather than a new idea:
// the lock is not here, it is in the reconciler, and what this saves is
// the pointless goroutine and the activity-log line for a refusal the
// user can already see. Both refusals matter more here than they do for
// an operation, because CheckAddonUpdates logs them UNCONDITIONALLY and
// the empty-list one before it takes any lock at all. The activity feed
// is a 200-entry ring (recon.eventLogMaxLen), so 200 presses of a button
// that cannot do anything would evict every applied change, rollback and
// add-on update from the only place they are visible. The durable record
// in /data/history.jsonl is untouched either way.
//
// Racy by construction, exactly as opRoute's is: AddonCheckRunning is a
// TryLock on the lock the check is about to take, so a press that slips
// through is still refused correctly one layer down. This is an early
// out, never the lock.
func checkRoute(agent Agent) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if status := agent.Status(); !status.AutoUpdateEnabled || status.AddonCheckRunning {
			writeFragment(w, safeStatus(agent), "")
			return
		}
		done := make(chan struct{})
		// opContext for opRoute's reason: a cancelled context here would
		// abandon an update Supervisor is already executing.
		ctx := opContext(r)
		go func() {
			defer close(done)
			defer recoverOp("add-on update check")
			agent.CheckAddonUpdates(ctx)
		}()
		awaitStart(done, func() bool { return agent.Status().AddonCheckRunning })
		writeFragment(w, safeStatus(agent), "")
	}
}

// recoverOp turns a panic inside a background operation into a logged
// error instead of a dead add-on. net/http's own recover does not reach a
// bare goroutine, and nothing under gitsync, applier, differ or
// importLive recovers, so one bad click would take the process down.
//
// The agent stays usable: every operation defers its opLock Unlock, so
// the lock unwinds through the panic. internal/hook duplicates this
// deliberately - httpx is the only package both import.
func recoverOp(name string) {
	if v := recover(); v != nil {
		slog.Error("web: operation panicked", "op", name, "panic", v, "stack", string(debug.Stack()))
	}
}

// opStartBudget and opStartStep bound how long a route waits for the
// operation it just started to show up in the status - opRoute for Busy,
// checkRoute for AddonCheckRunning.
const (
	opStartBudget = 50 * time.Millisecond
	opStartStep   = 5 * time.Millisecond
)

// awaitBusy waits for a just-started operation to mark the agent busy, or
// to finish, whichever comes first. Mostly cosmetic - it keeps the next
// fragment from rendering the pre-click state - and nothing is lost when
// the budget expires, since the fragment polls while busy.
//
// One part is NOT cosmetic, so do not delete this as decoration: holding
// the handler open until the operation takes opLock puts it inside
// srv.Shutdown's in-flight drain, and so makes it visible to
// recon.WaitIdle. Without it both servers can be down, WaitIdle reports
// idle, and an apply is about to write (see waitIdleGrace in cmd/).
func awaitBusy(agent Agent, done <-chan struct{}) {
	awaitStart(done, agent.Busy)
}

// awaitStart waits for started to report true or done to close, giving up
// after opStartBudget. The shared half of awaitBusy and checkRoute's wait
// on AddonCheckRunning.
//
// It must never become a spin loop, for any predicate passed here: both
// are a TryLock on the very lock the operation is racing to take, so a
// tight poll measurably delays it. The step is not politeness.
func awaitStart(done <-chan struct{}, started func() bool) {
	for waited := time.Duration(0); waited < opStartBudget; waited += opStartStep {
		if started() {
			return
		}
		select {
		case <-done:
			return
		case <-time.After(opStartStep):
		}
	}
}

// opContext carries r's values but NOT its cancellation. Since opRoute
// answers immediately, net/http cancels the request context every single
// time the handler returns, so this is the only thing keeping these
// operations alive at all.
//
// The failure modes are real: applier.Apply reads a cancelled context as
// a failed check_config and rolls back files it already wrote, and
// regapply's inverse-replay cannot redial to undo itself, so it stops
// early and drops one stash entry unreverted. Nothing runs unbounded -
// every leaf operation carries its own timeout.
func opContext(r *http.Request) context.Context {
	return context.WithoutCancel(r.Context())
}

// requireIngress refuses any request not from Supervisor's ingress proxy
// address, unless DevEnvVar is set.
func requireIngress(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if os.Getenv(DevEnvVar) != "1" && httpx.RemoteHost(r) != ingressProxyAddr {
			http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireSameOrigin refuses a state-changing request that the browser
// itself marks as initiated by another site. requireIngress only checks
// the network hop, and a cross-site form POST from any page the logged-in
// user visits arrives through that same proxy riding their ingress
// session - the unguessable ingress path is secrecy, not a control.
//
// The dashboard's htmx calls and its own forms send
// "Sec-Fetch-Site: same-origin"; a user-typed URL sends "none"; a
// non-browser API caller sends no header at all. Only an explicit
// cross-site marking is refused, so nothing scriptable breaks.
func requireSameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			switch r.Header.Get("Sec-Fetch-Site") {
			case "", "none", "same-origin":
			default:
				http.Error(w, "cross-site request refused", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

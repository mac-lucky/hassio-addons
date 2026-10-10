// Package web is the add-on's ingress panel: the state of every secret, the
// problems to fix, and the two actions (Sync now, Restart Home Assistant).
// Served as one page whose #app region htmx re-polls; the server answers
// 204 when the region is unchanged, so an idle panel never re-renders.
//
// Every URL rendered here is relative (hx-post="sync"): Supervisor's
// ingress proxy strips its own prefix, so relative URLs resolve with no
// server-side rewriting. No route ever renders a secret value; the
// engine's Status holds none.
package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/mac-lucky/hassio-addons/onepassword_secrets/internal/engine"
	"github.com/mac-lucky/hassio-addons/onepassword_secrets/internal/httpx"
	"github.com/mac-lucky/hassio-addons/onepassword_secrets/internal/state"
)

//go:embed templates/*.html
var templateFiles embed.FS

//go:embed static
var staticFiles embed.FS

// ingressProxyAddr is the only address Supervisor's ingress proxy connects
// from; anything else is refused.
const ingressProxyAddr = "172.30.32.2"

// DevEnvVar, set to "1", lifts the ingress-address check and arms the
// dev previews (which also need `-tags dev`).
const DevEnvVar = "OPS_DEV"

// Agent is what the engine must offer the panel.
type Agent interface {
	Status() engine.Status
	SyncNow()
	RestartCoreNow(ctx context.Context) error
	History(n int) []state.Entry
	KeyHistory(key string, n int) []state.Entry
}

var _ Agent = (*engine.Engine)(nil)

var funcMap = template.FuncMap{
	"iso":        isoTime,
	"date":       dateOnly,
	"printable":  escapeFormatChars,
	"join":       strings.Join,
	"stateLabel": stateLabel,
	"rowLabel":   rowLabel,
	"kindLabel":  kindLabel,
	"plural":     plural,
	"useText":    useText,
	"tickClass":  tickClass,
	"attention":  rowNeedsAttention,
	"setupDone":  setupDone,
	"dueText":    dueText,
	"add":        func(a, b int) int { return a + b },
	"lower":      strings.ToLower,
	"qesc":       url.QueryEscape,
	"keyID":      keyID,
}

// keyID is an opaque, attribute- and URL-safe element id for a key: keys
// are arbitrary strings (spaces, symbols, anything a YAML file names), and
// the keyring's links must find their row.
func keyID(key string) string { return "k-" + hex.EncodeToString([]byte(key)) }

var templates = template.Must(template.New("").Funcs(funcMap).ParseFS(templateFiles, "templates/*.html"))

// assetVersion is a content hash of everything under static/, appended as
// ?v= so a release busts the year-long cache.
var assetVersion = computeAssetVersion()

func computeAssetVersion() string {
	h := sha256.New()
	err := fs.WalkDir(staticFiles, "static", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, err := staticFiles.ReadFile(name)
		if err != nil {
			return err
		}
		h.Write([]byte(name))
		h.Write([]byte{0})
		h.Write(body)
		return nil
	})
	if err != nil {
		panic(err)
	}
	return hex.EncodeToString(h.Sum(nil))[:8]
}

// page is the template data.
type page struct {
	S        engine.Status
	AssetVer string
	Hash     string
	Preview  string
	Ticks    []engine.SecretRow
	Now      time.Time
}

func newPage(s engine.Status, preview string) page {
	ticks := append([]engine.SecretRow(nil), s.Secrets...)
	sort.SliceStable(ticks, func(i, j int) bool { return tickOrder(ticks[i]) < tickOrder(ticks[j]) })
	return page{S: s, AssetVer: assetVersion, Preview: preview, Ticks: ticks}
}

// New returns the panel's handler.
func New(agent Agent) http.Handler {
	mux := http.NewServeMux()
	staticSub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		panic(err)
	}
	mux.Handle("GET /static/", http.StripPrefix("/static/", cacheStatic(staticSub, http.FileServerFS(staticSub))))

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		s, preview := currentStatus(agent, r)
		p := newPage(s, preview)
		body, hash := renderFragment(p)
		p.Hash = hash
		var buf bytes.Buffer
		if err := templates.ExecuteTemplate(&buf, "index.html", struct {
			page
			Fragment template.HTML
		}{p, template.HTML(body)}); err != nil { // #nosec G203 -- body is our own rendered template
			slog.Error("web: rendering the page failed", "error", err)
			http.Error(w, "render error", http.StatusInternalServerError)
			return
		}
		writeHTMLHeaders(w)
		_, _ = w.Write(scrubFormatChars(buf.Bytes()))
	})

	mux.HandleFunc("GET /fragment", func(w http.ResponseWriter, r *http.Request) {
		s, preview := currentStatus(agent, r)
		body, hash := renderFragment(newPage(s, preview))
		if hash == r.URL.Query().Get("h") {
			// Nothing but the check time moved: hand that to the page's
			// script instead of swapping #app, which would drop focus,
			// the search box's caret and open sections every poll.
			if !s.LastSync.IsZero() {
				if trig, err := json.Marshal(map[string]string{"lastSync": isoTime(s.LastSync)}); err == nil {
					w.Header().Set("HX-Trigger", string(trig))
				}
			}
			w.Header().Set("Cache-Control", "no-cache")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeHTMLHeaders(w)
		_, _ = w.Write(scrubFormatChars(body)) // #nosec G705 -- html/template output
	})

	mux.HandleFunc("GET /key", func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Query().Get("k")
		entries := agent.KeyHistory(key, 8)
		if _, ok := devPreview(r); ok {
			entries = devKeyHistory(key)
		}
		var buf bytes.Buffer
		if err := templates.ExecuteTemplate(&buf, "keyhistory", entries); err != nil {
			http.Error(w, "render error", http.StatusInternalServerError)
			return
		}
		writeHTMLHeaders(w)
		_, _ = w.Write(scrubFormatChars(buf.Bytes()))
	})

	mux.HandleFunc("GET /history", func(w http.ResponseWriter, r *http.Request) {
		entries := agent.History(300)
		if _, ok := devPreview(r); ok {
			entries = devStatus("healthy").Activity
		}
		var buf bytes.Buffer
		if err := templates.ExecuteTemplate(&buf, "history.html", struct {
			AssetVer string
			Entries  []state.Entry
		}{assetVersion, entries}); err != nil {
			http.Error(w, "render error", http.StatusInternalServerError)
			return
		}
		writeHTMLHeaders(w)
		_, _ = w.Write(scrubFormatChars(buf.Bytes()))
	})

	mux.HandleFunc("GET /status.json", func(w http.ResponseWriter, r *http.Request) {
		s, _ := currentStatus(agent, r)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-cache")
		_ = json.NewEncoder(w).Encode(s)
	})

	mux.HandleFunc("POST /sync", func(w http.ResponseWriter, _ *http.Request) {
		agent.SyncNow()
		w.Header().Set("HX-Trigger", "synced")
		w.WriteHeader(http.StatusAccepted)
	})

	mux.HandleFunc("POST /restart-core", func(w http.ResponseWriter, r *http.Request) {
		if agent.Status().Syncing {
			http.Error(w, "A sync is running; try again when it finishes.", http.StatusConflict)
			return
		}
		go func() {
			if err := agent.RestartCoreNow(context.WithoutCancel(r.Context())); err != nil {
				slog.Warn("web: restart requested from the panel failed", "error", err)
			}
		}()
		w.WriteHeader(http.StatusAccepted)
	})

	return requireIngress(requireSameOrigin(securityHeaders(mux)))
}

func currentStatus(agent Agent, r *http.Request) (engine.Status, string) {
	if name, ok := devPreview(r); ok {
		return devStatus(name), name
	}
	return agent.Status(), ""
}

// renderFragment renders #app and hashes it for the 204 short-circuit.
// The hash leaves out the last check time, which moves every poll
// interval: an idle panel answers 204 and the script updates that one
// time from the HX-Trigger header instead.
func renderFragment(p page) ([]byte, string) {
	var buf bytes.Buffer
	hashed := p
	hashed.S.LastSync = time.Time{}
	if err := templates.ExecuteTemplate(&buf, "main", hashed); err != nil {
		slog.Error("web: rendering the fragment failed", "error", err)
		return renderFailed(p.Preview), ""
	}
	sum := sha256.Sum256(buf.Bytes())
	p.Hash = hex.EncodeToString(sum[:])[:16]
	buf.Reset()
	if err := templates.ExecuteTemplate(&buf, "main", p); err != nil {
		return renderFailed(p.Preview), ""
	}
	return buf.Bytes(), p.Hash
}

// renderFailed keeps polling, so one bad render does not freeze the panel.
func renderFailed(preview string) []byte {
	target := "fragment"
	if preview != "" {
		target += "?preview=" + url.QueryEscape(preview)
	}
	return []byte(`<div id="app" hx-get="` + template.HTMLEscapeString(target) + `" hx-trigger="every 5s" hx-swap="outerHTML">The panel could not be rendered; the add-on log has the reason. Retrying.</div>`)
}

func writeHTMLHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

const staticCacheControl = "public, max-age=31536000, immutable"

func cacheStatic(fsys fs.FS, next http.Handler) http.Handler {
	etag := `"` + assetVersion + `"`
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if fs.ValidPath(name) {
			if info, err := fs.Stat(fsys, name); err == nil && info.Mode().IsRegular() {
				w.Header().Set("ETag", etag)
				w.Header().Set("Cache-Control", staticCacheControl)
			}
		}
		next.ServeHTTP(w, r)
	})
}

// requireIngress refuses anything not from Supervisor's ingress proxy.
func requireIngress(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if os.Getenv(DevEnvVar) != "1" && httpx.RemoteHost(r) != ingressProxyAddr {
			http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireSameOrigin refuses a state-changing request the browser marks as
// cross-site: the ingress session would otherwise carry a forged POST from
// any page the logged-in user visits.
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

// Template helpers.

func isoTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func dateOnly(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02")
}

// scrubFormatChars rewrites every invisible format character (bidi
// overrides, zero-width joiners: Unicode Cf) left in rendered HTML as a
// visible <U+XXXX>, in text and attributes alike. Keys and file names come
// from the config tree, where anyone with write access to a YAML file can
// plant one to make the panel lie about a name (Trojan Source); doing it on
// the output covers every place a name is rendered, not just the ones a
// template remembered to wrap in printable.
func scrubFormatChars(b []byte) []byte {
	if !bytes.ContainsFunc(b, func(r rune) bool { return unicode.Is(unicode.Cf, r) }) {
		return b
	}
	var out bytes.Buffer
	out.Grow(len(b) + 32)
	for _, r := range string(b) {
		if unicode.Is(unicode.Cf, r) {
			fmt.Fprintf(&out, "&lt;U+%04X&gt;", r)
			continue
		}
		out.WriteRune(r)
	}
	return out.Bytes()
}

// escapeFormatChars makes invisible format characters visible, so a file
// name or key from the config cannot hide text (Trojan Source).
func escapeFormatChars(s string) string {
	if !strings.ContainsFunc(s, func(r rune) bool { return unicode.Is(unicode.Cf, r) }) {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		if unicode.Is(unicode.Cf, r) {
			fmt.Fprintf(&b, "<U+%04X>", r)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func stateLabel(s string) string {
	switch s {
	case engine.StateHealthy:
		return "In sync"
	case engine.StateAttention:
		return "Needs a look"
	case engine.StateError:
		return "Problem"
	case engine.StateUnconfigured:
		return "Not set up"
	case engine.StateStarting:
		return "Starting"
	}
	return s
}

func rowLabel(s string) string {
	switch s {
	case engine.RowSynced:
		return "In sync"
	case engine.RowPending:
		return "Will change"
	case engine.RowMissing:
		return "Missing"
	case engine.RowUnmanaged:
		return "Only in file"
	case engine.RowConflict:
		return "Conflict"
	case engine.RowUnused:
		return "Not used"
	}
	return s
}

func kindLabel(k state.Kind) string {
	switch k {
	case state.KindChange:
		return "Changed"
	case state.KindRefresh:
		return "Applied"
	case state.KindRestart:
		return "Restart"
	case state.KindError:
		return "Error"
	case state.KindDryRun:
		return "Dry run"
	}
	return "Info"
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

func useText(u engine.Use) string {
	switch u.Kind {
	case "ha":
		return u.Label
	case "addon":
		return u.Label + " add-on"
	case "esphome":
		return u.Label + " (ESPHome)"
	}
	return u.Label
}

func tickClass(r engine.SecretRow) string {
	switch {
	case r.State == engine.RowMissing || r.State == engine.RowConflict:
		return "t-bad"
	case r.State == engine.RowUnmanaged:
		return "t-file"
	case r.State == engine.RowPending:
		return "t-pending"
	case r.Overdue:
		return "t-due"
	case r.State == engine.RowUnused:
		return "t-unused"
	}
	return "t-ok"
}

func tickOrder(r engine.SecretRow) int {
	switch tickClass(r) {
	case "t-bad":
		return 0
	case "t-due":
		return 1
	case "t-pending":
		return 2
	case "t-file":
		return 3
	case "t-ok":
		return 4
	}
	return 5
}

func rowNeedsAttention(r engine.SecretRow) bool {
	return r.State == engine.RowMissing || r.State == engine.RowConflict || r.State == engine.RowUnmanaged || r.Overdue || r.DueSoon
}

func setupDone(steps []engine.SetupStep) int {
	n := 0
	for _, s := range steps {
		if s.Done {
			n++
		}
	}
	return n
}

func dueText(r engine.SecretRow) string {
	if r.Due.IsZero() {
		if r.Interval == "on-compromise" {
			return "rotate on compromise"
		}
		return ""
	}
	until := time.Until(r.Due)
	days := int(until.Hours() / 24)
	switch {
	case r.Overdue:
		over := int(-until.Hours() / 24)
		if over == 0 {
			return "rotation overdue since today"
		}
		return fmt.Sprintf("rotation %s overdue", plural(over, "day", "days"))
	case days == 0:
		return "rotation due today"
	default:
		return fmt.Sprintf("rotation due in %s", plural(days, "day", "days"))
	}
}

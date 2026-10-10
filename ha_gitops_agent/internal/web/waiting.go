package web

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/execx"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/recon"
)

// Waiting builds the ingress handler served while startup waits for a
// secret:// option to resolve, before any Reconciler exists: the dashboard
// shell around one card saying what it waits for (status's
// WaitingForSecret), polled like the real page so the dashboard replaces
// it on its own once main swaps New in. Every other route answers 503.
// status is called per request and must be safe for concurrent use.
func Waiting(status func() recon.Status) http.Handler {
	current := func() recon.Status {
		s := status()
		s.RepoURL = execx.RedactURL(s.RepoURL)
		return s
	}

	mux := http.NewServeMux()
	mux.Handle("GET /static/", staticHandler())
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		renderPage(w, current(), "")
	})
	mux.HandleFunc("GET /fragment", func(w http.ResponseWriter, r *http.Request) {
		serveFragment(w, r, current(), "")
	})
	mux.HandleFunc("GET /status.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(statusJSON{Status: current()}); err != nil {
			slog.Warn("web: failed to encode status.json", "error", err)
		}
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "the agent has not started yet: "+current().WaitingForSecret, http.StatusServiceUnavailable)
	})
	return requireIngress(requireSameOrigin(mux))
}

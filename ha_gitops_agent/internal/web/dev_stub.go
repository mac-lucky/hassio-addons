//go:build !dev

package web

// Production stand-in for dev.go's preview fixtures: untagged builds
// never have a preview, and keep the fixture strings out of the binary.

import (
	"net/http"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/history"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/recon"
)

// devPreviewStatus reports no preview for any request, mirroring the
// dev-tagged implementation requestStatus calls in both builds.
func devPreviewStatus(*http.Request) (recon.Status, string, bool) {
	return recon.Status{}, "", false
}

// devPreviewHistory is the same answer for GET /history.
func devPreviewHistory(*http.Request) ([]history.Record, string, bool) {
	return nil, "", false
}

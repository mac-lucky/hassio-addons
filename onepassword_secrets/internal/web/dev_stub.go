//go:build !dev

package web

import (
	"net/http"

	"github.com/mac-lucky/hassio-addons/onepassword_secrets/internal/engine"
	"github.com/mac-lucky/hassio-addons/onepassword_secrets/internal/state"
)

// devPreview never matches in a release build: previews need -tags dev.
func devPreview(*http.Request) (string, bool) { return "", false }

func devStatus(string) engine.Status { return engine.Status{} }

func devKeyHistory(string) []state.Entry { return nil }

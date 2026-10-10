//go:build dev

package web

import (
	"net/http"
	"strings"
	"testing"
)

func TestEveryPreviewRenders(t *testing.T) {
	t.Setenv(DevEnvVar, "1")
	h := New(&fakeAgent{status: sampleStatus()})
	for _, name := range previews {
		for _, path := range []string{"/?preview=" + name, "/fragment?preview=" + name, "/status.json?preview=" + name} {
			w := request(t, h, "GET", path, "127.0.0.1:1", nil)
			if w.Code != http.StatusOK {
				t.Fatalf("%s = %d", path, w.Code)
			}
			if strings.Contains(w.Body.String(), "render error") {
				t.Fatalf("%s failed to render", path)
			}
		}
	}
	if w := request(t, h, "GET", "/?preview=nope", "127.0.0.1:1", nil); !strings.Contains(w.Body.String(), "wifi_password") {
		t.Fatal("an unknown preview should fall back to the live status")
	}
}

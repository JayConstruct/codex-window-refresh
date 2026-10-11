package plugin

import (
	"strings"
	"testing"
)

// Static resources must work before scheduler initialization, without reading
// credentials or runtime state. All browser assets are restricted to one origin.
func TestPanelAssetsUsePublicStaticRoutesAndRestrictedBrowserPolicy(t *testing.T) {
	s := New(nil, Options{})
	for _, asset := range []struct{ path, contentType, marker string }{
		{"/status", "text/html", "./panel.js"},
		{"/panel.js", "text/javascript", "createSession"},
		{"/panel.css", "text/css", ".shell"},
	} {
		t.Run(asset.path, func(t *testing.T) {
			r := managementCall(t, s, "GET", ResourcePrefix+asset.path)
			if r.StatusCode != 200 || !strings.HasPrefix(r.Headers.Get("Content-Type"), asset.contentType) || !strings.Contains(string(r.Body), asset.marker) {
				t.Fatal("public asset is unavailable or has the wrong response type")
			}
			csp := r.Headers.Get("Content-Security-Policy")
			for _, restriction := range []string{"default-src 'none'", "script-src 'self'", "style-src 'self'", "connect-src 'self'", "frame-ancestors 'self'", "base-uri 'none'", "form-action 'none'"} {
				if !strings.Contains(csp, restriction) {
					t.Fatalf("missing browser restriction: %s", restriction)
				}
			}
			if r.Headers.Get("Cache-Control") != "no-store" || r.Headers.Get("X-Content-Type-Options") != "nosniff" || r.Headers.Get("Referrer-Policy") != "no-referrer" {
				t.Fatal("asset browser headers are incomplete")
			}
			if r := managementCall(t, s, "POST", ResourcePrefix+asset.path); r.StatusCode != 404 {
				t.Fatal("public resource accepts mutations")
			}
		})
	}
}

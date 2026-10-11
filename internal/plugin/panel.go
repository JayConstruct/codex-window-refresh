package plugin

import (
	_ "embed"
	"net/http"
)

// The public resources contain no runtime data or credentials. Every live read
// and mutation goes through CPA's authenticated management routes.
//
//go:embed web/panel.html
var statusHTML string

//go:embed web/panel.js
var panelJS []byte

//go:embed web/panel.css
var panelCSS []byte

func panelResource(path string) (managementResponse, bool) {
	var body []byte
	var contentType string
	switch path {
	case ResourcePrefix + "/status":
		body, contentType = []byte(statusHTML), "text/html; charset=utf-8"
	case ResourcePrefix + "/panel.js":
		body, contentType = panelJS, "text/javascript; charset=utf-8"
	case ResourcePrefix + "/panel.css":
		body, contentType = panelCSS, "text/css; charset=utf-8"
	default:
		return managementResponse{}, false
	}
	return managementResponse{StatusCode: http.StatusOK, Body: body, Headers: http.Header{
		"Content-Type": {contentType}, "Cache-Control": {"no-store"},
		"Content-Security-Policy": {"default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; frame-ancestors 'self'; base-uri 'none'; form-action 'none'"},
		"X-Content-Type-Options":  {"nosniff"}, "Referrer-Policy": {"no-referrer"},
	}}, true
}

package core

// QuotaDiagnostic contains public classifications only, never remote messages,
// response bodies, credential material, or connection addresses.
type QuotaDiagnostic struct {
	Code       string `json:"code"`
	Stage      string `json:"stage"`
	HTTPStatus int    `json:"http_status,omitempty"`
	ElapsedMS  int64  `json:"elapsed_ms"`
}

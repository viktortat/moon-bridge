//go:build js && wasm

package provider

import (
	"net/http"

	"github.com/syumai/workers/cloudflare/fetch"
)

func newHTTPClient(cfg HTTPConfig) *http.Client {
	return fetch.NewClient().HTTPClient(fetch.RedirectModeFollow)
}

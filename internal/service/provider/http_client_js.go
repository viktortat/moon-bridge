//go:build js && wasm

package provider

import (
	"net/http"
	"syscall/js"

	"github.com/syumai/workers/cloudflare/fetch"
)

func newHTTPClient(cfg HTTPConfig) *http.Client {
	// __realGlobal is set by vendored wasm_exec.js before the proxy is created.
	realGlobal := js.Global().Get("__realGlobal")
	c := fetch.NewClient(fetch.WithBinding(realGlobal))
	return c.HTTPClient(fetch.RedirectModeFollow)
}

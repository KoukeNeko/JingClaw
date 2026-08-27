// Package webui serves the console that ships inside the daemon.
//
// It exists for the machine with no desktop on it: a server reached over SSH,
// a container, somebody else's Linux box. Those are the places this agent is
// most useful and the places a native client cannot go, so the answer has to
// travel inside the same binary rather than being something else to install.
//
// The files are deliberately plain — one page, no bundler, no dependency to
// resolve at build time. A static binary that needs a node_modules directory
// to have been present on the machine that built it is not a static binary.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed assets
var assets embed.FS

// Handler serves the console.
//
// It is served without a credential, which is deliberate and narrow: these
// files are code, not data, and a browser cannot present a bearer token on the
// request that fetches the page it would get the token from. Everything the
// console can *do* goes through the API, which is behind the check.
func Handler() http.Handler {
	root, err := fs.Sub(assets, "assets")
	if err != nil {
		// The directory is embedded at build time, so this cannot fail in a
		// binary that compiled.
		panic("webui: " + err.Error())
	}

	files := http.FileServerFS(root)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The console holds a credential in the page. Letting another site
		// frame it, or letting a browser guess at a content type, are the two
		// cheap ways that becomes somebody else's.
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")

		// Everything the page needs is in the page. Saying so means a stray
		// script tag cannot quietly fetch anything from anywhere else.
		w.Header().Set("Content-Security-Policy",
			"default-src 'none'; script-src 'self'; style-src 'self'; "+
				"img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'")

		// One page, so anything that is not a file it knows about is that page
		// rather than a 404. It keeps a refresh on a deep link working.
		if r.URL.Path != "/" && !strings.Contains(pathTail(r.URL.Path), ".") {
			r.URL.Path = "/"
		}

		files.ServeHTTP(w, r)
	})
}

func pathTail(path string) string {
	if index := strings.LastIndex(path, "/"); index >= 0 {
		return path[index+1:]
	}
	return path
}

// Package webui is the page the daemon serves to a browser: the dashboard for
// sessions, approvals and settings.
//
// It is plain files, with no build step, and they are embedded so the one
// binary is the whole thing: a page that has to be installed beside the
// program is a page that is the wrong version of it.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed assets
var embedded embed.FS

// Assets are the files of the page, rooted at its index.
func Assets() fs.FS {
	assets, err := fs.Sub(embedded, "assets")
	if err != nil {
		// The directory is part of the embed above; it cannot be missing.
		panic(err)
	}
	return assets
}

// policy is what the page is allowed to do.
//
// The page is an administrator's: it can approve a command and change what the
// agent may run. So it loads nothing from anywhere else, is not framed, and
// may only talk back to the daemon it came from. 'unsafe-eval' is there
// because the page's framework compiles its expressions; the rule that goes
// with it is that nothing the agent or a person wrote is ever put into the
// page as markup.
const policy = "default-src 'none'; " +
	"script-src 'self' 'unsafe-eval'; " +
	"style-src 'self'; style-src-attr 'unsafe-inline'; " +
	"img-src 'self' data:; " +
	"connect-src 'self'; " +
	"base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// Handler serves the page. Mount it under a prefix with http.StripPrefix.
//
// Open to anyone who can reach the daemon, behind the same host check as
// everything else: the files hold no secrets, and the page cannot do anything
// until it has been given a credential.
func Handler() http.Handler {
	files := http.FileServerFS(Assets())

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "read only", http.StatusMethodNotAllowed)
			return
		}

		header := w.Header()
		header.Set("Content-Security-Policy", policy)
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("Referrer-Policy", "no-referrer")
		header.Set("Cross-Origin-Opener-Policy", "same-origin")
		// The files change with the program, and a stale script talking to a
		// newer daemon is a bug nobody can find.
		header.Set("Cache-Control", "no-store")

		files.ServeHTTP(w, r)
	})
}

// Copyright (c) 2026 AMDL-Web contributors. SPDX-License-Identifier: MIT

package main

import (
	"embed"
	"io/fs"
	"net/http"
)

// The panel is a page on the main port for a person: the accounts, logging
// one in with its two-step code, and removing one, over the API beside it.
// It has no access control of its own, as the API has none: whoever reaches
// the port can use the accounts either way.
//
//go:embed panel
var panelFiles embed.FS

// panelCSP lets the page load only its own files and talk only to its
// origin, and keeps it out of frames.
const panelCSP = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src data:; " +
	"form-action 'none'; base-uri 'none'; frame-ancestors 'none'"

func servePanel(mux *http.ServeMux) {
	files, _ := fs.Sub(panelFiles, "panel")
	static := http.FileServerFS(files)
	headers := func(w http.ResponseWriter) {
		w.Header().Set("Content-Security-Policy", panelCSP)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
	}
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		headers(w)
		http.ServeFileFS(w, r, files, "index.html")
	})
	mux.Handle("GET /panel/", http.StripPrefix("/panel/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "" || r.URL.Path == "index.html" {
			http.NotFound(w, r)
			return
		}
		headers(w)
		static.ServeHTTP(w, r)
	})))
}

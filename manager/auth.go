// Copyright (c) 2026 AMDL-Web contributors. SPDX-License-Identifier: MIT

package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
)

// requireKey lets through only requests with "Authorization: Bearer <key>",
// and every request when key is "". A refusal is the Cloudflare gateway's:
// a plain 401 with WWW-Authenticate, unlike the {code, msg} of the manager's
// answers, so a client tells a wrong key from a wrapper that failed.
func requireKey(key string, next http.Handler) http.Handler {
	if key == "" {
		return next
	}
	want := sha256.Sum256([]byte(key))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		given, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		sum := sha256.Sum256([]byte(given))
		if !ok || subtle.ConstantTimeCompare(sum[:], want[:]) != 1 {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("WWW-Authenticate", "Bearer")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte("unauthorized\n"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

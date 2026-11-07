// Copyright (c) 2026 AMDL-Web contributors. SPDX-License-Identifier: MIT

package main

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestPanelIsServedBesideTheAPI(t *testing.T) {
	h := newHarness(t, nil)
	get := func(path string) (*http.Response, string) {
		t.Helper()
		resp, err := http.Get(h.srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp, string(body)
	}
	for path, want := range map[string]string{
		"/":                "text/html",
		"/panel/panel.js":  "text/javascript",
		"/panel/panel.css": "text/css",
	} {
		resp, body := get(path)
		if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), want) || body == "" {
			t.Errorf("%s: %d %q", path, resp.StatusCode, resp.Header.Get("Content-Type"))
		}
		if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") {
			t.Errorf("%s: CSP %q", path, csp)
		}
	}
	for _, path := range []string{"/panel/", "/panel/index.html", "/nothing", "/panel/../server.go"} {
		if resp, _ := get(path); resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMovedPermanently {
			t.Errorf("%s: %d, want nothing", path, resp.StatusCode)
		}
	}
	// The API beside it is as it was.
	if resp, body := get("/status"); resp.StatusCode != 200 || !strings.Contains(body, `"code":0`) {
		t.Errorf("/status: %d %s", resp.StatusCode, body)
	}
	resp, err := http.Post(h.srv.URL+"/", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("POST /: %d", resp.StatusCode)
	}
}

func TestAPIKeyGuardsEverythingButThePanel(t *testing.T) {
	const key = "0123456789abcdef0123456789abcdef-key"
	h := newHarness(t, nil, func(m *Manager) { m.cfg.apiKey = key })
	ask := func(method, path, auth string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(method, h.srv.URL+path, strings.NewReader("{}"))
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode == http.StatusUnauthorized && resp.Header.Get("WWW-Authenticate") != "Bearer" {
			t.Errorf("%s %s: 401 without WWW-Authenticate", method, path)
		}
		return resp.StatusCode, string(body)
	}
	for _, c := range []struct {
		method, path, auth string
		want               int
	}{
		{"GET", "/", "", 200},
		{"GET", "/panel/panel.js", "", 200},
		{"GET", "/status", "", 401},
		{"GET", "/status", "Bearer wrong", 401},
		{"GET", "/status", "Basic " + key, 401},
		{"GET", "/status", "Bearer " + key, 200},
		{"GET", "/token", "", 401},
		{"POST", "/login", "", 401},
		{"POST", "/logout", "", 401},
		{"GET", "/key?adamId=1&uri=x", "", 401},
		{"POST", "/", "", 401},
	} {
		if got, body := ask(c.method, c.path, c.auth); got != c.want {
			t.Errorf("%s %s (%q): %d %s, want %d", c.method, c.path, c.auth, got, body, c.want)
		}
	}
}

func TestAShortAPIKeyStopsTheManager(t *testing.T) {
	t.Setenv("API_KEY", "too-short")
	var stderr strings.Builder
	if code := run(nil, &stderr); code != 2 || !strings.Contains(stderr.String(), "API_KEY") {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
}

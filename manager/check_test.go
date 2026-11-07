// Copyright (c) 2026 AMDL-Web contributors. SPDX-License-Identifier: MIT

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCheckFetchesAKeyThroughTheAccountAsked(t *testing.T) {
	h := bootHarness(t, map[string]string{"a@x": "us", "b@x": "jp"}, []string{"a@x", "b@x"}, nil)
	waitFor(t, 10*time.Second, "ready", func() bool { return h.status()["ready"] == true })
	a, b := instanceID("a@x"), instanceID("b@x")
	if err := os.WriteFile(filepath.Join(h.accountDir(b), "behave-key"), []byte("500"), 0o644); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if _, env := h.call("POST", "/check", map[string]any{"id": a}); env["code"] != float64(0) {
			t.Fatalf("a: %v", env)
		}
	}
	if _, env := h.call("POST", "/check", map[string]any{"id": b}); env["code"] != float64(-1) || env["msg"] != "the wrapper answered code 500" {
		t.Fatalf("b: %v", env)
	}
	for _, tc := range []struct {
		method string
		body   any
		msg    string
	}{
		{"GET", nil, "method not allowed"},
		{"POST", map[string]any{"id": "x"}, "invalid id (expected a UUID)"},
		{"POST", map[string]any{"id": instanceID("nobody")}, "no such account"},
	} {
		if _, env := h.call(tc.method, "/check", tc.body); env["msg"] != tc.msg {
			t.Errorf("%s %v: %v", tc.method, tc.body, env)
		}
	}
}

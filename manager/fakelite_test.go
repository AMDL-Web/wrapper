// Copyright (c) 2026 AMDL-Web contributors. SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The tests run the test binary itself as wrapper-lite: the fake launcher
// script in data/wrapper execs it with FAKE_LITE=1. It behaves like lite as
// far as the manager can see: login mode writes the token files, service
// mode answers lite's routes. Tests steer it with the username (login) and
// with control files in the account directory (service).
func TestMain(m *testing.M) {
	switch {
	case os.Getenv("FAKE_LITE") == "1":
		os.Exit(fakeLite(os.Args[1:]))
	case os.Getenv("FAKE_SLEEP") == "1":
		time.Sleep(5 * time.Minute)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fakeLite(args []string) int {
	opts := map[string]string{}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--code-from-file":
			opts["code-from-file"] = "1"
		default:
			if i+1 < len(args) {
				opts[strings.TrimPrefix(args[i], "--")] = args[i+1]
				i++
			}
		}
	}
	cwd, _ := os.Getwd()
	dir := filepath.Join(cwd, "rootfs", opts["base-dir"])
	if login, ok := opts["login"]; ok {
		return fakeLogin(dir, opts["base-dir"], login, opts["proxy"])
	}
	return fakeService(dir, opts)
}

func has(dir, name string) bool { _, err := os.Stat(filepath.Join(dir, name)); return err == nil }

func fakeLogin(dir, baseDir, cred, proxy string) int {
	user, _, _ := strings.Cut(cred, ":")
	fmt.Println("[INFO] wrapper-lite login mode")
	if proxy != "" {
		_ = os.WriteFile(filepath.Join(dir, "login-proxy"), []byte(proxy), 0o644)
	}
	switch {
	case strings.Contains(user, "fail"):
		fmt.Println("[WARN] auth error: code=-5000, message=bad password")
		return 1
	case strings.Contains(user, "notokens"):
		return 0
	case strings.Contains(user, "hang"):
		time.Sleep(5 * time.Minute)
		return 1
	case strings.Contains(user, "2fa"):
		fmt.Fprintf(os.Stderr, "[WARN] Enter your 2FA code into %s/2fa.txt\n", baseDir)
		code := ""
		for range 200 {
			if raw, err := os.ReadFile(filepath.Join(dir, "2fa.txt")); err == nil {
				code = strings.TrimSpace(string(raw))
				_ = os.Remove(filepath.Join(dir, "2fa.txt"))
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if code != "123456" {
			fmt.Println("[WARN] 2FA code timeout (60s), aborting login")
			return 1
		}
	}
	region := "us"
	if i := strings.Index(user, "region-"); i >= 0 {
		region = user[i+7 : i+9]
	}
	_ = os.WriteFile(filepath.Join(dir, "STOREFRONT_ID"), []byte(region), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "MUSIC_TOKEN"), []byte("music-"+user), 0o644)
	fmt.Println("[INFO] login complete, exiting")
	if strings.Contains(user, "late") {
		time.Sleep(2 * time.Second)
	}
	return 0
}

func fakeService(dir string, opts map[string]string) int {
	id := filepath.Base(dir)
	switch {
	case has(dir, "START_FAIL"):
		fmt.Println("[ERROR] cannot start")
		return 3
	case has(dir, "START_HANG"):
		time.Sleep(5 * time.Minute)
		return 0
	}
	if has(dir, "SPAWN") {
		child := exec.Command(os.Args[0])
		child.Env = append(os.Environ(), "FAKE_LITE=", "FAKE_SLEEP=1")
		if child.Start() == nil {
			_ = os.WriteFile(filepath.Join(dir, "child.pid"), []byte(strconv.Itoa(child.Process.Pid)), 0o644)
		}
	}
	_ = os.WriteFile(filepath.Join(dir, "service-args"), []byte(strings.Join(os.Args[1:], " ")), 0o644)
	ln, err := net.Listen("tcp", net.JoinHostPort(opts["host"], opts["port"]))
	if err != nil {
		fmt.Println("[ERROR]", err)
		return 1
	}
	fmt.Printf("[INFO] token cache loaded (dev=abcdef music=ghijkl)\n")
	fmt.Printf("[INFO] wrapper-lite listening on %s:%s\n", opts["host"], opts["port"])
	var inflight, peak atomic.Int32
	envelope := func(w http.ResponseWriter, status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	read := func(name string) string {
		raw, _ := os.ReadFile(filepath.Join(dir, name))
		return strings.TrimSpace(string(raw))
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		regions := []string{}
		if !has(dir, "NO_REGION") && read("STOREFRONT_ID") != "" {
			regions = append(regions, read("STOREFRONT_ID"))
		}
		envelope(w, 200, map[string]any{"code": 0, "msg": "SUCCESS", "data": map[string]any{"regions": regions}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if has(dir, "TOKEN_503") {
			envelope(w, 503, map[string]any{"code": 503, "msg": "music token not available"})
			return
		}
		envelope(w, 200, map[string]any{"code": 0, "msg": "SUCCESS", "data": map[string]any{
			"region": read("STOREFRONT_ID"), "music_token": read("MUSIC_TOKEN"), "refreshed_at": 1700000000, "next_refresh_at": 1700001800}})
	})
	resource := func(w http.ResponseWriter, r *http.Request) {
		n := inflight.Add(1)
		defer inflight.Add(-1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		body, _ := io.ReadAll(r.Body)
		behave := read("behave" + strings.ReplaceAll(r.URL.Path, "/", "-"))
		if d, err := time.ParseDuration(read("DELAY")); err == nil {
			time.Sleep(d)
		}
		switch {
		case behave == "text":
			_, _ = io.WriteString(w, "not json")
		case behave != "":
			code, _ := strconv.Atoi(behave)
			envelope(w, 500, map[string]any{"code": code, "msg": "fake failure", "who": id})
		default:
			envelope(w, 200, map[string]any{"code": 0, "msg": "SUCCESS", "data": map[string]any{
				"who": id, "query": r.URL.RawQuery, "body": string(body), "method": r.Method, "ctype": r.Header.Get("Content-Type")}})
		}
	}
	for _, p := range []string{"/m3u8", "/key", "/lyrics", "/webplayback", "/license"} {
		mux.HandleFunc(p, resource)
	}
	mux.HandleFunc("/fake/peak", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, peak.Load())
	})
	mux.HandleFunc("/fake/say", func(w http.ResponseWriter, r *http.Request) {
		fmt.Println(r.URL.Query().Get("line"))
	})
	mux.HandleFunc("/fake/exit", func(w http.ResponseWriter, r *http.Request) {
		os.Exit(7)
	})
	_ = http.Serve(ln, mux)
	return 0
}

// syncBuffer is a log destination tests can read while the manager writes.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// harness is a manager in a temporary directory with the fake payload, served
// by an httptest server.
type harness struct {
	t    *testing.T
	m    *Manager
	data string
	srv  *httptest.Server
	logs *syncBuffer
}

// installFakePayload puts the fake launcher and an empty lite into data.
func installFakePayload(t *testing.T, data string) {
	t.Helper()
	wrapper := filepath.Join(data, "wrapper")
	if err := os.MkdirAll(filepath.Join(wrapper, "rootfs", "system", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf("#!/bin/sh\nexport FAKE_LITE=1\nexec '%s' \"$@\"\n", exe)
	for _, name := range []string{"wrapper-lite-rootless", "wrapper-lite"} {
		if err := os.WriteFile(filepath.Join(wrapper, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(wrapper, liteInRootfs), nil, 0o755); err != nil {
		t.Fatal(err)
	}
}

// seedAccount writes an account directory as a finished login leaves it.
func seedAccount(t *testing.T, data, id, region string, controls ...string) {
	t.Helper()
	dir := filepath.Join(data, "wrapper", "rootfs", "data", "instances", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"STOREFRONT_ID": region, "MUSIC_TOKEN": "music-" + id}
	for _, c := range controls {
		files[c] = ""
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// noApple answers like an unreachable Apple: every probe is unknown.
func noApple(t *testing.T) *httptest.Server {
	s := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(s.Close)
	return s
}

func newHarness(t *testing.T, entries []regEntry, tune ...func(*Manager)) *harness {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("needs sh")
	}
	data := filepath.Join(t.TempDir(), "data")
	installFakePayload(t, data)
	if entries != nil {
		if err := writeRegistry(filepath.Join(data, "instances.json"), entries); err != nil {
			t.Fatal(err)
		}
	}
	logs := &syncBuffer{}
	m := newManager(config{host: "127.0.0.1", launcher: "rootless", dataDir: data, guest: true}, log.New(logs, "", 0))
	m.readyTimeout, m.pollEvery = 10*time.Second, 50*time.Millisecond
	m.restartMin, m.restartMax = 100*time.Millisecond, 500*time.Millisecond
	apple := noApple(t)
	m.catalog.webURL, m.catalog.apiURL = apple.URL, apple.URL
	for _, f := range tune {
		f(m)
	}
	m.boot()
	srv := httptest.NewServer(m.mainServer().Handler)
	h := &harness{t: t, m: m, data: data, srv: srv, logs: logs}
	t.Cleanup(func() {
		m.stopAll(time.Second)
		srv.Close()
		if t.Failed() {
			t.Logf("manager log:\n%s", logs.String())
		}
	})
	return h
}

// call sends a request to the manager and decodes the envelope.
func (h *harness) call(method, path string, body any) (int, map[string]any) {
	h.t.Helper()
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, h.srv.URL+path, rd)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		h.t.Errorf("%s %s: Cache-Control %q", method, path, cc)
	}
	raw, _ := io.ReadAll(resp.Body)
	var env map[string]any
	if err := json.Unmarshal(raw, &env); err != nil {
		h.t.Fatalf("%s %s: %d %q is not JSON", method, path, resp.StatusCode, raw)
	}
	return resp.StatusCode, env
}

// status reads /status's data.
func (h *harness) status() map[string]any {
	h.t.Helper()
	_, env := h.call("GET", "/status", nil)
	return env["data"].(map[string]any)
}

// waitFor polls cond until it holds or d passes.
func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// readyIDs lists the ids /status reports ready.
func (h *harness) readyIDs() []string {
	var ids []string
	for _, x := range h.status()["instances"].([]any) {
		in := x.(map[string]any)
		if in["ready"] == true {
			ids = append(ids, in["id"].(string))
		}
	}
	return ids
}

func (h *harness) accountDir(id string) string {
	return filepath.Join(h.data, "wrapper", "rootfs", "data", "instances", id)
}

// port is the loopback port of id's running service.
func (h *harness) port(id string) int {
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	if a := h.m.find(id); a != nil {
		return a.port
	}
	return 0
}

// lite sends a request straight to id's fake lite.
func (h *harness) lite(id, path string) string {
	h.t.Helper()
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d%s", h.port(id), path))
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return string(raw)
}

// Copyright (c) 2026 AMDL-Web contributors. SPDX-License-Identifier: MIT

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStatusOfAnEmptyManager(t *testing.T) {
	h := newHarness(t, nil)
	resp, err := http.Get(h.srv.URL + "/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	want := `{"code":0,"msg":"SUCCESS","data":{"clientCount":0,"instances":[],"ready":true,"regions":[]}}`
	if resp.StatusCode != 200 || string(raw) != want {
		t.Fatalf("got %d %s, want %s", resp.StatusCode, raw, want)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type %q", ct)
	}
}

func TestStatusListsAccountsAndRegionsOnce(t *testing.T) {
	a, b, c := instanceID("a@x"), instanceID("b@x"), instanceID("c@x")
	h := newHarnessSeeded(t, []regEntry{{ID: a, Name: "a@x"}, {ID: b, Name: "b@x"}, {ID: c}}, func(data string) {
		seedAccount(t, data, a, "us")
		seedAccount(t, data, b, "US")
		seedAccount(t, data, c, "jp", "NO_REGION")
	})
	waitFor(t, 10*time.Second, "two ready accounts", func() bool { return len(h.readyIDs()) == 2 })
	st := h.status()
	if got := st["regions"]; !reflect.DeepEqual(got, []any{"us"}) {
		t.Fatalf("regions %v", got)
	}
	if st["clientCount"] != float64(3) || st["ready"] != false {
		t.Fatalf("clientCount %v ready %v", st["clientCount"], st["ready"])
	}
	inst := st["instances"].([]any)
	want := []map[string]any{
		{"id": a, "name": "a@x", "region": "us", "ready": true},
		{"id": b, "name": "b@x", "region": "US", "ready": true},
		{"id": c, "name": "", "region": "", "ready": false},
	}
	for i, w := range want {
		if !reflect.DeepEqual(inst[i], w) {
			t.Fatalf("instance %d: %v, want %v", i, inst[i], w)
		}
	}
}

// bootHarness seeds accounts before the manager boots.
func bootHarness(t *testing.T, accounts map[string]string, order []string, controls map[string][]string, tune ...func(*Manager)) *harness {
	t.Helper()
	var entries []regEntry
	for _, name := range order {
		entries = append(entries, regEntry{ID: instanceID(name), Name: name, Region: "zz", Port: 1})
	}
	return newHarnessSeeded(t, entries, func(data string) {
		for _, name := range order {
			seedAccount(t, data, instanceID(name), accounts[name], controls[name]...)
		}
	}, tune...)
}

// newHarnessSeeded lets seed write account directories before the boot.
func newHarnessSeeded(t *testing.T, entries []regEntry, seed func(data string), tune ...func(*Manager)) *harness {
	t.Helper()
	h := newHarness(t, entries, append([]func(*Manager){func(m *Manager) { seed(m.cfg.dataDir) }}, tune...)...)
	return h
}

func TestToken(t *testing.T) {
	h := bootHarness(t, map[string]string{"a@x": "us", "b@x": "jp", "c@x": "jp"}, []string{"a@x", "b@x", "c@x"},
		map[string][]string{"c@x": {"TOKEN_503"}})
	waitFor(t, 10*time.Second, "ready", func() bool { return h.status()["ready"] == true })
	a, b, c := instanceID("a@x"), instanceID("b@x"), instanceID("c@x")
	token := func(q string) (int, map[string]any) { return h.call("GET", "/token"+q, nil) }
	music := func(env map[string]any) string {
		d, _ := env["data"].(map[string]any)
		s, _ := d["music_token"].(string)
		return s
	}
	cases := []struct {
		q          string
		status     int
		code       float64
		musicToken string
	}{
		{"", 200, 0, "music-" + a},
		{"?region=JP", 200, 0, "music-" + b},
		{"?instance=" + strings.ToUpper(b), 200, 0, "music-" + b},
		{"?instance=%20" + b + "%20&region=us", 200, 0, "music-" + b},
		{"?instance=" + c, 503, 503, ""},
		{"?instance=" + instanceID("nobody"), 503, 503, ""},
		{"?instance=not-a-uuid", 503, 503, ""},
		{"?region=fr", 503, 503, ""},
	}
	for _, tc := range cases {
		status, env := token(tc.q)
		if status != tc.status || env["code"] != tc.code || music(env) != tc.musicToken {
			t.Errorf("/token%s: %d %v", tc.q, status, env)
		}
	}
	_, env := token("?instance=" + instanceID("nobody"))
	if env["msg"] != "no logged-in account with this instance id" || env["data"] != nil {
		t.Errorf("unknown instance: %v", env)
	}
	if _, env := h.call("POST", "/token", nil); env["code"] != float64(-1) || env["msg"] != "method not allowed" {
		t.Errorf("POST /token: %v", env)
	}
	if _, ok := env["data"]; ok {
		t.Errorf("a 503 has no data: %v", env)
	}
	if strings.Contains(h.logs.String(), "music-"+a) {
		t.Errorf("a token reached the log")
	}
}

func TestTokenWithoutAccounts(t *testing.T) {
	h := newHarness(t, nil)
	status, env := h.call("GET", "/token", nil)
	if status != 503 || env["code"] != float64(503) || env["msg"] != "no logged-in account" {
		t.Fatalf("%d %v", status, env)
	}
}

func TestResourceValidationTouchesNoAccount(t *testing.T) {
	h := bootHarness(t, map[string]string{"a@x": "us"}, []string{"a@x"}, nil)
	waitFor(t, 10*time.Second, "ready", func() bool { return h.status()["ready"] == true })
	for _, tc := range []struct{ path, mentions string }{
		{"/m3u8", "adamId"},
		{"/webplayback", "adamId"},
		{"/lyrics?syllable=1", "adamId"},
		{"/key?uri=skd://x", "adamId"},
		{"/key?adamId=1", "uri"},
		{"/key?adamId=1&uri=skd://itunes.apple.com/P000000000/s1/e1", "uri"},
	} {
		_, env := h.call("GET", tc.path, nil)
		if env["code"] != float64(-1) || !strings.Contains(env["msg"].(string), tc.mentions) {
			t.Errorf("%s: %v", tc.path, env)
		}
	}
	if peak := h.lite(instanceID("a@x"), "/fake/peak"); peak != "0" {
		t.Errorf("an invalid request reached lite (peak %s)", peak)
	}
	_, env := h.call("GET", "/key?adamId=0&uri=skd://itunes.apple.com/P000000000/s1/e1", nil)
	if env["code"] != float64(0) {
		t.Fatalf("prefetch key: %v", env)
	}
	if q := env["data"].(map[string]any)["query"].(string); !strings.Contains(q, "adamId=0") || !strings.Contains(q, "uri=") {
		t.Errorf("forwarded query %q", q)
	}
}

func TestLicense(t *testing.T) {
	h := bootHarness(t, map[string]string{"a@x": "us"}, []string{"a@x"}, nil)
	waitFor(t, 10*time.Second, "ready", func() bool { return h.status()["ready"] == true })
	if _, env := h.call("GET", "/license", nil); env["code"] != float64(-1) || env["msg"] != "method not allowed" {
		t.Errorf("GET: %v", env)
	}
	resp, _ := http.Post(h.srv.URL+"/license", "application/json", strings.NewReader("{not json"))
	var env map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&env)
	resp.Body.Close()
	if env["code"] != float64(-1) || !strings.Contains(env["msg"].(string), "JSON") {
		t.Errorf("non-JSON: %v", env)
	}
	if _, env := h.call("POST", "/license", map[string]any{"adamId": "1", "challenge": "c"}); env["code"] != float64(-1) || !strings.Contains(env["msg"].(string), "uri") {
		t.Errorf("missing uri: %v", env)
	}
	body := `{"adamId":"1","challenge":"abc","uri":"data:x","drm-type":"wv","extra":[1,2]}`
	resp, err := http.Post(h.srv.URL+"/license", "text/plain", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_ = json.NewDecoder(resp.Body).Decode(&env)
	resp.Body.Close()
	data := env["data"].(map[string]any)
	if env["code"] != float64(0) || data["body"] != body || data["method"] != "POST" || data["ctype"] != "application/json" {
		t.Errorf("forwarded: %v", env)
	}
}

func TestFailover(t *testing.T) {
	accounts := map[string]string{"a@x": "us", "b@x": "us", "c@x": "us", "d@x": "us"}
	order := []string{"a@x", "b@x", "c@x", "d@x"}
	h := bootHarness(t, accounts, order, nil)
	waitFor(t, 10*time.Second, "ready", func() bool { return h.status()["ready"] == true })
	behave := func(name, route, v string) {
		p := filepath.Join(h.accountDir(instanceID(name)), "behave-"+strings.TrimPrefix(route, "/"))
		if v == "" {
			_ = os.Remove(p)
			return
		}
		if err := os.WriteFile(p, []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// One healthy account among failing ones: always found within 3 tries
	// only when at most 2 fail; with 3 failing, a 500 body comes back.
	behave("a@x", "/m3u8", "500")
	behave("b@x", "/m3u8", "500")
	behave("c@x", "/m3u8", "500")
	sawOK, sawFail := false, false
	for range 40 {
		_, env := h.call("GET", "/m3u8?adamId=1", nil)
		switch env["code"] {
		case float64(0):
			sawOK = true
			if env["data"].(map[string]any)["who"] != instanceID("d@x") {
				t.Fatalf("success from %v", env)
			}
		case float64(500):
			sawFail = true
		default:
			t.Fatalf("unexpected %v", env)
		}
	}
	if !sawOK || !sawFail {
		t.Fatalf("ok %v, fail %v: want both over 40 random orders", sawOK, sawFail)
	}

	// 400 and lyrics 404 are final at once.
	for _, n := range order {
		behave(n, "/m3u8", "")
		behave(n, "/key", "400")
		behave(n, "/lyrics", "404")
	}
	before := h.logs.String()
	if _, env := h.call("GET", "/key?adamId=1&uri=skd://a", nil); env["code"] != float64(400) {
		t.Fatalf("400: %v", env)
	}
	if _, env := h.call("GET", "/lyrics?adamId=1", nil); env["code"] != float64(404) {
		t.Fatalf("lyrics 404: %v", env)
	}
	if strings.Count(h.logs.String()[len(before):], "answered code") != 0 {
		t.Fatalf("a final answer was retried:\n%s", h.logs.String()[len(before):])
	}

	// m3u8 404 is retried; at most 3 attempts.
	for _, n := range order {
		behave(n, "/m3u8", "404")
	}
	before = h.logs.String()
	if _, env := h.call("GET", "/m3u8?adamId=1", nil); env["code"] != float64(404) {
		t.Fatalf("m3u8 404: %v", env)
	}
	if n := strings.Count(h.logs.String()[len(before):], "answered code 404"); n != 3 {
		t.Fatalf("%d attempts, want 3", n)
	}

	// A reply that is not JSON counts as success.
	behave("a@x", "/webplayback", "text")
	behave("b@x", "/webplayback", "text")
	behave("c@x", "/webplayback", "text")
	behave("d@x", "/webplayback", "text")
	resp, _ := http.Get(h.srv.URL + "/webplayback?adamId=1")
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(raw) != "not json" {
		t.Fatalf("non-JSON reply: %q", raw)
	}
}

func TestEveryCandidateUnreachable(t *testing.T) {
	h := bootHarness(t, map[string]string{"a@x": "us"}, []string{"a@x"}, nil)
	waitFor(t, 10*time.Second, "ready", func() bool { return h.status()["ready"] == true })
	h.m.mu.Lock()
	a := h.m.accounts[0]
	h.m.mu.Unlock()
	_ = os.WriteFile(filepath.Join(h.accountDir(a.id), "DELAY"), []byte("3s"), 0o644)
	old := upstreamLimit
	upstreamLimit = 300 * time.Millisecond
	defer func() { upstreamLimit = old }()
	start := time.Now()
	_, env := h.call("GET", "/m3u8?adamId=1", nil)
	if env["code"] != float64(-1) || env["msg"] != "no available instance" {
		t.Fatalf("%v", env)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("a hung lite held the request %s", time.Since(start))
	}
	// The slots were freed.
	_ = os.Remove(filepath.Join(h.accountDir(a.id), "DELAY"))
	if len(a.slots) != 0 {
		t.Fatalf("%d slots still held", len(a.slots))
	}
	if _, env := h.call("GET", "/m3u8?adamId=1", nil); env["code"] != float64(0) {
		t.Fatalf("after the hang: %v", env)
	}
}

func TestNoAccountNoCandidate(t *testing.T) {
	h := newHarness(t, nil)
	if _, env := h.call("GET", "/m3u8?adamId=1", nil); env["msg"] != "no available instance" || env["code"] != float64(-1) {
		t.Fatalf("%v", env)
	}
}

func TestTwoRequestsInFlightPerLite(t *testing.T) {
	h := bootHarness(t, map[string]string{"a@x": "us"}, []string{"a@x"}, nil)
	waitFor(t, 10*time.Second, "ready", func() bool { return h.status()["ready"] == true })
	id := instanceID("a@x")
	_ = os.WriteFile(filepath.Join(h.accountDir(id), "DELAY"), []byte("400ms"), 0o644)
	var wg sync.WaitGroup
	errs := make(chan string, 5)
	start := time.Now()
	for range 5 {
		wg.Go(func() {
			resp, err := http.Get(h.srv.URL + "/key?adamId=1&uri=skd://a")
			if err != nil {
				errs <- err.Error()
				return
			}
			var env map[string]any
			_ = json.NewDecoder(resp.Body).Decode(&env)
			resp.Body.Close()
			if env["code"] != float64(0) {
				errs <- fmt.Sprint(env)
			}
		})
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	if peak := h.lite(id, "/fake/peak"); peak != "2" {
		t.Fatalf("peak in flight %s, want 2", peak)
	}
	if d := time.Since(start); d < 1100*time.Millisecond {
		t.Fatalf("5 requests of 400 ms took %s with 2 slots", d)
	}
}

func TestUnknownPathAndShutdownRoute(t *testing.T) {
	h := newHarness(t, nil)
	resp, _ := http.Get(h.srv.URL + "/nope")
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("unknown path: %d", resp.StatusCode)
	}
	stopped := make(chan struct{}, 1)
	h.m.requestStop = func() { stopped <- struct{}{} }
	if _, env := h.call("GET", "/shutdown", nil); env["msg"] != "method not allowed" {
		t.Fatalf("GET /shutdown: %v", env)
	}
	select {
	case <-stopped:
		t.Fatal("GET stopped the manager")
	default:
	}
	if _, env := h.call("POST", "/shutdown", nil); env["code"] != float64(0) || !reflect.DeepEqual(env["data"], map[string]any{}) {
		t.Fatalf("POST /shutdown: %v", env)
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("POST did not stop the manager")
	}
	if !strings.Contains(h.logs.String(), "shutdown requested") {
		t.Fatal("no shutdown line")
	}

	// Outside the guest the route does not exist.
	m := newManager(config{dataDir: t.TempDir()}, h.m.logger)
	rec := &recorder{header: http.Header{}}
	m.mainServer().Handler.ServeHTTP(rec, mustRequest("POST", "/shutdown"))
	if rec.status != 404 {
		t.Fatalf("outside the guest: %d", rec.status)
	}
}

type recorder struct {
	header http.Header
	status int
	body   strings.Builder
}

func (r *recorder) Header() http.Header         { return r.header }
func (r *recorder) Write(p []byte) (int, error) { return r.body.Write(p) }
func (r *recorder) WriteHeader(s int)           { r.status = s }

func mustRequest(method, path string) *http.Request {
	req, err := http.NewRequest(method, "http://manager"+path, nil)
	if err != nil {
		panic(err)
	}
	return req
}

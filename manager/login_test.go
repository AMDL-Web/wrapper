// Copyright (c) 2026 AMDL-Web contributors. SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const pw = "s3cret-Passw0rd"

func (h *harness) login(user string, code ...string) map[string]any {
	h.t.Helper()
	body := map[string]any{"username": user, "password": pw}
	if len(code) > 0 {
		body["code"] = code[0]
	}
	_, env := h.call("POST", "/login", body)
	return env
}

func (h *harness) registryIDs() []string {
	h.t.Helper()
	entries, err := readRegistry(filepath.Join(h.data, "instances.json"))
	if err != nil {
		h.t.Fatal(err)
	}
	var ids []string
	for _, e := range entries {
		ids = append(ids, e.ID)
	}
	return ids
}

func (h *harness) noLogins() bool {
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	return len(h.m.logins) == 0
}

func TestLoginWithoutCode(t *testing.T) {
	h := newHarness(t, nil, func(m *Manager) { m.cfg.proxy = "http://proxy.invalid:3128" })
	user := "user+region-jp@example.com"
	env := h.login(user)
	id := instanceID(user)
	if env["code"] != float64(0) || env["msg"] != "SUCCESS" || env["data"].(map[string]any)["loginId"] != id {
		t.Fatalf("%v", env)
	}
	st := h.status()
	inst := st["instances"].([]any)[0].(map[string]any)
	if inst["id"] != id || inst["name"] != user || inst["region"] != "jp" || inst["ready"] != true {
		t.Fatalf("status %v", st)
	}
	entries, _ := readRegistry(filepath.Join(h.data, "instances.json"))
	if len(entries) != 1 || entries[0].ID != id || entries[0].Name != user || entries[0].Region != "jp" {
		t.Fatalf("registry %+v", entries)
	}
	if raw, _ := os.ReadFile(filepath.Join(h.accountDir(id), "login-proxy")); string(raw) != "http://proxy.invalid:3128" {
		t.Errorf("the login child got proxy %q", raw)
	}
	args, _ := os.ReadFile(filepath.Join(h.accountDir(id), "service-args"))
	if !strings.Contains(string(args), "--base-dir /data/instances/"+id+" --host 127.0.0.1 --port ") ||
		!strings.HasSuffix(string(args), "--log-level info --proxy http://proxy.invalid:3128") {
		t.Errorf("service args %q", args)
	}
	if env := h.login(user); env["msg"] != "already login" {
		t.Fatalf("second login: %v", env)
	}
	assertNoPassword(t, h)
}

// assertNoPassword checks that the password is in no log line and no file
// the manager wrote under data/ (the fake lite writes none either).
func assertNoPassword(t *testing.T, h *harness) {
	t.Helper()
	if strings.Contains(h.logs.String(), pw) {
		t.Fatal("the password reached the log")
	}
	_ = filepath.WalkDir(h.data, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if raw, _ := os.ReadFile(p); bytes.Contains(raw, []byte(pw)) {
				t.Errorf("the password is in %s", p)
			}
		}
		return nil
	})
}

func TestLoginWithCode(t *testing.T) {
	h := newHarness(t, nil)
	user := "2fa@example.com"
	id := instanceID(user)
	env := h.login(user)
	if env["code"] != float64(2) || env["data"].(map[string]any)["loginId"] != id {
		t.Fatalf("first answer %v", env)
	}
	if h.noLogins() {
		t.Fatal("the child did not stay")
	}
	env = h.login(user, " 123456 ")
	if env["code"] != float64(0) || env["data"].(map[string]any)["loginId"] != id {
		t.Fatalf("with code %v", env)
	}
	if ids := h.readyIDs(); len(ids) != 1 || ids[0] != id {
		t.Fatalf("ready %v", ids)
	}
	if _, err := os.Stat(filepath.Join(h.accountDir(id), "2fa.txt")); err == nil {
		t.Fatal("2fa.txt is left")
	}

	// A wrong code ends the login as a failure.
	user = "2fa-wrong@example.com"
	if env := h.login(user); env["code"] != float64(2) {
		t.Fatalf("%v", env)
	}
	env = h.login(user, "000000")
	if env["code"] != float64(-1) || env["msg"] != "login failed (exit code 1)" {
		t.Fatalf("wrong code %v", env)
	}
	if _, err := os.Stat(h.accountDir(instanceID(user))); err == nil {
		t.Fatal("a failed login left its directory")
	}
	assertNoPassword(t, h)
}

func TestCodeWithoutPendingLogin(t *testing.T) {
	h := newHarness(t, nil)
	env := h.login("someone@example.com", "123456")
	if env["msg"] != "no pending login for this account, restart login without code" {
		t.Fatalf("%v", env)
	}
	// A login in flight that has not asked for a code takes none either.
	go h.login("hang@example.com")
	waitFor(t, 5*time.Second, "the login child", func() bool { return !h.noLogins() })
	if env := h.login("hang@example.com", "123456"); !strings.HasPrefix(env["msg"].(string), "no pending login") {
		t.Fatalf("%v", env)
	}
}

func TestLoginValidation(t *testing.T) {
	h := newHarness(t, nil)
	for _, tc := range []struct {
		body any
		msg  string
	}{
		{map[string]any{"username": "a"}, "missing username or password"},
		{map[string]any{"password": "a", "code": "1"}, "missing username or password"},
		{map[string]any{"username": "a:b", "password": "c"}, "colon"},
		{map[string]any{"username": "a", "password": "c\nd"}, "line break"},
		{[]int{1}, "invalid JSON body"},
	} {
		if _, env := h.call("POST", "/login", tc.body); env["code"] != float64(-1) || !strings.Contains(env["msg"].(string), tc.msg) {
			t.Errorf("%v: %v", tc.body, env)
		}
	}
	if _, env := h.call("GET", "/login", nil); env["msg"] != "method not allowed" {
		t.Errorf("GET: %v", env)
	}
}

// childPID is the pid of the login child of id, 0 if none.
func (h *harness) childPID(id string) int {
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	if l := h.m.logins[id]; l != nil && l.proc != nil {
		return l.proc.cmd.Process.Pid
	}
	return 0
}

// gone reports whether pid has ended within a few seconds; a zombie not yet
// reaped by init counts as ended.
func gone(pid int) bool {
	deadline := time.Now().Add(3 * time.Second)
	for {
		if syscall.Kill(pid, 0) == syscall.ESRCH {
			return true
		}
		if stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
			if _, after, ok := strings.Cut(string(stat), ") "); ok && strings.HasPrefix(after, "Z") {
				return true
			}
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestNewLoginReplacesOneWaitingForCode(t *testing.T) {
	h := newHarness(t, nil)
	user := "2fa-again@example.com"
	id := instanceID(user)
	if env := h.login(user); env["code"] != float64(2) {
		t.Fatalf("%v", env)
	}
	first := h.childPID(id)
	if env := h.login(user); env["code"] != float64(2) {
		t.Fatalf("replacing login: %v", env)
	}
	if !gone(first) {
		t.Fatalf("the first child %d is still there", first)
	}
	if second := h.childPID(id); second == 0 || second == first {
		t.Fatalf("second child %d", second)
	}
	if env := h.login(user, "123456"); env["code"] != float64(0) {
		t.Fatalf("code for the second: %v", env)
	}
}

func TestCancelledLogin(t *testing.T) {
	h := newHarness(t, nil)
	user := "hang@example.com"
	id := instanceID(user)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan time.Duration, 1)
	go func() {
		raw, _ := json.Marshal(map[string]string{"username": user, "password": pw})
		req, _ := http.NewRequestWithContext(ctx, "POST", h.srv.URL+"/login", bytes.NewReader(raw))
		_, _ = http.DefaultClient.Do(req)
	}()
	waitFor(t, 5*time.Second, "the child", func() bool { return h.childPID(id) != 0 })
	pid := h.childPID(id)
	start := time.Now()
	cancel()
	go func() {
		waitFor(t, 5*time.Second, "cleanup", func() bool { return h.noLogins() })
		done <- time.Since(start)
	}()
	if d := <-done; d > 3*time.Second {
		t.Fatalf("cleanup took %s", d)
	}
	if !gone(pid) {
		t.Fatal("the child survived")
	}
	if _, err := os.Stat(h.accountDir(id)); err == nil {
		t.Fatal("the directory survived")
	}
	if len(h.status()["instances"].([]any)) != 0 {
		t.Fatal("registered")
	}
	// A retry runs its own child to its own end.
	go h.login(user)
	waitFor(t, 5*time.Second, "a new child", func() bool { return h.childPID(id) != 0 && h.childPID(id) != pid })
}

func TestCancelAfterTheChildLoggedIn(t *testing.T) {
	h := newHarness(t, nil)
	user := "late@example.com"
	id := instanceID(user)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		raw, _ := json.Marshal(map[string]string{"username": user, "password": pw})
		req, _ := http.NewRequestWithContext(ctx, "POST", h.srv.URL+"/login", bytes.NewReader(raw))
		_, _ = http.DefaultClient.Do(req)
	}()
	waitFor(t, 5*time.Second, "tokens", func() bool { return fileExists(filepath.Join(h.accountDir(id), "MUSIC_TOKEN")) })
	cancel()
	if env := h.login(user); env["msg"] != "already login" {
		t.Fatalf("retry: %v", env)
	}
	if ids := h.registryIDs(); len(ids) != 1 || ids[0] != id {
		t.Fatalf("registry %v", ids)
	}
}

func TestLoginTimeout(t *testing.T) {
	h := newHarness(t, nil, func(m *Manager) { m.loginTimeout = 500 * time.Millisecond })
	user := "hang@example.com"
	start := time.Now()
	if env := h.login(user); env["msg"] != "login timeout" {
		t.Fatalf("%v", env)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("took %s", d)
	}
	waitFor(t, 5*time.Second, "cleanup", h.noLogins)
	if _, err := os.Stat(h.accountDir(instanceID(user))); err == nil {
		t.Fatal("the directory survived")
	}
}

func TestLoginFailures(t *testing.T) {
	h := newHarness(t, nil)
	for user, msg := range map[string]string{
		"fail@example.com":     "login failed (exit code 1)",
		"notokens@example.com": "login failed: login process exited without caching tokens",
	} {
		if env := h.login(user); env["msg"] != msg || env["code"] != float64(-1) {
			t.Errorf("%s: %v", user, env)
		}
		if _, err := os.Stat(h.accountDir(instanceID(user))); err == nil {
			t.Errorf("%s left its directory", user)
		}
	}
	if len(h.status()["instances"].([]any)) != 0 {
		t.Fatal("a failed login registered")
	}
}

func TestAccountLimit(t *testing.T) {
	var entries []regEntry
	for i := range 9 {
		entries = append(entries, regEntry{ID: instanceID(fmt.Sprintf("u%d@x", i)), Name: fmt.Sprintf("u%d@x", i)})
	}
	h := newHarnessSeeded(t, entries, func(data string) {
		for _, e := range entries {
			seedAccount(t, data, e.ID, "us")
		}
	})
	waitFor(t, 15*time.Second, "ready", func() bool { return h.status()["ready"] == true })
	// The tenth slot goes to one of two competing logins.
	results := make(chan string, 2)
	for _, u := range []string{"2fa-one@x", "2fa-two@x"} {
		go func() { results <- h.login(u)["msg"].(string) }()
	}
	got := []string{<-results, <-results}
	limit := 0
	for _, m := range got {
		if strings.HasPrefix(m, "account limit reached") {
			limit++
		}
	}
	if limit != 1 {
		t.Fatalf("%v", got)
	}
	env := h.login("eleventh@x")
	if env["msg"] != "account limit reached: this manager holds at most 10 accounts, log one out first" {
		t.Fatalf("%v", env)
	}
	if _, err := os.Stat(h.accountDir(instanceID("eleventh@x"))); err == nil {
		t.Fatal("a refused login created its directory")
	}
	if env := h.login("u3@x"); env["msg"] != "already login" {
		t.Fatalf("held account: %v", env)
	}
}

func TestLogout(t *testing.T) {
	names := []string{"a@x", "b@x", "c@x"}
	var entries []regEntry
	for _, n := range names {
		entries = append(entries, regEntry{ID: instanceID(n), Name: n})
	}
	h := newHarnessSeeded(t, entries, func(data string) {
		for _, n := range names {
			seedAccount(t, data, instanceID(n), "us", "SPAWN")
		}
	})
	waitFor(t, 10*time.Second, "ready", func() bool { return h.status()["ready"] == true })
	b := instanceID("b@x")
	pidRaw, _ := os.ReadFile(filepath.Join(h.accountDir(b), "child.pid"))
	child, _ := strconv.Atoi(string(pidRaw))
	_, env := h.call("POST", "/logout", map[string]any{"username": "zzz@x", "id": strings.ToUpper(b)})
	if env["code"] != float64(0) || fmt.Sprint(env["data"]) != fmt.Sprint(map[string]any{"id": strings.ToUpper(b), "username": "zzz@x"}) {
		t.Fatalf("logout: %v", env)
	}
	if child == 0 || !gone(child) {
		t.Fatalf("the lite's child %d survived", child)
	}
	if _, err := os.Stat(h.accountDir(b)); err == nil {
		t.Fatal("directory left")
	}
	if ids := h.registryIDs(); len(ids) != 2 || ids[0] != instanceID("a@x") || ids[1] != instanceID("c@x") {
		t.Fatalf("registry %v", ids)
	}
	if _, err := os.Stat(filepath.Join(h.data, "instances.json.tmp")); err == nil {
		t.Fatal("temporary registry left")
	}
	if _, env := h.call("POST", "/logout", map[string]any{"id": b}); env["msg"] != "no such account" {
		t.Fatalf("second logout %v", env)
	}
	_, env = h.call("POST", "/logout", map[string]any{"username": "a@x"})
	if env["code"] != float64(0) || fmt.Sprint(env["data"]) != "map[username:a@x]" {
		t.Fatalf("by username %v", env)
	}
	if _, env := h.call("POST", "/logout", map[string]any{"id": "nope"}); env["msg"] != "invalid id (expected a UUID)" {
		t.Fatalf("bad id %v", env)
	}
	if _, env := h.call("POST", "/logout", map[string]any{}); env["msg"] != "missing username or id" {
		t.Fatalf("empty %v", env)
	}
	if ids := h.readyIDs(); len(ids) != 1 || ids[0] != instanceID("c@x") {
		t.Fatalf("left %v", ids)
	}
}

func TestLogoutOfAnAccountThatNeverStarts(t *testing.T) {
	id := instanceID("broken@x")
	h := newHarnessSeeded(t, []regEntry{{ID: id}}, func(data string) { seedAccount(t, data, id, "us", "START_FAIL") })
	waitFor(t, 5*time.Second, "a failed start", func() bool { return strings.Contains(h.logs.String(), "did not start") })
	if _, env := h.call("POST", "/logout", map[string]any{"id": id}); env["code"] != float64(0) {
		t.Fatalf("%v", env)
	}
	if st := h.status(); st["ready"] != true || st["clientCount"] != float64(0) {
		t.Fatalf("%v", st)
	}
}

func TestReloginOfAnAccountThatIsNotServing(t *testing.T) {
	user := "dead@x"
	id := instanceID(user)
	h := newHarnessSeeded(t, []regEntry{{ID: id, Name: user}}, func(data string) { seedAccount(t, data, id, "us", "START_HANG") })
	time.Sleep(300 * time.Millisecond) // its service hangs before reporting a region
	env := h.login(user)
	if env["code"] != float64(0) {
		t.Fatalf("%v", env)
	}
	if ids := h.readyIDs(); len(ids) != 1 || ids[0] != id {
		t.Fatalf("ready %v", ids)
	}
	if ids := h.registryIDs(); len(ids) != 1 {
		t.Fatalf("registry %v", ids)
	}
}

func TestConcurrentLoginsOfDifferentAccounts(t *testing.T) {
	h := newHarness(t, nil)
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Go(func() {
			if env := h.login(fmt.Sprintf("p%d@x", i)); env["code"] != float64(0) {
				t.Errorf("%d: %v", i, env)
			}
		})
	}
	wg.Wait()
	if n := len(h.registryIDs()); n != 4 {
		t.Fatalf("%d registered", n)
	}
}

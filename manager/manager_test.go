// Copyright (c) 2026 AMDL-Web contributors. SPDX-License-Identifier: MIT

package main

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInstanceID(t *testing.T) {
	if got := instanceID("a@example.com"); got != "b193ebc0-ca20-50a7-b158-6a8111b2a0f3" {
		t.Fatalf("got %s", got)
	}
	if instanceID(" a@example.com") == instanceID("a@example.com") || instanceID("A@example.com") == instanceID("a@example.com") {
		t.Fatal("usernames are not trimmed or folded")
	}
	if !isUUID(instanceID("x")) || isUUID(strings.ToUpper(instanceID("x"))) {
		t.Fatal("isUUID")
	}
}

func TestRegistryFileFormat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "instances.json")
	// Files of older managers: names missing, extra fields.
	old := `[
  {"id": "b193ebc0-ca20-50a7-b158-6a8111b2a0f3", "region": "us", "port": 41234},
  {"id": "6ba7b810-9dad-51d1-80b4-00c04fd430c8", "region": "", "port": 0, "name": "x@y", "degraded": true}
]`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := readRegistry(path)
	if err != nil || len(entries) != 2 || entries[0].Name != "" || entries[1].Name != "x@y" {
		t.Fatalf("%+v %v", entries, err)
	}
	if err := writeRegistry(path, entries); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	want := "[\n  {\n    \"id\": \"b193ebc0-ca20-50a7-b158-6a8111b2a0f3\",\n    \"region\": \"us\",\n    \"port\": 41234,\n    \"name\": \"\"\n  },"
	if !strings.HasPrefix(string(raw), want) {
		t.Fatalf("written:\n%s", raw)
	}
	if _, err := os.Stat(path + ".tmp"); err == nil {
		t.Fatal("temporary file left")
	}
	if err := writeRegistry(path, nil); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(path); string(raw) != "[]\n" {
		t.Fatalf("empty registry %q", raw)
	}
}

func TestCorruptRegistryIsSetAside(t *testing.T) {
	data := filepath.Join(t.TempDir(), "data")
	h := newHarness(t, nil, func(m *Manager) {
		data = m.cfg.dataDir
		_ = os.WriteFile(filepath.Join(data, "instances.json"), []byte("{broken"), 0o600)
	})
	if raw, err := os.ReadFile(filepath.Join(data, "instances.json.corrupt")); err != nil || string(raw) != "{broken" {
		t.Fatalf("corrupt copy: %q %v", raw, err)
	}
	if st := h.status(); st["ready"] != true || st["clientCount"] != float64(0) {
		t.Fatalf("%v", st)
	}
}

func TestBootReadinessAndRestartAfterACrash(t *testing.T) {
	a, b := instanceID("a@x"), instanceID("b@x")
	h := newHarnessSeeded(t, []regEntry{{ID: a, Name: "a@x"}, {ID: b, Name: "b@x"}}, func(data string) {
		seedAccount(t, data, a, "us")
		seedAccount(t, data, b, "jp", "NO_REGION")
	})
	// b has not come up: the manager is not ready and b has no token.
	waitFor(t, 5*time.Second, "a up", func() bool { return len(h.readyIDs()) == 1 })
	if st := h.status(); st["ready"] != false {
		t.Fatalf("ready with b down: %v", st)
	}
	if status, _ := h.call("GET", "/token?instance="+b, nil); status != 503 {
		t.Fatalf("token of a starting account: %d", status)
	}
	_ = os.Remove(filepath.Join(h.accountDir(b), "NO_REGION"))
	waitFor(t, 5*time.Second, "ready", func() bool { return h.status()["ready"] == true })

	// A crash: restarted on a new port with the same id and name.
	port := h.port(a)
	h.lite(a, "/fake/exit")
	waitFor(t, 5*time.Second, "restart", func() bool { p := h.port(a); return p != 0 && p != port })
	waitFor(t, 5*time.Second, "both ready", func() bool { return len(h.readyIDs()) == 2 })
	st := h.status()
	if st["ready"] != true {
		t.Fatal("a crash lowered ready")
	}
	if inst := st["instances"].([]any)[0].(map[string]any); inst["id"] != a || inst["name"] != "a@x" {
		t.Fatalf("after restart %v", inst)
	}
	if !strings.Contains(h.logs.String(), "is down") {
		t.Fatal("no down line")
	}
	if !strings.Contains(h.logs.String(), "[wrapper "+a[:8]+"] [INFO] token cache loaded (dev=abcdef music=ghijkl)") {
		t.Fatal("lite's lines are not relayed with the prefix")
	}
}

func TestFailingStartsBackOff(t *testing.T) {
	id := instanceID("broken@x")
	h := newHarnessSeeded(t, []regEntry{{ID: id}}, func(data string) { seedAccount(t, data, id, "us", "START_FAIL") },
		func(m *Manager) { m.restartMin, m.restartMax = 100*time.Millisecond, 400*time.Millisecond })
	time.Sleep(1500 * time.Millisecond)
	// 100+200+400+400... ms: about 5 tries in 1.5 s, not hundreds.
	if n := strings.Count(h.logs.String(), "did not start"); n < 2 || n > 8 {
		t.Fatalf("%d starts in 1.5 s", n)
	}
	_ = os.Remove(filepath.Join(h.accountDir(id), "START_FAIL"))
	waitFor(t, 5*time.Second, "up", func() bool { return h.status()["ready"] == true })
}

func TestDeadSessionIsNotRouted(t *testing.T) {
	a, b := instanceID("a@x"), instanceID("b@x")
	h := newHarnessSeeded(t, []regEntry{{ID: a, Name: "a@x"}, {ID: b, Name: "b@x"}}, func(data string) {
		seedAccount(t, data, a, "us")
		seedAccount(t, data, b, "us")
	})
	waitFor(t, 5*time.Second, "ready", func() bool { return h.status()["ready"] == true })
	say := func(id, line string) { h.lite(id, "/fake/say?line="+url.QueryEscape(line)) }

	say(a, "[INFO] credentialHandler: {title: Sign In, message: , 2FA: false}")
	waitFor(t, 5*time.Second, "a dead", func() bool { ids := h.readyIDs(); return len(ids) == 1 && ids[0] == b })
	for range 10 {
		_, env := h.call("GET", "/m3u8?adamId=1", nil)
		if env["data"].(map[string]any)["who"] != b {
			t.Fatalf("routed to the dead account: %v", env)
		}
	}
	if status, _ := h.call("GET", "/token?instance="+a, nil); status != 503 {
		t.Fatal("token of a dead account")
	}
	if st := h.status(); st["clientCount"] != float64(2) || st["ready"] != true {
		t.Fatalf("%v", st)
	}

	// A refresh that gets a new token brings it back.
	say(a, "[INFO] background token refresh starting")
	say(a, "[INFO] background token refresh completed")
	waitFor(t, 5*time.Second, "a back", func() bool { return len(h.readyIDs()) == 2 })

	// Two refreshes in a row without a new token take it out.
	say(b, "[INFO] background token refresh starting")
	say(b, "[WARN] createMusicToken error")
	say(b, "[WARN] using cached MUSIC_TOKEN (len=10)")
	say(b, "[INFO] background token refresh completed")
	if len(h.readyIDs()) != 2 {
		t.Fatal("one refresh without a new token took the account out")
	}
	// What lite prints when it cannot even ask Apple: only the fallback.
	say(b, "[INFO] background token refresh starting")
	say(b, "[WARN] using cached MUSIC_TOKEN (len=10)")
	say(b, "[INFO] background token refresh completed")
	waitFor(t, 5*time.Second, "b dead", func() bool { ids := h.readyIDs(); return len(ids) == 1 && ids[0] == a })

	// A dead account can log in again.
	if env := h.login("b@x"); env["code"] != float64(0) {
		t.Fatalf("relogin: %v", env)
	}
	waitFor(t, 5*time.Second, "both", func() bool { return len(h.readyIDs()) == 2 })
}

func TestStopAllTerminatesServices(t *testing.T) {
	id := instanceID("a@x")
	h := newHarnessSeeded(t, []regEntry{{ID: id}}, func(data string) { seedAccount(t, data, id, "us", "SPAWN") })
	waitFor(t, 5*time.Second, "ready", func() bool { return h.status()["ready"] == true })
	h.m.mu.Lock()
	p := h.m.accounts[0].proc
	h.m.mu.Unlock()
	raw, _ := os.ReadFile(filepath.Join(h.accountDir(id), "child.pid"))
	start := time.Now()
	h.m.stopAll(time.Second)
	if !p.exited() || time.Since(start) > 3*time.Second {
		t.Fatalf("exited %v after %s", p.exited(), time.Since(start))
	}
	pid := 0
	for _, c := range string(raw) {
		pid = pid*10 + int(c-'0')
	}
	if !gone(pid) {
		t.Fatal("a group member survived")
	}
	time.Sleep(300 * time.Millisecond)
	if p := h.port(id); p != 0 {
		t.Fatal("restarted after the stop")
	}
}

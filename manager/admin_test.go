// Copyright (c) 2026 AMDL-Web contributors. SPDX-License-Identifier: MIT

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func (h *harness) admin() *httptest.Server {
	s := httptest.NewServer(h.m.adminServer().Handler)
	h.t.Cleanup(s.Close)
	return s
}

func snapshot(t *testing.T, h *harness) []byte {
	t.Helper()
	resp, err := http.Get(h.admin().URL + "/snapshot")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Type") != "application/gzip" || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("headers %v", resp.Header)
	}
	raw, _ := io.ReadAll(resp.Body)
	return raw
}

func restore(t *testing.T, h *harness, archive []byte) map[string]any {
	t.Helper()
	resp, err := http.Post(h.admin().URL+"/restore", "application/gzip", bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var env map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&env)
	return env
}

type tarEntry struct {
	name string
	kind byte
	body string
	link string
}

func makeArchive(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Typeflag: e.kind, Mode: 0o666, Size: int64(len(e.body)), Linkname: e.link, ModTime: time.Unix(1761000000, 0)}
		if e.kind != tar.TypeReg {
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(tw, e.body)
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

// tree lists the files under dir with their content and mtime.
func tree(t *testing.T, dir string) map[string]string {
	out := map[string]string{}
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		info, _ := d.Info()
		raw, _ := os.ReadFile(p)
		rel, _ := filepath.Rel(dir, p)
		out[rel] = string(raw) + "@" + info.ModTime().UTC().Format(time.RFC3339Nano)
		return nil
	})
	return out
}

func TestSnapshotAndRestore(t *testing.T) {
	a, b := instanceID("a@x"), instanceID("b@x")
	unlisted := instanceID("gone@x")
	mtime := time.Unix(1700000000, 0)
	src := newHarnessSeeded(t, []regEntry{{ID: a, Name: "a@x"}, {ID: b}}, func(data string) {
		seedAccount(t, data, a, "us")
		seedAccount(t, data, b, "jp")
		seedAccount(t, data, unlisted, "us")
		dir := filepath.Join(data, "wrapper", "rootfs", "data", "instances", a)
		_ = os.MkdirAll(filepath.Join(dir, "mpl_db"), 0o755)
		_ = os.WriteFile(filepath.Join(dir, "mpl_db", "x.db"), []byte{0, 1, 2}, 0o644)
		_ = os.WriteFile(filepath.Join(dir, "token_cache.json.tmp"), []byte("tmp"), 0o644)
		_ = os.WriteFile(filepath.Join(dir, "2fa.txt"), []byte("123456"), 0o644)
		_ = os.Symlink("/etc/passwd", filepath.Join(dir, "link"))
		_ = os.Chtimes(filepath.Join(dir, "MUSIC_TOKEN"), mtime, mtime)
	})
	waitFor(t, 5*time.Second, "ready", func() bool { return src.status()["ready"] == true })
	archive := snapshot(t, src)

	// The archive: the registry first, then only the listed accounts.
	zr, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
		if h.Typeflag == tar.TypeDir && (h.Mode != 0o777 || !strings.HasSuffix(h.Name, "/")) ||
			h.Typeflag == tar.TypeReg && h.Mode != 0o666 {
			t.Errorf("%s: mode %o", h.Name, h.Mode)
		}
	}
	if names[0] != registryEntry {
		t.Fatalf("first entry %s", names[0])
	}
	joined := strings.Join(names, "\n")
	for _, bad := range []string{unlisted, ".tmp", "2fa.txt", "/link"} {
		if strings.Contains(joined, bad) {
			t.Fatalf("archive holds %s:\n%s", bad, joined)
		}
	}
	if !strings.Contains(joined, accountsPrefix+a+"/\n") || !strings.Contains(joined, accountsPrefix+a+"/mpl_db/x.db") {
		t.Fatalf("archive:\n%s", joined)
	}

	dst := newHarness(t, nil)
	env := restore(t, dst, archive)
	if env["code"] != float64(0) || env["data"].(map[string]any)["restored"] != float64(2) || env["data"].(map[string]any)["ready"] != float64(2) {
		t.Fatalf("%v", env)
	}
	for _, id := range []string{a, b} {
		want := tree(t, src.accountDir(id))
		delete(want, "token_cache.json.tmp")
		delete(want, "2fa.txt")
		delete(want, "service-args")
		got := tree(t, dst.accountDir(id))
		delete(got, "service-args")
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s/%s: %q, want %q", id[:8], k, got[k], v)
			}
		}
	}
	if _, err := os.Stat(dst.accountDir(unlisted)); err == nil {
		t.Fatal("an unlisted account was restored")
	}
	st := dst.status()
	if st["clientCount"] != float64(2) || st["instances"].([]any)[0].(map[string]any)["name"] != "a@x" {
		t.Fatalf("%v", st)
	}
	if _, err := os.Stat(filepath.Join(dst.data, ".restore")); err == nil {
		t.Fatal("staging left")
	}
	if env := restore(t, dst, archive); env["code"] != float64(-1) || !strings.Contains(env["msg"].(string), "already") {
		t.Fatalf("restore into a non-empty manager: %v", env)
	}
}

func TestRestoreFixtures(t *testing.T) {
	for _, name := range []string{"snapshot-ustar.tar.gz", "snapshot-pax.tar.gz"} {
		t.Run(name, func(t *testing.T) {
			archive, err := os.ReadFile(filepath.Join("testdata", name))
			if err != nil {
				t.Fatal(err)
			}
			h := newHarness(t, nil)
			env := restore(t, h, archive)
			if env["code"] != float64(0) || env["data"].(map[string]any)["restored"] != float64(2) {
				t.Fatalf("%v", env)
			}
			a := "b193ebc0-ca20-50a7-b158-6a8111b2a0f3"
			st, err := os.Stat(filepath.Join(h.accountDir(a), "MUSIC_TOKEN"))
			if err != nil || st.ModTime().Unix() != 1761000002 {
				t.Fatalf("%v %v", st, err)
			}
			if raw, _ := os.ReadFile(filepath.Join(h.accountDir(a), "mpl_db", "kvs.db")); string(raw) != "\x00\x01binary" {
				t.Fatalf("%q", raw)
			}
			waitFor(t, 5*time.Second, "restored accounts up", func() bool { return len(h.readyIDs()) == 2 })
			if inst := h.status()["instances"].([]any); inst[0].(map[string]any)["name"] != "a@example.com" || inst[1].(map[string]any)["name"] != "" {
				t.Fatalf("%v", inst)
			}
		})
	}
}

func TestRestoreRefusals(t *testing.T) {
	id := instanceID("a@x")
	reg := `[{"id":"` + id + `"}]`
	ok := []tarEntry{{name: registryEntry, kind: tar.TypeReg, body: reg}, {name: accountsPrefix + id + "/MUSIC_TOKEN", kind: tar.TypeReg, body: "t"}}
	with := func(extra ...tarEntry) []byte { return makeArchive(t, append(append([]tarEntry{}, ok...), extra...)) }
	valid := with()
	cases := map[string][]byte{
		"escape":            with(tarEntry{name: "../escape", kind: tar.TypeReg, body: "x"}),
		"absolute":          with(tarEntry{name: "/" + accountsPrefix + id + "/x", kind: tar.TypeReg, body: "x"}),
		"payload path":      with(tarEntry{name: "data/wrapper/rootfs/system/bin/lite", kind: tar.TypeReg, body: "x"}),
		"dotdot in account": with(tarEntry{name: accountsPrefix + id + "/../" + instanceID("b") + "/x", kind: tar.TypeReg, body: "x"}),
		"non-uuid dir":      with(tarEntry{name: accountsPrefix + "not-a-uuid/x", kind: tar.TypeReg, body: "x"}),
		"upper-case uuid":   with(tarEntry{name: accountsPrefix + strings.ToUpper(id) + "/x", kind: tar.TypeReg, body: "x"}),
		"symlink":           with(tarEntry{name: accountsPrefix + id + "/link", kind: tar.TypeSymlink, link: "/etc/passwd"}),
		"broken registry":   makeArchive(t, []tarEntry{{name: registryEntry, kind: tar.TypeReg, body: `{"id":1}`}}),
		"non-uuid id":       makeArchive(t, []tarEntry{{name: registryEntry, kind: tar.TypeReg, body: `[{"id":"x"}]`}}),
		"corrupt tail":      valid[:len(valid)-6],
		"not gzip":          []byte("plain"),
	}
	for name, archive := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, nil)
			env := restore(t, h, archive)
			if env["code"] != float64(-1) || !strings.HasPrefix(env["msg"].(string), "restore failed") {
				t.Fatalf("%v", env)
			}
			if name == "broken registry" && !strings.Contains(env["msg"].(string), "registry") {
				t.Fatalf("%v", env)
			}
			for _, p := range []string{".restore", "instances.json", "wrapper/rootfs/data/instances/" + id, "escape"} {
				if _, err := os.Stat(filepath.Join(h.data, p)); err == nil {
					t.Fatalf("%s exists after a failed restore", p)
				}
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(h.data), "escape")); err == nil {
				t.Fatal("written outside data")
			}
		})
	}

	t.Run("too large", func(t *testing.T) {
		old := maxRestoreFiles
		maxRestoreFiles = 10
		defer func() { maxRestoreFiles = old }()
		h := newHarness(t, nil)
		env := restore(t, h, with(tarEntry{name: accountsPrefix + id + "/big", kind: tar.TypeReg, body: strings.Repeat("x", 20)}))
		if env["code"] != float64(-1) {
			t.Fatalf("%v", env)
		}
	})

	t.Run("empty snapshot", func(t *testing.T) {
		h := newHarness(t, nil)
		env := restore(t, h, makeArchive(t, nil))
		if env["code"] != float64(0) || env["data"].(map[string]any)["restored"] != float64(0) || h.status()["ready"] != true {
			t.Fatalf("%v", env)
		}
		empty := snapshot(t, h)
		zr, err := gzip.NewReader(bytes.NewReader(empty))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tar.NewReader(zr).Next(); err != io.EOF {
			t.Fatalf("snapshot of an empty manager: %v", err)
		}
	})
}

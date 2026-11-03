// Copyright (c) 2026 AMDL-Web contributors. SPDX-License-Identifier: MIT

package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func makeZip(t *testing.T, files map[string]string) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(w, body)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(buf.Bytes())
	return buf.Bytes(), hex.EncodeToString(sum[:])
}

func payloadFiles(version string) map[string]string {
	return map[string]string{
		"wrapper-lite-rootless":       "#!/bin/sh\n# " + version,
		"wrapper-lite":                "#!/bin/sh\n# " + version,
		"rootfs/system/bin/lite":      "lite " + version,
		"rootfs/system/bin/linker64":  "linker",
		"rootfs/system/lib64/libx.so": "lib",
		"README.md":                   "readme",
	}
}

// zipServer serves zips by path and counts requests; fail answers 503 that
// many times first.
type zipServer struct {
	*httptest.Server
	zips     map[string][]byte
	requests atomic.Int32
	fail     atomic.Int32
}

func newZipServer(t *testing.T, zips map[string][]byte) *zipServer {
	z := &zipServer{zips: zips}
	z.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		z.requests.Add(1)
		if z.fail.Add(-1) >= 0 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		body, ok := z.zips[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(z.Close)
	return z
}

type payloadRun struct {
	p      *payload
	logs   *syncBuffer
	delays []time.Duration
}

func testPayload(t *testing.T, data, url, digest string) *payloadRun {
	logs := &syncBuffer{}
	p := newPayload(config{dataDir: data, launcher: "rootless", payloadURL: url, payloadSHA256: digest}, log.New(logs, "", 0))
	r := &payloadRun{p: p, logs: logs}
	p.sleep = func(_ context.Context, d time.Duration) error {
		r.delays = append(r.delays, d)
		return nil
	}
	return r
}

var installPhrases = []string{"downloading wrapper-lite", "wrapper-lite does not exist", "installed from another source"}

func assertNoInstall(t *testing.T, r *payloadRun) {
	t.Helper()
	for _, s := range installPhrases {
		if strings.Contains(r.logs.String(), s) {
			t.Fatalf("a ready payload was installed again:\n%s", r.logs.String())
		}
	}
}

func TestPayloadInstallAndReuse(t *testing.T) {
	v1, d1 := makeZip(t, payloadFiles("v1"))
	v2, d2 := makeZip(t, payloadFiles("v2"))
	srv := newZipServer(t, map[string][]byte{"/a/p.zip": v1, "/b/p.zip": v1, "/c/p.zip": v2})
	data := filepath.Join(t.TempDir(), "data")
	account := filepath.Join(data, "wrapper", "rootfs", "data", "instances", instanceID("x"), "MUSIC_TOKEN")

	r := testPayload(t, data, srv.URL+"/a/p.zip", d1)
	if err := r.p.ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"wrapper-lite does not exist", "downloading wrapper-lite from " + srv.URL + "/a/p.zip", "wrapper-lite ready"} {
		if !strings.Contains(r.logs.String(), s) {
			t.Fatalf("no %q in\n%s", s, r.logs.String())
		}
	}
	rec, _ := os.ReadFile(filepath.Join(data, "wrapper", ".payload-source"))
	if string(rec) != srv.URL+"/a/p.zip#sha256="+d1+"\n" {
		t.Fatalf("record %q", rec)
	}
	for _, f := range []string{"wrapper-lite-rootless", "wrapper-lite", "rootfs/system/bin/lite", "rootfs/system/bin/linker64"} {
		st, err := os.Stat(filepath.Join(data, "wrapper", f))
		if err != nil || st.Mode().Perm()&0o100 == 0 {
			t.Fatalf("%s: %v %v", f, st, err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(account), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(account, []byte("keep"), 0o644)

	// The next boots: same digest, from the same or another URL.
	for _, u := range []string{"/a/p.zip", "/b/p.zip"} {
		r := testPayload(t, data, srv.URL+u, d1)
		if err := r.p.ensure(context.Background()); err != nil {
			t.Fatal(err)
		}
		assertNoInstall(t, r)
	}
	// A disk installed under an older URL spelling.
	_ = os.WriteFile(filepath.Join(data, "wrapper", ".payload-source"), []byte("http://10.0.2.2:41000/tok/wrapper-lite-linux-x86_64.zip#sha256="+d1+"\n"), 0o644)
	r = testPayload(t, data, srv.URL+"/b/p.zip", d1)
	if err := r.p.ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertNoInstall(t, r)

	// Another digest reinstalls; accounts stay.
	r = testPayload(t, data, srv.URL+"/c/p.zip", d2)
	if err := r.p.ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.logs.String(), "installed from another source") {
		t.Fatalf("%s", r.logs.String())
	}
	if raw, _ := os.ReadFile(filepath.Join(data, "wrapper", liteInRootfs)); string(raw) != "lite v2" {
		t.Fatalf("lite %q", raw)
	}
	if raw, _ := os.ReadFile(account); string(raw) != "keep" {
		t.Fatal("the reinstall touched account data")
	}

	// Without a digest the record must equal the URL.
	r = testPayload(t, data, srv.URL+"/c/p.zip", "")
	if err := r.p.ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.logs.String(), "installed from another source") {
		t.Fatal("a digest record is not the URL's")
	}
	r = testPayload(t, data, srv.URL+"/c/p.zip", "")
	if err := r.p.ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertNoInstall(t, r)
	if _, err := os.Stat(filepath.Join(data, "wrapper-lite-amd64.zip")); err == nil {
		t.Fatal("a zip is left")
	}
}

func TestPayloadWithoutRecordIsReplaced(t *testing.T) {
	v1, d1 := makeZip(t, payloadFiles("v1"))
	srv := newZipServer(t, map[string][]byte{"/p.zip": v1})
	data := filepath.Join(t.TempDir(), "data")
	installFakePayload(t, data)
	r := testPayload(t, data, srv.URL+"/p.zip", d1)
	if err := r.p.ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.logs.String(), "installed from another source") || srv.requests.Load() != 1 {
		t.Fatalf("%s", r.logs.String())
	}
}

func TestPayloadDigestMismatch(t *testing.T) {
	v1, _ := makeZip(t, payloadFiles("v1"))
	srv := newZipServer(t, map[string][]byte{"/p.zip": v1})
	wrong := strings.Repeat("0", 64)

	data := filepath.Join(t.TempDir(), "data")
	r := testPayload(t, data, srv.URL+"/p.zip", wrong)
	err := r.p.ensure(context.Background())
	if !errors.Is(err, errDigest) || !strings.Contains(err.Error(), wrong) || srv.requests.Load() != 1 || len(r.delays) != 0 {
		t.Fatalf("%v, %d requests, delays %v", err, srv.requests.Load(), r.delays)
	}
	if _, err := os.Stat(filepath.Join(data, "wrapper")); err == nil {
		t.Fatal("a mismatching payload wrote files")
	}

	// An installed payload from another source is not kept on a mismatch.
	installFakePayload(t, data)
	r = testPayload(t, data, srv.URL+"/p.zip", wrong)
	if err := r.p.ensure(context.Background()); !errors.Is(err, errDigest) {
		t.Fatalf("%v", err)
	}
}

func TestPayloadRetriesWhileNothingIsInstalled(t *testing.T) {
	v1, d1 := makeZip(t, payloadFiles("v1"))
	srv := newZipServer(t, map[string][]byte{"/p.zip": v1})
	srv.fail.Store(6)
	data := filepath.Join(t.TempDir(), "data")
	r := testPayload(t, data, srv.URL+"/p.zip", d1)
	if err := r.p.ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, 60 * time.Second, 60 * time.Second}
	if len(r.delays) != len(want) {
		t.Fatalf("delays %v", r.delays)
	}
	for i := range want {
		if r.delays[i] != want[i] {
			t.Fatalf("delays %v", r.delays)
		}
	}
	if !strings.Contains(r.logs.String(), "HTTP 503") {
		t.Fatal("the error is not logged")
	}
}

func TestPayloadKeepsAnInstalledOneOnNetworkFailure(t *testing.T) {
	srv := newZipServer(t, nil)
	data := filepath.Join(t.TempDir(), "data")
	installFakePayload(t, data)
	r := testPayload(t, data, srv.URL+"/missing.zip", strings.Repeat("a", 64))
	if err := r.p.ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.logs.String(), "keeping the installed one") || len(r.delays) != 0 {
		t.Fatalf("%s", r.logs.String())
	}
}

func TestPayloadRefusesEscapingEntries(t *testing.T) {
	files := payloadFiles("v1")
	files["../escape"] = "x"
	z, d := makeZip(t, files)
	srv := newZipServer(t, map[string][]byte{"/p.zip": z})
	root := t.TempDir()
	data := filepath.Join(root, "data")
	r := testPayload(t, data, srv.URL+"/p.zip", d)
	if err := r.p.update(context.Background()); err == nil || !strings.Contains(err.Error(), "escape") {
		t.Fatalf("%v", err)
	}
	if _, err := os.Stat(filepath.Join(data, "escape")); err == nil {
		t.Fatal("written outside")
	}

	files = payloadFiles("v1")
	files["rootfs/data/instances/x/MUSIC_TOKEN"] = "overwrite"
	z, d = makeZip(t, files)
	srv = newZipServer(t, map[string][]byte{"/p.zip": z})
	r = testPayload(t, data, srv.URL+"/p.zip", d)
	if err := r.p.update(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(data, "wrapper", "rootfs", "data", "instances", "x", "MUSIC_TOKEN")); err == nil {
		t.Fatal("the payload wrote account data")
	}
}

func TestFlagErrors(t *testing.T) {
	d := strings.Repeat("a", 64)
	for _, args := range [][]string{
		{"-payload-sha256", d},
		{"-payload-url", "http://x/p.zip", "-payload-sha256", "ABC"},
		{"-payload-url", "http://x/p.zip", "-payload-sha256", strings.ToUpper(d)},
		{"-launcher", "chroot"},
		{"-mirror", "x"},
		{"-debug"},
		{"extra"},
	} {
		var stderr bytes.Buffer
		if code := run(args, &stderr); code != 2 || stderr.Len() == 0 {
			t.Errorf("%v: exit %d, stderr %q", args, code, stderr.String())
		}
	}
	c, err := parseFlags([]string{"--host", "0.0.0.0", "--port=9", "-admin-port", "8081", "-launcher", "root", "-proxy", "http://p:1"}, io.Discard)
	if err != nil || c.host != "0.0.0.0" || c.port != 9 || c.adminPort != 8081 || c.launcher != "root" || c.proxy != "http://p:1" {
		t.Fatalf("%+v %v", c, err)
	}
	if c, _ := parseFlags(nil, io.Discard); c.host != "localhost" || c.port != 8080 || c.launcher != "rootless" || c.adminPort != 0 {
		t.Fatalf("defaults %+v", c)
	}
}

func TestPrepareAndUpdateWrapper(t *testing.T) {
	v1, d1 := makeZip(t, payloadFiles("v1"))
	srv := newZipServer(t, map[string][]byte{"/p.zip": v1})
	t.Chdir(t.TempDir())
	if code := run([]string{"-prepare", "-payload-url", srv.URL + "/p.zip", "-payload-sha256", d1}, io.Discard); code != 0 {
		t.Fatalf("prepare: %d", code)
	}
	if !fileExists(filepath.Join("data", "wrapper", "wrapper-lite-rootless")) || srv.requests.Load() != 1 {
		t.Fatal("not installed")
	}
	if code := run([]string{"-prepare", "-payload-url", srv.URL + "/p.zip", "-payload-sha256", d1}, io.Discard); code != 0 || srv.requests.Load() != 1 {
		t.Fatalf("second prepare: %d, %d requests", code, srv.requests.Load())
	}
	if code := run([]string{"-update-wrapper", "-payload-url", srv.URL + "/p.zip"}, io.Discard); code != 0 || srv.requests.Load() != 2 {
		t.Fatalf("update: %d", code)
	}
	if code := run([]string{"-update-wrapper", "-payload-url", srv.URL + "/gone.zip"}, io.Discard); code != 1 {
		t.Fatalf("failed update: %d", code)
	}
	if code := run([]string{"-payload-url", srv.URL + "/p.zip", "-payload-sha256", strings.Repeat("0", 64)}, io.Discard); code != 1 {
		t.Fatalf("mismatch at boot: %d", code)
	}
}

func TestDefaultPayloadIsPinned(t *testing.T) {
	p := newPayload(config{dataDir: t.TempDir(), launcher: "root"}, log.New(io.Discard, "", 0))
	if !strings.HasPrefix(p.url, "https://github.com/AMDL-Web/wrapper/releases/download/lite-") || !hexDigest.MatchString(p.digest) {
		t.Fatalf("%s %s", p.url, p.digest)
	}
	if p.source() != p.url+"#sha256="+p.digest || p.launcher != "wrapper-lite" {
		t.Fatal(p.source())
	}
}

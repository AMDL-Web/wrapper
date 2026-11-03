package main

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// writePayloadZip builds a zip with the launcher and lite binary laid out like
// the build-lite artifact.
func writePayloadZip(t *testing.T, path string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for _, name := range []string{"wrapper-lite-rootless", "rootfs/system/bin/lite"} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte("x"))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
}

func TestPayloadFromAnotherSourceIsNotReady(t *testing.T) {
	t.Chdir(t.TempDir())
	if wrapperPayloadInstalled() || wrapperPayloadReady() {
		t.Fatal("empty dir reported a payload")
	}

	// A payload as upstream wrapper-manager leaves it: files, no source record.
	for _, p := range []string{launcherPath(), filepath.Join(wrapperDir, "rootfs", "system", "bin", "lite")} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("old"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if !wrapperPayloadInstalled() {
		t.Fatal("installed payload not seen")
	}
	if wrapperPayloadReady() {
		t.Fatal("payload without a source record reported ready")
	}

	if err := os.WriteFile(payloadSourcePath(), []byte("https://nightly.link/WorldObservationLog/wrapper/x.zip\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if wrapperPayloadReady() {
		t.Fatal("payload from another URL reported ready")
	}

	// Account data must survive the reinstall.
	acct := filepath.Join(instanceBaseHost, "id", "MUSIC_TOKEN")
	if err := os.MkdirAll(filepath.Dir(acct), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(acct, []byte("t"), 0o600); err != nil {
		t.Fatal(err)
	}
	zipPath := filepath.Join(t.TempDir(), "p.zip")
	writePayloadZip(t, zipPath)
	if _, err := installWrapperLite(zipPath); err != nil {
		t.Fatal(err)
	}
	if !wrapperPayloadReady() {
		t.Fatal("payload not ready after install")
	}
	if _, err := os.Stat(acct); err != nil {
		t.Fatalf("account data lost: %v", err)
	}
}

// servePayload serves a payload zip and returns its URL and sha256.
func servePayload(t *testing.T) (url, sum string) {
	t.Helper()
	return servePayloadAfter(t, 0)
}

// servePayloadAfter serves the payload once failures requests have been
// answered 503.
func servePayloadAfter(t *testing.T, failures int32) (url, sum string) {
	t.Helper()
	zipPath := filepath.Join(t.TempDir(), "p.zip")
	writePayloadZip(t, zipPath)
	body, err := os.ReadFile(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	var seen atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if seen.Add(1) <= failures {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	h := sha256.Sum256(body)
	return srv.URL + "/p.zip", hex.EncodeToString(h[:])
}

func setPin(t *testing.T, url, sum string) {
	t.Helper()
	oldURL, oldSum := PayloadURL, PayloadSHA256
	PayloadURL, PayloadSHA256 = url, sum
	t.Cleanup(func() { PayloadURL, PayloadSHA256 = oldURL, oldSum })
}

func TestPayloadSHA256AcceptsTheMatchingDownload(t *testing.T) {
	t.Chdir(t.TempDir())
	url, sum := servePayload(t)
	setPin(t, url, sum)
	zipPath, err := downloadWrapperLite(false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := installWrapperLite(zipPath); err != nil {
		t.Fatal(err)
	}
	if !wrapperPayloadReady() {
		t.Fatal("payload not ready after a verified install")
	}
	// The same URL under another digest is a different pin.
	setPin(t, url, strings.Repeat("0", 64))
	if wrapperPayloadReady() {
		t.Fatal("a payload installed under another digest reported ready")
	}
}

func TestPayloadSHA256RefusesAMismatchAndInstallsNothing(t *testing.T) {
	t.Chdir(t.TempDir())
	url, _ := servePayload(t)
	setPin(t, url, strings.Repeat("0", 64))
	zipPath, err := downloadWrapperLite(false)
	if !errors.Is(err, errPayloadSHA256) || zipPath != "" {
		t.Fatalf("mismatch: %q, %v", zipPath, err)
	}
	if _, err := os.Stat(filepath.Join("data", "wrapper-lite-"+runtime.GOARCH+".zip")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the refused download was written: %v", err)
	}
	if wrapperPayloadInstalled() {
		t.Fatal("a payload was installed")
	}
}

func TestPayloadSHA256MismatchDoesNotKeepAnInstalledPayload(t *testing.T) {
	t.Chdir(t.TempDir())
	// An installed payload from another source is normally kept when the
	// replacement fails; a digest mismatch must fail instead.
	for _, p := range []string{launcherPath(), filepath.Join(wrapperDir, "rootfs", "system", "bin", "lite")} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("old"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	url, _ := servePayload(t)
	setPin(t, url, strings.Repeat("0", 64))
	if err := prepareWrapper(false); !errors.Is(err, errPayloadSHA256) {
		t.Fatalf("prepareWrapper kept the unverified payload: %v", err)
	}
	if wrapperPayloadReady() {
		t.Fatal("the unverified payload reported ready")
	}
}

func TestPrepareWrapperGivesUpOnADigestMismatch(t *testing.T) {
	t.Chdir(t.TempDir())
	url, _ := servePayload(t)
	setPin(t, url, strings.Repeat("0", 64))
	done := make(chan error, 1)
	go func() { done <- PrepareWrapper(false) }()
	select {
	case err := <-done:
		if !errors.Is(err, errPayloadSHA256) {
			t.Fatalf("PrepareWrapper: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("PrepareWrapper retried a pin that cannot match")
	}
}

func TestPrepareWrapperRetriesUntilThePayloadArrives(t *testing.T) {
	t.Chdir(t.TempDir())
	first, max := prepareRetryFirst, prepareRetryMax
	prepareRetryFirst, prepareRetryMax = time.Millisecond, 2*time.Millisecond
	t.Cleanup(func() { prepareRetryFirst, prepareRetryMax = first, max })
	url, sum := servePayloadAfter(t, 3)
	setPin(t, url, sum)
	done := make(chan struct{})
	var err error
	go func() { err = PrepareWrapper(false); close(done) }()
	select {
	case <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("PrepareWrapper did not install the payload once it was served")
	}
	if !wrapperPayloadReady() {
		t.Fatal("payload not ready after PrepareWrapper returned")
	}
}

package main

import (
	"archive/zip"
	"os"
	"path/filepath"
	"testing"
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

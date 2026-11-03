package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeTestFile(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o666); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotRoundTrip(t *testing.T) {
	inTempDir(t)
	a, b, stray := InstanceID("a@example.com"), InstanceID("b@example.com"), InstanceID("gone@example.com")
	writeTestFile(t, statePath, `[{"id":"`+a+`","region":"jp","port":1,"name":"a@example.com"},{"id":"`+b+`","region":"us","port":2,"name":"b@example.com"}]`)
	writeTestFile(t, instanceDir(a)+"/MUSIC_TOKEN", "token-a")
	writeTestFile(t, instanceDir(a)+"/mpl_db/db", "db-a")
	writeTestFile(t, instanceDir(a)+"/token_cache.json.tmp", "partial")
	writeTestFile(t, instanceDir(a)+"/2fa.txt", "123456")
	writeTestFile(t, instanceDir(b)+"/MUSIC_TOKEN", "token-b")
	writeTestFile(t, instanceDir(stray)+"/MUSIC_TOKEN", "token-gone")
	writeTestFile(t, "data/wrapper/rootfs/system/bin/lite", "payload")
	old := time.Now().Add(-6 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(instanceDir(a)+"/MUSIC_TOKEN", old, old); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := writeSnapshot(&buf); err != nil {
		t.Fatal(err)
	}

	inTempDir(t)
	n, err := extractSnapshot(&buf)
	if err != nil || n != 2 {
		t.Fatalf("extract: %d accounts, %v; want 2", n, err)
	}
	for p, want := range map[string]string{
		instanceDir(a) + "/MUSIC_TOKEN": "token-a",
		instanceDir(a) + "/mpl_db/db":   "db-a",
		instanceDir(b) + "/MUSIC_TOKEN": "token-b",
	} {
		if got, err := os.ReadFile(p); err != nil || string(got) != want {
			t.Errorf("%s = %q, %v; want %q", p, got, err, want)
		}
	}
	for _, p := range []string{instanceDir(a) + "/token_cache.json.tmp", instanceDir(a) + "/2fa.txt", instanceDir(stray), "data/wrapper/rootfs/system/bin/lite"} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s must not be in the snapshot", p)
		}
	}
	if fi, err := os.Stat(instanceDir(a) + "/MUSIC_TOKEN"); err != nil || !fi.ModTime().Equal(old) {
		t.Errorf("MUSIC_TOKEN mtime not kept: %v", fi.ModTime())
	}
	if got := LoadInstance(); len(got) != 2 || got[0].Name != "a@example.com" {
		t.Errorf("registry = %+v", got)
	}
}

func TestSnapshotWithoutAccounts(t *testing.T) {
	inTempDir(t)
	var buf bytes.Buffer
	if err := writeSnapshot(&buf); err != nil {
		t.Fatal(err)
	}
	inTempDir(t)
	if n, err := extractSnapshot(&buf); err != nil || n != 0 {
		t.Fatalf("got %d, %v; want 0 accounts", n, err)
	}
}

func tarOf(t *testing.T, hdrs ...*tar.Header) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, h := range hdrs {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Size > 0 {
			_, _ = tw.Write(bytes.Repeat([]byte("x"), int(h.Size)))
		}
	}
	_ = tw.Close()
	_ = gz.Close()
	return &buf
}

func TestRestoreRefusesForeignEntries(t *testing.T) {
	id := InstanceID("a@example.com")
	for _, h := range []*tar.Header{
		{Typeflag: tar.TypeReg, Name: "../escape", Size: 1},
		{Typeflag: tar.TypeReg, Name: "data/wrapper/rootfs/system/bin/lite", Size: 1},
		{Typeflag: tar.TypeReg, Name: instanceBaseHost + "/" + id + "/../../../../evil", Size: 1},
		{Typeflag: tar.TypeReg, Name: instanceBaseHost + "/not-a-uuid/x", Size: 1},
		{Typeflag: tar.TypeSymlink, Name: instanceBaseHost + "/" + id + "/link", Linkname: "/etc/passwd"},
	} {
		inTempDir(t)
		if _, err := extractSnapshot(tarOf(t, h)); err == nil {
			t.Errorf("%s (%c) was accepted", h.Name, h.Typeflag)
		}
		if _, err := os.Stat("escape"); err == nil {
			t.Fatal("wrote outside the data dir")
		}
	}
}

func TestRestoreNeedsAnEmptyManager(t *testing.T) {
	inTempDir(t)
	withInstances(t, &WrapperInstance{Id: "a", Region: "jp"})
	rec := httptest.NewRecorder()
	handleRestore(rec, httptest.NewRequest(http.MethodPost, "/restore", strings.NewReader("")))
	if !strings.Contains(rec.Body.String(), "already holds accounts") {
		t.Fatalf("got %s, want a refusal", rec.Body)
	}
}

func TestRestoreStartsNothingFromAnEmptySnapshot(t *testing.T) {
	inTempDir(t)
	withInstances(t)
	var buf bytes.Buffer
	if err := writeSnapshot(&buf); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	handleRestore(rec, httptest.NewRequest(http.MethodPost, "/restore", &buf))
	if !strings.Contains(rec.Body.String(), `"restored":0`) || !isReady() {
		t.Fatalf("got %s ready=%v, want 0 restored and ready", rec.Body, isReady())
	}
}

func TestRestoreKeepsOnlyListedAccounts(t *testing.T) {
	inTempDir(t)
	listed, unlisted := InstanceID("a@example.com"), InstanceID("b@example.com")
	reg := `[{"id":"` + listed + `","region":"jp","port":1,"name":"a@example.com"}]`
	snap := tarOf(t,
		&tar.Header{Typeflag: tar.TypeReg, Name: statePath, Size: int64(len(reg))},
		&tar.Header{Typeflag: tar.TypeReg, Name: instanceBaseHost + "/" + listed + "/MUSIC_TOKEN", Size: 3},
		&tar.Header{Typeflag: tar.TypeReg, Name: instanceBaseHost + "/" + unlisted + "/MUSIC_TOKEN", Size: 3},
	)
	// tarOf fills bodies with "x", so rewrite the registry entry's body.
	snap = rebuildWithBody(t, snap, statePath, reg)
	n, err := extractSnapshot(snap)
	if err != nil || n != 1 {
		t.Fatalf("got %d, %v; want 1 account", n, err)
	}
	if _, err := os.Stat(instanceDir(listed) + "/MUSIC_TOKEN"); err != nil {
		t.Error("the listed account's files are missing")
	}
	if _, err := os.Stat(instanceDir(unlisted)); !os.IsNotExist(err) {
		t.Error("an account the registry does not list was restored")
	}
	if _, err := os.Stat(restoreStaging); !os.IsNotExist(err) {
		t.Error("the staging dir was left behind")
	}
}

func TestFailedRestoreLeavesNothing(t *testing.T) {
	inTempDir(t)
	id := InstanceID("a@example.com")
	snap := tarOf(t,
		&tar.Header{Typeflag: tar.TypeReg, Name: instanceBaseHost + "/" + id + "/MUSIC_TOKEN", Size: 3},
		&tar.Header{Typeflag: tar.TypeReg, Name: "../escape", Size: 1},
	)
	if _, err := extractSnapshot(snap); err == nil {
		t.Fatal("a bad snapshot was accepted")
	}
	for _, p := range []string{instanceDir(id), statePath, restoreStaging} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s exists after a failed restore", p)
		}
	}
}

func TestRestoreRefusesABrokenRegistry(t *testing.T) {
	inTempDir(t)
	snap := rebuildWithBody(t, tarOf(t, &tar.Header{Typeflag: tar.TypeReg, Name: statePath, Size: 5}), statePath, "[{oops")
	if _, err := extractSnapshot(snap); err == nil || !strings.Contains(err.Error(), "registry") {
		t.Fatalf("got %v, want a registry error", err)
	}
}

func TestRestoreCapsUnpackedSize(t *testing.T) {
	inTempDir(t)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: instanceBaseHost + "/" + InstanceID("a@example.com") + "/big", Size: maxSnapshotBytes + 1})
	_ = gz.Close()
	if _, err := extractSnapshot(&buf); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Fatalf("got %v, want the size cap", err)
	}
}

// rebuildWithBody re-reads a test tarball and gives the entry name body.
func rebuildWithBody(t *testing.T, src *bytes.Buffer, name, body string) *bytes.Buffer {
	t.Helper()
	gr, err := gzip.NewReader(src)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gr)
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	tw := tar.NewWriter(gz)
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		data := make([]byte, h.Size)
		_, _ = io.ReadFull(tr, data)
		if h.Name == name {
			data = []byte(body)
			h.Size = int64(len(data))
		}
		_ = tw.WriteHeader(h)
		_, _ = tw.Write(data)
	}
	_ = tw.Close()
	_ = gz.Close()
	return &out
}

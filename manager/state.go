package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gofrs/uuid/v5"
	log "github.com/sirupsen/logrus"
)

// The admin API serves a host that keeps the manager's accounts somewhere
// else between runs, such as a container whose disk is lost when it sleeps.
// It listens on its own port (-admin-port, off by default), which must never
// be exposed: a snapshot holds every account's session and tokens.

// statePath is the manager registry, the first entry of every snapshot.
const statePath = "data/instances.json"

// restoreReadyTimeout bounds how long POST /restore waits for the restored
// accounts' wrapper-lite to come up.
const restoreReadyTimeout = 90 * time.Second

// writeSnapshot writes a gzipped tar of instances.json and the data dirs of
// the accounts it lists. Temporary files and a pending 2FA code are left out.
func writeSnapshot(w io.Writer) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	if err := addFileToTar(tw, statePath); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return finishTar(tw, gz)
		}
		return err
	}
	for _, inst := range LoadInstance() {
		root := instanceDir(inst.Id)
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			name := d.Name()
			if strings.HasSuffix(name, ".tmp") || name == "2fa.txt" {
				return nil
			}
			if d.IsDir() {
				return addDirToTar(tw, p)
			}
			if !d.Type().IsRegular() {
				return nil
			}
			return addFileToTar(tw, p)
		})
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return finishTar(tw, gz)
}

func finishTar(tw *tar.Writer, gz *gzip.Writer) error {
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

func addDirToTar(tw *tar.Writer, p string) error {
	return tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: filepath.ToSlash(p) + "/", Mode: 0o777, ModTime: time.Now()})
}

func addFileToTar(tw *tar.Writer, p string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	// The mtime travels with the file: wrapper-lite dates its music token by
	// MUSIC_TOKEN's mtime.
	hdr := &tar.Header{Typeflag: tar.TypeReg, Name: filepath.ToSlash(p), Mode: 0o666, Size: fi.Size(), ModTime: fi.ModTime()}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	_, err = io.CopyN(tw, f, fi.Size())
	return err
}

// snapshotEntryAllowed reports whether a snapshot entry may be written: the
// registry itself, or a path inside one account's data dir.
func snapshotEntryAllowed(name string) bool {
	if name == statePath {
		return true
	}
	if name != path.Clean(name) || strings.HasPrefix(name, "/") {
		return false
	}
	rest, ok := strings.CutPrefix(name, instanceBaseHost+"/")
	if !ok {
		return false
	}
	id, _, _ := strings.Cut(rest, "/")
	parsed, err := uuid.FromString(id)
	return err == nil && parsed.String() == id
}

// extractSnapshot unpacks a snapshot into the working directory and returns
// how many accounts its registry lists. It refuses any entry outside the
// registry and the account data dirs.
func extractSnapshot(r io.Reader) (int, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return 0, fmt.Errorf("not a gzip stream: %w", err)
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, err
		}
		name := strings.TrimSuffix(hdr.Name, "/")
		if !snapshotEntryAllowed(name) {
			return 0, fmt.Errorf("snapshot entry %q is outside the account data", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(name, 0o777); err != nil {
				return 0, err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(name), 0o777); err != nil {
				return 0, err
			}
			if err := writeSnapshotFile(name, tr, hdr.ModTime); err != nil {
				return 0, err
			}
		default:
			return 0, fmt.Errorf("snapshot entry %q is not a file or a directory", hdr.Name)
		}
	}
	return len(LoadInstance()), nil
}

func writeSnapshotFile(name string, r io.Reader, mtime time.Time) error {
	tmp := name + ".restore"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o666)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chtimes(tmp, mtime, mtime); err != nil {
		return err
	}
	return os.Rename(tmp, name)
}

// handleSnapshot implements GET /snapshot.
func handleSnapshot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteLiteError(w, "method not allowed")
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set(headerCacheControl, cacheControlNoStore)
	if err := writeSnapshot(w); err != nil {
		// The status line is gone once the stream started; a cut stream
		// fails the reader's gzip check.
		log.Errorf("snapshot failed: %v", err)
		panic(http.ErrAbortHandler)
	}
}

// handleRestore implements POST /restore: it unpacks a snapshot into a
// manager that holds no account yet, starts the accounts and answers once
// they are up.
func handleRestore(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		WriteLiteError(w, "method not allowed")
		return
	}
	if len(SnapshotInstances()) != 0 {
		WriteLiteError(w, "the manager already holds accounts")
		return
	}
	n, err := extractSnapshot(http.MaxBytesReader(w, r.Body, 512<<20))
	if err != nil {
		WriteLiteError(w, fmt.Sprintf("restore failed: %v", err))
		return
	}
	restoreInstances(func(id string) { go WrapperStart(id) })
	deadline := time.Now().Add(restoreReadyTimeout)
	for countReady() < n && time.Now().Before(deadline) {
		time.Sleep(250 * time.Millisecond)
	}
	ready := countReady()
	log.Infof("restored %d accounts, %d up", n, ready)
	WriteLiteSuccess(w, map[string]any{"restored": n, "ready": ready})
}

// processRSS returns a process's resident set in bytes, read from /proc.
func processRSS(pid int) (int64, error) {
	f, err := os.Open(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "VmRSS:"); ok {
			kb, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(v), " kB"), 10, 64)
			return kb << 10, err
		}
	}
	return 0, errors.New("no VmRSS")
}

// groupRSS sums the resident sets of every process in a process group: the
// launcher and the wrapper-lite it forks.
func groupRSS(pgid int) int64 {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	var total int64
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if g, err := syscall.Getpgid(pid); err != nil || g != pgid {
			continue
		}
		if rss, err := processRSS(pid); err == nil {
			total += rss
		}
	}
	return total
}

// handleStats implements GET /stats: the manager's resident set and each
// account's (its launcher and wrapper-lite together), in bytes.
func handleStats(w http.ResponseWriter, r *http.Request) {
	self, _ := processRSS(os.Getpid())
	accounts := make([]map[string]any, 0)
	for _, inst := range SnapshotInstances() {
		var rss int64
		if inst.Cmd != nil && inst.Cmd.Process != nil {
			rss = groupRSS(inst.Cmd.Process.Pid)
		}
		accounts = append(accounts, map[string]any{"id": inst.Id, "region": inst.Region, "rss": rss})
	}
	WriteLiteSuccess(w, map[string]any{"manager_rss": self, "accounts": accounts})
}

// adminMux serves the admin API.
func adminMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/snapshot", handleSnapshot)
	mux.HandleFunc("/restore", handleRestore)
	mux.HandleFunc("/stats", handleStats)
	return mux
}

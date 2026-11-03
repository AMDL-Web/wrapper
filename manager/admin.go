// Copyright (c) 2026 AMDL-Web contributors. SPDX-License-Identifier: MIT

package main

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The admin API backs accounts up and restores them into a fresh manager.
// It is unauthenticated and its port must never be exposed.
const (
	registryEntry  = "data/instances.json"
	accountsPrefix = "data/wrapper/rootfs/data/instances/"
	maxRestoreBody = 512 << 20
	restoreWait    = 90 * time.Second
)

// maxRestoreFiles caps the bytes of all files a restore unpacks.
var maxRestoreFiles int64 = 1 << 30

var restoreMu sync.Mutex

func (m *Manager) adminServer() *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/snapshot", m.handleSnapshot)
	mux.HandleFunc("/restore", m.handleRestore)
	return &http.Server{Handler: noStore(mux), ReadTimeout: 5 * time.Minute, WriteTimeout: 10 * time.Minute}
}

// handleSnapshot streams a gzip-compressed tar of the registry and the
// directories of the accounts it lists.
func (m *Manager) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		failure("method not allowed").write(w)
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.WriteHeader(http.StatusOK)
	if err := m.writeSnapshot(w); err != nil {
		m.logger.Printf("snapshot failed: %v", err)
		panic(http.ErrAbortHandler) // the reader's gzip check fails
	}
}

func (m *Manager) writeSnapshot(w io.Writer) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	raw, err := os.ReadFile(m.registryPath)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return err
	default:
		st, err := os.Stat(m.registryPath)
		if err != nil {
			return err
		}
		if err := writeTarFile(tw, registryEntry, raw, st.ModTime()); err != nil {
			return err
		}
		entries, err := parseRegistry(raw)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if isUUID(e.ID) {
				if err := m.tarAccount(tw, e.ID); err != nil {
					return err
				}
			}
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

func writeTarFile(tw *tar.Writer, name string, data []byte, mtime time.Time) error {
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o666, Size: int64(len(data)), ModTime: mtime, Format: tar.FormatPAX}); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}

// tarAccount adds an account directory: directories and regular files with
// their modification times, without temporary files and 2fa.txt. PAX keeps
// the times exact; lite dates the music token by MUSIC_TOKEN's.
func (m *Manager) tarAccount(tw *tar.Writer, id string) error {
	root := m.accountDir(id)
	if _, err := os.Lstat(root); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		name := accountsPrefix + id
		if rel != "." {
			name += "/" + filepath.ToSlash(rel)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: name + "/", Mode: 0o777, ModTime: info.ModTime(), Format: tar.FormatPAX})
		case !info.Mode().IsRegular(), strings.HasSuffix(d.Name(), ".tmp"), d.Name() == "2fa.txt":
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o666, Size: info.Size(), ModTime: info.ModTime(), Format: tar.FormatPAX}); err != nil {
			return err
		}
		_, err = io.CopyN(tw, f, info.Size())
		return err
	})
}

func (m *Manager) handleRestore(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		failure("method not allowed").write(w)
		return
	}
	restoreMu.Lock()
	defer restoreMu.Unlock()
	m.mu.Lock()
	busy := len(m.accounts) > 0 || len(m.logins) > 0
	m.mu.Unlock()
	if busy {
		failure("restore refused: the manager already holds accounts").write(w)
		return
	}
	entries, err := m.restore(http.MaxBytesReader(w, r.Body, maxRestoreBody))
	if err != nil {
		m.logger.Printf("restore failed: %v", err)
		failure("restore failed: " + err.Error()).write(w)
		return
	}
	accounts := m.load(entries, false)
	deadline := time.Now().Add(restoreWait)
	ready := 0
	for {
		ready = 0
		m.mu.Lock()
		for _, a := range accounts {
			if a.ready() {
				ready++
			}
		}
		m.mu.Unlock()
		if ready == len(accounts) || time.Now().After(deadline) {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	m.logger.Printf("restored %d account(s), %d up", len(entries), ready)
	success(map[string]any{"restored": len(entries), "ready": ready}).write(w)
}

// restore unpacks a snapshot into data/.restore, checks it and moves the
// accounts the registry lists into place, then the registry. It returns
// the restored registry; without one nothing is restored.
func (m *Manager) restore(body io.Reader) ([]regEntry, error) {
	stage := filepath.Join(m.cfg.dataDir, ".restore")
	if err := os.RemoveAll(stage); err != nil {
		return nil, err
	}
	defer os.RemoveAll(stage)
	if err := os.MkdirAll(stage, 0o777); err != nil {
		return nil, err
	}
	gz, err := gzip.NewReader(body)
	if err != nil {
		return nil, fmt.Errorf("not a gzip stream: %w", err)
	}
	tr := tar.NewReader(gz)
	var total int64
	hasRegistry := false
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading the archive: %w", err)
		}
		name := strings.TrimSuffix(h.Name, "/")
		if err := checkEntryName(name); err != nil {
			return nil, err
		}
		dst := filepath.Join(stage, filepath.FromSlash(name))
		switch h.Typeflag {
		case tar.TypeDir:
			if name == registryEntry {
				return nil, fmt.Errorf("%s is a directory", name)
			}
			if err := os.MkdirAll(dst, 0o777); err != nil {
				return nil, err
			}
		case tar.TypeReg, '\x00':
			n, err := writeStaged(dst, tr, maxRestoreFiles-total)
			total += n
			if err != nil {
				return nil, err
			}
			if err := os.Chtimes(dst, h.ModTime, h.ModTime); err != nil {
				return nil, err
			}
			hasRegistry = hasRegistry || name == registryEntry
		default:
			return nil, fmt.Errorf("%s is not a file or directory", h.Name)
		}
	}
	if _, err := io.Copy(io.Discard, gz); err != nil {
		return nil, fmt.Errorf("the gzip stream is corrupt: %w", err)
	}
	if !hasRegistry {
		return nil, nil
	}
	raw, err := os.ReadFile(filepath.Join(stage, filepath.FromSlash(registryEntry)))
	if err != nil {
		return nil, err
	}
	entries, err := parseRegistry(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid registry: %w", err)
	}
	for _, e := range entries {
		if !isUUID(e.ID) {
			return nil, fmt.Errorf("the registry lists %q, which is not a UUID", e.ID)
		}
	}
	if err := os.MkdirAll(m.instancesDir, 0o777); err != nil {
		return nil, err
	}
	for _, e := range entries {
		staged := filepath.Join(stage, filepath.FromSlash(accountsPrefix+e.ID))
		if st, err := os.Stat(staged); err != nil || !st.IsDir() {
			continue
		}
		if err := os.RemoveAll(m.accountDir(e.ID)); err != nil {
			return nil, err
		}
		if err := os.Rename(staged, m.accountDir(e.ID)); err != nil {
			return nil, err
		}
	}
	syncDisks()
	if err := writeFileDurably(m.registryPath, raw); err != nil {
		return nil, err
	}
	return entries, nil
}

// checkEntryName accepts the registry and clean paths inside an account
// directory named by a canonical UUID.
func checkEntryName(name string) error {
	if name == registryEntry {
		return nil
	}
	rest, ok := strings.CutPrefix(name, accountsPrefix)
	id, _, _ := strings.Cut(rest, "/")
	if !ok || !isUUID(id) || path.IsAbs(name) || path.Clean(name) != name || strings.Contains("/"+name+"/", "/../") {
		return fmt.Errorf("the archive holds %q, which is neither the registry nor inside an account directory", name)
	}
	return nil
}

// writeStaged copies one file of at most limit bytes.
func writeStaged(dst string, r io.Reader, limit int64) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o777); err != nil {
		return 0, err
	}
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o666)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(f, io.LimitReader(r, limit+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > limit {
		err = fmt.Errorf("the archive unpacks to more than %d bytes", maxRestoreFiles)
	}
	return n, err
}

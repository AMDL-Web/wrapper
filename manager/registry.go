// Copyright (c) 2026 AMDL-Web contributors. SPDX-License-Identifier: MIT

package main

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
)

// instanceID is the account's id: a UUID version 5 over the nil namespace of
// the username exactly as sent. Existing data disks name account directories
// by it.
func instanceID(username string) string {
	h := sha1.New()
	h.Write(make([]byte, 16)) // the nil namespace
	h.Write([]byte(username))
	b := h.Sum(nil)[:16]
	b[6] = b[6]&0x0f | 0x50
	b[8] = b[8]&0x3f | 0x80
	s := hex.EncodeToString(b)
	return s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:]
}

var uuidText = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// isUUID reports whether s is a UUID in canonical lowercase text.
func isUUID(s string) bool { return uuidText.MatchString(s) }

// regEntry is one account in data/instances.json. Region and port are
// written for readers of the file and ignored on read.
type regEntry struct {
	ID     string `json:"id"`
	Region string `json:"region"`
	Port   int    `json:"port"`
	Name   string `json:"name"`
}

// errCorrupt means the registry exists but is not a JSON array of accounts.
var errCorrupt = errors.New("the account registry is not a JSON array")

// readRegistry reads the registry at path; a missing file is no accounts.
func readRegistry(path string) ([]regEntry, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return parseRegistry(raw)
}

// parseRegistry decodes a registry file's content.
func parseRegistry(raw []byte) ([]regEntry, error) {
	var entries []regEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("%w: %v", errCorrupt, err)
	}
	if entries == nil {
		return nil, fmt.Errorf("%w: null", errCorrupt)
	}
	return entries, nil
}

// writeRegistry replaces the registry at path atomically and durably.
func writeRegistry(path string, entries []regEntry) error {
	if entries == nil {
		entries = []regEntry{}
	}
	raw, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	return writeFileDurably(path, append(raw, '\n'))
}

// writeFileDurably writes data next to path, syncs it, renames it over path
// and syncs the directory, so a crash leaves the old or the new content.
func writeFileDurably(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o777)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return syncDir(filepath.Dir(path))
}

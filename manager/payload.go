// Copyright (c) 2026 AMDL-Web contributors. SPDX-License-Identifier: MIT

package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// The default payload: a wrapper-lite release asset this repository's
// publish workflow made, pinned with its digest. Release assets never change,
// so the digest holds. Override at build time with
// -ldflags "-X main.defaultPayloadURL=... -X main.defaultPayloadSHA256=...".
var (
	defaultPayloadURL    = "https://github.com/AMDL-Web/wrapper/releases/download/lite-8885e67ac96b/wrapper-lite-linux-x86_64.zip"
	defaultPayloadSHA256 = "10e16cdce9ee2648e1bfcbd3b8b138737bb90041e7a27e7971830c6a6aefe290"
)

const (
	payloadRecord     = ".payload-source"
	liteInRootfs      = "rootfs/system/bin/lite"
	downloadTimeout   = 30 * time.Minute
	maxPayloadBytes   = 1 << 30
	firstInstallDelay = 5 * time.Second
	maxInstallDelay   = 60 * time.Second
)

// payload installs the wrapper-lite payload zip into data/wrapper/.
type payload struct {
	dir      string // data/wrapper
	dataDir  string
	launcher string // the launcher's file name
	url      string
	digest   string
	// acceptInstalled takes whatever is installed as ready: no default
	// payload exists for this architecture.
	acceptInstalled bool
	client          *http.Client
	logger          *log.Logger
	sleep           func(context.Context, time.Duration) error
}

// errDigest means the download does not match the pinned digest.
var errDigest = errors.New("the payload's sha256 does not match")

func newPayload(cfg config, logger *log.Logger) *payload {
	p := &payload{
		dir:      filepath.Join(cfg.dataDir, "wrapper"),
		dataDir:  cfg.dataDir,
		launcher: launcherName(cfg.launcher),
		url:      cfg.payloadURL,
		digest:   cfg.payloadSHA256,
		client:   &http.Client{Timeout: downloadTimeout, Transport: outboundTransport(cfg.proxy)},
		logger:   logger,
		sleep:    sleepCtx,
	}
	if p.url == "" {
		p.url, p.digest = defaultPayloadURL, defaultPayloadSHA256
	}
	return p
}

// launcherName is the payload's launcher for -launcher.
func launcherName(kind string) string {
	if kind == "root" {
		return "wrapper-lite"
	}
	return "wrapper-lite-rootless"
}

// source is what the record of an install from this run says.
func (p *payload) source() string {
	if p.digest == "" {
		return p.url
	}
	return p.url + "#sha256=" + p.digest
}

// installed reports whether the launcher and lite are in place.
func (p *payload) installed() bool {
	for _, f := range []string{p.launcher, liteInRootfs} {
		if st, err := os.Stat(filepath.Join(p.dir, f)); err != nil || !st.Mode().IsRegular() {
			return false
		}
	}
	return true
}

// ready reports whether the installed payload is the one this run wants.
// With a digest only the digest counts: the URL may change between boots.
func (p *payload) ready() bool {
	if !p.installed() {
		return false
	}
	if p.acceptInstalled {
		return true
	}
	raw, err := os.ReadFile(filepath.Join(p.dir, payloadRecord))
	if err != nil {
		return false
	}
	rec := strings.TrimSpace(string(raw))
	if p.digest != "" {
		return strings.HasSuffix(rec, "#sha256="+p.digest)
	}
	return rec == p.source()
}

// ensure installs the payload unless it is ready, retrying network failures
// for as long as nothing usable is installed.
func (p *payload) ensure(ctx context.Context) error {
	if p.ready() {
		return nil
	}
	had := p.installed()
	if had {
		p.logger.Printf("wrapper-lite was installed from another source, replacing it...")
	} else {
		p.logger.Printf("wrapper-lite does not exist, downloading...")
	}
	delay := firstInstallDelay
	for {
		err := p.update(ctx)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, errDigest):
			return err
		case had && p.installed():
			p.logger.Printf("warning: installing wrapper-lite failed (%v); keeping the installed one", err)
			return nil
		}
		p.logger.Printf("installing wrapper-lite failed: %v; retrying in %s", err, delay)
		if err := p.sleep(ctx, delay); err != nil {
			return err
		}
		delay = min(2*delay, maxInstallDelay)
	}
}

// update downloads the payload, checks it and installs it, once.
func (p *payload) update(ctx context.Context) error {
	zipped, err := p.download(ctx)
	if err != nil {
		return err
	}
	if err := p.install(zipped); err != nil {
		return err
	}
	_ = os.Remove(filepath.Join(p.dataDir, "wrapper-lite-"+runtime.GOARCH+".zip")) // an older manager's leftover
	return nil
}

// download fetches the zip into memory and checks its digest.
func (p *payload) download(ctx context.Context) ([]byte, error) {
	p.logger.Printf("downloading wrapper-lite from %s ...", p.url)
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPayloadBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxPayloadBytes {
		return nil, fmt.Errorf("the payload is larger than %d bytes", maxPayloadBytes)
	}
	if p.digest != "" {
		sum := sha256.Sum256(body)
		if got := hex.EncodeToString(sum[:]); got != p.digest {
			return nil, fmt.Errorf("%w: downloaded %s, want %s", errDigest, got, p.digest)
		}
	}
	return body, nil
}

// install unpacks zipped into the payload directory and records its source
// last, so that a crash part way leaves a payload that is not ready.
func (p *payload) install(zipped []byte) error {
	zr, err := zip.NewReader(bytes.NewReader(zipped), int64(len(zipped)))
	if err != nil {
		return fmt.Errorf("the payload is not a zip: %w", err)
	}
	if err := os.MkdirAll(p.dir, 0o777); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(p.dir, payloadRecord)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, f := range zr.File {
		if err := p.extract(f); err != nil {
			return err
		}
	}
	if !p.installed() {
		return fmt.Errorf("the payload has no %s or %s", p.launcher, liteInRootfs)
	}
	for _, name := range []string{"wrapper-lite", "wrapper-lite-rootless", liteInRootfs, "rootfs/system/bin/linker64"} {
		_ = os.Chmod(filepath.Join(p.dir, name), 0o777) // a payload may lack one of them
	}
	syncDisks()
	if err := writeFileDurably(filepath.Join(p.dir, payloadRecord), []byte(p.source()+"\n")); err != nil {
		return err
	}
	lite := filepath.Join(p.dir, liteInRootfs)
	st, _ := os.Stat(lite)
	p.logger.Printf("wrapper-lite ready")
	p.logger.Printf("launcher: %s", filepath.Join(p.dir, p.launcher))
	if st != nil {
		p.logger.Printf("lite: %s (%d bytes)", lite, st.Size())
	}
	return nil
}

// extract writes one zip entry below the payload directory. Entries that
// would leave it are refused, and account data is never written.
func (p *payload) extract(f *zip.File) error {
	name := strings.TrimSuffix(f.Name, "/")
	if name == "" || strings.Contains(name, `\`) || !filepath.IsLocal(filepath.FromSlash(name)) {
		return fmt.Errorf("the payload entry %q leaves the payload directory", f.Name)
	}
	name = path.Clean(name)
	if name == "rootfs/data" || strings.HasPrefix(name, "rootfs/data/") {
		return nil
	}
	dst := filepath.Join(p.dir, filepath.FromSlash(name))
	mode := f.Mode()
	switch {
	case mode.IsDir():
		return os.MkdirAll(dst, 0o777)
	case !mode.IsRegular():
		return nil // links and devices: the payload ships none that lite needs
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o777); err != nil {
		return err
	}
	r, err := f.Open()
	if err != nil {
		return err
	}
	defer r.Close()
	// A new file renamed into place: a running copy of the old one keeps its
	// inode, and a crash leaves no half-written file under the real name.
	tmp := dst + ".payload-tmp"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o777)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, io.LimitReader(r, maxPayloadBytes))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, dst)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("extract %s: %w", f.Name, err)
	}
	return nil
}

// sleepCtx waits d or until ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// outboundTransport is the transport for the manager's own requests to the
// internet: through proxy when set, else the environment's proxy settings.
func outboundTransport(proxy string) *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = http.ProxyFromEnvironment
	if proxy != "" {
		if u, err := url.Parse(proxy); err == nil {
			t.Proxy = http.ProxyURL(u)
		}
	}
	return t
}

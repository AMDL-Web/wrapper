// Copyright (c) 2026 AMDL-Web contributors. SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// maxAccounts caps registered accounts plus logins in flight.
const maxAccounts = 10

// account is one registered Apple Music account and its wrapper-lite service.
type account struct {
	id   string
	name string
	// slots bounds resource requests in flight to its lite; tokenSlots
	// bounds /token separately, so token polling never queues behind keys.
	slots      chan struct{}
	tokenSlots chan struct{}

	// Guarded by Manager.mu.
	region string // "" while the service is not up
	dead   bool   // the service runs but its Apple session no longer works
	// deadRegion is the region of a dead session, back if it recovers.
	deadRegion string
	port       int
	proc       *process
	run        *serviceRun
}

func newAccount(id, name string) *account {
	return &account{id: id, name: name, slots: make(chan struct{}, 2), tokenSlots: make(chan struct{}, 2)}
}

// ready reports whether requests may go to the account. Caller holds mu.
func (a *account) ready() bool { return a.region != "" && !a.dead }

func (a *account) short() string { return a.id[:8] }

// serviceRun is one supervision of an account's service.
type serviceRun struct {
	stop     chan struct{} // closed to end the supervision
	done     chan struct{} // closed once it ended and its process is gone
	graceful bool          // stop with SIGTERM first (manager shutdown)
	once     sync.Once
}

func (r *serviceRun) end(graceful bool) {
	r.once.Do(func() {
		r.graceful = graceful
		close(r.stop)
	})
}

// Manager holds the accounts and their wrapper-lite processes.
type Manager struct {
	cfg          config
	logger       *log.Logger
	wrapperDir   string // data/wrapper, the launchers' working directory
	instancesDir string // data/wrapper/rootfs/data/instances
	registryPath string
	launcherPath string
	lites        *http.Client
	catalog      *catalog
	requestStop  func()

	readyTimeout time.Duration
	pollEvery    time.Duration
	restartMin   time.Duration
	restartMax   time.Duration
	loginTimeout time.Duration
	replaceWait  time.Duration

	mu       sync.Mutex
	accounts []*account // registry order
	logins   map[string]*login
	pending  map[string]bool // persisted at boot and not up yet
	ready    bool
	ports    map[int]bool
	stopping bool

	regMu sync.Mutex // serializes registry writes
}

func newManager(cfg config, logger *log.Logger) *Manager {
	wrapperDir := filepath.Join(cfg.dataDir, "wrapper")
	return &Manager{
		cfg:          cfg,
		logger:       logger,
		wrapperDir:   wrapperDir,
		instancesDir: filepath.Join(wrapperDir, "rootfs", "data", "instances"),
		registryPath: filepath.Join(cfg.dataDir, "instances.json"),
		launcherPath: filepath.Join(wrapperDir, launcherName(cfg.launcher)),
		lites:        liteClient(),
		catalog:      newCatalog(&http.Client{Timeout: 60 * time.Second, Transport: outboundTransport(cfg.proxy)}),
		requestStop:  func() {},
		readyTimeout: 60 * time.Second,
		pollEvery:    500 * time.Millisecond,
		restartMin:   2 * time.Second,
		restartMax:   30 * time.Second,
		loginTimeout: 90 * time.Second,
		replaceWait:  75 * time.Second,
		logins:       map[string]*login{},
		pending:      map[string]bool{},
		ports:        map[int]bool{},
	}
}

// liteClient talks to the lites on loopback: never through a proxy, and idle
// connections retire before lite closes them at 30 s.
func liteClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     15 * time.Second,
	}}
}

// accountDir is the account's directory as the manager sees it.
func (m *Manager) accountDir(id string) string { return filepath.Join(m.instancesDir, id) }

// boot loads the registry and starts every account in it.
func (m *Manager) boot() {
	entries, err := readRegistry(m.registryPath)
	if errors.Is(err, errCorrupt) {
		m.logger.Printf("warning: %v; moving it to %s.corrupt and starting without accounts", err, m.registryPath)
		if err := os.Rename(m.registryPath, m.registryPath+".corrupt"); err != nil {
			m.logger.Printf("warning: %v", err)
		}
		entries = nil
	} else if err != nil {
		m.logger.Printf("warning: reading the account registry: %v; starting without accounts", err)
		entries = nil
	}
	m.load(entries, true)
}

// load registers entries and starts their services. At boot the manager is
// ready once all of them have been up once.
func (m *Manager) load(entries []regEntry, atBoot bool) []*account {
	m.mu.Lock()
	var added []*account
	for _, e := range entries {
		if !isUUID(e.ID) || m.find(e.ID) != nil {
			m.logger.Printf("warning: skipping registry entry %q", e.ID)
			continue
		}
		a := newAccount(e.ID, e.Name)
		m.accounts = append(m.accounts, a)
		added = append(added, a)
		if atBoot {
			m.pending[a.id] = true
		}
	}
	if atBoot && len(m.pending) == 0 {
		m.ready = true
	}
	for _, a := range added {
		m.supervise(a, nil)
	}
	m.mu.Unlock()
	if len(added) > 0 {
		m.logger.Printf("starting %d account(s) from the registry", len(added))
	}
	return added
}

// find returns the registered account with id. Caller holds mu.
func (m *Manager) find(id string) *account {
	for _, a := range m.accounts {
		if a.id == id {
			return a
		}
	}
	return nil
}

// supervise starts supervising a's service, with first already up when set.
// Caller holds mu.
func (m *Manager) supervise(a *account, first *process) {
	run := &serviceRun{stop: make(chan struct{}), done: make(chan struct{})}
	a.run = run
	go m.superviseLoop(a, run, first)
}

// superviseLoop keeps a's service up until run ends: it starts it, waits for
// it to exit and starts it again, backing off while starts fail.
func (m *Manager) superviseLoop(a *account, run *serviceRun, p *process) {
	defer close(run.done)
	backoff := m.restartMin
	for {
		if p == nil {
			var err error
			p, err = m.startService(a, run.stop)
			if err != nil {
				if isClosed(run.stop) {
					return
				}
				m.logger.Printf("wrapper %s did not start: %v; retrying in %s", a.short(), err, backoff)
				if !waitOrStop(run.stop, backoff) {
					return
				}
				backoff = min(2*backoff, m.restartMax)
				continue
			}
		}
		up := time.Now()
		select {
		case <-p.done:
			m.down(a, p)
			p.kill() // what the launcher left behind
			if time.Since(up) > time.Minute {
				backoff = m.restartMin
			}
			m.logger.Printf("wrapper %s is down (%s); restarting in %s", a.short(), exitText(p), backoff)
			p = nil
			if !waitOrStop(run.stop, backoff) {
				return
			}
			backoff = min(2*backoff, m.restartMax)
		case <-run.stop:
			if run.graceful {
				p.terminate(3 * time.Second)
			} else {
				p.kill()
			}
			m.down(a, p)
			return
		}
	}
}

// stopService ends a's supervision and waits until its process is gone.
func (m *Manager) stopService(a *account, graceful bool) {
	m.mu.Lock()
	run := a.run
	a.run = nil
	m.mu.Unlock()
	if run != nil {
		run.end(graceful)
		<-run.done
	}
}

// startService starts a's service and waits until its lite reports a region.
func (m *Manager) startService(a *account, stop <-chan struct{}) (*process, error) {
	port, err := m.claimPort()
	if err != nil {
		return nil, err
	}
	args := []string{"--base-dir", "/data/instances/" + a.id, "--host", "127.0.0.1", "--port", strconv.Itoa(port), "--log-level", "info"}
	if m.cfg.proxy != "" {
		args = append(args, "--proxy", m.cfg.proxy)
	}
	var p *process
	watch := &sessionWatch{m: m, a: a}
	prefix := "[wrapper " + a.short() + "] "
	p, err = startProcess(m.launcherPath, m.wrapperDir, args, func(line string) {
		m.logger.Print(prefix + line)
		watch.see(line)
	})
	if err != nil {
		m.releasePort(port)
		return nil, err
	}
	watch.setProcess(p)
	region, err := m.waitRegion(p, port, stop)
	if err != nil {
		p.kill()
		m.releasePort(port)
		return nil, err
	}
	m.mu.Lock()
	a.proc, a.port, a.region, a.dead = p, port, region, false
	m.markUp(a)
	m.mu.Unlock()
	m.logger.Printf("wrapper %s is up in region %s on port %d", a.short(), region, port)
	return p, nil
}

// waitRegion polls lite's /status until it reports a region, the process
// exits, stop closes or readyTimeout passes.
func (m *Manager) waitRegion(p *process, port int, stop <-chan struct{}) (string, error) {
	deadline := time.NewTimer(m.readyTimeout)
	defer deadline.Stop()
	for {
		if region := m.liteRegion(port); region != "" {
			return region, nil
		}
		select {
		case <-p.done:
			return "", fmt.Errorf("it exited (%s) before reporting a region", exitText(p))
		case <-stop:
			return "", errors.New("stopped")
		case <-deadline.C:
			return "", fmt.Errorf("it reported no region within %s", m.readyTimeout)
		case <-time.After(m.pollEvery):
		}
	}
}

// liteRegion is the first region lite's /status on port reports, or "".
func (m *Manager) liteRegion(port int) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(port)+"/status", nil)
	resp, err := m.lites.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var body struct {
		Code *int `json:"code"`
		Data struct {
			Regions []string `json:"regions"`
		} `json:"data"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body) != nil || body.Code == nil || *body.Code != 0 || len(body.Data.Regions) == 0 {
		return ""
	}
	return strings.TrimSpace(body.Data.Regions[0])
}

// markUp records that a came up; the manager is ready once every account
// from the boot registry has. Caller holds mu.
func (m *Manager) markUp(a *account) {
	if m.pending[a.id] {
		delete(m.pending, a.id)
		if len(m.pending) == 0 && !m.ready {
			m.ready = true
			m.logger.Printf("every account from the registry is up")
		}
	}
}

// forget drops id from the boot gate: it was logged out. Caller holds mu.
func (m *Manager) forget(id string) {
	if m.pending[id] {
		delete(m.pending, id)
		if len(m.pending) == 0 {
			m.ready = true
		}
	}
}

// down records that p, a's service, has exited.
func (m *Manager) down(a *account, p *process) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if a.proc != p {
		return
	}
	delete(m.ports, a.port)
	a.proc, a.port, a.region, a.dead, a.deadRegion = nil, 0, "", false, ""
}

// claimPort picks a free loopback port no running service of this manager
// uses.
func (m *Manager) claimPort() (int, error) {
	for range 20 {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return 0, err
		}
		port := ln.Addr().(*net.TCPAddr).Port
		ln.Close()
		m.mu.Lock()
		taken := m.ports[port]
		if !taken {
			m.ports[port] = true
		}
		m.mu.Unlock()
		if !taken {
			return port, nil
		}
	}
	return 0, errors.New("no free loopback port")
}

func (m *Manager) releasePort(port int) {
	m.mu.Lock()
	delete(m.ports, port)
	m.mu.Unlock()
}

// saveRegistry writes the registered accounts to data/instances.json.
func (m *Manager) saveRegistry() error {
	m.regMu.Lock()
	defer m.regMu.Unlock()
	m.mu.Lock()
	entries := make([]regEntry, 0, len(m.accounts))
	for _, a := range m.accounts {
		entries = append(entries, regEntry{ID: a.id, Region: a.region, Port: a.port, Name: a.name})
	}
	m.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(m.registryPath), 0o777); err != nil {
		return err
	}
	err := writeRegistry(m.registryPath, entries)
	if err != nil {
		m.logger.Printf("warning: writing the account registry: %v", err)
	}
	return err
}

// stopAll stops every service and login child, giving services grace to
// stop by themselves.
func (m *Manager) stopAll(grace time.Duration) {
	m.mu.Lock()
	m.stopping = true
	var runs []*serviceRun
	for _, a := range m.accounts {
		if a.run != nil {
			runs = append(runs, a.run)
			a.run = nil
		}
	}
	var logins []*login
	for _, l := range m.logins {
		logins = append(logins, l)
	}
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, r := range runs {
		wg.Go(func() { r.end(true); <-r.done })
	}
	for _, l := range logins {
		wg.Go(func() { l.abort(); l.waitChild(grace + time.Second) })
	}
	wg.Wait()
}

func isClosed(c <-chan struct{}) bool {
	select {
	case <-c:
		return true
	default:
		return false
	}
}

// waitOrStop waits d and reports false when stop closed first.
func waitOrStop(stop <-chan struct{}, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-stop:
		return false
	}
}

func exitText(p *process) string {
	if p.err == nil {
		return "status 0"
	}
	return p.err.Error()
}

// sessionWatch reads a service's output for signs that its Apple session
// died, and for its recovery. Lite's credential handler runs only when Apple
// wants the account to sign in again, which a service cannot do; a token
// refresh that gets no new music token from Apple (lite falls back to the
// cached one) twice in a row means the same. A refresh that does get one
// clears it.
type sessionWatch struct {
	m        *Manager
	a        *account
	mu       sync.Mutex
	p        *process
	early    []string // lines seen before the process was known
	stale    bool     // the current refresh got no new token
	failures int      // refreshes in a row without a new token
}

func (w *sessionWatch) setProcess(p *process) {
	w.mu.Lock()
	w.p = p
	early := w.early
	w.early = nil
	w.mu.Unlock()
	for _, l := range early {
		w.see(l)
	}
}

func (w *sessionWatch) see(line string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.p == nil {
		if len(w.early) < 64 {
			w.early = append(w.early, line)
		}
		return
	}
	switch {
	case strings.Contains(line, "credentialHandler:"):
		w.m.sessionDead(w.a, w.p, "Apple asked it to sign in again")
	case strings.Contains(line, "background token refresh starting"):
		w.stale = false
	case strings.Contains(line, "using cached MUSIC_TOKEN"), strings.Contains(line, "createMusicToken"),
		strings.Contains(line, "failed to get music token"):
		w.stale = true
	case strings.Contains(line, "background token refresh failed"):
		w.refreshed(false)
	case strings.Contains(line, "background token refresh completed"):
		w.refreshed(!w.stale)
	}
}

func (w *sessionWatch) refreshed(fresh bool) {
	if fresh {
		w.failures = 0
		w.m.sessionAlive(w.a, w.p)
		return
	}
	w.failures++
	if w.failures >= 2 {
		w.m.sessionDead(w.a, w.p, fmt.Sprintf("%d token refreshes in a row got no new token from Apple", w.failures))
	}
}

// sessionDead takes a out of service while p is its process.
func (m *Manager) sessionDead(a *account, p *process, why string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if a.proc != p || a.dead || a.region == "" {
		return
	}
	a.dead, a.deadRegion, a.region = true, a.region, ""
	m.logger.Printf("wrapper %s: the Apple session no longer works (%s); not routing to it until it recovers or the account logs in again", a.short(), why)
}

// sessionAlive puts a back in service after its session worked again.
func (m *Manager) sessionAlive(a *account, p *process) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if a.proc != p || !a.dead {
		return
	}
	a.dead, a.region, a.deadRegion = false, a.deadRegion, ""
	m.logger.Printf("wrapper %s: the Apple session works again", a.short())
}

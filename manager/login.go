// Copyright (c) 2026 AMDL-Web contributors. SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Replies amdl tells apart by their text.
const (
	msgAlreadyLogin  = "already login"
	msgLoginTimeout  = "login timeout"
	msgLoginCanceled = "login canceled"
	msgNoPending     = "no pending login for this account, restart login without code"
	msgAccountLimit  = "account limit reached: this manager holds at most 10 accounts, log one out first"
	msgNoSuchAccount = "no such account"
)

// cancelWait bounds how long a cancelled login request waits for its child
// to be gone.
const cancelWait = 10 * time.Second

// login is one wrapper-lite login child for one account id.
type login struct {
	id       string
	username string
	dir      string
	proc     *process

	prompted   chan struct{} // lite asked for the two-step code
	promptOnce sync.Once
	answered   bool // a request was told code 2; guarded by Manager.mu

	aborted   chan struct{}
	abortOnce sync.Once

	settled chan struct{} // the child is gone and its directory kept or removed
	done    chan struct{} // result is final
	result  reply
}

func newLogin(id, username, dir string) *login {
	return &login{id: id, username: username, dir: dir,
		prompted: make(chan struct{}), aborted: make(chan struct{}),
		settled: make(chan struct{}), done: make(chan struct{})}
}

// abort asks for the child to be killed. A child that already logged in
// is not undone.
func (l *login) abort() { l.abortOnce.Do(func() { close(l.aborted) }) }

// waitChild waits up to d for the child to be gone.
func (l *login) waitChild(d time.Duration) {
	select {
	case <-l.settled:
	case <-time.After(d):
	}
}

// checkCredentials refuses what lite cannot take as user:pass on one line.
func checkCredentials(username, password string) string {
	if username == "" || password == "" {
		return "missing username or password"
	}
	if strings.ContainsAny(username, ":\x00\r\n") {
		return "the username must not contain a colon or a control line break"
	}
	if strings.ContainsAny(password, "\x00\r\n") {
		return "the password must not contain a NUL or a line break"
	}
	return ""
}

// startLogin logs username in without a code: it starts a login child, or
// replaces one that is waiting, and waits for the outcome or the code prompt.
func (m *Manager) startLogin(ctx context.Context, username, password string) reply {
	id := instanceID(username)
	for {
		m.mu.Lock()
		if m.stopping {
			m.mu.Unlock()
			return failure("the manager is stopping")
		}
		if a := m.find(id); a != nil && a.ready() {
			m.mu.Unlock()
			return failure(msgAlreadyLogin)
		}
		if old := m.logins[id]; old != nil {
			m.mu.Unlock()
			old.abort()
			select {
			case <-old.done:
			case <-time.After(m.replaceWait):
				return failure("the earlier login of this account did not end; try again later")
			case <-ctx.Done():
				return failure(msgLoginCanceled)
			}
			continue
		}
		if m.countExcept(id) >= maxAccounts {
			m.mu.Unlock()
			return failure(msgAccountLimit)
		}
		l := newLogin(id, username, m.accountDir(id))
		m.logins[id] = l
		held := m.find(id)
		m.mu.Unlock()

		if held != nil {
			// Registered but not serving: its service must be gone before
			// its directory is replaced.
			m.stopService(held, false)
		}
		if err := m.beginLogin(l, password); err != nil {
			m.loginFailed(l, failure(fmt.Sprintf("login failed: %v", err)))
			return l.result
		}
		go m.runLogin(l)
		return m.waitLogin(ctx, l, true)
	}
}

// countExcept counts distinct account ids registered or logging in, other
// than id. Caller holds mu.
func (m *Manager) countExcept(id string) int {
	seen := map[string]bool{}
	for _, a := range m.accounts {
		seen[a.id] = true
	}
	for lid := range m.logins {
		seen[lid] = true
	}
	delete(seen, id)
	return len(seen)
}

// beginLogin gives the login a fresh directory and starts its child.
func (m *Manager) beginLogin(l *login, password string) error {
	if err := os.RemoveAll(l.dir); err != nil {
		return err
	}
	if err := os.MkdirAll(l.dir, 0o777); err != nil {
		return err
	}
	args := []string{"--login", l.username + ":" + password, "--code-from-file", "--base-dir", "/data/instances/" + l.id}
	if m.cfg.proxy != "" {
		args = append(args, "--proxy", m.cfg.proxy)
	}
	p, err := startProcess(m.launcherPath, m.wrapperDir, args, func(line string) {
		if strings.Contains(strings.ToLower(line), "enter your 2fa code") {
			l.promptOnce.Do(func() { close(l.prompted) })
		}
	})
	if err != nil {
		return err
	}
	m.mu.Lock()
	l.proc = p
	m.mu.Unlock()
	m.logger.Printf("login of %s started", l.id[:8])
	return nil
}

// runLogin waits for the child and settles the login: a child that left both
// token files gets its account registered and served; anything else removes
// the directory.
func (m *Manager) runLogin(l *login) {
	select {
	case <-l.proc.done:
	case <-l.aborted:
	}
	l.proc.kill()
	code := l.proc.exitCode()
	if fileExists(filepath.Join(l.dir, "STOREFRONT_ID")) && fileExists(filepath.Join(l.dir, "MUSIC_TOKEN")) {
		m.loginSucceeded(l)
		return
	}
	switch {
	case isClosed(l.aborted):
		m.loginFailed(l, failure(msgLoginCanceled))
	case code != 0:
		m.loginFailed(l, failure(fmt.Sprintf("login failed (exit code %d)", code)))
	default:
		m.loginFailed(l, failure("login failed: login process exited without caching tokens"))
	}
}

// loginFailed removes the login's directory, and the account when the login
// took over a registered one, and ends the login with r.
func (m *Manager) loginFailed(l *login, r reply) {
	if err := os.RemoveAll(l.dir); err != nil {
		m.logger.Printf("warning: removing %s: %v", l.dir, err)
	}
	m.mu.Lock()
	unregistered := false
	if a := m.find(l.id); a != nil && a.run == nil {
		m.remove(a)
		unregistered = true
	}
	m.mu.Unlock()
	close(l.settled)
	if unregistered {
		_ = m.saveRegistry()
	}
	m.logger.Printf("login of %s ended: %s", l.id[:8], r.msg)
	m.endLogin(l, r)
}

// loginSucceeded registers the account, starts its service and waits until
// it serves.
func (m *Manager) loginSucceeded(l *login) {
	syncDisks()
	run := &serviceRun{stop: make(chan struct{}), done: make(chan struct{})}
	m.mu.Lock()
	a := m.find(l.id)
	if a == nil {
		a = newAccount(l.id, l.username)
		m.accounts = append(m.accounts, a)
	}
	a.name = l.username
	a.run = run
	m.mu.Unlock()
	close(l.settled)

	p, err := m.startService(a, run.stop)
	if err != nil {
		m.mu.Lock()
		if a.run == run {
			a.run = nil
		}
		m.remove(a)
		m.mu.Unlock()
		close(run.done)
		_ = os.RemoveAll(l.dir)
		_ = m.saveRegistry()
		m.logger.Printf("login of %s: the wrapper did not come up: %v", l.id[:8], err)
		m.endLogin(l, failure(fmt.Sprintf("logged in, but the wrapper did not come up: %v", err)))
		return
	}
	go m.superviseLoop(a, run, p)
	_ = m.saveRegistry()
	m.logger.Printf("login of %s succeeded", l.id[:8])
	m.endLogin(l, success(map[string]any{"loginId": l.id}))
}

// remove takes a out of the registry in memory. Caller holds mu.
func (m *Manager) remove(a *account) {
	for i, x := range m.accounts {
		if x == a {
			m.accounts = append(m.accounts[:i:i], m.accounts[i+1:]...)
			break
		}
	}
	m.forget(a.id)
}

func (m *Manager) endLogin(l *login, r reply) {
	m.mu.Lock()
	l.result = r
	if m.logins[l.id] == l {
		delete(m.logins, l.id)
	}
	m.mu.Unlock()
	close(l.done)
}

// waitLogin waits for the login's outcome, or with mayPrompt for the code
// prompt, which only one request is told about.
func (m *Manager) waitLogin(ctx context.Context, l *login, mayPrompt bool) reply {
	timer := time.NewTimer(m.loginTimeout)
	defer timer.Stop()
	var prompted <-chan struct{}
	if mayPrompt {
		prompted = l.prompted
	}
	for {
		select {
		case <-l.done:
			return l.result
		case <-prompted:
			prompted = nil
			if isClosed(l.done) {
				return l.result
			}
			m.mu.Lock()
			first := !l.answered
			l.answered = true
			m.mu.Unlock()
			if first {
				return reply{code: 2, msg: "2fa code require", data: map[string]any{"loginId": l.id}}
			}
		case <-ctx.Done():
			l.abort()
			l.waitChild(cancelWait)
			return failure(msgLoginCanceled)
		case <-timer.C:
			l.abort()
			return failure(msgLoginTimeout)
		}
	}
}

// submitCode hands the two-step code to the login waiting for it.
func (m *Manager) submitCode(ctx context.Context, username, code string) reply {
	id := instanceID(username)
	m.mu.Lock()
	l := m.logins[id]
	m.mu.Unlock()
	if l == nil || !isClosed(l.prompted) || isClosed(l.aborted) || isClosed(l.settled) {
		return failure(msgNoPending)
	}
	if err := writeFileDurably(filepath.Join(l.dir, "2fa.txt"), []byte(strings.TrimSpace(code)+"\n")); err != nil {
		return failure(fmt.Sprintf("writing the code failed: %v", err))
	}
	return m.waitLogin(ctx, l, false)
}

// logout removes the account with id: its service, its directory and its
// registry entry.
func (m *Manager) logout(id string) error {
	m.mu.Lock()
	a, l := m.find(id), m.logins[id]
	m.mu.Unlock()
	if a == nil {
		return errNoSuchAccount
	}
	if l != nil {
		// A login taking the account over must not write into the
		// directory being removed.
		l.abort()
		l.waitChild(cancelWait)
	}
	m.mu.Lock()
	if a = m.find(id); a == nil {
		// The login it was waiting for failed and took the account away.
		m.mu.Unlock()
		return nil
	}
	m.forget(id)
	m.mu.Unlock()
	m.stopService(a, false)
	if err := os.RemoveAll(m.accountDir(id)); err != nil {
		m.logger.Printf("warning: removing %s: %v", m.accountDir(id), err)
	}
	m.mu.Lock()
	m.remove(a)
	m.mu.Unlock()
	m.logger.Printf("account %s logged out", id[:8])
	return m.saveRegistry()
}

var errNoSuchAccount = errors.New(msgNoSuchAccount)

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}

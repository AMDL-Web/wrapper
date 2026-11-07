package main

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// handleCheck has one account fetch a key from Apple, the one every song
// starts with, so the panel can tell an account that only looks ready from
// one that serves. lite asks Apple's license server on every /key.
func (m *Manager) handleCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		failure("method not allowed").write(w)
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	if _, msg := readObject(r, &body); msg != "" {
		failure(msg).write(w)
		return
	}
	id := strings.ToLower(strings.TrimSpace(body.ID))
	if !isUUID(id) {
		failure("invalid id (expected a UUID)").write(w)
		return
	}
	m.mu.Lock()
	var a *account
	for _, c := range m.accounts {
		if c.id == id {
			a = c
		}
	}
	ready := a != nil && a.ready()
	m.mu.Unlock()
	if a == nil {
		failure("no such account").write(w)
		return
	}
	if !ready {
		failure("the account is not serving").write(w)
		return
	}
	start := time.Now()
	query := url.Values{"adamId": {"0"}, "uri": {prefetchKeyURI}}.Encode()
	reply, err := m.callLite(a, a.slots, http.MethodGet, "/key", query, nil)
	if err != nil {
		failure(fmt.Sprintf("the wrapper did not answer: %v", err)).write(w)
		return
	}
	if code := liteCode(reply); code != 0 {
		failure(fmt.Sprintf("the wrapper answered code %d", code)).write(w)
		return
	}
	success(map[string]any{"id": id, "ms": time.Since(start).Milliseconds()}).write(w)
}

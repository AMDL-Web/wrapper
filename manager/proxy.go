// Copyright (c) 2026 AMDL-Web contributors. SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"
)

// Forwarding limits: lite serializes playback work and Apple throttles.
var (
	slotWait       = 15 * time.Second // wait for a free slot on one lite
	upstreamLimit  = 12 * time.Second // one forwarded request
	requestBudget  = 20 * time.Second // no new attempt after this
	maxAttempts    = 3
	prefetchKeyURI = "skd://itunes.apple.com/P000000000/s1/e1"
)

// maxLiteReply caps a reply read from lite.
const maxLiteReply = 64 << 20

var errBusy = errors.New("the wrapper is busy")

// callLite sends one request to a's lite, holding one of slots, and returns
// the body whatever lite's HTTP status. It is bounded by upstreamLimit and
// not tied to the caller's connection.
func (m *Manager) callLite(a *account, slots chan struct{}, method, path, rawQuery string, body []byte) ([]byte, error) {
	select {
	case slots <- struct{}{}:
	case <-time.After(slotWait):
		return nil, errBusy
	}
	defer func() { <-slots }()
	m.mu.Lock()
	port := a.port
	m.mu.Unlock()
	if port == 0 {
		return nil, errors.New("the wrapper is not running")
	}
	ctx, cancel := context.WithTimeout(context.Background(), upstreamLimit)
	defer cancel()
	url := "http://127.0.0.1:" + strconv.Itoa(port) + path
	if rawQuery != "" {
		url += "?" + rawQuery
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.lites.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, maxLiteReply))
}

// liteCode is the code of lite's envelope; a body that is not one counts as
// success.
func liteCode(body []byte) int {
	var env struct {
		Code *int `json:"code"`
	}
	if json.Unmarshal(body, &env) != nil || env.Code == nil {
		return 0
	}
	return *env.Code
}

// resource answers /m3u8, /webplayback, /key and /lyrics.
func (m *Manager) resource(route string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		adamID := q.Get("adamId")
		if adamID == "" {
			failure("missing adamId").write(w)
			return
		}
		if route == "/key" {
			uri := q.Get("uri")
			if uri == "" {
				failure("missing uri").write(w)
				return
			}
			if uri == prefetchKeyURI && adamID != "0" {
				failure("invalid uri: the prefetch key uri needs adamId 0").write(w)
				return
			}
		}
		m.forward(w, r.Method, route, r.URL.RawQuery, adamID, nil)
	}
}

func (m *Manager) handleLicense(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		failure("method not allowed").write(w)
		return
	}
	var body struct {
		AdamID    any `json:"adamId"`
		Challenge any `json:"challenge"`
		URI       any `json:"uri"`
	}
	raw, msg := readObject(r, &body)
	if msg != "" {
		failure(msg).write(w)
		return
	}
	adamID, ok1 := body.AdamID.(string)
	challenge, ok2 := body.Challenge.(string)
	uri, ok3 := body.URI.(string)
	if !ok1 || !ok2 || !ok3 || adamID == "" || challenge == "" || uri == "" {
		failure("adamId, challenge and uri must be non-empty strings").write(w)
		return
	}
	m.forward(w, http.MethodPost, "/license", r.URL.RawQuery, adamID, raw)
}

// forward sends the request to the candidates for adamID in turn and writes
// back the first final answer.
func (m *Manager) forward(w http.ResponseWriter, method, route, rawQuery, adamID string, body []byte) {
	start := time.Now()
	candidates := m.candidates(adamID)
	if len(candidates) == 0 {
		failure(errNoCandidate.Error()).write(w)
		return
	}
	var last []byte
	for i, a := range candidates {
		if i >= maxAttempts || time.Since(start) >= requestBudget {
			break
		}
		reply, err := m.callLite(a, a.slots, method, route, rawQuery, body)
		if err != nil {
			m.logger.Printf("%s adamId=%s: wrapper %s failed: %v", route, adamID, a.short(), err)
			continue
		}
		code := liteCode(reply)
		if code == 0 || code == 400 || (route == "/lyrics" && code == 404) {
			writeBody(w, http.StatusOK, reply)
			return
		}
		m.logger.Printf("%s adamId=%s: wrapper %s answered code %d", route, adamID, a.short(), code)
		last = reply
	}
	if last != nil {
		writeBody(w, http.StatusOK, last)
		return
	}
	failure(errNoCandidate.Error()).write(w)
}

// candidates are the ready accounts whose storefront can serve adamID, in
// random order so that parallel requests spread over them.
func (m *Manager) candidates(adamID string) []*account {
	m.mu.Lock()
	var ready []*account
	regions := map[string]bool{}
	for _, a := range m.accounts {
		if a.ready() {
			ready = append(ready, a)
			regions[normRegion(a.region)] = true
		}
	}
	m.mu.Unlock()
	if len(ready) == 0 {
		return nil
	}
	if adamID != "0" {
		ctx, cancel := context.WithTimeout(context.Background(), catalogWait)
		serves := m.catalog.servingRegions(ctx, regions, adamID)
		cancel()
		if serves != nil {
			var kept []*account
			for _, a := range ready {
				if serves[normRegion(a.region)] {
					kept = append(kept, a)
				}
			}
			ready = kept
		}
	}
	rand.Shuffle(len(ready), func(i, j int) { ready[i], ready[j] = ready[j], ready[i] })
	return ready
}

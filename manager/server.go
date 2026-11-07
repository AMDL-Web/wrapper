// Copyright (c) 2026 AMDL-Web contributors. SPDX-License-Identifier: MIT

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// maxBody caps request bodies.
const maxBody = 4 << 20

// reply is the envelope of every manager-generated answer.
type reply struct {
	code int
	msg  string
	data any
}

func success(data any) reply { return reply{code: 0, msg: "SUCCESS", data: data} }

func failure(msg string) reply { return reply{code: -1, msg: msg} }

// write sends r with HTTP 200.
func (r reply) write(w http.ResponseWriter) { r.writeStatus(w, http.StatusOK) }

func (r reply) writeStatus(w http.ResponseWriter, status int) {
	body, _ := json.Marshal(struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data any    `json:"data"`
	}{r.code, r.msg, r.data})
	writeBody(w, status, body)
}

// writeBody sends a JSON body as is.
func writeBody(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// noStore marks every response as not cacheable.
func noStore(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		h.ServeHTTP(w, r)
	})
}

// mainServer serves the API on the main port.
func (m *Manager) mainServer() *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/status", m.handleStatus)
	mux.HandleFunc("/token", m.handleToken)
	mux.HandleFunc("/m3u8", m.resource("/m3u8"))
	mux.HandleFunc("/webplayback", m.resource("/webplayback"))
	mux.HandleFunc("/key", m.resource("/key"))
	mux.HandleFunc("/lyrics", m.resource("/lyrics"))
	mux.HandleFunc("/license", m.handleLicense)
	mux.HandleFunc("/login", m.handleLogin)
	mux.HandleFunc("/logout", m.handleLogout)
	mux.HandleFunc("/check", m.handleCheck)
	if m.cfg.guest {
		mux.HandleFunc("/shutdown", m.handleShutdown)
	}
	// The panel's page needs no key: it holds nothing, and asks for the key
	// before it shows anything.
	outer := http.NewServeMux()
	servePanel(outer)
	outer.Handle("/", requireKey(m.cfg.apiKey, mux))
	return &http.Server{Handler: noStore(outer), ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second}
}

// readObject reads a JSON object body. The message is "" on success.
func readObject(r *http.Request, into any) ([]byte, string) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		return nil, "reading the request body failed"
	}
	if len(raw) > maxBody {
		return nil, "the request body is larger than 4 MiB"
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil || obj == nil {
		return nil, "invalid JSON body"
	}
	if into != nil && json.Unmarshal(raw, into) != nil {
		return nil, "invalid JSON body"
	}
	return raw, ""
}

type instanceStatus struct {
	ID     string `json:"id"`
	Region string `json:"region"`
	Name   string `json:"name"`
	Ready  bool   `json:"ready"`
}

func (m *Manager) handleStatus(w http.ResponseWriter, _ *http.Request) {
	m.mu.Lock()
	regions := []string{}
	seen := map[string]bool{}
	instances := make([]instanceStatus, 0, len(m.accounts))
	for _, a := range m.accounts {
		ready := a.ready()
		instances = append(instances, instanceStatus{ID: a.id, Region: a.region, Name: a.name, Ready: ready})
		if key := strings.ToLower(a.region); ready && !seen[key] {
			seen[key] = true
			regions = append(regions, a.region)
		}
	}
	data := map[string]any{"regions": regions, "clientCount": len(m.accounts), "ready": m.ready, "instances": instances}
	m.mu.Unlock()
	success(data).write(w)
}

func (m *Manager) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		failure("method not allowed").write(w)
		return
	}
	q := r.URL.Query()
	instance := strings.ToLower(strings.TrimSpace(q.Get("instance")))
	region := strings.TrimSpace(q.Get("region"))
	a := m.tokenAccount(instance, region)
	if a == nil {
		msg := "no logged-in account"
		if instance != "" {
			msg = "no logged-in account with this instance id"
		}
		body, _ := json.Marshal(map[string]any{"code": 503, "msg": msg})
		writeBody(w, http.StatusServiceUnavailable, body)
		return
	}
	body, err := m.callLite(a, a.tokenSlots, http.MethodGet, "/token", "", nil)
	if err != nil {
		failure(fmt.Sprintf("the wrapper of %s did not answer: %v", a.short(), err)).write(w)
		return
	}
	status := http.StatusOK
	if liteCode(body) == 503 {
		status = http.StatusServiceUnavailable
	}
	writeBody(w, status, body)
}

// tokenAccount picks the ready account /token asks: by instance id, else the
// first in region, else the first.
func (m *Manager) tokenAccount(instance, region string) *account {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range m.accounts {
		if !a.ready() {
			continue
		}
		switch {
		case instance != "":
			if a.id == instance {
				return a
			}
		case region != "":
			if strings.EqualFold(a.region, region) {
				return a
			}
		default:
			return a
		}
	}
	return nil
}

func (m *Manager) handleLogin(w http.ResponseWriter, r *http.Request) {
	// The login waits up to 90 s, longer than the server's write timeout.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(105 * time.Second))
	if r.Method != http.MethodPost {
		failure("method not allowed").write(w)
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Code     string `json:"code"`
	}
	if _, msg := readObject(r, &body); msg != "" {
		failure(msg).write(w)
		return
	}
	if msg := checkCredentials(body.Username, body.Password); msg != "" {
		failure(msg).write(w)
		return
	}
	if strings.TrimSpace(body.Code) != "" {
		m.submitCode(r.Context(), body.Username, body.Code).write(w)
		return
	}
	m.startLogin(r.Context(), body.Username, body.Password).write(w)
}

func (m *Manager) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		failure("method not allowed").write(w)
		return
	}
	var body struct {
		Username *string `json:"username"`
		ID       *string `json:"id"`
	}
	if _, msg := readObject(r, &body); msg != "" {
		failure(msg).write(w)
		return
	}
	var id string
	switch {
	case body.ID != nil && *body.ID != "":
		id = strings.ToLower(strings.TrimSpace(*body.ID))
		if !isUUID(id) {
			failure("invalid id (expected a UUID)").write(w)
			return
		}
	case body.Username != nil && *body.Username != "":
		id = instanceID(*body.Username)
	default:
		failure("missing username or id").write(w)
		return
	}
	if err := m.logout(id); err != nil {
		failure(err.Error()).write(w)
		return
	}
	data := map[string]any{}
	if body.ID != nil && *body.ID != "" {
		data["id"] = *body.ID
	}
	if body.Username != nil && *body.Username != "" {
		data["username"] = *body.Username
	}
	success(data).write(w)
}

func (m *Manager) handleShutdown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		failure("method not allowed").write(w)
		return
	}
	m.logger.Printf("shutdown requested")
	syncDisks()
	success(map[string]any{}).write(w)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	m.requestStop()
}

// errNoCandidate is answered when no account can serve a request.
var errNoCandidate = errors.New("no available instance")

// Copyright (c) 2026 AMDL-Web contributors. SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Catalog probing: which storefronts can serve a song. Apple's web player
// API answers a song read with data where the song is available and with
// errors where it is not.
const (
	catalogWait  = 10 * time.Second // a request waits this long for probes
	probeLimit   = 60 * time.Second
	tokenTTL     = 24 * time.Hour
	servesTTL    = 24 * time.Hour
	notServesTTL = 5 * time.Minute
	maxProbes    = 4
	maxPageBytes = 32 << 20
)

var (
	indexJS     = regexp.MustCompile(`/assets/index~[^"']+\.js`)
	jwtPattern  = regexp.MustCompile(`eyJ[A-Za-z0-9_\-=]+\.[A-Za-z0-9_\-=]+\.[A-Za-z0-9_\-=]+`)
	regionText  = regexp.MustCompile(`^[a-z]{2}$`)
	catalogID   = regexp.MustCompile(`^[0-9]+$`)
	browserUA   = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36"
	errNoAnswer = errors.New("no answer")
)

// catalog asks Apple whether a storefront has a song, and remembers.
type catalog struct {
	client *http.Client
	webURL string // https://music.apple.com
	apiURL string // https://amp-api.music.apple.com

	tokenMu sync.Mutex
	jwt     string
	jwtAt   time.Time

	mu      sync.Mutex
	answers map[string]answer
	flights map[string]*flight
	sem     chan struct{}
}

type answer struct {
	serves bool
	until  time.Time
}

type flight struct {
	done          chan struct{}
	serves, known bool
}

func newCatalog(client *http.Client) *catalog {
	return &catalog{
		client:  client,
		webURL:  "https://music.apple.com",
		apiURL:  "https://amp-api.music.apple.com",
		answers: map[string]answer{},
		flights: map[string]*flight{},
		sem:     make(chan struct{}, maxProbes),
	}
}

func normRegion(r string) string { return strings.ToLower(strings.TrimSpace(r)) }

// servingRegions reports for each region whether it can serve adamID. It
// returns nil when no region's answer is known, so the caller tries every
// account rather than none: Apple may be unreachable.
func (c *catalog) servingRegions(ctx context.Context, regions map[string]bool, adamID string) map[string]bool {
	if !catalogID.MatchString(adamID) {
		return nil
	}
	flights := map[string]*flight{}
	for r := range regions {
		flights[r] = c.probe(r, adamID)
	}
	out := map[string]bool{}
	known := 0
	for r, f := range flights {
		select {
		case <-f.done:
		case <-ctx.Done():
			continue
		}
		if f.known {
			known++
			out[r] = f.serves
		}
	}
	if known == 0 {
		return nil
	}
	return out
}

// probe starts, or joins, the question whether region serves adamID.
func (c *catalog) probe(region, adamID string) *flight {
	key := region + "/" + adamID
	c.mu.Lock()
	defer c.mu.Unlock()
	if a, ok := c.answers[key]; ok && time.Now().Before(a.until) {
		f := &flight{done: make(chan struct{}), serves: a.serves, known: true}
		close(f.done)
		return f
	}
	if f := c.flights[key]; f != nil {
		return f
	}
	f := &flight{done: make(chan struct{})}
	c.flights[key] = f
	go func() {
		serves, err := c.ask(region, adamID)
		c.mu.Lock()
		if err == nil {
			f.serves, f.known = serves, true
			ttl := servesTTL
			if !serves {
				ttl = notServesTTL
			}
			c.answers[key] = answer{serves: serves, until: time.Now().Add(ttl)}
		}
		delete(c.flights, key)
		c.mu.Unlock()
		close(f.done)
	}()
	return f
}

// ask reads the song from region's catalog.
func (c *catalog) ask(region, adamID string) (bool, error) {
	if !regionText.MatchString(region) {
		return false, fmt.Errorf("region %q", region)
	}
	c.sem <- struct{}{}
	defer func() { <-c.sem }()
	ctx, cancel := context.WithTimeout(context.Background(), probeLimit)
	defer cancel()
	token, err := c.token(ctx)
	if err != nil {
		return false, err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.apiURL+"/v1/catalog/"+region+"/songs/"+adamID, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Origin", "https://music.apple.com")
	req.Header.Set("User-Agent", browserUA)
	resp, err := c.client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		c.dropToken(token)
		return false, fmt.Errorf("catalog answered %s", resp.Status)
	}
	var body struct {
		Data   []json.RawMessage `json:"data"`
		Errors json.RawMessage   `json:"errors"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxPageBytes)).Decode(&body); err != nil {
		return false, err
	}
	switch {
	case len(body.Data) > 0:
		return true, nil
	case body.Errors != nil:
		return false, nil
	}
	return false, errNoAnswer
}

// token is the web player's bearer token, scraped from music.apple.com and
// kept for a day.
func (c *catalog) token(ctx context.Context) (string, error) {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	if c.jwt != "" && time.Since(c.jwtAt) < tokenTTL {
		return c.jwt, nil
	}
	page, err := c.get(ctx, c.webURL)
	if err != nil {
		return "", err
	}
	script := indexJS.Find(page)
	if script == nil {
		return "", errors.New("no index script on the web player page")
	}
	js, err := c.get(ctx, c.webURL+string(script))
	if err != nil {
		return "", err
	}
	jwt := jwtPattern.Find(js)
	if jwt == nil {
		return "", errors.New("no token in the web player script")
	}
	c.jwt, c.jwtAt = string(jwt), time.Now()
	return c.jwt, nil
}

func (c *catalog) dropToken(token string) {
	c.tokenMu.Lock()
	if c.jwt == token {
		c.jwt = ""
	}
	c.tokenMu.Unlock()
}

func (c *catalog) get(ctx context.Context, url string) ([]byte, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("User-Agent", browserUA)
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxPageBytes))
}

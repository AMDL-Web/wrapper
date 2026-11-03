// Copyright (c) 2026 AMDL-Web contributors. SPDX-License-Identifier: MIT

package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const fakeJWT = "eyJhbGciOiJFUzI1NiJ9.eyJpc3MiOiJ0ZXN0In0.c2lnbmF0dXJl"

// fakeApple serves a web player page, its script with a token, and catalog
// song reads: available in the storefronts listed per song.
type fakeApple struct {
	*httptest.Server
	songs    map[string][]string
	scrapes  atomic.Int32
	probes   atomic.Int32
	reject   atomic.Bool // answer 401 to catalog reads
	mu       sync.Mutex
	badAuths int
}

func newFakeApple(t *testing.T, songs map[string][]string) *fakeApple {
	f := &fakeApple{songs: songs}
	mux := http.NewServeMux()
	mux.HandleFunc("/{$}", func(w http.ResponseWriter, r *http.Request) {
		f.scrapes.Add(1)
		fmt.Fprint(w, `<html><script src="/assets/index~a1b2c3.js"></script></html>`)
	})
	mux.HandleFunc("/assets/index~a1b2c3.js", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `var x="%s";`, fakeJWT)
	})
	mux.HandleFunc("/v1/catalog/{region}/songs/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.probes.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+fakeJWT || r.Header.Get("Origin") != "https://music.apple.com" || !strings.HasPrefix(r.Header.Get("User-Agent"), "Mozilla/5.0") {
			f.mu.Lock()
			f.badAuths++
			f.mu.Unlock()
		}
		if f.reject.Load() {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		time.Sleep(20 * time.Millisecond)
		for _, r2 := range f.songs[r.PathValue("id")] {
			if r2 == r.PathValue("region") {
				fmt.Fprintf(w, `{"data":[{"id":"%s","type":"songs"}]}`, r.PathValue("id"))
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"errors":[{"status":"404"}]}`)
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func testCatalog(f *fakeApple) *catalog {
	c := newCatalog(f.Client())
	c.webURL, c.apiURL = f.URL, f.URL
	return c
}

func TestCatalogProbe(t *testing.T) {
	f := newFakeApple(t, map[string][]string{"100": {"jp"}})
	c := testCatalog(f)
	regions := map[string]bool{"us": true, "jp": true}
	got := c.servingRegions(context.Background(), regions, "100")
	if !got["jp"] || got["us"] || len(got) != 2 {
		t.Fatalf("%v", got)
	}
	if f.badAuths != 0 {
		t.Fatal("probe headers")
	}
	// Answers are remembered; the token is scraped once.
	c.servingRegions(context.Background(), regions, "100")
	if f.probes.Load() != 2 || f.scrapes.Load() != 1 {
		t.Fatalf("%d probes, %d scrapes", f.probes.Load(), f.scrapes.Load())
	}
	// Concurrent requests for one song probe once per region.
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() { c.servingRegions(context.Background(), regions, "200") })
	}
	wg.Wait()
	if f.probes.Load() != 4 {
		t.Fatalf("%d probes after 10 concurrent requests", f.probes.Load())
	}
	if c.servingRegions(context.Background(), regions, "not-a-number") != nil {
		t.Fatal("a non-numeric id is probed")
	}
}

func TestCatalogUnknownMeansEveryone(t *testing.T) {
	f := newFakeApple(t, nil)
	f.reject.Store(true)
	c := testCatalog(f)
	if got := c.servingRegions(context.Background(), map[string]bool{"us": true}, "100"); got != nil {
		t.Fatalf("%v", got)
	}
	// The refused token is dropped and scraped again next time.
	c.servingRegions(context.Background(), map[string]bool{"jp": true}, "101")
	if f.scrapes.Load() != 2 {
		t.Fatalf("%d scrapes", f.scrapes.Load())
	}
}

func TestRoutingByStorefront(t *testing.T) {
	f := newFakeApple(t, map[string][]string{"100": {"jp"}, "200": {"us", "jp"}})
	h := bootHarness(t, map[string]string{"a@x": "us", "b@x": "jp"}, []string{"a@x", "b@x"}, nil, func(m *Manager) {
		m.catalog = testCatalog(f)
	})
	waitFor(t, 5*time.Second, "ready", func() bool { return h.status()["ready"] == true })
	seen := map[any]int{}
	for range 20 {
		_, env := h.call("GET", "/m3u8?adamId=100", nil)
		if who := env["data"].(map[string]any)["who"]; who != instanceID("b@x") {
			t.Fatalf("song 100 is only in jp: %v", env)
		}
		_, env = h.call("GET", "/m3u8?adamId=200", nil)
		seen[env["data"].(map[string]any)["who"]]++
	}
	if len(seen) != 2 {
		t.Fatalf("a song in both storefronts went to %v", seen)
	}
	if _, env := h.call("GET", "/m3u8?adamId=300", nil); env["msg"] != "no available instance" {
		t.Fatalf("a song in no storefront: %v", env)
	}
	// The shared prefetch key needs no probe.
	before := f.probes.Load()
	if _, env := h.call("GET", "/key?adamId=0&uri=skd://itunes.apple.com/P000000000/s1/e1", nil); env["code"] != float64(0) {
		t.Fatalf("%v", env)
	}
	if f.probes.Load() != before {
		t.Fatal("adamId 0 was probed")
	}
}

package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// fakeLiteToken serves /token the way wrapper-lite does: the token, or HTTP
// 503 before the account has one.
func fakeLiteToken(t *testing.T, status int, body string) *WrapperInstance {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	p, _ := strconv.Atoi(port)
	return &WrapperInstance{Id: "tok-" + port, Region: "cn", Port: p}
}

func withInstances(t *testing.T, insts ...*WrapperInstance) {
	t.Helper()
	instancesMu.Lock()
	saved := Instances
	Instances = insts
	instancesMu.Unlock()
	t.Cleanup(func() {
		instancesMu.Lock()
		Instances = saved
		instancesMu.Unlock()
	})
}

func getToken(query string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	handleToken(rec, httptest.NewRequest(http.MethodGet, "/token"+query, nil))
	return rec
}

func TestHandleTokenForwardsTheInstanceAnswer(t *testing.T) {
	ok := `{"code":0,"msg":"SUCCESS","data":{"region":"cn","music_token":"x","refreshed_at":1,"next_refresh_at":2}}`
	withInstances(t, &WrapperInstance{Id: "starting"}, fakeLiteToken(t, http.StatusOK, ok))

	rec := getToken("")
	if rec.Code != http.StatusOK || rec.Body.String() != ok {
		t.Fatalf("got %d %s, want 200 and the lite body", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get(headerCacheControl); cc != cacheControlNoStore {
		t.Errorf("Cache-Control = %q, want %q", cc, cacheControlNoStore)
	}
	if rec := getToken("?region=CN"); rec.Code != http.StatusOK {
		t.Errorf("region match is case-insensitive: got %d", rec.Code)
	}
	if rec := getToken("?region=us"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("unknown region: got %d, want 503", rec.Code)
	}
}

func TestHandleTokenKeeps503(t *testing.T) {
	none := `{"code":503,"msg":"music token not available"}`
	withInstances(t, fakeLiteToken(t, http.StatusServiceUnavailable, none))
	if rec := getToken(""); rec.Code != http.StatusServiceUnavailable || rec.Body.String() != none {
		t.Fatalf("got %d %s, want lite's 503", rec.Code, rec.Body)
	}

	withInstances(t)
	if rec := getToken(""); rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), `"code":503`) {
		t.Fatalf("no account: got %d %s, want 503", rec.Code, rec.Body)
	}
}

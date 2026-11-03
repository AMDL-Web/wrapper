package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeLauncher installs a wrapper-lite-rootless that behaves as script says,
// and returns the file each run appends its pid to.
func fakeLauncher(t *testing.T, script string) string {
	t.Helper()
	inTempDir(t)
	runs := filepath.Join(mustAbs("."), "runs")
	body := "#!/bin/sh\necho $$ >> " + runs + "\n" + script + "\n"
	if err := os.MkdirAll(filepath.Dir(launcherPath()), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(launcherPath(), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	withInstances(t)
	return runs
}

// pids returns the pids the fake launcher recorded, oldest first.
func pids(t *testing.T, runs string) []int {
	t.Helper()
	raw, _ := os.ReadFile(runs)
	var out []int
	for _, f := range strings.Fields(string(raw)) {
		n, err := strconv.Atoi(f)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	return out
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func waitGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for alive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("login child %d still runs", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestCanceledLoginKillsItsChildAndCleansUp(t *testing.T) {
	runs := fakeLauncher(t, "exec sleep 60")
	id := InstanceID("cancel@example.invalid")
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)
	start := time.Now()
	res := startLogin(ctx, LoginRequest{Username: "cancel@example.invalid", Password: "x"})
	if res.Code != -1 || res.Msg != errLoginCanceled.Error() {
		t.Fatalf("got %+v, want the canceled login", res)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("the canceled login answered after %s", d)
	}
	got := pids(t, runs)
	if len(got) != 1 {
		t.Fatalf("%d login children, want 1", len(got))
	}
	// The child was reaped before the answer, so it is gone, not a zombie.
	if alive(got[0]) {
		t.Error("the canceled login's child still runs")
	}
	if getLogin(id) != nil {
		t.Error("the canceled login is still registered")
	}
	if _, err := os.Stat(instanceDir(id)); !os.IsNotExist(err) {
		t.Error("the canceled login left its instance dir")
	}

	// A retry right away runs its own child to its own end.
	if err := os.WriteFile(launcherPath(), []byte("#!/bin/sh\necho $$ >> "+runs+"\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	res = startLogin(context.Background(), LoginRequest{Username: "cancel@example.invalid", Password: "x"})
	if res.Code != -1 || res.Msg != "login failed (exit code 3)" {
		t.Fatalf("retry: got %+v, want the retry's own failure", res)
	}
	if n := len(pids(t, runs)); n != 2 {
		t.Errorf("%d login children after the retry, want 2", n)
	}
}

func TestANewLoginReplacesOneWaitingForItsCode(t *testing.T) {
	runs := fakeLauncher(t, "echo 'Enter your 2FA code'\nexec sleep 60")
	id := InstanceID("2fa@example.invalid")
	res := startLogin(context.Background(), LoginRequest{Username: "2fa@example.invalid", Password: "x"})
	if res.Code != 2 {
		t.Fatalf("got %+v, want a code request", res)
	}
	first := pids(t, runs)[0]
	if !alive(first) {
		t.Fatal("the child waiting for its code is gone")
	}
	// The user starts over instead of entering the code.
	res = startLogin(context.Background(), LoginRequest{Username: "2fa@example.invalid", Password: "y"})
	waitGone(t, first)
	if res.Code != 2 {
		t.Fatalf("got %+v, want the new login's own code request", res)
	}
	got := pids(t, runs)
	if len(got) != 2 || !alive(got[1]) {
		t.Fatalf("login children %v: want the old one gone and its replacement waiting", got)
	}
	st := getLogin(id)
	if st == nil {
		t.Fatal("the new login is not registered")
	}
	abortLogin(st)
	waitGone(t, got[1])
	if getLogin(id) != nil {
		t.Error("an aborted login is still registered")
	}
	if _, err := os.Stat(instanceDir(id)); !os.IsNotExist(err) {
		t.Error("an aborted login left its instance dir")
	}
}

func TestAReplacedLoginLeavesTheNewOnesDir(t *testing.T) {
	inTempDir(t)
	id := InstanceID("late@example.invalid")
	old := newLoginState(id)
	loginRegistry.Lock()
	loginRegistry.m[id] = old
	loginRegistry.Unlock()
	fresh, ok := reserveLogin(id)
	if !ok {
		t.Fatal("reserve")
	}
	t.Cleanup(func() { removeLogin(id) })
	if err := os.MkdirAll(instanceDir(id), 0o777); err != nil {
		t.Fatal(err)
	}
	loginFailed(id, old, errLoginCanceled)
	if _, err := os.Stat(instanceDir(id)); err != nil {
		t.Errorf("the replaced login's late failure removed the new login's dir: %v", err)
	}
	if getLogin(id) != fresh {
		t.Error("the replaced login's late failure dropped the new login")
	}
}

func TestShutdownAnswersThenStops(t *testing.T) {
	stop := make(chan os.Signal, 1)
	rec := httptest.NewRecorder()
	handleShutdown(stop)(rec, httptest.NewRequest(http.MethodPost, "/shutdown", nil))
	if !strings.Contains(rec.Body.String(), `"code":0`) {
		t.Errorf("answer %s", rec.Body)
	}
	select {
	case sig := <-stop:
		if sig != syscall.SIGTERM {
			t.Errorf("signal %v", sig)
		}
	default:
		t.Error("the manager was not told to stop")
	}
	rec = httptest.NewRecorder()
	handleShutdown(stop)(rec, httptest.NewRequest(http.MethodGet, "/shutdown", nil))
	if len(stop) != 0 {
		t.Error("a GET stopped the manager")
	}
}

func TestInstancesJSONWriteLeavesNoTemp(t *testing.T) {
	inTempDir(t)
	withInstances(t, &WrapperInstance{Id: "a", Region: "jp", Port: 1})
	SaveInstances()
	if _, err := os.Stat("data/instances.json.tmp"); !os.IsNotExist(err) {
		t.Error("the temp file stayed")
	}
	if got := LoadInstance(); len(got) != 1 || got[0].Id != "a" {
		t.Errorf("instances.json = %+v", got)
	}
}

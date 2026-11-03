package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

type statusReply struct {
	Code int `json:"code"`
	Data struct {
		Regions     []string         `json:"regions"`
		ClientCount int              `json:"clientCount"`
		Instances   []StatusInstance `json:"instances"`
	} `json:"data"`
}

func getStatus(t *testing.T) statusReply {
	t.Helper()
	rec := httptest.NewRecorder()
	handleStatus(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	var reply statusReply
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatalf("decode /status: %v: %s", err, rec.Body)
	}
	return reply
}

func TestStatusListsEveryInstance(t *testing.T) {
	a := &WrapperInstance{Id: "a", Region: "jp", Name: "a@example.com"}
	b := &WrapperInstance{Id: "b", Region: "jp", Name: "b@example.com"}
	starting := &WrapperInstance{Id: "c", Name: "c@example.com"}
	withInstances(t, a, b, starting)
	for i := 0; i < fairplayFailThreshold; i++ {
		recordFairplayFailure("b")
	}
	t.Cleanup(func() { resetFairplayCircuit("b") })

	reply := getStatus(t)
	if len(reply.Data.Regions) != 1 || reply.Data.ClientCount != 3 {
		t.Errorf("regions %v, clientCount %d: want one deduplicated region and 3 clients", reply.Data.Regions, reply.Data.ClientCount)
	}
	want := []StatusInstance{
		{Id: "a", Region: "jp", Name: "a@example.com", Ready: true},
		{Id: "b", Region: "jp", Name: "b@example.com", Ready: true, Degraded: true},
		{Id: "c", Name: "c@example.com"},
	}
	if fmt.Sprint(reply.Data.Instances) != fmt.Sprint(want) {
		t.Errorf("instances = %+v, want %+v", reply.Data.Instances, want)
	}

	withInstances(t)
	if reply := getStatus(t); reply.Data.Instances == nil || len(reply.Data.Instances) != 0 {
		t.Errorf("no accounts: instances = %v, want []", reply.Data.Instances)
	}
}

func tokenBody(region, token string) string {
	return fmt.Sprintf(`{"code":0,"msg":"SUCCESS","data":{"region":%q,"music_token":%q,"refreshed_at":1,"next_refresh_at":2}}`, region, token)
}

func TestHandleTokenByInstance(t *testing.T) {
	a := fakeLiteToken(t, http.StatusOK, tokenBody("jp", "a"))
	b := fakeLiteToken(t, http.StatusOK, tokenBody("jp", "b"))
	a.Region, b.Region = "jp", "jp"
	a.Id, b.Id = InstanceID("a@example.com"), InstanceID("b@example.com")
	starting := &WrapperInstance{Id: InstanceID("c@example.com")}
	withInstances(t, a, b, starting)

	for _, tc := range []struct {
		query string
		want  string
	}{
		{"?instance=" + a.Id, tokenBody("jp", "a")},
		{"?instance=" + b.Id, tokenBody("jp", "b")},
		{"?instance=" + strings.ToUpper(b.Id), tokenBody("jp", "b")},
		{"?instance=" + b.Id + "&region=us", tokenBody("jp", "b")},
		{"?region=jp", tokenBody("jp", "a")},
	} {
		if rec := getToken(tc.query); rec.Code != http.StatusOK || rec.Body.String() != tc.want {
			t.Errorf("%s: got %d %s, want 200 %s", tc.query, rec.Code, rec.Body, tc.want)
		}
	}
	for _, query := range []string{"?instance=" + starting.Id, "?instance=" + InstanceID("gone@example.com"), "?instance=not-a-uuid"} {
		rec := getToken(query)
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), `"code":503`) {
			t.Errorf("%s: got %d %s, want 503", query, rec.Code, rec.Body)
		}
	}
}

func inTempDir(t *testing.T) {
	t.Helper()
	t.Chdir(t.TempDir())
	if err := os.MkdirAll("data", 0o777); err != nil {
		t.Fatal(err)
	}
}

func TestInstancesJSONKeepsName(t *testing.T) {
	inTempDir(t)
	withInstances(t, &WrapperInstance{Id: "a", Region: "jp", Port: 1, Name: "a@example.com"}, &WrapperInstance{Id: "b", Region: "us", Port: 2})
	SaveInstances()
	got := LoadInstance()
	if len(got) != 2 || got[0].Name != "a@example.com" || got[1].Name != "" {
		t.Fatalf("loaded %+v, want the Apple ID kept for a and empty for b", got)
	}
	raw, _ := os.ReadFile("data/instances.json")
	if !strings.Contains(string(raw), `"name": "a@example.com"`) {
		t.Errorf("instances.json does not store the name unmasked: %s", raw)
	}
}

func TestLoginRefusesAnEleventhAccount(t *testing.T) {
	inTempDir(t)
	held := make([]*WrapperInstance, 0, MaxAccounts)
	for i := 0; i < MaxAccounts-1; i++ {
		held = append(held, &WrapperInstance{Id: InstanceID(fmt.Sprintf("held%d@example.com", i)), Region: "us"})
	}
	withInstances(t, held...)
	pending, ok := reserveLogin(InstanceID("pending@example.com"))
	if !ok {
		t.Fatal("the tenth account must be allowed")
	}
	t.Cleanup(func() { removeLogin(pending.Id) })

	res := startLogin(context.Background(), LoginRequest{Username: "eleventh@example.com", Password: "x"})
	if res.Code != -1 || !strings.Contains(res.Msg, "account limit reached") || !strings.Contains(res.Msg, "10") {
		t.Fatalf("got %+v, want the account-limit error", res)
	}
	if getLogin(InstanceID("eleventh@example.com")) != nil {
		t.Error("a refused login must not register")
	}
	if _, err := os.Stat(instanceDir(InstanceID("eleventh@example.com"))); !os.IsNotExist(err) {
		t.Error("a refused login must not create an instance dir")
	}

	// An account already held still gets its own answer, not the cap.
	if res := startLogin(context.Background(), LoginRequest{Username: "held0@example.com", Password: "x"}); res.Msg != "already login" {
		t.Errorf("held account: got %+v, want already login", res)
	}
}

func TestLogoutByID(t *testing.T) {
	inTempDir(t)
	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	inst := &WrapperInstance{Id: InstanceID("a@example.com"), Region: "jp", Name: "a@example.com", Cmd: cmd}
	other := &WrapperInstance{Id: InstanceID("b@example.com"), Region: "jp", Name: "b@example.com"}
	withInstances(t, inst, other)
	if err := os.MkdirAll(instanceDir(inst.Id), 0o777); err != nil {
		t.Fatal(err)
	}

	if err := startLogout(LogoutRequest{Id: inst.Id}); err != nil {
		t.Fatalf("logout by id: %v", err)
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the account's process was not killed")
	}
	if GetInstance(inst.Id) != nil || GetInstance(other.Id) == nil {
		t.Error("logout must drop exactly that account from the registry")
	}
	if _, err := os.Stat(instanceDir(inst.Id)); !os.IsNotExist(err) {
		t.Error("logout must delete the account's data dir")
	}
	if got := LoadInstance(); len(got) != 1 || got[0].Id != other.Id {
		t.Errorf("instances.json = %+v, want only the other account", got)
	}
	if err := startLogout(LogoutRequest{Id: inst.Id}); err == nil || err.Error() != "no such account" {
		t.Errorf("second logout: %v, want no such account", err)
	}
}

func TestConcurrentLoginsStayWithinTheCap(t *testing.T) {
	held := make([]*WrapperInstance, 0, MaxAccounts-1)
	for i := 0; i < MaxAccounts-1; i++ {
		held = append(held, &WrapperInstance{Id: InstanceID(fmt.Sprintf("held%d@example.com", i)), Region: "us"})
	}
	withInstances(t, held...)

	ids := []string{InstanceID("x@example.com"), InstanceID("y@example.com")}
	results := make(chan bool, len(ids))
	start := make(chan struct{})
	for _, id := range ids {
		go func(id string) {
			<-start
			_, ok := reserveLogin(id)
			results <- ok
		}(id)
	}
	close(start)
	granted := 0
	for range ids {
		if <-results {
			granted++
		}
	}
	for _, id := range ids {
		removeLogin(id)
	}
	if granted != 1 {
		t.Fatalf("%d of two concurrent logins got the last slot, want 1", granted)
	}
}

func TestLateExitKeepsTheReloggedInstance(t *testing.T) {
	old := &WrapperInstance{Id: "a", Region: "jp"}
	fresh := &WrapperInstance{Id: "a"}
	withInstances(t, fresh)
	RemoveInstance(old)
	if GetInstance("a") != fresh {
		t.Fatal("removing the previous process's instance dropped the re-login")
	}
	RemoveInstance(fresh)
	if GetInstance("a") != nil {
		t.Fatal("the instance itself was not removed")
	}
}

func TestRestoredInstancesWaitForTheirRegion(t *testing.T) {
	inTempDir(t)
	withInstances(t)
	saved := isReady()
	t.Cleanup(func() { setReady(saved) })
	setReady(false)
	if err := os.WriteFile("data/instances.json", []byte(`[{"id":"a","region":"jp","port":1,"name":"a@example.com"},{"id":"b","region":"us","port":2,"name":""}]`), 0o666); err != nil {
		t.Fatal(err)
	}

	var started []string
	restoreInstances(func(id string) { started = append(started, id) })
	if fmt.Sprint(started) != "[a b]" {
		t.Errorf("started %v, want [a b]", started)
	}
	reply := getStatus(t)
	want := []StatusInstance{{Id: "a", Name: "a@example.com"}, {Id: "b"}}
	if fmt.Sprint(reply.Data.Instances) != fmt.Sprint(want) {
		t.Errorf("instances = %+v, want %+v (not ready before lite reports a region)", reply.Data.Instances, want)
	}
	if isReady() {
		t.Error("the manager must not be ready before its restored accounts are up")
	}
	if rec := getToken("?instance=a"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("token of a restoring account: got %d, want 503", rec.Code)
	}
}

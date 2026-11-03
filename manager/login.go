package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/gofrs/uuid/v5"
	log "github.com/sirupsen/logrus"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// loginWait bounds one POST /login: the wait for the lite login child to ask
// for a code or to finish. It ends before the request's write deadline
// (loginWriteTimeout), so the client always gets an answer, and before
// amdl's own limit for a login, so the timeout it reports is this one.
const loginWait = 90 * time.Second

// loginWriteTimeout is POST /login's write deadline, in place of the
// server's WriteTimeout, which is shorter than a login.
const loginWriteTimeout = loginWait + 15*time.Second

// abortWait bounds how long an abort waits for the login to end: the killed
// child reaped and its half-made instance dir removed, or, for a child that
// had already logged in, the account's service started.
const abortWait = liteReadyTimeout + 15*time.Second

// errLoginCanceled ends a login whose client went away or that a new login of
// the same account replaced.
var errLoginCanceled = errors.New("login canceled")

// LoginResult describes the outcome of a login attempt for the HTTP layer.
type LoginResult struct {
	Code    int // 0 = success, 2 = 2FA required, -1 = failure
	Msg     string
	LoginId string
}

// LoginRequest is the JSON body of POST /login.
type LoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Code     string `json:"code"`
}

// LogoutRequest is the JSON body of POST /logout. Either username (an Apple
// account name) or id (the instance UUID) may be supplied; id takes
// precedence when both are present.
type LogoutRequest struct {
	Username string `json:"username"`
	Id       string `json:"id"`
}

// LoginState captures the progress of an in-flight account login.
type LoginState struct {
	Id       string
	Username string

	mu          sync.Mutex
	pending2FA  bool
	reported2FA bool
	cmd         *exec.Cmd
	// aborted means the login was canceled: its child is killed, and its
	// exit cleans up instead of counting as a failure of the account.
	aborted bool

	done chan error // nil => success; error => failure
	// exited is closed once the login has ended: the child is reaped, and a
	// failed login's instance dir is gone or a successful one's service has
	// started.
	exited chan struct{}
}

// loginRegistry tracks in-flight logins keyed by instance id.
var loginRegistry = struct {
	sync.Mutex
	m map[string]*LoginState
}{m: make(map[string]*LoginState)}

func newLoginState(id string) *LoginState {
	return &LoginState{
		Id:       id,
		Username: "",
		done:     make(chan error, 1),
		exited:   make(chan struct{}),
	}
}

func setLoginUsername(id, username string) {
	loginRegistry.Lock()
	defer loginRegistry.Unlock()
	if st, ok := loginRegistry.m[id]; ok {
		st.Username = username
	}
}

func getLogin(id string) *LoginState {
	loginRegistry.Lock()
	defer loginRegistry.Unlock()
	return loginRegistry.m[id]
}

func removeLogin(id string) {
	loginRegistry.Lock()
	delete(loginRegistry.m, id)
	loginRegistry.Unlock()
}

// releaseLogin drops st from the registry and reports whether it was still
// the account's login: a login that a newer one replaced owns nothing there,
// not even the instance dir.
func releaseLogin(id string, st *LoginState) bool {
	loginRegistry.Lock()
	defer loginRegistry.Unlock()
	cur, ok := loginRegistry.m[id]
	if ok && cur != st {
		return false
	}
	delete(loginRegistry.m, id)
	return true
}

// abortLogin kills an in-flight login's child and waits, up to abortWait,
// until runLoginChild has reaped it and removed its instance dir. A child
// that has already logged in is not undone: the wait then lasts until the
// account's service has started.
func abortLogin(st *LoginState) {
	st.mu.Lock()
	st.aborted = true
	cmd := st.cmd
	st.mu.Unlock()
	if cmd != nil {
		killGroup(cmd)
	}
	select {
	case <-st.exited:
	case <-time.After(abortWait):
		log.Warnf("[wrapper %s] the canceled login did not end within %s", shortID(st.Id), abortWait)
	}
}

func (s *LoginState) isAborted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.aborted
}

// killGroup kills a launcher's process group: the launcher and the lite it
// forked.
func killGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		_ = cmd.Process.Kill()
	}
}

// hasTokenFiles reports whether a successful login artifact exists in the
// instance data dir (lite writes STOREFRONT_ID and MUSIC_TOKEN there).
func hasTokenFiles(id string) bool {
	for _, name := range []string{"STOREFRONT_ID", "MUSIC_TOKEN"} {
		if _, err := os.Stat(instanceDir(id) + "/" + name); err != nil {
			return false
		}
	}
	return true
}

// MaxAccounts caps how many accounts one manager holds: each runs its own
// wrapper-lite process, so the cap bounds the manager's memory.
const MaxAccounts = 10

// reserveLogin registers a fresh login for id unless the manager already holds
// or is logging in MaxAccounts other accounts. Counting and registering under
// one lock keeps concurrent logins from overshooting the cap together.
func reserveLogin(id string) (*LoginState, bool) {
	loginRegistry.Lock()
	defer loginRegistry.Unlock()
	ids := make(map[string]bool)
	for _, inst := range SnapshotInstances() {
		ids[inst.Id] = true
	}
	for other := range loginRegistry.m {
		ids[other] = true
	}
	delete(ids, id)
	if len(ids) >= MaxAccounts {
		return nil, false
	}
	st := newLoginState(id)
	loginRegistry.m[id] = st
	return st, true
}

// startLogin begins (or continues) a login for an account. It gives up when
// ctx ends, the request's context: the client went away, so the login child is
// killed and its half-made instance dir removed.
//
// When req.Code == "" and no login is in flight, it launches the one-shot
// lite --login child and returns as soon as the outcome is known:
//   - code 0: tokens cached, service instance started (or starting);
//   - code 2: lite requires a 2FA code; the caller should retry with req.Code;
//   - code -1: login failed (bad credentials, no subscription, timeout).
//
// When req.Code != "", the code is written to 2fa.txt and the pending child
// finishes the login; this call blocks (polling) until the child exits.
//
// A login without a code replaces one of the same account still in flight,
// such as one waiting for a code that will not come: the old child is killed
// and reaped first, so two children never share the account's dir.
func startLogin(ctx context.Context, req LoginRequest) LoginResult {
	id := InstanceID(req.Username)

	// Same account already registered as a ready instance -> already login.
	if inst := GetInstance(id); inst != nil && inst.Region != "" {
		return LoginResult{Code: -1, Msg: "already login", LoginId: id}
	}

	// If a login for this account is pending 2FA, just feed the code.
	if req.Code != "" {
		if st := getLogin(id); st != nil && st.isPending2FA() {
			if err := provide2FACode(id, req.Code); err != nil {
				return LoginResult{Code: -1, Msg: err.Error(), LoginId: id}
			}
			return waitForLoginFinish(ctx, id, st)
		}
		return LoginResult{Code: -1, Msg: "no pending login for this account, restart login without code", LoginId: id}
	}

	// No code: (re)start from scratch.
	if old := getLogin(id); old != nil {
		log.Infof("[wrapper %s] a new login replaces the one in flight", shortID(id))
		abortLogin(old)
		// The old one may have logged in before it was stopped.
		if inst := GetInstance(id); inst != nil && inst.Region != "" {
			return LoginResult{Code: -1, Msg: "already login", LoginId: id}
		}
	}
	st, ok := reserveLogin(id)
	if !ok {
		return LoginResult{Code: -1, Msg: fmt.Sprintf("account limit reached: this manager holds at most %d accounts, log one out first", MaxAccounts), LoginId: id}
	}
	setLoginUsername(id, req.Username)
	// Clean stale data from a previous failed attempt.
	_ = RemoveWrapperDataQuiet(id)

	go runLoginChild(id, req.Username, req.Password, st)
	return waitForLoginFinish(ctx, id, st)
}

// runLoginChild launches the one-shot lite --login child, monitors its exit,
// and drives the state machine to completion.
func runLoginChild(id, username, password string, st *LoginState) {
	defer close(st.exited)
	log.Infof("[wrapper %s] login started for %s", shortID(id), username)
	cmd, err := startLiteLogin(id, username, password)
	if err != nil {
		loginFailed(id, st, fmt.Errorf("failed to start login process: %w", err))
		return
	}
	st.mu.Lock()
	st.cmd = cmd
	aborted := st.aborted
	st.mu.Unlock()
	if aborted {
		killGroup(cmd) // canceled while the child started
	}

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	// Poll for 2FA requirement while the child runs.
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case werr := <-waitCh:
			// Child exited and reaped: its pid may be reused, so an abort
			// from now on kills nothing.
			st.mu.Lock()
			st.cmd = nil
			st.mu.Unlock()
			// Token files present => success; else classify.
			if st.isAborted() && !hasTokenFiles(id) {
				loginFailed(id, st, errLoginCanceled)
				return
			}
			if hasTokenFiles(id) {
				finishLoginSuccess(id, username, st)
				return
			}
			cause := classifyLoginFailure(werr)
			loginFailed(id, st, cause)
			return
		case <-ticker.C:
			if st.isPending2FA() {
				// 2FA announced; leave the child running (it polls 2fa.txt)
				// and keep waiting for its exit. The HTTP layer will already
				// have returned code 2 to the client; nothing else to do.
				continue
			}
		}
	}
}

// classifyLoginFailure maps a child exit to a human-readable cause.
func classifyLoginFailure(werr error) error {
	exitCode := -1
	if werr != nil {
		var ee interface{ ExitCode() int }
		if errors.As(werr, &ee) {
			exitCode = ee.ExitCode()
		}
	}
	switch exitCode {
	case 0:
		return errors.New("login process exited without caching tokens")
	default:
		return fmt.Errorf("login failed (exit code %d)", exitCode)
	}
}

// waitForLoginFinish blocks until the in-flight login resolves: the child
// exits (success/failure), or 2FA becomes required. When 2FA is required it
// is reported once (code 2); a second call (the client resubmits with a code)
// blocks until the child exits with the final outcome. When ctx ends first,
// the login is aborted: its child is killed and its instance dir removed
// before this returns, so a retry starts clean. When loginWait passes first,
// the abort runs on after the answer, and a retry waits for it.
func waitForLoginFinish(ctx context.Context, id string, st *LoginState) LoginResult {
	timeout := time.NewTimer(loginWait)
	defer timeout.Stop()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()

	for {
		select {
		case err := <-st.done:
			if err == nil {
				return LoginResult{Code: 0, Msg: "SUCCESS", LoginId: id}
			}
			return LoginResult{Code: -1, Msg: err.Error(), LoginId: id}
		case <-ctx.Done():
			log.Infof("[wrapper %s] the client went away; canceling the login", shortID(id))
			abortLogin(st)
			return LoginResult{Code: -1, Msg: errLoginCanceled.Error(), LoginId: id}
		case <-timeout.C:
			// The answer must beat the write deadline, and the abort may
			// wait for a service that is starting: it ends on its own, and a
			// retry waits for it (startLogin).
			go abortLogin(st)
			return LoginResult{Code: -1, Msg: "login timeout", LoginId: id}
		case <-tick.C:
			if st.claimPending2FA() {
				return LoginResult{Code: 2, Msg: "2fa code require", LoginId: id}
			}
		}
	}
}

func (s *LoginState) isPending2FA() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pending2FA
}

// claimPending2FA reports whether the login just became 2FA-pending and has
// not yet been reported to a client. It returns true exactly once per login.
func (s *LoginState) claimPending2FA() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending2FA && !s.reported2FA {
		s.reported2FA = true
		return true
	}
	return false
}

// login2FARequired marks the login as awaiting a 2FA code.
func login2FARequired(id string) {
	st := getLogin(id)
	if st == nil {
		return
	}
	st.mu.Lock()
	st.pending2FA = true
	st.mu.Unlock()
	log.Infof("[wrapper %s] 2FA code required", shortID(id))
}

// provide2FACode writes the user-supplied code into the instance 2fa.txt,
// which the waiting lite login child polls.
func provide2FACode(id string, code string) error {
	dir := instanceDir(id)
	if err := os.MkdirAll(dir, 0777); err != nil {
		return err
	}
	path := dir + "/2fa.txt"
	return os.WriteFile(path, []byte(strings.TrimSpace(code)), 0777)
}

// finishLoginSuccess is called when tokens are on disk: it starts the service
// process and, once the instance reports ready, resolves the login. username
// is recorded as the instance's Name. It runs within runLoginChild, so the
// login has not ended (exited) until the service has started.
func finishLoginSuccess(id, username string, st *LoginState) {
	syscall.Sync() // the tokens the child wrote, before anything counts on them
	instance := getInstanceOrNew(id)
	instance.NoRestart = false
	instance.Name = username
	if err := startLiteService(instance); err != nil {
		log.Errorf("[wrapper %s] failed to start after login: %v", shortID(id), err)
		loginFailed(id, st, fmt.Errorf("failed to start service: %w", err))
		return
	}
	if countReady() >= ShouldStartInstances {
		setReady(true)
	}
	SaveInstances()
	resolveLogin(id, st, nil)
}

// resolveLogin completes the pending login.
func resolveLogin(id string, st *LoginState, cause error) {
	releaseLogin(id, st)
	select {
	case st.done <- cause:
	default:
	}
}

// loginFailed cleans up the instance data and resolves the pending login with
// an error.
func loginFailed(id string, st *LoginState, cause error) {
	log.Warnf("[wrapper %s] login failed: %v", shortID(id), cause)
	// The dir goes while st still holds the account, under the registry's
	// lock: a retry cannot reserve it and start writing there in between,
	// and a login that replaced st keeps its dir.
	loginRegistry.Lock()
	if loginRegistry.m[id] == st {
		_ = RemoveWrapperDataQuiet(id)
		delete(loginRegistry.m, id)
	}
	loginRegistry.Unlock()
	resolveLogin(id, st, cause)
}

// RemoveWrapperDataQuiet removes an instance data dir without panicking.
func RemoveWrapperDataQuiet(id string) error {
	if err := os.RemoveAll(instanceDir(id)); err != nil {
		return err
	}
	return nil
}

// resolveLogoutID determines the instance id to log out from a request that
// supplies either the account username or the instance UUID (id wins).
func resolveLogoutID(req LogoutRequest) (string, error) {
	if req.Id != "" {
		parsed, err := uuid.FromString(strings.TrimSpace(req.Id))
		if err != nil {
			return "", errors.New("invalid id (expected a UUID)")
		}
		return parsed.String(), nil
	}
	if req.Username == "" {
		return "", errors.New("missing username or id")
	}
	return InstanceID(req.Username), nil
}

// startLogout terminates and removes an account instance identified by either
// its username or its instance UUID.
func startLogout(req LogoutRequest) error {
	id, err := resolveLogoutID(req)
	if err != nil {
		return err
	}
	instance := GetInstance(id)
	if instance == nil {
		return errors.New("no such account")
	}
	instance.NoRestart = true
	if err := KillWrapper(id); err != nil {
		return fmt.Errorf("failed to kill wrapper: %w", err)
	}
	RemoveWrapperData(id)
	// Drop it from the registry now rather than when the process exit is
	// reaped, so the instances.json written here no longer lists it.
	RemoveInstance(instance)
	SaveInstances()
	log.Infof("[wrapper %s] logged out %s", shortID(id), id)
	return nil
}

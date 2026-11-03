// Copyright (c) 2026 AMDL-Web contributors. SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func testConfig() config {
	return config{Host: "127.0.0.1", HostPort: 8080, GuestPort: 8080, MemoryMB: 1024, CPUs: 2,
		Kernel: "/g/vmlinuz-lite-qemu", Initrd: "/g/initrd.cpio.gz"}
}

func argValue(args []string, flag string) string {
	i := slices.Index(args, flag)
	if i < 0 || i+1 >= len(args) {
		return ""
	}
	return args[i+1]
}

func noEnv(string) string { return "" }

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestQemuArgsPerAccel(t *testing.T) {
	for _, tc := range []struct{ accel, wantAccel, wantCPU string }{
		{"kvm", "kvm", "host"},
		{"hvf", "hvf", "host"},
		{"whpx", "whpx,kernel-irqchip=off", "qemu64-v1"},
		{"tcg", "tcg", "max"},
	} {
		a := qemuArgs(testConfig(), tc.accel, "/d/data.img")
		if got := argValue(a, "-accel"); got != tc.wantAccel {
			t.Errorf("%s: -accel = %q, want %q", tc.accel, got, tc.wantAccel)
		}
		if got := argValue(a, "-cpu"); got != tc.wantCPU {
			t.Errorf("%s: -cpu = %q, want %q", tc.accel, got, tc.wantCPU)
		}
	}
}

func TestQemuArgsShape(t *testing.T) {
	a := qemuArgs(testConfig(), "tcg", "/d/data.img")
	checks := map[string]string{
		"-m": "1024", "-smp": "2", "-kernel": "/g/vmlinuz-lite-qemu", "-initrd": "/g/initrd.cpio.gz",
		"-append":  "console=ttyS0 quiet panic=1 net.ifnames=0 biosdevname=0",
		"-display": "none", "-serial": "stdio",
		"-nic":   "user,model=e1000,hostfwd=tcp:127.0.0.1:8080-:8080",
		"-drive": "file=/d/data.img,format=raw,if=virtio",
	}
	for k, want := range checks {
		if got := argValue(a, k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if !slices.Contains(a, "-no-reboot") {
		t.Error("missing -no-reboot")
	}
	for _, x := range a {
		if strings.Contains(x, "fw_cfg") {
			t.Errorf("unexpected fw_cfg argument %q", x)
		}
	}
}

func TestQemuArgsHostAndPorts(t *testing.T) {
	c := testConfig()
	c.Host, c.HostPort, c.GuestPort = "0.0.0.0", 9000, 8081
	if got := argValue(qemuArgs(c, "tcg", "x"), "-nic"); got != "user,model=e1000,hostfwd=tcp:0.0.0.0:9000-:8081" {
		t.Errorf("-nic = %q", got)
	}
}

func TestQemuArgsCommaInImagePath(t *testing.T) {
	got := argValue(qemuArgs(testConfig(), "tcg", "/my,data/data.img"), "-drive")
	if want := "file=/my,,data/data.img,format=raw,if=virtio"; got != want {
		t.Errorf("-drive = %q, want %q", got, want)
	}
}

func TestChooseAccel(t *testing.T) {
	has := func(paths ...string) func(string) bool {
		return func(p string) bool { return slices.Contains(paths, p) }
	}
	for _, tc := range []struct {
		req, goos string
		exists    func(string) bool
		want      string
	}{
		{"auto", "linux", has("/dev/kvm"), "kvm"},
		{"auto", "linux", has(), "tcg"},
		{"auto", "windows", has(), "whpx"},
		{"auto", "darwin", has("/dev/kvm"), "tcg"},
		{"tcg", "linux", has("/dev/kvm"), "tcg"},
		{"hvf", "darwin", has(), "hvf"},
	} {
		if got := chooseAccel(tc.req, tc.goos, tc.exists); got != tc.want {
			t.Errorf("chooseAccel(%s, %s) = %s, want %s", tc.req, tc.goos, got, tc.want)
		}
	}
}

func TestFindQEMUOrder(t *testing.T) {
	assets := "/a"
	bundled := filepath.Join(assets, "qemu", "bin", "qemu-system-x86_64")
	files := map[string]bool{"/flag/qemu": true, "/env/qemu": true, bundled: true}
	exists := func(p string) bool { return files[p] }
	look := func(string) (string, error) { return "/path/qemu-system-x86_64", nil }
	noLook := func(string) (string, error) { return "", errors.New("not found") }

	c, err := parseConfig([]string{"--qemu-bin", "/flag/qemu"}, envOf(map[string]string{"QEMU_BIN": "/env/qemu"}), "/x", "/h", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := findQEMU(c.QEMUBin, assets, "linux", look, exists); got != "/flag/qemu" {
		t.Errorf("flag lost: %s", got)
	}
	c, _ = parseConfig(nil, envOf(map[string]string{"QEMU_BIN": "/env/qemu"}), "/x", "/h", io.Discard)
	if got, _ := findQEMU(c.QEMUBin, assets, "linux", look, exists); got != "/env/qemu" {
		t.Errorf("QEMU_BIN lost: %s", got)
	}
	if got, _ := findQEMU("", assets, "linux", look, exists); got != "/path/qemu-system-x86_64" {
		t.Errorf("PATH lost: %s", got)
	}
	if got, _ := findQEMU("", assets, "linux", noLook, exists); got != bundled {
		t.Errorf("bundled lost: %s", got)
	}
	if _, err := findQEMU("", assets, "linux", noLook, func(string) bool { return false }); err == nil {
		t.Error("expected an error when QEMU is nowhere")
	}
	if _, err := findQEMU("/nope", assets, "linux", noLook, exists); err == nil {
		t.Error("expected an error for a missing configured QEMU")
	}
	winBundled := filepath.Join(assets, "qemu", "bin", "qemu-system-x86_64.exe")
	if got, _ := findQEMU("", assets, "windows", noLook, func(p string) bool { return p == winBundled }); got != winBundled {
		t.Errorf("windows name: %s", got)
	}
}

func TestParseConfigDefaults(t *testing.T) {
	c, err := parseConfig(nil, noEnv, "/opt/bin", "/home/u", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want := config{Host: "127.0.0.1", HostPort: 8080, GuestPort: 8080, MemoryMB: 1024, CPUs: 2, Accel: "auto",
		AssetsDir: filepath.Join("/opt/bin", "guest"), DataDir: filepath.Join("/home/u", ".wrapper-manager"),
		Kernel: filepath.Join("/opt/bin", "guest", "vmlinuz-lite-qemu"), Initrd: filepath.Join("/opt/bin", "guest", "wrapper-manager-initramfs.cpio.gz")}
	if c != want {
		t.Errorf("got %+v\nwant %+v", c, want)
	}
}

func TestParseConfigFlagsAndEnv(t *testing.T) {
	env := envOf(map[string]string{"HOST_PORT": "9001", "GUEST_PORT": "9002", "MEMORY": "2048", "SMP": "4", "QEMU_BIN": "/e/q"})
	c, err := parseConfig(nil, env, "/x", "/h", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.HostPort != 9001 || c.GuestPort != 9002 || c.MemoryMB != 2048 || c.CPUs != 4 || c.QEMUBin != "/e/q" {
		t.Errorf("env fallbacks not applied: %+v", c)
	}
	c, err = parseConfig([]string{"--host-port", "7000", "-guest-port=7001", "--memory", "512", "--smp", "1",
		"--qemu-bin", "/f/q", "--host", "0.0.0.0", "--assets-dir", "/g", "--data-dir", "/d", "--accel", "kvm",
		"--kernel", "/k", "--initrd", "/i"}, env, "/x", "/h", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want := config{Host: "0.0.0.0", HostPort: 7000, GuestPort: 7001, MemoryMB: 512, CPUs: 1, Accel: "kvm",
		QEMUBin: "/f/q", AssetsDir: "/g", DataDir: "/d", Kernel: "/k", Initrd: "/i"}
	if c != want {
		t.Errorf("got %+v\nwant %+v", c, want)
	}
}

func TestParseConfigNasInvocation(t *testing.T) {
	c, err := parseConfig([]string{"--host", "0.0.0.0", "--host-port", "8080", "--assets-dir", "/guest", "--data-dir", "/data"}, noEnv, "/usr/local/bin", "/root", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.Kernel != filepath.Join("/guest", "vmlinuz-lite-qemu") || c.Initrd != filepath.Join("/guest", "wrapper-manager-initramfs.cpio.gz") || c.DataDir != "/data" {
		t.Errorf("%+v", c)
	}
}

func TestParseConfigRejectsBadValues(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		env  map[string]string
	}{
		{"port zero", []string{"--host-port", "0"}, nil},
		{"port too big", []string{"--guest-port", "70000"}, nil},
		{"port text", []string{"--host-port", "x"}, nil},
		{"memory negative", []string{"--memory", "-5"}, nil},
		{"smp zero", []string{"--smp", "0"}, nil},
		{"accel unknown", []string{"--accel", "xen"}, nil},
		{"empty host", []string{"--host", ""}, nil},
		{"unknown flag", []string{"--nope"}, nil},
		{"env port", nil, map[string]string{"HOST_PORT": "abc"}},
		{"env memory", nil, map[string]string{"MEMORY": "0"}},
		{"env smp", nil, map[string]string{"SMP": "two"}},
	} {
		_, err := parseConfig(tc.args, envOf(tc.env), "/x", "/h", io.Discard)
		if !errors.Is(err, errUsage) {
			t.Errorf("%s: err = %v, want a usage error", tc.name, err)
		}
	}
}

func TestRealMainUsageErrorExits2(t *testing.T) {
	var out, errb bytes.Buffer
	if code := realMain([]string{"--smp", "0"}, noEnv, &out, &errb); code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
}

func TestRealMainMissingGuestFilesExits1(t *testing.T) {
	var out, errb bytes.Buffer
	dir := t.TempDir()
	code := realMain([]string{"--assets-dir", dir, "--data-dir", filepath.Join(dir, "data")}, noEnv, &out, &errb)
	if code != 1 || !strings.Contains(errb.String(), "vmlinuz-lite-qemu") {
		t.Errorf("exit = %d, stderr = %q", code, errb.String())
	}
}

func TestRealMainNoQEMUExits1(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"vmlinuz-lite-qemu", "wrapper-manager-initramfs.cpio.gz"} {
		os.WriteFile(filepath.Join(dir, f), nil, 0o600)
	}
	t.Setenv("PATH", t.TempDir())
	var out, errb bytes.Buffer
	code := realMain([]string{"--assets-dir", dir, "--data-dir", filepath.Join(dir, "data")}, noEnv, &out, &errb)
	if code != 1 || !strings.Contains(errb.String(), "not found") {
		t.Errorf("exit = %d, stderr = %q", code, errb.String())
	}
}

func TestEnsureDataImage(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "data")
	img, err := ensureDataImage(dir)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(img)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != 512<<20 {
		t.Errorf("size = %d, want 512 MiB", fi.Size())
	}
	if runtime.GOOS != "windows" {
		if di, _ := os.Stat(dir); di.Mode().Perm() != 0o700 {
			t.Errorf("data dir mode = %v, want 0700", di.Mode().Perm())
		}
	}
	if err := os.WriteFile(img, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureDataImage(dir); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(img); string(b) != "keep" {
		t.Errorf("existing data.img was modified: %q", b)
	}
}

func TestNeedFallback(t *testing.T) {
	for _, tc := range []struct {
		accel       string
		code        int
		sawInit     bool
		interrupted bool
		want        bool
	}{
		{"kvm", 1, false, false, true},
		{"whpx", 1, false, false, true},
		{"hvf", 3, false, false, true},
		{"kvm", 1, true, false, false},
		{"kvm", 0, false, false, false},
		{"tcg", 1, false, false, false},
		{"kvm", 1, false, true, false},
	} {
		if got := needFallback(tc.accel, tc.code, tc.sawInit, tc.interrupted); got != tc.want {
			t.Errorf("needFallback(%+v) = %v", tc, got)
		}
	}
}

func TestInitWatcherSpansWrites(t *testing.T) {
	var out bytes.Buffer
	w := &initWatcher{out: &out}
	w.Write([]byte("booting\n[in"))
	if w.seen {
		t.Fatal("seen too early")
	}
	w.Write([]byte("it] hello\n"))
	if !w.seen {
		t.Error("marker split across writes not seen")
	}
	if out.String() != "booting\n[init] hello\n" {
		t.Errorf("output not passed through: %q", out.String())
	}
}

func fakeRunner(accels *[]string, results ...[2]any) runner {
	return func(bin string, args []string, sigs <-chan os.Signal, out io.Writer) (int, bool, error) {
		*accels = append(*accels, argValue(args, "-accel"))
		r := results[len(*accels)-1]
		return r[0].(int), r[1].(bool), nil
	}
}

func TestLaunchFallsBackToTCG(t *testing.T) {
	var accels []string
	code := launch(testConfig(), "q", "img", "kvm", fakeRunner(&accels, [2]any{1, false}, [2]any{0, true}), nil, io.Discard, io.Discard)
	if code != 0 || !slices.Equal(accels, []string{"kvm", "tcg"}) {
		t.Errorf("code = %d, accels = %v", code, accels)
	}
}

func TestLaunchNoFallbackAfterInit(t *testing.T) {
	var accels []string
	code := launch(testConfig(), "q", "img", "kvm", fakeRunner(&accels, [2]any{1, true}), nil, io.Discard, io.Discard)
	if code != 1 || len(accels) != 1 {
		t.Errorf("code = %d, accels = %v", code, accels)
	}
}

func TestLaunchNoFallbackOnCleanExit(t *testing.T) {
	var accels []string
	code := launch(testConfig(), "q", "img", "whpx", fakeRunner(&accels, [2]any{0, false}), nil, io.Discard, io.Discard)
	if code != 0 || len(accels) != 1 {
		t.Errorf("code = %d, accels = %v", code, accels)
	}
}

func TestLaunchReturnsFallbackExitCode(t *testing.T) {
	var accels []string
	code := launch(testConfig(), "q", "img", "kvm", fakeRunner(&accels, [2]any{1, false}, [2]any{7, true}), nil, io.Discard, io.Discard)
	if code != 7 || len(accels) != 2 {
		t.Errorf("code = %d, accels = %v", code, accels)
	}
}

func writeFakeQEMU(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake QEMU")
	}
	p := filepath.Join(t.TempDir(), "fake-qemu")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLaunchWithFakeQEMUFallback(t *testing.T) {
	log := filepath.Join(t.TempDir(), "calls")
	bin := writeFakeQEMU(t, `echo "$2" >> `+log+`
case "$2" in
tcg) echo "[init] guest up"; exit 0;;
*) echo "accel unavailable" >&2; exit 1;;
esac
`)
	var out, errb bytes.Buffer
	code := launch(testConfig(), bin, "img", "kvm", runQEMU, nil, &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, errb.String())
	}
	b, _ := os.ReadFile(log)
	if string(b) != "kvm\ntcg\n" {
		t.Errorf("accelerators tried: %q", b)
	}
	if !strings.Contains(out.String(), "accel unavailable") || !strings.Contains(out.String(), "[init] guest up") {
		t.Errorf("combined output not copied: %q", out.String())
	}
}

func TestLaunchWithFakeQEMUNoFallbackAfterInit(t *testing.T) {
	log := filepath.Join(t.TempDir(), "calls")
	bin := writeFakeQEMU(t, `echo "$2" >> `+log+`
echo "[init] guest up"; exit 3
`)
	code := launch(testConfig(), bin, "img", "kvm", runQEMU, nil, io.Discard, io.Discard)
	b, _ := os.ReadFile(log)
	if code != 3 || string(b) != "kvm\n" {
		t.Errorf("exit = %d, tried %q", code, b)
	}
}

func TestLaunchForwardsSignal(t *testing.T) {
	bin := writeFakeQEMU(t, `trap 'echo got-term; exit 0' TERM
echo "[init] up"
while :; do sleep 0.1; done
`)
	sigs := make(chan os.Signal, 1)
	var out syncBuffer
	done := make(chan int, 1)
	go func() { done <- launch(testConfig(), bin, "img", "kvm", runQEMU, sigs, &out, io.Discard) }()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(out.String(), "[init] up") {
		if time.Now().After(deadline) {
			t.Fatal("fake QEMU never started")
		}
		time.Sleep(20 * time.Millisecond)
	}
	sigs <- syscall.SIGTERM
	select {
	case code := <-done:
		if code != 0 || !strings.Contains(out.String(), "got-term") {
			t.Errorf("exit = %d, output %q", code, out.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("launcher did not exit after SIGTERM")
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

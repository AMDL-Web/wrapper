// Copyright (c) 2026 AMDL-Web contributors. SPDX-License-Identifier: MIT

// Command wrapper-manager-qemu runs the wrapper-manager guest in QEMU on a
// plain host: it finds QEMU, picks an accelerator, keeps a persistent data
// disk and forwards a host port to the guest's manager.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
)

// runner runs one QEMU process to its end and reports its exit code and
// whether the guest printed an "[init]" line. A signal received on sigs is
// forwarded to QEMU.
type runner func(bin string, args []string, sigs <-chan os.Signal, out io.Writer) (exitCode int, sawInit bool, err error)

func main() {
	os.Exit(realMain(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

// realMain is main with its inputs injected; it returns the exit status.
func realMain(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	exe, _ := os.Executable()
	home, _ := os.UserHomeDir()
	c, err := parseConfig(args, getenv, filepath.Dir(exe), home, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintln(stderr, "wrapper-manager-qemu:", err)
		return 2
	}
	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }
	for _, f := range []string{c.Kernel, c.Initrd} {
		if !exists(f) {
			fmt.Fprintf(stderr, "wrapper-manager-qemu: guest file %s is missing (see --assets-dir, --kernel, --initrd)\n", f)
			return 1
		}
	}
	bin, err := findQEMU(c.QEMUBin, c.AssetsDir, runtime.GOOS, exec.LookPath, exists)
	if err != nil {
		fmt.Fprintln(stderr, "wrapper-manager-qemu:", err)
		return 1
	}
	image, err := ensureDataImage(c.DataDir)
	if err != nil {
		fmt.Fprintln(stderr, "wrapper-manager-qemu: data disk:", err)
		return 1
	}
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	accel := chooseAccel(c.Accel, runtime.GOOS, exists)
	return launch(c, bin, image, accel, runQEMU, sigs, stdout, stderr)
}

// launch runs the guest with accel and, if that accelerator fails to
// initialise, once more with plain emulation. It returns QEMU's exit status.
func launch(c config, bin, image, accel string, run runner, sigs <-chan os.Signal, stdout, stderr io.Writer) int {
	interrupted := make(chan struct{})
	relay := make(chan os.Signal, 4)
	go func() {
		defer close(relay)
		for s := range sigs {
			select {
			case <-interrupted:
			default:
				close(interrupted)
			}
			relay <- s
		}
	}()
	fmt.Fprintf(stderr, "wrapper-manager-qemu: %s, accelerator %s, port %s:%d -> guest %d\n", bin, accel, c.Host, c.HostPort, c.GuestPort)
	code, sawInit, err := run(bin, qemuArgs(c, accel, image), relay, stdout)
	if err != nil {
		fmt.Fprintln(stderr, "wrapper-manager-qemu:", err)
		return 1
	}
	wasInterrupted := false
	select {
	case <-interrupted:
		wasInterrupted = true
	default:
	}
	if needFallback(accel, code, sawInit, wasInterrupted) {
		fmt.Fprintf(stderr, "wrapper-manager-qemu: accelerator %s failed to start (exit %d); retrying with tcg\n", accel, code)
		code, _, err = run(bin, qemuArgs(c, "tcg", image), relay, stdout)
		if err != nil {
			fmt.Fprintln(stderr, "wrapper-manager-qemu:", err)
			return 1
		}
	}
	return code
}

// runQEMU starts QEMU, copies its combined output to out, forwards signals
// and waits for it to exit.
func runQEMU(bin string, args []string, sigs <-chan os.Signal, out io.Writer) (int, bool, error) {
	w := &initWatcher{out: out}
	cmd := exec.Command(bin, args...)
	cmd.Stdout, cmd.Stderr = w, w
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		return 0, false, fmt.Errorf("start %s: %w", bin, err)
	}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go func() {
		for {
			select {
			case s, ok := <-sigs:
				if !ok {
					return
				}
				if err := cmd.Process.Signal(s); err != nil {
					cmd.Process.Kill()
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	err := cmd.Wait()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0, w.seen, nil
	case errors.As(err, &ee):
		code := ee.ExitCode()
		if code < 0 {
			code = 1
		}
		return code, w.seen, nil
	}
	return 0, w.seen, err
}

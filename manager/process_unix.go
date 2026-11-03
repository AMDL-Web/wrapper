// Copyright (c) 2026 AMDL-Web contributors. SPDX-License-Identifier: MIT

//go:build unix

package main

import (
	"os"
	"os/exec"
	"syscall"
)

// ownGroup makes cmd the leader of a new process group.
func ownGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// signalGroup sends SIGKILL (kill) or SIGTERM to cmd's process group.
func signalGroup(cmd *exec.Cmd, kill bool) {
	if cmd.Process == nil {
		return
	}
	sig := syscall.SIGTERM
	if kill {
		sig = syscall.SIGKILL
	}
	_ = syscall.Kill(-cmd.Process.Pid, sig) // a group already gone is fine
}

// syncDisks flushes every file system to disk.
func syncDisks() { syscall.Sync() }

// syncDir makes a rename inside dir durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

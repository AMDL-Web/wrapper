// Copyright (c) 2026 AMDL-Web contributors. SPDX-License-Identifier: MIT

//go:build !unix

package main

import "os/exec"

// The manager runs wrapper-lite, which needs Linux; these keep the package
// building elsewhere.

func ownGroup(*exec.Cmd) {}

func signalGroup(cmd *exec.Cmd, _ bool) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func syncDisks() {}

func syncDir(string) error { return nil }

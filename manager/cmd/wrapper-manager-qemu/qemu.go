// Copyright (c) 2026 AMDL-Web contributors. SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// qemuArgs builds QEMU's arguments for the manager guest. It passes no
// fw_cfg item, so the guest uses its default manager arguments.
func qemuArgs(c config, accel, image string) []string {
	acc, cpu := accel, "max"
	switch accel {
	case "kvm", "hvf":
		cpu = "host"
	case "whpx":
		acc, cpu = "whpx,kernel-irqchip=off", "qemu64-v1"
	}
	return []string{
		"-accel", acc, "-cpu", cpu,
		"-m", strconv.Itoa(c.MemoryMB), "-smp", strconv.Itoa(c.CPUs),
		"-kernel", c.Kernel, "-initrd", c.Initrd,
		"-append", "console=ttyS0 quiet panic=1 net.ifnames=0 biosdevname=0",
		"-display", "none", "-serial", "stdio", "-no-reboot",
		"-nic", fmt.Sprintf("user,model=e1000,hostfwd=tcp:%s:%d-:%d", c.Host, c.HostPort, c.GuestPort),
		"-drive", "file=" + optValue(image) + ",format=raw,if=virtio",
	}
}

// optValue escapes a path inside a QEMU option list, where a comma separates
// options and ",," is a literal comma.
func optValue(s string) string { return strings.ReplaceAll(s, ",", ",,") }

// needFallback reports whether a failed run should be retried with plain
// emulation: a hardware accelerator, a non-zero exit, and no guest line yet.
func needFallback(accel string, exitCode int, sawInit, interrupted bool) bool {
	return accel != "tcg" && exitCode != 0 && !sawInit && !interrupted
}

// initWatcher copies QEMU's output to out and notes whether the guest has
// printed a line containing "[init]".
type initWatcher struct {
	out  io.Writer
	tail []byte
	seen bool
}

const initMarker = "[init]"

// Write forwards p and scans it, together with the end of the previous write,
// for the marker.
func (w *initWatcher) Write(p []byte) (int, error) {
	if !w.seen {
		buf := append(w.tail, p...)
		if bytes.Contains(buf, []byte(initMarker)) {
			w.seen, w.tail = true, nil
		} else if n := len(initMarker) - 1; len(buf) > n {
			w.tail = append([]byte(nil), buf[len(buf)-n:]...)
		} else {
			w.tail = append([]byte(nil), buf...)
		}
	}
	return w.out.Write(p)
}

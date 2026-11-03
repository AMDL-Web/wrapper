// Copyright (c) 2026 AMDL-Web contributors. SPDX-License-Identifier: MIT

package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
)

const qemuName = "qemu-system-x86_64"

// config is the resolved launcher configuration.
type config struct {
	Host      string
	HostPort  int
	GuestPort int
	MemoryMB  int
	CPUs      int
	Accel     string
	QEMUBin   string
	AssetsDir string
	DataDir   string
	Kernel    string
	Initrd    string
}

// errUsage marks a bad flag or environment value; the launcher exits 2.
var errUsage = errors.New("usage error")

var accels = map[string]bool{"auto": true, "kvm": true, "whpx": true, "hvf": true, "tcg": true}

// parseConfig resolves the flags in args, falling back to the environment for
// the options that have a variable, then to defaults. exeDir and home supply
// the default assets and data directories.
func parseConfig(args []string, getenv func(string) string, exeDir, home string, usageOut io.Writer) (config, error) {
	fs := flag.NewFlagSet("wrapper-manager-qemu", flag.ContinueOnError)
	fs.SetOutput(usageOut)
	var c config
	var hostPort, guestPort, memory, smp, qemuBin string
	fs.StringVar(&c.Host, "host", "127.0.0.1", "address the forwarded port binds to")
	fs.StringVar(&hostPort, "host-port", "", "host port forwarded to the guest (env HOST_PORT, default 8080)")
	fs.StringVar(&guestPort, "guest-port", "", "guest port the manager listens on (env GUEST_PORT, default 8080)")
	fs.StringVar(&memory, "memory", "", "guest memory in MiB (env MEMORY, default 1024)")
	fs.StringVar(&smp, "smp", "", "guest CPUs (env SMP, default 2)")
	fs.StringVar(&c.Accel, "accel", "auto", "accelerator: auto, kvm, whpx, hvf or tcg")
	fs.StringVar(&qemuBin, "qemu-bin", "", "path to qemu-system-x86_64 (env QEMU_BIN)")
	fs.StringVar(&c.AssetsDir, "assets-dir", filepath.Join(exeDir, "guest"), "directory with the guest kernel and initramfs")
	fs.StringVar(&c.DataDir, "data-dir", filepath.Join(home, ".wrapper-manager"), "directory holding data.img")
	fs.StringVar(&c.Kernel, "kernel", "", "guest kernel (default <assets-dir>/vmlinuz-lite-qemu)")
	fs.StringVar(&c.Initrd, "initrd", "", "guest initramfs (default <assets-dir>/wrapper-manager-initramfs.cpio.gz)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return c, err
		}
		return c, fmt.Errorf("%w: %v", errUsage, err)
	}
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	pick := func(name, flagVal, envName, def string) string {
		if given[name] && flagVal != "" {
			return flagVal
		}
		if v := getenv(envName); v != "" {
			return v
		}
		return def
	}
	var err error
	if c.HostPort, err = port("host-port", pick("host-port", hostPort, "HOST_PORT", "8080")); err != nil {
		return c, err
	}
	if c.GuestPort, err = port("guest-port", pick("guest-port", guestPort, "GUEST_PORT", "8080")); err != nil {
		return c, err
	}
	if c.MemoryMB, err = positive("memory", pick("memory", memory, "MEMORY", "1024")); err != nil {
		return c, err
	}
	if c.CPUs, err = positive("smp", pick("smp", smp, "SMP", "2")); err != nil {
		return c, err
	}
	c.QEMUBin = pick("qemu-bin", qemuBin, "QEMU_BIN", "")
	if c.Host == "" {
		return c, fmt.Errorf("%w: --host must not be empty", errUsage)
	}
	if !accels[c.Accel] {
		return c, fmt.Errorf("%w: unknown --accel %q (auto, kvm, whpx, hvf, tcg)", errUsage, c.Accel)
	}
	if c.Kernel == "" {
		c.Kernel = filepath.Join(c.AssetsDir, "vmlinuz-lite-qemu")
	}
	if c.Initrd == "" {
		c.Initrd = filepath.Join(c.AssetsDir, "wrapper-manager-initramfs.cpio.gz")
	}
	return c, nil
}

func port(name, s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 65535 {
		return 0, fmt.Errorf("%w: %s must be a port from 1 to 65535, got %q", errUsage, name, s)
	}
	return n, nil
}

func positive(name, s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%w: %s must be a positive number, got %q", errUsage, name, s)
	}
	return n, nil
}

// findQEMU returns the QEMU to run: the configured one (flag or QEMU_BIN),
// else qemu-system-x86_64 on PATH, else <assets>/qemu/bin.
func findQEMU(configured, assetsDir, goos string, lookPath func(string) (string, error), exists func(string) bool) (string, error) {
	if configured != "" {
		if exists(configured) {
			return configured, nil
		}
		if p, err := lookPath(configured); err == nil {
			return p, nil
		}
		return "", fmt.Errorf("the configured QEMU %s does not exist", configured)
	}
	name := qemuName
	if goos == "windows" {
		name += ".exe"
	}
	if p, err := lookPath(name); err == nil {
		return p, nil
	}
	if p := filepath.Join(assetsDir, "qemu", "bin", name); exists(p) {
		return p, nil
	}
	return "", fmt.Errorf("%s not found: install QEMU, put it on PATH, or pass --qemu-bin", name)
}

// chooseAccel resolves --accel auto: KVM on Linux when /dev/kvm exists, WHPX
// on Windows, plain emulation otherwise (macOS included).
func chooseAccel(requested, goos string, exists func(string) bool) string {
	if requested != "auto" {
		return requested
	}
	switch {
	case goos == "linux" && exists("/dev/kvm"):
		return "kvm"
	case goos == "windows":
		return "whpx"
	}
	return "tcg"
}

// ensureDataImage creates the data directory and a sparse 512 MiB data.img
// when the image is missing; an existing image is never touched.
func ensureDataImage(dataDir string) (string, error) {
	img := filepath.Join(dataDir, "data.img")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", err
	}
	f, err := os.OpenFile(img, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return img, nil
	}
	if err != nil {
		return "", err
	}
	if err := f.Truncate(dataImageSize); err != nil {
		f.Close()
		return "", err
	}
	return img, f.Close()
}

const dataImageSize = 512 << 20

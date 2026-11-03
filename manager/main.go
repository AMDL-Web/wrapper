// Copyright (c) 2026 AMDL-Web contributors. SPDX-License-Identifier: MIT

// Command wrapper-manager runs one wrapper-lite process per Apple Music
// account behind one HTTP port. It keeps its state in data/ under the working
// directory.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// config is the parsed command line.
type config struct {
	host, proxy, launcher     string
	port, adminPort           int
	payloadURL, payloadSHA256 string
	prepare, updateWrapper    bool
	// guest enables POST /shutdown (WRAPPER_MANAGER_GUEST=1).
	guest bool
	// dataDir is the absolute path of data/ in the working directory.
	dataDir string
}

var hexDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)

// usageError is a flag error: exit status 2.
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

// parseFlags reads args into a config.
func parseFlags(args []string, stderr io.Writer) (config, error) {
	fs := flag.NewFlagSet("wrapper-manager", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var c config
	fs.StringVar(&c.host, "host", "localhost", "bind address of the main HTTP server")
	fs.IntVar(&c.port, "port", 8080, "port of the main HTTP server")
	fs.StringVar(&c.proxy, "proxy", "", "proxy URL for outbound requests, also passed to wrapper-lite")
	fs.StringVar(&c.launcher, "launcher", "rootless", "rootless (user namespaces) or root")
	fs.StringVar(&c.payloadURL, "payload-url", "", "wrapper-lite payload zip to install instead of the default")
	fs.StringVar(&c.payloadSHA256, "payload-sha256", "", "required sha256 of the payload zip (needs -payload-url)")
	fs.BoolVar(&c.prepare, "prepare", false, "install the payload and exit")
	fs.BoolVar(&c.updateWrapper, "update-wrapper", false, "download and install the payload unconditionally and exit")
	fs.IntVar(&c.adminPort, "admin-port", 0, "port of the admin API (snapshot, restore); 0 is off; never expose it")
	if err := fs.Parse(args); err != nil {
		return c, usageError{err.Error()}
	}
	if fs.NArg() > 0 {
		return c, usageError{fmt.Sprintf("unexpected argument %q", fs.Arg(0))}
	}
	if c.launcher != "rootless" && c.launcher != "root" {
		return c, usageError{fmt.Sprintf("-launcher must be rootless or root, not %q", c.launcher)}
	}
	if c.payloadSHA256 != "" {
		if c.payloadURL == "" {
			return c, usageError{"-payload-sha256 needs -payload-url"}
		}
		if !hexDigest.MatchString(c.payloadSHA256) {
			return c, usageError{"-payload-sha256 must be 64 lowercase hex digits"}
		}
	}
	if c.port < 0 || c.port > 65535 || c.adminPort < 0 || c.adminPort > 65535 {
		return c, usageError{"ports must be between 0 and 65535"}
	}
	if c.proxy != "" {
		if u, err := url.Parse(c.proxy); err != nil || u.Scheme == "" || u.Host == "" {
			return c, usageError{fmt.Sprintf("-proxy %q is not a URL", c.proxy)}
		}
	}
	return c, nil
}

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

// run is the whole program; it returns the exit status.
func run(args []string, stderr io.Writer) int {
	cfg, err := parseFlags(args, stderr)
	if err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(stderr, "wrapper-manager:", err)
		}
		return 2
	}
	cfg.guest = os.Getenv("WRAPPER_MANAGER_GUEST") == "1"
	if cfg.dataDir, err = filepath.Abs("data"); err != nil {
		fmt.Fprintln(stderr, "wrapper-manager:", err)
		return 1
	}
	logger := log.New(os.Stdout, "", log.LstdFlags)
	p := newPayload(cfg, logger)
	if cfg.payloadURL == "" && runtime.GOARCH != "amd64" {
		if !p.installed() {
			fmt.Fprintf(stderr, "wrapper-manager: there is no default wrapper-lite payload for %s; pass -payload-url\n", runtime.GOARCH)
			return 2
		}
		p.acceptInstalled = true
	}

	if cfg.updateWrapper {
		if err := p.update(context.Background()); err != nil {
			logger.Printf("updating wrapper-lite failed: %v", err)
			return 1
		}
		logger.Printf("wrapper-lite updated from %s", p.source())
		return 0
	}
	if err := p.ensure(context.Background()); err != nil {
		logger.Printf("wrapper-lite cannot be installed: %v", err)
		return 1
	}
	if cfg.prepare {
		logger.Printf("wrapper-lite is installed in %s; nothing else to do", p.dir)
		return 0
	}
	return serve(cfg, logger)
}

// serve runs the manager until a signal or POST /shutdown.
func serve(cfg config, logger *log.Logger) int {
	m := newManager(cfg, logger)
	m.boot()

	stop := make(chan struct{})
	var stopOnce sync.Once
	m.requestStop = func() { stopOnce.Do(func() { close(stop) }) }

	mainLn, err := net.Listen("tcp", net.JoinHostPort(cfg.host, strconv.Itoa(cfg.port)))
	if err != nil {
		logger.Printf("listen on %s:%d: %v", cfg.host, cfg.port, err)
		m.stopAll(3 * time.Second)
		return 1
	}
	servers := []*http.Server{m.mainServer()}
	listeners := []net.Listener{mainLn}
	if cfg.adminPort != 0 {
		adminLn, err := net.Listen("tcp", net.JoinHostPort(cfg.host, strconv.Itoa(cfg.adminPort)))
		if err != nil {
			logger.Printf("listen on %s:%d: %v", cfg.host, cfg.adminPort, err)
			mainLn.Close()
			m.stopAll(3 * time.Second)
			return 1
		}
		servers = append(servers, m.adminServer())
		listeners = append(listeners, adminLn)
	}
	logger.Printf("wrapperManager running at %s:%d", cfg.host, cfg.port)
	if cfg.adminPort != 0 {
		logger.Printf("admin API at %s:%d", cfg.host, cfg.adminPort)
	}
	failed := make(chan error, len(servers))
	for i, s := range servers {
		go func() {
			if err := s.Serve(listeners[i]); err != nil && !errors.Is(err, http.ErrServerClosed) {
				failed <- err
			}
		}()
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	status := 0
	select {
	case s := <-sigs:
		logger.Printf("received %s, stopping", s)
	case <-stop:
		logger.Printf("stopping")
	case err := <-failed:
		logger.Printf("server failed: %v", err)
		status = 1
	}
	signal.Stop(sigs)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for _, s := range servers {
		wg.Go(func() { _ = s.Shutdown(ctx) })
	}
	wg.Go(func() { m.stopAll(3 * time.Second) })
	wg.Wait()
	syncDisks()
	logger.Printf("stopped")
	return status
}

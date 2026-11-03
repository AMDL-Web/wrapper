// Copyright (c) 2026 AMDL-Web contributors. SPDX-License-Identifier: MIT

package main

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"
)

// process is one launcher started in its own process group, so that one
// signal to the group reaches the launcher and the wrapper-lite it forked.
type process struct {
	cmd  *exec.Cmd
	done chan struct{} // closed once the launcher has exited
	// output is closed once every line was handed on: the last process
	// holding the pipe has exited.
	output chan struct{}
	err    error // the launcher's exit, valid after done
}

// startProcess runs the launcher at path with args in dir, handing each line
// of its standard output and error, without the line ending, to onLine.
func startProcess(path, dir string, args []string, onLine func(string)) (*process, error) {
	cmd := exec.Command(path, args...)
	cmd.Dir = dir
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = w, w
	ownGroup(cmd)
	if err := cmd.Start(); err != nil {
		r.Close()
		w.Close()
		return nil, err
	}
	w.Close()
	p := &process{cmd: cmd, done: make(chan struct{}), output: make(chan struct{})}
	go func() {
		defer close(p.output)
		defer r.Close()
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			onLine(strings.TrimRight(sc.Text(), "\r"))
		}
	}()
	go func() {
		p.err = cmd.Wait()
		close(p.done)
	}()
	return p, nil
}

// exited reports whether the launcher has exited.
func (p *process) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// exitCode is the launcher's exit status, -1 when a signal ended it or it is
// unknown. Valid after done.
func (p *process) exitCode() int {
	var ee *exec.ExitError
	if p.err == nil {
		return 0
	}
	if errors.As(p.err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// kill sends SIGKILL to the whole group and waits for the launcher. A group
// member that outlives the launcher is killed too.
func (p *process) kill() {
	signalGroup(p.cmd, true)
	<-p.done
	signalGroup(p.cmd, true)
}

// terminate asks the group to stop (SIGTERM) and kills it after grace.
func (p *process) terminate(grace time.Duration) {
	signalGroup(p.cmd, false)
	select {
	case <-p.done:
	case <-time.After(grace):
	}
	p.kill()
}

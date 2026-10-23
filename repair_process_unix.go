//go:build !windows

package main

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type repairProcess struct {
	pid, parent, group int
	started            string
}

func runRepairCommand(cmd *exec.Cmd, agent string) error {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.WaitDelay = 5 * time.Second
	var mu sync.Mutex
	owned := make(map[int]repairProcess)
	rootObserved := false
	capture := func() map[int]repairProcess {
		current := repairProcesses()
		if current == nil {
			return nil
		}
		mu.Lock()
		defer mu.Unlock()
		if !rootObserved {
			if root, ok := current[cmd.Process.Pid]; ok {
				owned[root.pid] = root
			}
			rootObserved = true
		}
		for {
			added := false
			for _, process := range current {
				parent, ok := owned[process.parent]
				if !ok || current[parent.pid].started != parent.started || owned[process.pid].started == process.started {
					continue
				}
				owned[process.pid] = process
				added = true
			}
			if !added {
				return current
			}
		}
	}
	cmd.Cancel = func() error {
		capture()
		signal := syscall.SIGINT
		if agent == "claude" {
			signal = syscall.SIGTERM
		}
		return cmd.Process.Signal(signal)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	capture()
	stop, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				capture()
			}
		}
	}()
	err := cmd.Wait()
	close(stop)
	<-stopped
	// Sample while the CLI runs so an abrupt exit cannot orphan every known
	// tool. Identity checks are best effort, not containment of escaping processes.
	current := capture()
	mu.Lock()
	defer mu.Unlock()
	for _, child := range owned {
		live, ok := current[child.pid]
		if !ok || live.started != child.started {
			continue
		}
		if live.group == child.group && child.group == child.pid {
			_ = syscall.Kill(-child.group, syscall.SIGKILL)
		} else {
			_ = syscall.Kill(child.pid, syscall.SIGKILL)
		}
	}
	// Wait has reaped the original root; a process now using its PID is unrelated.
	if _, reused := current[cmd.Process.Pid]; current != nil && !reused {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	return err
}

func repairProcesses() map[int]repairProcess {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/ps", "-axo", "pid=,ppid=,pgid=,lstart=")
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	processes := make(map[int]repairProcess)
	for line := range strings.SplitSeq(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 8 {
			continue
		}
		pid, pidErr := strconv.Atoi(fields[0])
		parent, parentErr := strconv.Atoi(fields[1])
		group, groupErr := strconv.Atoi(fields[2])
		if pidErr == nil && parentErr == nil && groupErr == nil && pid > 1 && group > 1 {
			processes[pid] = repairProcess{pid, parent, group, strings.Join(fields[3:], " ")}
		}
	}
	return processes
}

package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func runRepairCommand(cmd *exec.Cmd, _ string) error {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return fmt.Errorf("creating agent process job: %w", err)
	}
	defer windows.CloseHandle(job)
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		return fmt.Errorf("configuring agent process job: %w", err)
	}
	cmd.WaitDelay = 5 * time.Second
	cmd.Cancel = func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		kill := exec.CommandContext(ctx, "taskkill.exe", "/PID", strconv.Itoa(cmd.Process.Pid), "/T", "/F")
		kill.WaitDelay = time.Second
		if err := kill.Run(); err != nil {
			return errors.Join(err, cmd.Process.Kill())
		}
		return nil
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err == nil {
		err = windows.AssignProcessToJobObject(job, process)
		windows.CloseHandle(process)
	}
	if err != nil {
		_ = cmd.Cancel()
		return errors.Join(fmt.Errorf("assigning agent process job: %w", err), cmd.Wait())
	}
	return cmd.Wait()
}

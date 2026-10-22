package helmcli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
)

// ExitError retains the underlying Helm exit status, including signal termination.
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string { return e.Err.Error() }
func (e *ExitError) Unwrap() error { return e.Err }

func execute(ctx context.Context, binary string, args, env []string, streams Streams) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// binary is the validated local HELM_BIN; arguments are passed without a shell.
	// nosemgrep: cache.semgrep-rules.go.lang.security.audit.dangerous-exec-command
	cmd := exec.Command(binary, args...)
	cmd.WaitDelay = 3 * time.Second
	cmd.Env = env
	cmd.Stdin = streams.In
	cmd.Stdout = streams.Out
	cmd.Stderr = streams.Err
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var timer *time.Timer
	var timeout <-chan time.Time
	cancellation := ctx.Done()
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	terminate := func(sig syscall.Signal) {
		_ = syscall.Kill(-cmd.Process.Pid, sig)
		if timer == nil {
			timer = time.NewTimer(3 * time.Second)
			timeout = timer.C
		}
	}
	for {
		select {
		case err := <-done:
			// Reap the child and stop any descendants it left in our private process group.
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			if err == nil {
				return nil
			}
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				code := exit.ExitCode()
				if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
					code = 128 + int(status.Signal())
				}
				return &ExitError{Code: code, Err: err}
			}
			return err
		case sig := <-signals:
			if unix, ok := sig.(syscall.Signal); ok {
				terminate(unix)
			}
		case <-cancellation:
			cancellation = nil
			terminate(syscall.SIGTERM)
		case <-timeout:
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			timeout = nil
		}
	}
}

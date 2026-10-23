package helmcli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestProcessHelper(t *testing.T) {
	if os.Getenv("BALE_TEST_CHILD") == "" {
		return
	}
	mode := os.Getenv("BALE_TEST_CHILD")
	switch mode {
	case "status":
		_, _ = os.Stdout.WriteString("stdout")
		_, _ = os.Stderr.WriteString("stderr")
		os.Exit(23)
	case "term", "ignore":
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGTERM, os.Interrupt)
		ready := os.Getenv("BALE_TEST_READY")
		if err := os.WriteFile(ready+".tmp", []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
			os.Exit(99)
		}
		// Publish readiness only after the complete PID has been written and closed.
		if err := os.Rename(ready+".tmp", ready); err != nil {
			os.Exit(99)
		}
		for sig := range ch {
			if mode == "ignore" {
				continue
			}
			if sig == os.Interrupt {
				os.Exit(42)
			}
			os.Exit(41)
		}
	}
	os.Exit(98)
}
func TestExecutePreservesExitAndStreams(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	err = execute(context.Background(), self, []string{"-test.run=^TestProcessHelper$"}, append(os.Environ(), "BALE_TEST_CHILD=status"), Streams{Out: &out, Err: &stderr})
	var exit *ExitError
	if !errors.As(err, &exit) || exit.Code != 23 || out.String() != "stdout" || stderr.String() != "stderr" {
		t.Fatalf("%v %s %s", err, out.String(), stderr.String())
	}
}
func TestExecuteCancellationAndSignals(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		sig    syscall.Signal
		want   int
		ignore bool
	}{{"cancel", 0, 41, false}, {"interrupt", syscall.SIGINT, 42, false}, {"terminate", syscall.SIGTERM, 41, false}, {"uncooperative", 0, 137, true}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ready := t.TempDir() + "/ready"
			mode := "term"
			if tc.ignore {
				mode = "ignore"
			}
			env := append(os.Environ(), "BALE_TEST_CHILD="+mode, "BALE_TEST_READY="+ready)
			done := make(chan error, 1)
			go func() { done <- execute(ctx, self, []string{"-test.run=^TestProcessHelper$"}, env, Streams{}) }()
			deadline := time.Now().Add(5 * time.Second)
			var raw []byte
			for time.Now().Before(deadline) {
				raw, err = os.ReadFile(ready)
				if err == nil {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if len(raw) == 0 {
				cancel()
				t.Fatal("child did not start")
			}
			if tc.sig == 0 {
				cancel()
			} else {
				if err := syscall.Kill(os.Getpid(), tc.sig); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case err := <-done:
				var exit *ExitError
				if !errors.As(err, &exit) || exit.Code != tc.want {
					t.Fatalf("%v", err)
				}
			case <-time.After(6 * time.Second):
				t.Fatal("child not reaped")
			}
			pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
			if err != nil {
				t.Fatal(err)
			}
			if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
				t.Fatalf("child remains: %v", err)
			}
		})
	}
}

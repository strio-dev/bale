//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestCommandPipeCancellation(t *testing.T) {
	for _, write := range []bool{false, true} {
		t.Run(fmt.Sprintf("write=%t", write), func(t *testing.T) {
			reader, writer, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = reader.Close(); _ = writer.Close() }()
			original := reader
			if write {
				original = writer
			}
			// Inherited CLI pipes start in blocking mode, unlike os.Pipe's Go
			// runtime-managed descriptors. Fd reproduces that inherited state.
			fd := original.Fd()
			before, err := unix.FcntlInt(fd, unix.F_GETFL, 0)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			owned, cleanup, err := commandPipe(ctx, original)
			if err != nil {
				t.Fatal(err)
			}
			_ = streamIsTerminal(owned)
			flags, err := unix.FcntlInt(fd, unix.F_GETFL, 0)
			if err != nil || flags&unix.O_NONBLOCK == 0 {
				t.Fatalf("terminal check disabled polling: flags=%d err=%v", flags, err)
			}
			finished := make(chan error, 1)
			go func() {
				if write {
					_, err = owned.Write(bytes.Repeat([]byte("x"), 1<<20))
				} else {
					_, err = owned.Read(make([]byte, 1))
				}
				finished <- err
			}()
			select {
			case err := <-finished:
				t.Fatalf("pipe did not block: %v", err)
			case <-time.After(20 * time.Millisecond):
			}
			cancel()
			select {
			case err := <-finished:
				if err == nil {
					t.Fatal("cancelled I/O succeeded")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("cancelled pipe remained blocked")
			}
			if err := cleanup(); err != nil {
				t.Fatal(err)
			}
			after, err := unix.FcntlInt(fd, unix.F_GETFL, 0)
			if err != nil || after != before {
				t.Fatalf("caller flags changed: before=%d after=%d err=%v", before, after, err)
			}
			if _, err := original.Stat(); err != nil {
				t.Fatalf("caller file was closed: %v", err)
			}
		})
	}
}

func TestCommandPipeProcess(t *testing.T) {
	if mode := os.Getenv("BALE_TEST_PIPE_MODE"); mode != "" {
		var args []string
		switch mode {
		case "load":
			args = []string{"load", "--store", os.Getenv("BALE_TEST_PIPE_STORE")}
		case "create":
			args = []string{"create", "-f", "-"}
		case "save":
			args = []string{"save", "--layout", os.Getenv("BALE_TEST_PIPE_LAYOUT")}
		default:
			os.Exit(99)
		}
		_, _ = fmt.Fprintln(os.Stderr, "READY")
		os.Exit(run(args, os.Stdout, os.Stderr))
	}
	for _, mode := range []string{"load", "create"} {
		t.Run(mode, func(t *testing.T) {
			store := filepath.Join(t.TempDir(), "store")
			scratch := t.TempDir()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCommandPipeProcess$")
			command.Env = append(os.Environ(), "BALE_TEST_PIPE_MODE="+mode, "BALE_TEST_PIPE_STORE="+store, "TMPDIR="+scratch)
			stdin, err := command.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = stdin.Close() }()
			stderr, err := command.StderrPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if command.ProcessState == nil {
					_ = command.Process.Kill()
					_ = command.Wait()
				}
			}()
			if ready, err := bufio.NewReader(stderr).ReadString('\n'); err != nil || ready != "READY\n" {
				t.Fatalf("child startup: %q %v", ready, err)
			}
			if mode == "load" {
				for {
					staged, _ := filepath.Glob(filepath.Join(store, ".load-*"))
					if len(staged) > 0 {
						break
					}
					if ctx.Err() != nil {
						t.Fatal("load never started reading")
					}
					time.Sleep(time.Millisecond)
				}
			} else {
				// Let run install its signal handler and enter recipe input. The
				// unit test above separately proves blocked reads are interrupted.
				time.Sleep(30 * time.Millisecond)
			}
			if err := command.Process.Signal(syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
			err = command.Wait()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 125 || ctx.Err() != nil {
				t.Fatalf("cancelled child did not exit promptly with 125: %v context=%v", err, ctx.Err())
			}
			if mode == "load" {
				staged, _ := filepath.Glob(filepath.Join(store, ".load-*"))
				if len(staged) != 0 {
					t.Fatalf("cancelled load left staging: %v", staged)
				}
				index, err := os.ReadFile(filepath.Join(store, "index.json"))
				if err != nil || !strings.Contains(string(index), `"manifests":[]`) {
					t.Fatalf("cancelled load retained content: %s %v", index, err)
				}
			}
			assertCommandStagingRemoved(t, scratch)
		})
	}
}

func assertCommandStagingRemoved(t *testing.T, directory string) {
	t.Helper()
	// TestMain creates its own bale-cli-tests directory before the helper runs.
	// Only the command's archive staging belongs to this cancellation assertion.
	for _, pattern := range []string{".bale-save-*", "bale-load-*"} {
		staged, err := filepath.Glob(filepath.Join(directory, pattern))
		if err != nil || len(staged) != 0 {
			t.Fatalf("cancelled command left private staging: %v %v", staged, err)
		}
	}
}

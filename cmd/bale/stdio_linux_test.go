package main

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestSaveCancellationWithStalledStdout(t *testing.T) {
	layout, _, _ := cliBundle(t)
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close(); _ = writer.Close() }()
	if _, err := unix.FcntlInt(writer.Fd(), unix.F_SETPIPE_SZ, 4096); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCommandPipeProcess$")
	scratch := t.TempDir()
	command.Env = append(os.Environ(), "BALE_TEST_PIPE_MODE=save", "BALE_TEST_PIPE_LAYOUT="+layout, "TMPDIR="+scratch)
	command.Stdout = writer
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	defer func() {
		if command.ProcessState == nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	}()
	// An archive header proves the real save path passed terminal checks and
	// started output. Leaving the read end open then stalls the archive writer.
	if _, err := io.ReadFull(reader, make([]byte, 512)); err != nil {
		t.Fatal(err)
	}
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	err = command.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 125 || ctx.Err() != nil {
		t.Fatalf("stalled save did not cancel promptly with 125: %v context=%v", err, ctx.Err())
	}
	assertCommandStagingRemoved(t, scratch)
}

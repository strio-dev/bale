package main

import (
	"context"
	"io"
	"os"
)

// commandPipe makes inherited pipes interruptible without transferring ownership
// of the caller's file. SDK readers and writers never pass through this helper.
func commandPipe(ctx context.Context, file *os.File) (*os.File, func() error, error) {
	info, err := file.Stat()
	if err != nil || info.Mode()&(os.ModeNamedPipe|os.ModeSocket) == 0 {
		return file, func() error { return nil }, nil
	}
	owned, interrupt, cleanup, err := duplicateCommandPipe(file)
	if err != nil {
		return nil, nil, err
	}
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { interrupt(); close(done) })
	return owned, func() error {
		if !stop() {
			<-done
		}
		return cleanup()
	}, nil
}

func commandOutput(ctx context.Context, out io.Writer) (io.Writer, func() error, error) {
	if file, ok := out.(*os.File); ok {
		return commandPipe(ctx, file)
	}
	return out, func() error { return nil }, nil
}

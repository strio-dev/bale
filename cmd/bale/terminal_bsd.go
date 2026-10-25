//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package main

import "golang.org/x/sys/unix"

func streamIsTerminal(stream any) bool {
	file, ok := stream.(interface{ Fd() uintptr })
	if !ok {
		return false
	}
	_, err := unix.IoctlGetTermios(int(file.Fd()), unix.TIOCGETA)
	return err == nil
}

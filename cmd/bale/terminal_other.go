//go:build !linux && !darwin && !dragonfly && !freebsd && !netbsd && !openbsd && !windows

package main

func streamIsTerminal(any) bool { return false }

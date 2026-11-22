//go:build !windows

package transport

func symlinkPrivilegeNotHeld(error) bool { return false }

//go:build !windows

package app

import "syscall"

// detachAttr puts the background server in its own session so closing the
// terminal does not send SIGHUP to it.
func detachAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

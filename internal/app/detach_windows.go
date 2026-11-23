//go:build windows

package app

import "syscall"

// detachAttr makes a spawned literouter outlive its parent terminal.
// DETACHED_PROCESS: no console of its own; CREATE_NEW_PROCESS_GROUP: a Ctrl+C
// in the parent stays local.
func detachAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CreationFlags: 0x00000008 | 0x00000200, // DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP
		HideWindow:    true,
	}
}

//go:build unix

package runtime

import "syscall"

func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// signalGroup signals the whole process group of pid.
func signalGroup(pid int, sig syscall.Signal) error {
	return syscall.Kill(-pid, sig)
}

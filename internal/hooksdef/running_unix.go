//go:build !windows

package hooksdef

import "syscall"

var syscallZero = syscall.Signal(0)

//go:build windows

package main

import (
	"errors"
	"syscall"
)

var errHostUnsupported = errors.New("'brain host' is not supported on Windows")

func detachedProcAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{} }

func pidAlive(int) bool { return false }

func terminatePid(int, int) error { return errHostUnsupported }

func lockPromotion(string) (func(), error) { return nil, errHostUnsupported }

func freeBytes(string) int64 { return 0 }

//go:build windows

package main

import "os"

// fileOwnerUID is unavailable on Windows, so the ownership part of the client
// doctor's config check is skipped there (the HOME and path checks still run).
func fileOwnerUID(os.FileInfo) int { return -1 }

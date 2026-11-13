//go:build darwin

package brainupgrade

import "golang.org/x/sys/unix"

// cloneFile makes dst a copy-on-write clone of src (APFS), falling back to a
// byte copy where the filesystem cannot clone.
func cloneFile(src, dst string) error {
	if err := unix.Clonefile(src, dst, 0); err == nil {
		return nil
	}
	return copyFile(src, dst)
}

//go:build !darwin

package brainupgrade

// cloneFile copies src to dst.
func cloneFile(src, dst string) error { return copyFile(src, dst) }

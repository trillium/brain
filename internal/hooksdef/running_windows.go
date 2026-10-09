//go:build windows

package hooksdef

import "os"

// Windows has no signal 0; the running-marker check is not performed there
// (see markRunning), so this is never consulted.
var syscallZero os.Signal = os.Kill

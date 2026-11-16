// SPDX-License-Identifier: MIT

package main

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// detachStdout keeps DuckDB's own output out of the pod's log. DuckDB writes to file descriptor 1
// directly (query logs when logging goes to 'stdout', profiler trees, progress bars), and that
// output can carry customer SQL, which the service never logs. So descriptor 1 is pointed at
// /dev/null, and the service's request log goes to the returned file: a duplicate of the original
// descriptor 1, which is still the container's stdout.
//
// The statement guard (internal/server/guard.go) refuses every statement known to make DuckDB
// print; this is the second line, for whatever a future DuckDB adds.
func detachStdout() (*os.File, error) {
	saved, err := unix.Dup(1)
	if err != nil {
		return nil, fmt.Errorf("dup stdout: %w", err)
	}
	unix.CloseOnExec(saved)
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		_ = unix.Close(saved)
		return nil, fmt.Errorf("open %s: %w", os.DevNull, err)
	}
	defer devNull.Close()
	if err := unix.Dup2(int(devNull.Fd()), 1); err != nil {
		_ = unix.Close(saved)
		return nil, fmt.Errorf("redirect stdout: %w", err)
	}
	return os.NewFile(uintptr(saved), "stdout"), nil
}

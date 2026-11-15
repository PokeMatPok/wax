//go:build !windows

package player

import "os"

// procAlive and bindToLifetime are no-ops off Windows: on Unix mpv is
// not orphaned on SIGKILL of the parent in a way we can control here.
func procAlive(pid int) bool { return false }

func bindToLifetime(p *os.Process) {}

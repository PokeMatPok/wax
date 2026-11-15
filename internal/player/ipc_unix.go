//go:build !windows

package player

import (
	"fmt"
	"net"
)

// mpv on Linux/macOS accepts a filesystem socket for --input-ipc-server.
func dialMpvPipe(name string) (net.Conn, error) {
	return net.Dial("unix", name)
}

func defaultPipeName(pid int) string {
	return fmt.Sprintf("/tmp/ytmpv-%d.sock", pid)
}

// recordMpvPID and killOrphans are no-ops off Windows; there mpv is
// started in the same process group and does not survive Wax.
func recordMpvPID(pid int) {}

func killOrphans() {}

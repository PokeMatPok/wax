//go:build windows

package player

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// dialMpvPipe connects to mpv's JSON IPC named pipe.
func dialMpvPipe(name string) (net.Conn, error) {
	return winio.DialPipe(name, nil)
}

// defaultPipeName is a Windows named pipe path.
func defaultPipeName(pid int) string {
	return `\\.\pipe\ytmpv-` + strconv.Itoa(pid)
}

// mpvPIDPath is where the pid of the last spawned mpv is recorded so an
// orphan from a previous Wax run can be identified on the next start.
func mpvPIDPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "ytm-client", "mpv.pid")
}

// recordMpvPID stores pid so killOrphans can find it on a later run.
func recordMpvPID(pid int) {
	p := mpvPIDPath()
	if p == "" {
		return
	}
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte(strconv.Itoa(pid)), 0o644)
}

// killOrphans terminates an mpv left running by a previous Wax run that
// died before it could stop mpv itself (taskkill, crash, power loss).
// The bindToLifetime job object is the primary defense; this sweep
// covers environments where the job assignment was refused. A recorded
// PID is only killed if it is still a live mpv.exe, so PID reuse by an
// unrelated process is safe.
func killOrphans() {
	p := mpvPIDPath()
	if p == "" {
		return
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return
	}
	os.Remove(p)
	for _, line := range strings.Fields(string(data)) {
		pid, err := strconv.Atoi(line)
		if err != nil || pid == os.Getpid() {
			continue
		}
		terminateIfMpv(pid)
	}
}

// terminateIfMpv kills the process only if its image name is mpv.exe.
func terminateIfMpv(pid int) {
	h, err := windows.OpenProcess(
		windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_TERMINATE,
		false, uint32(pid))
	if err != nil {
		return
	}
	defer windows.CloseHandle(h)

	var buf [syscall.MAX_PATH]uint16
	var n uint32 = uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err != nil {
		return
	}
	img := strings.ToLower(filepath.Base(windows.UTF16ToString(buf[:n])))
	if img != "mpv.exe" {
		return
	}
	windows.TerminateProcess(h, 1)
}

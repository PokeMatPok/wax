//go:build windows

package player

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// procAlive reports whether a process with the given PID exists.
func procAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	windows.CloseHandle(h)
	return true
}

// bindToLifetime puts mpv into a kill-on-close job object: the job
// handle dies with the Wax process (taskkill, crash, hard exit all
// count), and the OS then terminates everything in the job. This makes
// orphaned-mpv structurally impossible instead of best-effort.
func bindToLifetime(p *os.Process) {
	j, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return
	}
	var li windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	li.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(j,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&li)), uint32(unsafe.Sizeof(li))); err != nil {
		windows.CloseHandle(j)
		return
	}
	h, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(p.Pid))
	if err != nil {
		windows.CloseHandle(j)
		return
	}
	defer windows.CloseHandle(h)
	// fails harmlessly if the environment already jobs the process
	// tree (e.g. some debuggers/sandboxes); the sweep remains then.
	windows.AssignProcessToJobObject(j, h)
	// deliberately never close j: it stays open for Wax's remaining
	// lifetime, closing implicitly at process death.
}

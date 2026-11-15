//go:build windows

package extract

import (
	"os/exec"
	"syscall"
)

// hideChildConsole hides the console window child processes (yt-dlp)
// would otherwise flash from the GUI-subsystem build: no window plus
// CREATE_NO_WINDOW for good measure.
func hideChildConsole(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= 0x08000000 // CREATE_NO_WINDOW
}

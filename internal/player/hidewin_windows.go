//go:build windows

package player

import (
	"os/exec"
	"syscall"
)

// hideChildConsole hides the console window child processes (mpv)
// would otherwise flash from the GUI-subsystem build.
func hideChildConsole(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= 0x08000000 // CREATE_NO_WINDOW
}

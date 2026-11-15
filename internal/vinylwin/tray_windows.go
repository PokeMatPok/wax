//go:build windows

// Tray icon + hide-on-close support for the YTM overlay package.
// Setup() makes the process DPI-aware (call before creating windows);
// the Run loop installs the tray icon and subclasses the main window so
// its close button hides it instead of terminating the process.
package vinylwin

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

var (
	shell32       = syscall.NewLazyDLL("shell32.dll")
	procShellNI   = shell32.NewProc("Shell_NotifyIconW")
	procLoadIcon  = user32.NewProc("LoadIconW")
	procShowW     = user32.NewProc("ShowWindow")
	procSetFore   = user32.NewProc("SetForegroundWindow")
	procGetCurPos = user32.NewProc("GetCursorPos")
	procPopup     = user32.NewProc("CreatePopupMenu")
	procAppend    = user32.NewProc("AppendMenuW")
	procTrackPop  = user32.NewProc("TrackPopupMenu")
	procDestroyM  = user32.NewProc("DestroyMenu")
	procFindWin   = user32.NewProc("FindWindowW")
	procDPIAware  = user32.NewProc("SetProcessDpiAwarenessContext")
	procOldAPI    = user32.NewProc("SetProcessDPIAware")
	procGetLongX  = user32.NewProc("GetWindowLongPtrW")
	procSetLongX  = user32.NewProc("SetWindowLongPtrW")
)

const (
	nimAdd    = 0
	nimModify = 1
	nimDelete = 2
	nifMsg    = 0x00000001
	nifIcon   = 0x00000002
	nifTip    = 0x00000004
	idcApp    = 32512
	wmClose   = 0x0010
	wmApp     = 0x8000
	swHide    = 0
	swShow    = 5
	swRestore = 9
	tpmRetCmd = 0x0100
	tpmNoNot  = 0x0080

	gwlpWndProc = -4
)

// notifyIconDataW mirrors NOTIFYICONDATAW (x64).
type notifyIconDataW struct {
	CbSize           uint32
	HWnd             uintptr
	UID              uint32
	UFlags           uint32
	UCallbackMessage uint32
	HIcon            uintptr
	SzTip            [128]uint16
	DwState          uint32
	DwStateMask      uint32
	SzInfo           [256]uint16
	UVersion         uint32
	SzInfoTitle      [64]uint16
	DwInfoFlags      uint32
	GuidItem         [16]byte
	HBalloonIcon     uintptr
}

// Setup makes the process DPI-aware so screen metrics and anchoring use
// physical pixels. Call it before any window is created.
func Setup() {
	// DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 = -4, PER_MONITOR_AWARE = -3
	if ret, _, _ := procDPIAware.Call(negPointer(4)); ret != 0 {
		return
	}
	if ret, _, _ := procDPIAware.Call(negPointer(3)); ret != 0 {
		return
	}
	procOldAPI.Call()
}

// negPointer returns uintptr(-n) for SetProcessDpiAwarenessContext pseudo-handles.
func negPointer(n int) uintptr {
	return ^(uintptr(n) - 1) // two's complement of -n
}

// global wiring for the main-window handle and callbacks
var (
	mainWin   uintptr
	onShowReq func()
	onQuitReq func()
	frameIDX  int32 = gwlpWndProc
)

func utf16Arg(s string) uintptr {
	p, err := syscall.UTF16PtrFromString(s)
	if err != nil {
		return 0
	}
	return uintptr(unsafe.Pointer(p))
}

// addTrayIcon puts the tray icon on o.win's message flow.
func (o *overlay) addTrayIcon(tip string) bool {
	icon, _, _ := procLoadIcon.Call(0, idcApp)
	nid := notifyIconDataW{
		CbSize:           uint32(unsafe.Sizeof(notifyIconDataW{})),
		HWnd:             o.win,
		UID:              1,
		UFlags:           nifMsg | nifIcon | nifTip,
		UCallbackMessage: wmApp + 1,
		HIcon:            icon,
	}
	copyUtf16(nid.SzTip[:], tip)
	o.tray = nid
	ret, _, _ := procShellNI.Call(nimAdd, uintptr(unsafe.Pointer(&o.tray)))
	return ret != 0
}

func (o *overlay) removeTrayIcon() {
	procShellNI.Call(nimDelete, uintptr(unsafe.Pointer(&o.tray)))
}

func copyUtf16(dst []uint16, s string) {
	u, err := syscall.UTF16FromString(s)
	if err != nil {
		u = nil
	}
	n := copy(dst, u)
	for i := n; i < len(dst); i++ {
		dst[i] = 0
	}
}

// subclassMain installs a wndproc on the main window that hides it on
// WM_CLOSE (process keeps running, playback continues).
func subclassMain(title string) {
	tp, err := syscall.UTF16PtrFromString(title)
	if err != nil {
		return
	}
	h, _, _ := procFindWin.Call(0, uintptr(unsafe.Pointer(tp)))
	if h == 0 {
		logFail(fmt.Sprintf("_subclass: window %q not found", title), nil)
		return
	}
	mainWin = h
	oldTrayProc, _, _ = procGetLongX.Call(h, uintptr(frameIDX))
	newProc := syscall.NewCallback(hideOnCloseProc)
	procSetLongX.Call(h, uintptr(frameIDX), newProc)
}

var oldTrayProc uintptr

// hideOnCloseProc: swallow WM_CLOSE (hide), forward everything else.
func hideOnCloseProc(hwnd uintptr, msg uintptr, wParam, lParam uintptr) uintptr {
	if msg == wmClose {
		procShowW.Call(hwnd, swHide)
		return 0
	}
	ret, _, _ := user32.NewProc("CallWindowProcW").Call(oldTrayProc, hwnd, msg, wParam, lParam)
	return ret
}

func showMain() {
	if mainWin == 0 {
		return
	}
	procShowW.Call(mainWin, swShow)
	procSetFore.Call(mainWin)
}

// trayMenu: right-click popup (Show / Quit). Runs on the overlay thread.
func (o *overlay) trayMenu() {
	m, _, _ := procPopup.Call()
	if m == 0 {
		return
	}
	procAppend.Call(m, 0 /*MF_STRING*/, 1, utf16Arg("Show YTM"))
	procAppend.Call(m, 0, 2, utf16Arg("Quit"))
	var cp point
	procGetCurPos.Call(uintptr(unsafe.Pointer(&cp)))
	procSetFore.Call(o.win)
	cmd, _, _ := procTrackPop.Call(m, uintptr(tpmRetCmd|tpmNoNot), uintptr(cp.X), uintptr(cp.Y), 0, o.win, 0)
	procDestroyM.Call(m)
	switch cmd {
	case 1:
		showMain()
	case 2:
		if onQuitReq != nil {
			o.removeTrayIcon()
			onQuitReq()
			os.Exit(0)
		}
	}
}

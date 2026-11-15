//go:build windows

// Package smtc registers Wax as the Windows System Media Transport Controls
// (SMTC) source: the title/artist/album + cover shown in the volume flyout and
// the lock screen, the playback status and timeline, and media-key / flyout
// button events routed back into the app's control path.
//
// Pure Go over WinRT/COM cgo-free: it owns a hidden top-level window (SMTC
// sessions are keyed to a window) plus one OS thread that both pumps that
// window's messages and serializes the COM calls onto the thread the SMTC
// object was created on. All public setters are goroutine-safe and non-blocking
// (superseded updates collapse, dropped under pressure rather than stalling the
// UI).
package smtc

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"
)

// Button identifies a transport control invocation (media key or flyout click).
type Button int

const (
	BtnPlay     Button = 0
	BtnPause    Button = 1
	BtnStop     Button = 2
	BtnNext     Button = 6
	BtnPrevious Button = 7
)

// String names the button, for logs.
func (b Button) String() string {
	switch b {
	case BtnPlay:
		return "Play"
	case BtnPause:
		return "Pause"
	case BtnStop:
		return "Stop"
	case BtnNext:
		return "Next"
	case BtnPrevious:
		return "Previous"
	}
	return fmt.Sprintf("Button(%d)", int(b))
}

// Active states the session can publish (MediaPlaybackStatus enum values).
const (
	statusStopped = 2
	statusPlaying = 3
	statusPaused  = 4
)

// ---------------------------------------------------------------- COM plumbing

type guid struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

func parseGUID(s string) guid {
	var g guid
	buf := make([]byte, 0, 32)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '-':
			continue
		case c >= '0' && c <= '9':
			buf = append(buf, c-'0')
		case c >= 'a' && c <= 'f':
			buf = append(buf, c-'a'+10)
		case c >= 'A' && c <= 'F':
			buf = append(buf, c-'A'+10)
		}
	}
	g.Data1 = uint32(buf[0])<<28 | uint32(buf[1])<<24 | uint32(buf[2])<<20 | uint32(buf[3])<<16 |
		uint32(buf[4])<<12 | uint32(buf[5])<<8 | uint32(buf[6])<<4 | uint32(buf[7])
	g.Data2 = uint16(buf[8])<<12 | uint16(buf[9])<<8 | uint16(buf[10])<<4 | uint16(buf[11])
	g.Data3 = uint16(buf[12])<<12 | uint16(buf[13])<<8 | uint16(buf[14])<<4 | uint16(buf[15])
	for i := 0; i < 8; i++ {
		g.Data4[i] = buf[16+i*2]<<4 | buf[17+i*2]
	}
	return g
}

func (g guid) String() string {
	return fmt.Sprintf("%08X-%04X-%04X-%02X%02X-%02X%02X%02X%02X%02X%02X",
		g.Data1, g.Data2, g.Data3, g.Data4[0], g.Data4[1], g.Data4[2], g.Data4[3],
		g.Data4[4], g.Data4[5], g.Data4[6], g.Data4[7])
}

// IIDs verified against Windows SDK 10.0.26100.0 headers (SystemMediaTransportControlsInterop.h),
// the C++/WinRT implementation headers, and .NET reflection over the projected types.
var (
	iidSMTC        = parseGUID("99FA3FF4-1742-42A6-902E-087D41F965EC") // ISystemMediaTransportControls
	iidSMTC2       = parseGUID("EA98D2F6-7F3C-4AF2-A586-72889808EFB1") // ISystemMediaTransportControls2 (timeline)
	iidInterop     = parseGUID("DDB0472D-C911-4A1F-86D9-DC3D71A95F5A") // ISystemMediaTransportControlsInterop
	iidDispUpdater = parseGUID("8ABBC53E-FA55-4ECF-AD8E-C984E5DD1550") // ...DisplayUpdater
	iidMusicProps  = parseGUID("6BBF0C59-D0A0-4D26-92A0-F978E1D18E7B") // IMusicDisplayProperties
	iidMusicProps2 = parseGUID("00368462-97D3-44B9-B00F-008AFCEFAF18") // IMusicDisplayProperties2 (album title)
	iidHandler     = parseGUID("0557E996-7B23-5BAE-AA81-EA0D671143A4") // ITypedEventHandler<SMTC,ButtonPressedEventArgs> pinterface
	iidRefStatics  = parseGUID("857309DC-3FBF-4E7D-986F-EF3B1A07A964") // IRandomAccessStreamReferenceStatics
	iidUriFactory  = parseGUID("44A9796F-723E-4FDF-A218-033E75B0C084") // IUriRuntimeClassFactory
	iidTimeline    = parseGUID("5125316A-C3A2-475B-8507-93534DC88F15") // ISystemMediaTransportControlsTimelineProperties
	iidActFactory  = parseGUID("00000035-0000-0000-C000-000000000046") // IActivationFactory
	iidIUnknown    = guid{Data4: [8]byte{0xC0, 0, 0, 0, 0, 0, 0, 0x46}}
	iidAgile       = parseGUID("94EA2B94-E9CC-49E0-C0FF-EE64CA8F5B90") // IAgileObject (QI-only marker)
	errNoIface     = uintptr(0x80004002)                               // E_NOINTERFACE
)

var (
	combase             = syscall.NewLazyDLL("combase.dll")
	procRoInitialize    = combase.NewProc("RoInitialize")
	procRoGetFactory    = combase.NewProc("RoGetActivationFactory")
	procCreateString    = combase.NewProc("WindowsCreateString")
	procDeleteString    = combase.NewProc("WindowsDeleteString")
	user32              = syscall.NewLazyDLL("user32.dll")
	procRegisterClassEx = user32.NewProc("RegisterClassExW")
	procCreateWindowEx  = user32.NewProc("CreateWindowExW")
	procDefWindowProc   = user32.NewProc("DefWindowProcW")
	procMsgWaitFor      = user32.NewProc("MsgWaitForMultipleObjects")
	procPeekMessage     = user32.NewProc("PeekMessageW")
	procTranslateMsg    = user32.NewProc("TranslateMessage")
	procDispatchMsg     = user32.NewProc("DispatchMessageW")
	kernel32            = syscall.NewLazyDLL("kernel32.dll")
	procGetModHandle    = kernel32.NewProc("GetModuleHandleW")
)

// vt dereferences the object's vtable pointer (obj -> *obj = vtable, entries from there).
func vt(obj uintptr) *[64]uintptr {
	return (*[64]uintptr)(unsafe.Pointer(*(*uintptr)(unsafe.Pointer(obj))))
}

func callProc(p *syscall.LazyProc, args ...uintptr) uintptr {
	r, _, _ := p.Call(args...)
	return r
}

// callV invokes a COM vtable entry: callV(obj, slot, extraArgs...)
func callV(obj uintptr, slot int, args ...uintptr) uintptr {
	a := make([]uintptr, 0, len(args)+1)
	a = append(a, obj)
	a = append(a, args...)
	r, _, _ := syscall.SyscallN(vt(obj)[slot], a...)
	return r
}

// mkHSTR creates an HSTRING from a Go string (caller frees).
func mkHSTR(s string) uintptr {
	w := utf16.Encode([]rune(s))
	var h uintptr
	callProc(procCreateString, uintptr(unsafe.Pointer(&w[0])), uintptr(len(w)), uintptr(unsafe.Pointer(&h)))
	return h
}

func dropHSTR(h uintptr) { callProc(procDeleteString, h) }

// ---- per-object convenience calls, all on the owning thread's vtables ----

const (
	// IUnknown/IInspectable occupy vtable slots 0..2 and 3..5; interface
	// methods start at slot 6. Slot numbers below are base(6)+method index.
	smtcSlotPlaybackStatusGet = 6
	smtcSlotPlaybackStatusPut = 7
	smtcSlotDisplayUpdaterGet = 8
	smtcSlotIsEnabledPut      = 11
	smtcSlotIsPlayPut         = 13
	smtcSlotIsStopPut         = 15
	smtcSlotIsPausePut        = 17
	smtcSlotIsPrevPut         = 25
	smtcSlotIsNextPut         = 27
	smtcSlotAddButtonPressed  = 32

	smtc2SlotUpdateTimeline = 12

	updSlotTypePut       = 7
	updSlotThumbnailPut  = 11
	updSlotMusicPropsGet = 12
	updSlotUpdate        = 17

	musicSlotTitlePut  = 7
	musicSlotAlbumPut  = 9 // v1 AlbumArtist; v2 below adds AlbumTitle
	musicSlotArtistPut = 11

	music2SlotAlbumTitlePut = 7

	uriSlotCreateUri = 6

	refSlotCreateFromUri = 7

	tlSlotActivate = 6 // IActivationFactory::ActivateInstance
	tlSlotEndPut   = 9
	tlSlotMaxPut   = 13
	tlSlotPosPut   = 15
)

// ---------------------------------------------------------------- logging

var logPath string

func logf(step, format string, args ...interface{}) {
	if logPath == "" {
		return
	}
	msg := fmt.Sprintf(format, args...)
	if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
		fmt.Fprintf(f, "%s smtc: %s: %s\n", time.Now().Format("2006-01-02 15:04:05"), step, msg)
		f.Close()
	}
}

func init() {
	if dir, err := os.UserConfigDir(); err == nil {
		_ = os.MkdirAll(filepath.Join(dir, "ytm-client"), 0o755)
		logPath = filepath.Join(dir, "ytm-client", "smtc.log")
		if fi, err := os.Stat(logPath); err == nil && fi.Size() > 1<<20 {
			os.Remove(logPath)
		}
	}
}

// ---------------------------------------------------------------- COM objects

// hiddenWindow creates an invisible top-level window to key the SMTC session
// to. Message-only windows (HWND_MESSAGE) are rejected by GetForWindow, so it
// is a plain 1x1 overlapping window that is never shown.
func hiddenWindow() uintptr {
	clsName, _ := syscall.UTF16PtrFromString("WaxSMTCWindow")
	title, _ := syscall.UTF16PtrFromString("")
	hInst := callProc(procGetModHandle, 0)
	var wcls struct {
		CbSize        uint32
		Style         uint32
		LpfnWndProc   uintptr
		CbClsExtra    int32
		CbWndExtra    int32
		HInstance     uintptr
		HIcon         uintptr
		HCursor       uintptr
		HbrBackground uintptr
		LpszMenuName  uintptr
		LpszClassName uintptr
		HIconSm       uintptr
	}
	wcls.CbSize = uint32(unsafe.Sizeof(wcls))
	wcls.LpfnWndProc = syscall.NewCallback(func(hwnd uintptr, msg uint32, wp, lp uintptr) uintptr {
		r, _, _ := syscall.SyscallN(procDefWindowProc.Addr(), hwnd, uintptr(msg), wp, lp)
		return r
	})
	wcls.HInstance = hInst
	wcls.LpszClassName = uintptr(unsafe.Pointer(clsName))
	if callProc(procRegisterClassEx, uintptr(unsafe.Pointer(&wcls))) == 0 {
		logf("register-class", "failed")
		return 0
	}
	const wsExToolWindow = 0x00000080
	const wsExNoActivate = 0x08000000
	hwnd := callProc(procCreateWindowEx,
		wsExToolWindow|wsExNoActivate,
		uintptr(unsafe.Pointer(clsName)), uintptr(unsafe.Pointer(title)),
		0, // WS_OVERLAPPED, never shown
		0, 0, 1, 1, 0, 0, hInst, 0)
	if hwnd == 0 {
		logf("create-window", "failed")
	}
	return hwnd
}

// handlerObject builds the COM callback object the OS invokes on
// ButtonPressed. Kept alive forever via a package global so the GC can never
// reclaim it while the OS still holds it.
var handlerKeeper unsafe.Pointer

func newHandler(onButton func(Button)) uintptr {
	qi := syscall.NewCallback(func(this uintptr, riid *guid, ppv *uintptr) uintptr {
		*ppv = 0
		g := *riid
		if g == iidIUnknown || g == iidHandler || g == iidAgile {
			*ppv = this
			callV(this, 1, this) // AddRef
			return 0
		}
		return errNoIface
	})
	addRef := syscall.NewCallback(func(this uintptr) uintptr { return 1 })
	release := syscall.NewCallback(func(this uintptr) uintptr { return 1 })
	invoke := syscall.NewCallback(func(this, sender, args uintptr) uintptr {
		if onButton != nil {
			var b int32
			callV(args, 6, uintptr(unsafe.Pointer(&b))) // get_Button
			onButton(Button(b))
		}
		return 0
	})
	vtMem := []uintptr{qi, addRef, release, invoke}
	vtArr := (*[4]uintptr)(unsafe.Pointer(&vtMem[0]))
	obj := &struct{ vt *[4]uintptr }{vt: vtArr}
	handlerKeeper = unsafe.Pointer(obj) // GC anchor
	return uintptr(unsafe.Pointer(obj))
}

// activation via RoGetActivationFactory: QI for IID, else fail
func factoryOf(class string, iid *guid) uintptr {
	h := mkHSTR(class)
	defer dropHSTR(h)
	var out uintptr
	hr := callProc(procRoGetFactory, h, uintptr(unsafe.Pointer(iid)), uintptr(unsafe.Pointer(&out)))
	if hr != 0 {
		logf("activation-factory", "%s hr=0x%08x", class, hr)
		return 0
	}
	return out
}

func mustQI(obj uintptr, iid *guid, what string) uintptr {
	var out uintptr
	r, _, _ := syscall.SyscallN(vt(obj)[0], obj, uintptr(unsafe.Pointer(iid)), uintptr(unsafe.Pointer(&out)))
	if r != 0 {
		logf("qi", "%s hr=0x%08x", what, r)
		return 0
	}
	return out
}

// ---------------------------------------------------------------- the session

type smtcState struct {
	smtc    uintptr // ISystemMediaTransportControls
	smtc2   uintptr // ISystemMediaTransportControls2
	updater uintptr // DisplayUpdater
	music   uintptr // IMusicDisplayProperties
	music2  uintptr // IMusicDisplayProperties2 (optional)
	handler uintptr
	uriFac  uintptr
	refFac  uintptr // IRandomAccessStreamReferenceStatics
	tlFac   uintptr // ActivationFactory for TimelineProperties

	tasks chan func()
}

func (st *smtcState) schedule(f func()) {
	select {
	case st.tasks <- f:
	default:
		// queue full: drop instead of stalling the caller (updates repeat anyway)
	}
}

func (st *smtcState) putBool(obj uintptr, slot int, v bool) {
	x := uintptr(0)
	if v {
		x = 1
	}
	r, _, _ := syscall.SyscallN(vt(obj)[slot], obj, x)
	if r != 0 {
		logf("put-bool", "slot=%d hr=0x%08x", slot, r)
	}
}

func (st *smtcState) putInt(obj uintptr, slot int, v int32) {
	if r, _, _ := syscall.SyscallN(vt(obj)[slot], obj, uintptr(v)); r != 0 {
		logf("put-int", "slot=%d hr=0x%08x", slot, r)
	}
}

func (st *smtcState) putStr(obj uintptr, slot int, s string) (ok bool) {
	h := mkHSTR(s)
	defer dropHSTR(h)
	r, _, _ := syscall.SyscallN(vt(obj)[slot], obj, h)
	if r != 0 {
		logf("put-str", "slot=%d hr=0x%08x", slot, r)
	}
	return r == 0
}

// getObjQI reads an object-returning slot and QIs it to iid (releasing the raw ref).
func (st *smtcState) getObjQI(obj uintptr, slot int, iid *guid, what string) uintptr {
	var raw uintptr
	r, _, _ := syscall.SyscallN(vt(obj)[slot], obj, uintptr(unsafe.Pointer(&raw)))
	if r != 0 {
		logf("get-obj", "slot=%d(%s) hr=0x%08x", slot, what, r)
		return 0
	}
	defer callV(raw, 2, raw) // Release the returned IInspectable ref
	return mustQI(raw, iid, what)
}

// Start registers the SMTC session and starts its pump thread. onButton is
// invoked (on a WinRT thread pool thread) whenever the user presses a media
// key or a flyout transport button while Wax is the active media session.
// Returns false when registration failed (no flyout integration this run).
func Start(onButton func(Button)) bool {
	st := &smtcState{tasks: make(chan func(), 16)}
	started := make(chan bool, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		if hr := callProc(procRoInitialize, 1); hr > 1 { // S_OK / S_FALSE pass, RPC_E_CHANGED_MODE fatal
			logf("ro-init", "hr=0x%08x", hr)
			started <- false
			return
		}
		ok := st.setup(onButton)
		if ok {
			current = st
		}
		started <- ok
		if ok {
			st.run()
		}
	}()
	return <-started
}

func (st *smtcState) setup(onButton func(Button)) bool {
	hwnd := hiddenWindow()
	if hwnd == 0 {
		return false
	}
	// The interop interface is not returned by RoGetActivationFactory
	// directly. Go through IActivationFactory, QI IUnknown, QI interop —
	// the path proven by the spike. Calling the returned object as interop
	// without this detour crashes the process (vtable slot mismatch).
	fac := factoryOf("Windows.Media.SystemMediaTransportControls", &iidActFactory)
	if fac == 0 {
		return false
	}
	if unk := mustQI(fac, &iidIUnknown, "factory IUnknown"); unk != 0 {
		fac = mustQI(unk, &iidInterop, "SMTC interop")
	}
	if fac == 0 {
		return false
	}
	var smtc uintptr
	hr, _, _ := syscall.SyscallN(vt(fac)[6], fac, hwnd, // GetForWindow
		uintptr(unsafe.Pointer(&iidSMTC)), uintptr(unsafe.Pointer(&smtc)))
	if hr != 0 {
		logf("get-for-window", "hr=0x%08x", hr)
		return false
	}
	st.smtc = smtc
	st.smtc2 = mustQI(smtc, &iidSMTC2, "SMTC2")
	st.updater = st.getObjQI(smtc, smtcSlotDisplayUpdaterGet, &iidDispUpdater, "DisplayUpdater")
	if st.updater == 0 {
		return false
	}
	// Music display properties are only fetchable once the playback type
	// is set to Music — getting them first fails with E_NOTSUPPORTED (0x80070032).
	st.putInt(st.updater, updSlotTypePut, 1) // MediaPlaybackType.Music
	st.music = st.getObjQI(st.updater, updSlotMusicPropsGet, &iidMusicProps, "MusicProperties")
	if st.music == 0 {
		return false
	}
	st.music2 = mustQI(st.music, &iidMusicProps2, "MusicProperties2") // optional: 0 when absent

	// Enable all transport buttons and start from a Stopped status (proven set).
	st.putBool(st.smtc, smtcSlotIsEnabledPut, true)
	st.putBool(st.smtc, smtcSlotIsPlayPut, true)
	st.putBool(st.smtc, smtcSlotIsStopPut, true)
	st.putBool(st.smtc, smtcSlotIsPausePut, true)
	st.putBool(st.smtc, smtcSlotIsPrevPut, true)
	st.putBool(st.smtc, smtcSlotIsNextPut, true)
	st.putInt(st.smtc, smtcSlotPlaybackStatusPut, statusStopped)

	st.uriFac = factoryOf("Windows.Foundation.Uri", &iidUriFactory)
	st.refFac = factoryOf("Windows.Storage.Streams.RandomAccessStreamReference", &iidRefStatics)
	st.tlFac = factoryOf("Windows.Media.SystemMediaTransportControlsTimelineProperties", &iidActFactory)

	st.handler = newHandler(onButton)
	var token int64
	r, _, _ := syscall.SyscallN(vt(st.smtc)[smtcSlotAddButtonPressed], st.smtc, st.handler,
		uintptr(unsafe.Pointer(&token)))
	if r != 0 {
		st.handler = 0
	}

	logf("register", "hwnd=0x%x token=%d", hwnd, token)
	return true
}

// run pumps the hidden window's messages and drains scheduled tasks on the
// same locked thread, until the process goes away.
func (st *smtcState) run() {
	const qsAllInput = 0x03FF
	const waitObject0 = 0
	const waitTimeout = 258
	for {
		// drain scheduled work first
		for {
			select {
			case f := <-st.tasks:
				func() {
					defer func() {
						if r := recover(); r != nil {
							logf("task-panic", "%v", r)
						}
					}()
					f()
				}()
				continue
			default:
			}
			break
		}
		res, _, _ := syscall.SyscallN(procMsgWaitFor.Addr(), 0, 0, 0, 50, qsAllInput)
		if uintptr(res) == waitTimeout {
			continue
		}
		if uintptr(res) != waitObject0 {
			// not a message (e.g. WAIT_COMPLETION...): avoid a busy spin
			time.Sleep(10 * time.Millisecond)
			continue
		}
		var msg [48]byte // MSG (48 bytes on x64)
		for {
			r, _, _ := syscall.SyscallN(procPeekMessage.Addr(),
				uintptr(unsafe.Pointer(&msg[0])), 0, 0, 0, 1) // PM_REMOVE
			if r == 0 {
				break
			}
			if *(*uint32)(unsafe.Pointer(&msg[4])) == 0x0012 { // WM_QUIT
				return
			}
			callProc(procTranslateMsg, uintptr(unsafe.Pointer(&msg[0])))
			callProc(procDispatchMsg, uintptr(unsafe.Pointer(&msg[0])))
		}
	}
}

// ---------------------------------------------------------------- public API

// SetTrack publishes the optimistic track metadata. Called as soon as the user
// picks a track (before the stream resolves). An empty coverPath omits the
// thumbnail (a later SetCover call can add it once the art is fetched).
func SetTrack(title, artist, album, coverPath string) {
	st := current
	if st == nil {
		return
	}
	st.schedule(func() {
		if st.music != 0 {
			st.putStr(st.music, musicSlotTitlePut, title)
			if st.music2 != 0 {
				st.putStr(st.music2, music2SlotAlbumTitlePut, album)
			} else {
				st.putStr(st.music, musicSlotAlbumPut, album)
			}
			st.putStr(st.music, musicSlotArtistPut, artist)
		}
		st.applyThumbnail(coverPath)
		if st.updater != 0 {
			callV(st.updater, updSlotUpdate)
		}
	})
}

// SetCover updates the thumbnail once the cover bytes have been fetched to disk.
func SetCover(coverPath string) {
	st := current
	if st == nil {
		return
	}
	st.schedule(func() {
		st.applyThumbnail(coverPath)
		if st.updater != 0 {
			callV(st.updater, updSlotUpdate)
		}
	})
}

func (st *smtcState) applyThumbnail(coverPath string) {
	if st.uriFac == 0 || st.refFac == 0 || coverPath == "" || st.updater == 0 {
		if coverPath != "" {
			logf("thumb", "factories not ready for %s", coverPath)
		}
		return
	}
	uriH := mkHSTR("file:///" + filepath.ToSlash(coverPath))
	defer dropHSTR(uriH)
	var uri uintptr
	if hr, _, _ := syscall.SyscallN(vt(st.uriFac)[uriSlotCreateUri], st.uriFac, uriH, uintptr(unsafe.Pointer(&uri))); hr != 0 {
		logf("create-uri", "hr=0x%08x", hr)
		return
	}
	defer callV(uri, 2, uri)
	var ref uintptr
	if hr, _, _ := syscall.SyscallN(vt(st.refFac)[refSlotCreateFromUri], st.refFac, uri, uintptr(unsafe.Pointer(&ref))); hr != 0 {
		logf("create-ref", "hr=0x%08x", hr)
		return
	}
	defer callV(ref, 2, ref)
	if hr, _, _ := syscall.SyscallN(vt(st.updater)[updSlotThumbnailPut], st.updater, ref); hr != 0 {
		logf("put-thumb", "hr=0x%08x", hr)
	}
}

// SetPlayback publishes the coarse playback state (Playing/Paused).
func SetPlayback(playing bool) {
	st := current
	if st == nil || st.smtc == 0 {
		return
	}
	v := int32(statusPaused)
	if playing {
		v = statusPlaying
	}
	st.schedule(func() { st.putInt(st.smtc, smtcSlotPlaybackStatusPut, v) })
}

// SetStopped flips the session to Stopped (explicit stop, engine shutdown).
func SetStopped() {
	st := current
	if st == nil || st.smtc == 0 {
		return
	}
	st.schedule(func() { st.putInt(st.smtc, smtcSlotPlaybackStatusPut, statusStopped) })
}

// SetTimeline publishes position and duration (throttle to ~1 Hz from the caller).
func SetTimeline(pos, dur time.Duration) {
	st := current
	if st == nil {
		return
	}
	st.schedule(func() {
		if st.smtc2 == 0 || st.tlFac == 0 {
			return
		}
		var inst uintptr
		if hr, _, _ := syscall.SyscallN(vt(st.tlFac)[tlSlotActivate], st.tlFac, uintptr(unsafe.Pointer(&inst))); hr != 0 {
			return
		}
		tl := mustQI(inst, &iidTimeline, "TimelineProperties")
		if tl == 0 {
			return
		}
		defer callV(tl, 2, tl)
		syscall.SyscallN(vt(tl)[7], tl, uintptr(0))              // StartTime
		syscall.SyscallN(vt(tl)[tlSlotEndPut], tl, uintptr(dur)) // EndTime
		syscall.SyscallN(vt(tl)[11], tl, uintptr(0))             // MinSeekTime
		syscall.SyscallN(vt(tl)[tlSlotMaxPut], tl, uintptr(dur)) // MaxSeekTime
		syscall.SyscallN(vt(tl)[tlSlotPosPut], tl, uintptr(pos)) // Position
		if hr, _, _ := syscall.SyscallN(vt(st.smtc2)[smtc2SlotUpdateTimeline], st.smtc2, inst); hr != 0 {
			logf("timeline", "hr=0x%08x", hr)
		}
	})
}

// Enable turns the whole session on/off (matches the settings toggle).
func Enable(on bool) {
	st := current
	if st == nil || st.smtc == 0 {
		return
	}
	st.schedule(func() { st.putBool(st.smtc, smtcSlotIsEnabledPut, on) })
}

// current is set by Start; all public setters guard on it.
var current *smtcState

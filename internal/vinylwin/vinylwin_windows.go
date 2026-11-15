//go:build windows

// Package vinylwin draws the spinning vinyl as a separate always-on-top
// overlay window pinned to the bottom-right of the screen (work area).
//
// It is a plain Win32 layered window: a per-pixel-alpha 32bpp DIB is
// rendered in Go and handed to the compositor with UpdateLayeredWindow.
// That gives true alpha transparency (no color-key hack) and no D3D, so
// it composes reliably. Clicks on the disc go to the app; clicks outside
// the disc fall through to whatever is beneath.
package vinylwin

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/golang/freetype"
	"github.com/golang/freetype/truetype"
	"golang.org/x/image/font"
	"golang.org/x/image/math/fixed"
)

// Options configure the overlay. All callbacks are called from the
// overlay goroutine and must be safe for that.
type Options struct {
	RadiusPx int // disc radius in physical pixels
	InsetPx  int // extra gap between disc edge and screen edges (0)
	// OffsetFrac pushes the disc center inward from the corner to show more
	// than the upper-left quarter of the disc: 0 = centered on the corner
	// (25% visible), 0.35 ≈ 40% visible, 1 = fully on-screen.
	OffsetFrac float64

	// Angle returns the current disc rotation in degrees. It is sampled
	// ~30x per second; the caller keeps it advancing (physics elsewhere).
	Angle func() float64
	// State returns whether playback is playing and the cover art (nil ok).
	State func() (playing bool, art image.Image)
	// Clicked runs when the user clicks the disc.
	Clicked func()
	// OnLean reports the disc's animated hover lean (px up/left, 0 at
	// rest) whenever it changes by more than ~0.5px, so companion
	// windows (lyrics panel) can track the disc's spring.
	OnLean func(px float64)
	// Track returns the current track's title and artist, shown as a
	// small hint under the hover glyph (skipped when both are empty).
	Track func() (title, artist string)
	// Queue returns the upcoming tracks (next-up first) for the deck
	// peeking out from behind the record's left edge.
	Queue func() []QItem
	// Jump runs when the user clicks deck card i (0 = next up).
	Jump func(i int)
	// MainWindowTitle: window whose close button hides to tray ("" = off).
	MainWindowTitle string
	// Tray tooltip text ("" = no tray icon).
	Tooltip string
	// OnQuit runs (on this goroutine) when Quit is chosen in the tray menu.
	OnQuit func()
}

// QItem is one upcoming track for the deck peek. Tail=true marks the
// display-only "+N more" card (not clickable, drawn dim).
type QItem struct {
	Title  string
	Artist string
	Tail   bool
}

var (
	user32        = syscall.NewLazyDLL("user32.dll")
	gdi32         = syscall.NewLazyDLL("gdi32.dll")
	kernel32      = syscall.NewLazyDLL("kernel32.dll")
	procGetModH   = kernel32.NewProc("GetModuleHandleW")
	procRegCls    = user32.NewProc("RegisterClassExW")
	procCreateW   = user32.NewProc("CreateWindowExW")
	procDefWnd    = user32.NewProc("DefWindowProcW")
	procMsgWait   = user32.NewProc("MsgWaitForMultipleObjects")
	procPeekMsg   = user32.NewProc("PeekMessageW")
	procTransMsg  = user32.NewProc("TranslateMessage")
	procDispMsg   = user32.NewProc("DispatchMessageW")
	procPostQuit  = user32.NewProc("PostQuitMessage")
	procLoadCur   = user32.NewProc("LoadCursorW")
	procGetDC     = user32.NewProc("GetDC")
	procRelDC     = user32.NewProc("ReleaseDC")
	procSysMet    = user32.NewProc("GetSystemMetrics")
	procSPI       = user32.NewProc("SystemParametersInfoW")
	procULW       = user32.NewProc("UpdateLayeredWindow")
	procTrackEv   = user32.NewProc("TrackMouseEvent")
	procCreateDIB = gdi32.NewProc("CreateDIBSection")
	procSelObj    = gdi32.NewProc("SelectObject")
	procDelObj    = gdi32.NewProc("DeleteObject")
)

const (
	wsPopup        = 0x80000000
	wsVisible      = 0x10000000
	wsExLayered    = 0x00080000
	wsExTopmost    = 0x00000008
	wsExToolWindow = 0x00000080
	wsExNoActivate = 0x08000000
	wmDestroy      = 0x0002
	wmLButtonDown  = 0x0201
	wmMouseMove    = 0x0200
	wmMouseLeave   = 0x02A3
	wmNCHitTest    = 0x0084
	wmQuit         = 0x0012
	htClient       = 1
	htTransparent  = 0xFFFFFFFF // (DWORD)-1 = HTTRANSPARENT
	tmeLeave       = 0x0002
	idcHand        = 32649
	spiGetWorkArea = 0x0048
	smCxScreen     = 0
	smCyScreen     = 1
	ulwAlpha       = 2
	acrSrcOver     = 0
	acrSrcAlpha    = 1
	qsAllInput     = 0x04FF
	mwmoInputAvail = 0x0001
	csHredraw      = 0x0002
	csVredraw      = 0x0001
)

type point struct{ X, Y int32 }
type size struct{ CX, CY int32 }
type msg struct {
	HWnd     uintptr
	Message  uint32
	WParam   uintptr
	LParam   uintptr
	Time     uint32
	Pt       point
	LPrivate uintptr
}
type rect struct{ Left, Top, Right, Bottom int32 }

type trackMouseEvent struct {
	CbSize      uint32
	DwFlags     uint32
	HwndTrack   uintptr
	DwHoverTime uint32
}

type bitmapInfoHeader struct {
	Size          uint32
	Width         int32
	Height        int32 // negative = top-down
	Planes        uint16
	BitCount      uint16
	Compression   uint32
	SizeImage     uint32
	XPelsPerMeter int32
	YPelsPerMeter int32
	ClrUsed       uint32
	ClrImportant  uint32
}

type bitmapInfo struct {
	Header bitmapInfoHeader
	Colors [1]uint32
}

type wndClassEx struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   uintptr
	Icon       uintptr
	Cursor     uintptr
	Background uintptr
	MenuName   uintptr
	ClassName  uintptr
	IconSm     uintptr
}

type blendFunc struct {
	BlendOp             byte
	BlendFlags          byte
	SourceConstantAlpha byte
	AlphaFormat         byte
}

// state shared between the message thread and rendering
type overlay struct {
	opts       Options
	win        uintptr // HWND
	hdcMem     uintptr
	hbmp       uintptr
	oldBmp     uintptr
	bits       []byte // BGRA premultiplied, top-down
	w, h       int    // window pixel size
	rad        int    // disc radius in px
	orgX       int32  // window screen origin (for hit-testing)
	orgY       int32
	lastArt    image.Image // identity of currently converted art
	artPix     *image.RGBA // converted art
	grain      []float32   // disc-space surface layer: wear blotches, rotating
	feat       []float32   // disc-space features: scratches + dust (0..1)
	dustW      int         // grain/feat canvas width
	dustR      int         // grain/feat disc center in canvas coords
	hover      bool        // mouse is over the disc
	scale      float64     // disc scale, spring toward 1 / hoverMax
	scaleVel   float64
	radMax     int     // window half-size: base radius with hover headroom
	deckW      int     // window width reserved for the deck on the left
	leanMax    float64 // full-lean up/left offset (px) at hover
	sentLean   float64 // last lean value reported through OnLean
	glyphPlay  []uint8 // rasterized Material play_arrow mask
	glyphPause []uint8 // rasterized Material pause mask
	font       *truetype.Font
	ctx        *freetype.Context
	measure    *image.Gray
	hintA      *image.Gray // title mask (label-center coords)
	hintB      *image.Gray // artist mask (label-center coords)
	hintKey    string      // "title\x00artist" of the last drawn hint
	marqX      float64     // hint marquee offset (px)
	marqT      time.Time   // last marquee tick
	visTarget  bool        // overlay should be fully shown
	visA       float64     // eased visibility 0..1 (fade + corner sink)
	cleared    bool        // hidden: transparent frame already pushed
	// deck: queue records peeking out from behind the left edge
	deckOn   bool      // dwell timer fired / pointer over cards
	deckAt   time.Time // when the deck started revealing (stagger origin)
	deckA    float64   // eased reveal 0..1
	dwellT   time.Time // mouse entered the left zone (dwell clock)
	cardHov  int       // hovered card index, -1 none
	cardR    []float64 // per-card eased reveal
	cardH    []float64 // per-card eased hover
	rects    [][4]int  // clickable card rects (window coords), refreshed per frame
	mx, my   int       // last mouse position (window coords)
	queue    []QItem   // queue snapshot from this frame
	lastT    time.Time
	ulwFails int
	frames   int
	tray     notifyIconDataW
}

// hoverGrow is the disc's size multiplier while the mouse is over it.
// On hover the disc also leans up/left until it is nearly fully on
// screen, stopping hoverPad px short of the screen edge.
const (
	hoverGrow  = 1.10
	hoverPad   = 8.0  // px gap between hovered disc edge and screen edge
	hoverSlack = 80.0 // hit-test ring around the disc while hovered

	deckMaxCards   = 4           // queue records peeking out at once
	deckPeek       = 26.0        // px of crescent the first card shows
	deckStep       = 22.0        // extra crescent per card deeper in the deck
	deckHoverShift = 32.0        // hovered card + cards behind it shift left
	deckDwell      = time.Second // left-zone hover before the deck reveals
	winTopPad      = 110.0       // window headroom above the disc for raised cards + their hint
)

// logFail prints one overlay diagnostic line to stderr.
func logFail(step string, procErr error) {
	msg := step
	if procErr != nil {
		msg = fmt.Sprintf("%s: %v", step, procErr)
	}
	fmt.Fprintf(os.Stderr, "vinylwin: %s\n", msg)
	// the GUI build has no console: mirror diagnostics to a log file
	logOnce.Do(func() {
		if dir, err := os.UserConfigDir(); err == nil {
			os.MkdirAll(filepath.Join(dir, "ytm-client"), 0o755)
			logPath = filepath.Join(dir, "ytm-client", "vinylwin.log")
		}
	})
	if logPath == "" {
		return
	}
	if fi, err := os.Stat(logPath); err == nil && fi.Size() > 1<<20 {
		os.Remove(logPath) // simple size cap, re-truncated next line
	}
	if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
		fmt.Fprintf(f, "%s %s: %s\n", time.Now().Format("2006-01-02 15:04:05"), step, msg)
		f.Close()
	}
}

var (
	logOnce sync.Once
	logPath string
)

// Run creates and services the overlay window. It locks its OS thread
// (Win32 requirement) and returns only when the window is destroyed or
// Quit is called. Start it in its own goroutine.
func Run(opts Options) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	rad := opts.RadiusPx
	if rad < 8 {
		rad = 8 // sanity floor
	}
	o := &overlay{opts: opts, rad: rad}
	visCh = make(chan bool, 8) // initial state: hidden until playback

	cname, _ := syscall.UTF16PtrFromString("ytmVinylOverlay")
	cls := wndClassEx{
		Size:      uint32(unsafe.Sizeof(wndClassEx{})),
		Style:     csHredraw | csVredraw,
		WndProc:   syscall.NewCallback(o.wndProc),
		Instance:  hInstance(),
		Cursor:    loadCursor(idcHand),
		ClassName: uintptr(unsafe.Pointer(cname)),
	}
	if ret, _, _ := procRegCls.Call(uintptr(unsafe.Pointer(&cls))); ret == 0 {
		logFail("_RegisterClassExW", procRegCls.Find())
		return
	}

	// pin to bottom-right of the work area (screen minus taskbar)
	var wa rect
	procSPI.Call(spiGetWorkArea, 0, uintptr(unsafe.Pointer(&wa)), 0) // 4 args: uiAction, uiParam, pvParam, fWinIni
	if wa.Right <= 0 || wa.Bottom <= 0 {
		// SPI failed (observed on some setups): fall back to primary screen
		if v, _, _ := procSysMet.Call(smCxScreen); v != 0 {
			wa.Right = int32(v)
		}
		if v, _, _ := procSysMet.Call(smCyScreen); v != 0 {
			wa.Bottom = int32(v)
		}
		logFail(fmt.Sprintf("_SPI_GETWORKAREA empty, fallback screen %dx%d", wa.Right, wa.Bottom), nil)
	}
	offPx := int32(math.Round(float64(rad) * opts.OffsetFrac))
	if offPx < 0 {
		offPx = 0
	}
	// hover: the disc grows ~10% and leans up/left until it is almost
	// fully on screen. The window's top/left edge holds the fully
	// leaned disc; the right/bottom keep slack so the hover hit-test
	// ring stays inside the window (otherwise WM_MOUSELEAVE fires and
	// the disc would flicker back while sliding out from the cursor).
	radFh := float64(rad) * hoverGrow // hovered disc radius
	leanMax := radFh - float64(offPx) + hoverPad
	if leanMax < 0 {
		leanMax = 0
	}
	radMax := int(2*radFh - float64(offPx) + hoverPad + 2)
	// room to the left for the deck: up to 4 records peeking out plus
	// the hover separation of a pulled-out card
	deckW := int32(deckMaxCards*deckPeek + (deckMaxCards-1)*deckStep + deckHoverShift + 48)
	winW := int32(radMax) + offPx + int32(hoverSlack) + deckW
	winH := int32(radMax) + offPx + int32(hoverSlack) + int32(winTopPad)
	cx := wa.Right - offPx // disc center x (abs): offset inward from corner
	cy := wa.Bottom - offPx
	o.orgX = cx - int32(radMax) - deckW            // window top-left: disc center at
	o.orgY = cy - int32(radMax) - int32(winTopPad) // (radMax+deckW, radMax+topPad) inside
	o.radMax = radMax
	o.deckW = int(deckW)
	o.leanMax = leanMax
	o.scale = 1

	name, _ := syscall.UTF16PtrFromString("")
	o.win, _, _ = procCreateW.Call(
		uintptr(wsExLayered|wsExTopmost|wsExToolWindow|wsExNoActivate),
		uintptr(unsafe.Pointer(cname)), uintptr(unsafe.Pointer(name)),
		uintptr(wsPopup|wsVisible),
		uintptr(o.orgX), uintptr(o.orgY), uintptr(winW), uintptr(winH),
		0, 0, cls.Instance, 0)
	if o.win == 0 {
		logFail("_CreateWindowExW", procCreateW.Find())
		return
	}

	o.w, o.h = int(winW), int(winH)
	if !o.initSurface() {
		logFail("_initSurface/mainloop", nil)
		return
	}
	defer o.freeSurface()

	// main window hiding-to-tray + tray icon
	if opts.MainWindowTitle != "" {
		subclassMain(opts.MainWindowTitle)
	}
	onQuitReq = opts.OnQuit
	if opts.Tooltip != "" {
		if !o.addTrayIcon(opts.Tooltip) {
			logFail("_tray icon add", nil)
		}
	}
	logFail("_started ok", nil)

	for {
		// pick up visibility requests (drain to the latest)
	drain:
		for {
			select {
			case v := <-visCh:
				if o.visTarget != v {
					o.visTarget = v
					o.cleared = false
					if !v {
						o.hover = false
					}
				}
			default:
				break drain
			}
		}

		// draw a frame
		o.render()

		// wait up to ~33ms, then pump all pending messages
		procMsgWait.Call(0, 0, 0, uintptr(33), uintptr(qsAllInput|mwmoInputAvail))
		for {
			var m msg
			ret, _, _ := procPeekMsg.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0, 1 /*PM_REMOVE*/)
			if ret == 0 {
				break
			}
			if m.Message == wmQuit {
				return
			}
			procTransMsg.Call(uintptr(unsafe.Pointer(&m)))
			procDispMsg.Call(uintptr(unsafe.Pointer(&m)))
			if m.Message == wmDestroy {
				return
			}
		}
		time.Sleep(0)
	}
}

// Quit tells a running overlay to exit (safe from any goroutine; no-op if
// there is no overlay window in this process... edges case: window may not
// exist yet).  Included for symmetry; the process usually outlives it.
var current *overlay

func Quit() {
	if c := current; c != nil && c.win != 0 {
		user32.NewProc("PostMessageW").Call(c.win, wmQuit, 0, 0)
	}
}

func (o *overlay) initSurface() bool {
	hdcScreen, _, _ := procGetDC.Call(0)
	if hdcScreen == 0 {
		return false
	}
	defer procRelDC.Call(0, hdcScreen)

	bi := bitmapInfo{}
	bi.Header.Size = uint32(unsafe.Sizeof(bi.Header))
	bi.Header.Width = int32(o.w)
	bi.Header.Height = -int32(o.h) // top-down
	bi.Header.Planes = 1
	bi.Header.BitCount = 32

	var bitsPtr unsafe.Pointer
	hdcMem, _, _ := gdi32.NewProc("CreateCompatibleDC").Call(hdcScreen)
	if hdcMem == 0 {
		return false
	}
	o.hdcMem = hdcMem
	ret, _, _ := procCreateDIB.Call(hdcMem, uintptr(unsafe.Pointer(&bi)), 0 /*DIB_RGB_COLORS*/, uintptr(unsafe.Pointer(&bitsPtr)), 0, 0)
	o.hbmp = ret
	if ret == 0 || bitsPtr == nil {
		return false
	}
	o.bits = unsafe.Slice((*byte)(bitsPtr), o.w*o.h*4)
	o.oldBmp, _, _ = procSelObj.Call(hdcMem, ret)
	return true
}

func (o *overlay) freeSurface() {
	if o.hdcMem != 0 {
		if o.hbmp != 0 {
			procSelObj.Call(o.hdcMem, o.oldBmp)
			procDelObj.Call(o.hbmp)
		}
		gdi32.NewProc("DeleteDC").Call(o.hdcMem)
	}
}

func hInstance() uintptr {
	h, _, _ := procGetModH.Call(0)
	return h
}

func loadCursor(id uintptr) uintptr {
	c, _, _ := procLoadCur.Call(0, id)
	return c
}

var _ = hInstance

// hoverLean returns the current up/left offset (px) of the disc center,
// same easing the renderer uses, so hit-testing tracks the animation.
func (o *overlay) hoverLean() float64 {
	la := (o.scale - 1) / (hoverGrow - 1)
	if la < 0 {
		la = 0
	}
	if la > 1 {
		la = 1
	}
	return la * o.leanMax
}

func (o *overlay) wndProc(hwnd uintptr, message uint32, wParam, lParam uintptr) uintptr {
	switch message {
	case wmNCHitTest:
		// hidden or fading: fully click-through
		if !o.visTarget || o.visA < 0.05 {
			return htTransparent
		}
		// only the disc circle receives clicks while idle; while
		// hovered the region gains a slack ring, so the disc leaning
		// out from under the cursor doesn't drop the hover state.
		// Queue-card crescents and everything else are transparent
		// except where a card actually is.
		pad := 0.0
		if o.hover {
			pad = hoverSlack
		}
		lean := o.hoverLean()
		sink := (1 - o.visA) * float64(o.rad) * 0.35
		sx := int32(shortFromUintptr(lParam))
		sy := int32(shortFromUintptr(lParam >> 16))
		wx, wy := sx-o.orgX, sy-o.orgY                     // window coords
		ccx := float64(o.radMax+o.deckW) - lean + sink     // center x (window coords)
		ccy := float64(o.radMax) + winTopPad - lean + sink // center y
		dx := float64(wx) - ccx
		dy := float64(wy) - ccy
		r := float64(o.rad)*o.scale + pad
		if dx*dx+dy*dy <= r*r {
			return htClient
		}
		if o.deckA > 0.1 {
			for _, rc := range o.rects {
				if wx >= int32(rc[0]) && wx < int32(rc[2]) &&
					wy >= int32(rc[1]) && wy < int32(rc[3]) {
					return htClient
				}
			}
		}
		return htTransparent
	case wmMouseMove:
		if !o.visTarget {
			return 0
		}
		o.mx, o.my = int(int32(shortFromUintptr(lParam))), int(int32(shortFromUintptr(lParam>>16)))
		if !o.hover {
			o.hover = true
			tme := trackMouseEvent{
				CbSize:    uint32(unsafe.Sizeof(trackMouseEvent{})),
				DwFlags:   tmeLeave,
				HwndTrack: hwnd,
			}
			procTrackEv.Call(uintptr(unsafe.Pointer(&tme)))
		}
		return 0
	case wmMouseLeave:
		o.hover = false
		return 0
	case wmLButtonDown:
		// a card click jumps the queue; otherwise the disc toggles
		for i := range o.rects {
			rc := o.rects[i]
			if o.mx >= rc[0] && o.mx < rc[2] && o.my >= rc[1] && o.my < rc[3] {
				if o.opts.Jump != nil {
					o.opts.Jump(i)
				}
				return 0
			}
		}
		if o.visTarget && o.visA > 0.5 && o.opts.Clicked != nil {
			o.opts.Clicked()
		}
		return 0
	case wmApp + 1: // tray icon notifications
		switch lParam {
		case 0x0202: // WM_LBUTTONUP
			showMain()
		case 0x0205: // WM_RBUTTONUP
			o.trayMenu()
		}
		return 0
	case wmDestroy:
		o.removeTrayIcon()
		procPostQuit.Call(0)
		return 0
	}
	ret, _, _ := procDefWnd.Call(hwnd, uintptr(message), wParam, lParam)
	return ret
}

func shortFromUintptr(v uintptr) uint16 {
	return uint16(uint32(v) & 0xFFFF)
}

// render converts the current disc state into the BGRA bitmap and pushes it
// to the compositor.
func (o *overlay) render() {
	w, h, rad := o.w, o.h, o.rad
	bits := o.bits

	deg := 0.0
	playing := false
	if o.opts.Angle != nil {
		deg = o.opts.Angle()
	}
	art := image.Image(nil)
	if o.opts.State != nil {
		playing, art = o.opts.State()
	}
	if art != o.lastArt {
		o.artPix = convertRGBA(art)
		o.lastArt = art
	}
	artPix := o.artPix

	// hover spring: the disc grows to hoverGrow times its size with a
	// slightly underdamped spring (a hint of overshoot feels good)
	now := time.Now()
	dt := 0.033
	if !o.lastT.IsZero() {
		if d := now.Sub(o.lastT).Seconds(); d > 0.001 && d < 0.25 {
			dt = d
		}
	}
	o.lastT = now

	// visibility ease: fade in ~160ms, out ~200ms; while gone, push one
	// transparent frame and stop rendering until requested again
	if o.visTarget {
		o.visA += (1 - o.visA) * (1 - math.Exp(-dt/0.16))
		if o.visA > 0.995 {
			o.visA = 1
		}
	} else {
		o.visA -= o.visA * (1 - math.Exp(-dt/0.20))
		if o.visA < 0.005 {
			o.visA = 0
		}
	}
	if o.visA == 0 && !o.visTarget {
		if !o.cleared {
			for i := range bits {
				bits[i] = 0
			}
			o.push()
			o.cleared = true
		}
		return
	}
	o.cleared = false

	target := 1.0
	if o.hover {
		target = hoverGrow
	}
	const (
		k = 170.0 // spring stiffness
		c = 21.0  // damping (2*sqrt(k)*0.8: slight overshoot)
	)
	o.scaleVel += (-k*(o.scale-target) - c*o.scaleVel) * dt
	o.scale += o.scaleVel * dt
	hoverA := (o.scale - 1) / (hoverGrow - 1) // 0..1 glyph fade
	if o.scale < 1 {
		o.scale, o.scaleVel = 1, 0
		hoverA = 0
	}
	if hoverA > 1 {
		hoverA = 1
	}
	lean := hoverA * o.leanMax // eased up/left offset while hovered
	if o.opts.OnLean != nil && math.Abs(lean-o.sentLean) > 0.5 {
		o.sentLean = lean
		o.opts.OnLean(lean)
	}

	// deck state: queue records peek from behind the left edge after a
	// short dwell on the left third of the disc
	radF := float64(rad) * o.scale // current disc radius (animated)
	// while fading in/out the record sinks toward the screen corner
	sink := (1 - o.visA) * float64(rad) * 0.35
	ctr := float64(o.radMax+o.deckW) - lean + sink      // disc center x (window coords)
	ctrY := float64(o.radMax) + winTopPad - lean + sink // disc center y (no deck offset)
	o.queue = nil
	if o.visTarget && o.opts.Queue != nil {
		o.queue = o.opts.Queue()
	}
	inLeft := o.hover && float64(o.mx)-ctr < -radF*0.35
	if !o.hover {
		o.dwellT = time.Time{}
		o.deckOn = false
	} else if inLeft && o.deckA < 0.5 {
		if o.dwellT.IsZero() {
			o.dwellT = now
		} else if now.Sub(o.dwellT) >= deckDwell && !o.deckOn {
			o.deckOn, o.deckAt = true, now
		}
	} else if !inLeft && !o.deckOn {
		o.dwellT = time.Time{}
	}
	dTgt := 0.0
	if o.deckOn {
		dTgt = 1
	}
	o.deckA += (dTgt - o.deckA) * (1 - math.Exp(-dt/0.13))
	if !o.deckOn && o.deckA < 0.004 {
		o.deckA = 0
	}
	// hovered card: last frame's clickable rects, front card first
	hovIdx := -1
	if o.deckA > 0.1 {
		for i := range o.rects {
			rc := o.rects[i]
			if o.mx >= rc[0] && o.mx < rc[2] && o.my >= rc[1] && o.my < rc[3] {
				hovIdx = i
				break
			}
		}
	}
	o.cardHov = hovIdx

	if o.frames == 0 {
		var wr rect
		user32.NewProc("GetWindowRect").Call(o.win, uintptr(unsafe.Pointer(&wr)))
		logFail(fmt.Sprintf("_first frame: win rect=(%d,%d)-(%d,%d) size=%dx%d rad=%d",
			wr.Left, wr.Top, wr.Right, wr.Bottom, o.w, o.h, o.rad), nil)
	}
	o.frames++

	for i := range bits {
		bits[i] = 0 // fully transparent by default
	}

	L := radF * 0.44 // label radius
	holeR := math.Max(2.5, radF*0.028)
	edge := 1.25 // px of fuzz on outer edge

	// fixed light from upper-left (screen coords are y-down)
	lightDir := -3 * math.Pi / 4
	grooveFreq := 2 * math.Pi / 2.5   // a groove every ~2.5px
	spiralFreq := 0.9 / (2 * math.Pi) // grooves drift subtly, like a spiral

	ang := deg * math.Pi / 180
	co, si := math.Cos(ang), math.Sin(ang)

	// disc-locked surface layer (wear blotches, scratches, dust): built
	// once at 12% oversize, sampled rotated with the record so the
	// surface spins along (and still covers the disc when hovered)
	if o.grain == nil {
		base := float64(rad)
		o.grain, o.feat, o.dustW, o.dustR = buildDust(base*1.12, base*0.44)
	}

	// deck cards: eased reveal (staggered) + eased hover lift, plus the
	// geometry the pixel loop and wndProc (clicks / hit-testing) need.
	// When the queue is deeper than the cap, one extra dimmed "+N" card
	// signals the hidden tail — it's display-only, not clickable.
	nq := len(o.queue)
	if nq > deckMaxCards+1 {
		nq = deckMaxCards + 1
	}
	for len(o.cardR) < nq {
		o.cardR = append(o.cardR, 0)
	}
	for len(o.cardH) < nq {
		o.cardH = append(o.cardH, 0)
	}
	type cardV struct {
		cx, cy, rf, a float64
		tail          bool // dim "+N more" indicator card
	}
	var cards []cardV
	var cardPos [][3]float64
	o.rects = o.rects[:0] // rebuilt every frame with current geometry
	for i := 0; i < nq; i++ {
		rev := 0.0
		if o.deckOn && now.Sub(o.deckAt) > time.Duration(i)*70*time.Millisecond {
			rev = 1
		}
		o.cardR[i] += (rev - o.cardR[i]) * (1 - math.Exp(-dt/0.14))
		hTgt := 0.0
		if o.cardHov == i {
			hTgt = 1
		}
		o.cardH[i] += (hTgt - o.cardH[i]) * (1 - math.Exp(-dt/0.10))
		prog := o.cardR[i]
		if prog <= 0.002 {
			continue
		}
		tail := i == deckMaxCards // extra card beyond the clickable cap
		hov := o.cardH[i]
		off := (deckPeek + deckStep*float64(i)) * prog
		// the hovered card gains horizontal space: it and every card
		// behind it shift left together, so its visible crescent widens
		// instead of sliding over (and blocking) the track behind it
		push := hov
		for k := 0; k < i; k++ {
			if o.cardH[k] > push {
				push = o.cardH[k]
			}
		}
		cx2 := ctr - off - push*deckHoverShift
		// cards ride the record's lean so they stay aligned with its
		// visible arc; winTopPad keeps the raised card inside the window
		cy2 := ctrY - hov*12 // hover raises the selected card (y grows downward)
		rf2 := radF * 0.99
		a2 := prog * (0.88 - 0.08*float64(i))
		if tail {
			a2 *= 0.45
		}
		cards = append(cards, cardV{cx2, cy2, rf2, a2 * o.visA, tail})
		if !tail {
			cardPos = append(cardPos, [3]float64{cx2, cy2, rf2})
			o.rects = append(o.rects, [4]int{
				int(cx2 - rf2), int(cy2 - rf2), int(ctr - radF + 6), int(cy2 + rf2)})
		}
	}

	for y := 0; y < h; y++ {
		dy := float64(y) - ctrY
		row := bits[y*w*4:]
		for x := 0; x < w; x++ {
			dx := float64(x) - ctr
			r := math.Sqrt(dx*dx + dy*dy)
			if r > radF+edge {
				// outside the main disc: queue-card crescents peeking
				// out from behind it (static vinyl, no art, no spin)
				for ci := range cards {
					cd := &cards[ci]
					dx2 := float64(x) - cd.cx
					dy2 := float64(y) - cd.cy
					r2 := math.Sqrt(dx2*dx2 + dy2*dy2)
					if r2 > cd.rf+edge {
						continue
					}
					a2 := (cd.rf + edge - r2) / (2 * edge)
					if a2 <= 0 {
						continue
					}
					if a2 > 1 {
						a2 = 1
					}
					holeR2 := math.Max(2.5, cd.rf*0.028)
					holeA2 := (r2 - (holeR2 - 0.75)) / 1.5
					if holeA2 <= 0 {
						continue // spindle hole shows the card behind
					}
					holeA2 = math.Min(holeA2, 1)
					theta2 := math.Atan2(dy2, dx2)
					cosL2 := math.Cos(theta2 - lightDir)
					if cosL2 < 0 {
						cosL2 = 0
					}
					l2c := cosL2 * cosL2
					gr2 := math.Sin(r2*grooveFreq + 2*theta2) // static grooves
					dep := 0.92 - 0.06*float64(ci)            // deeper cards darker
					if cd.tail {
						dep = 0.55 // "+N more" tail card: clearly inert
					}
					// vinyl surface: same palette as the main disc, no spin
					vb := (0.60 + 0.26*cosL2 + 0.10*(1-gr2)) * (0.7 + 0.3*l2c)
					vb += 0.30 * math.Max(0, -math.Cos(theta2-lightDir)) // rim glint
					cr, cg, cb := 0x0f*vb, 0x0f*vb, 0x12*vb
					// neutral slate label — a record with a plain sleeve tone
					if !cd.tail && r2 < cd.rf*0.44 {
						lbx := 0.78 + 0.24*cosL2
						cr, cg, cb = 0x2e*lbx*dep, 0x31*lbx*dep, 0x3a*lbx*dep
						ringW := math.Max(1.0, cd.rf*0.016)
						if dOut := math.Abs(r2 - cd.rf*0.44); dOut < ringW {
							lb := 1.0 + 0.5*(1-dOut/ringW)
							cr, cg, cb = cr*lb, cg*lb, cb*lb
						}
					}
					vii := x * 4
					va2 := a2 * holeA2 * cd.a
					row[vii] = byte(clampF(cb) * va2)
					row[vii+1] = byte(clampF(cg) * va2)
					row[vii+2] = byte(clampF(cr) * va2)
					row[vii+3] = byte(255 * va2)
					break
				}
				continue
			}
			a := (radF + edge - r) / (2 * edge) // antialiased outer edge
			if a <= 0 {
				continue
			}
			a = math.Min(a, 1)
			// antialiased spindle hole (alpha falls to 0 in the hole)
			holeA := (r - (holeR - 0.75)) / 1.5
			if holeA <= 0 {
				continue
			}
			a *= math.Min(holeA, 1)

			theta := math.Atan2(dy, dx)
			thR := theta - ang          // angle in disc (rotating) coordinates
			hR := 0.5 * math.Sin(2*ang) // tiny highlight precession
			// rotate pixel into disc coordinates for texture sampling
			cT, sT := math.Cos(ang), math.Sin(ang)
			rx := cT*dx + sT*dy
			ry := -sT*dx + cT*dy
			ox := int(rx) + o.dustR
			oy := int(ry) + o.dustR
			if ox < 0 {
				ox = 0
			} else if ox > o.dustW-3 {
				ox = o.dustW - 3
			}
			if oy < 0 {
				oy = 0
			} else if oy > o.dustW-3 {
				oy = o.dustW - 3
			}
			dIdx := oy*o.dustW + ox
			grainDev := float64(o.grain[dIdx])
			feat := float64(o.feat[dIdx])

			// light factor + cheap integer powers (no per-pixel math.Pow)
			cosL := math.Cos(theta - lightDir)
			if cosL < 0 {
				cosL = 0
			}
			l2 := cosL * cosL
			l4 := l2 * l2
			l8 := l4 * l4
			l16 := l8 * l8
			dark := 1 - cosL
			d2 := dark * dark
			d4 := d2 * d2
			d8 := d4 * d4
			d12 := d8 * d4

			// static roughness (screen-space micrograin, symmetric so it
			// also reads as cleanliness) + coarser blotch layer
			n := noise2(x, y)
			n2 := noise2(x/7+31, y/7+57)
			rough := 0.90 + 0.20*n + 0.10*n2

			// circular grooves (subtle spiral drift in disc frame);
			// they glint toward the light, anisotropic metal look
			groove := math.Sin(r*grooveFreq + spiralFreq*thR*2*math.Pi)
			glintStrength := 0.06 + 0.40*l2
			// sharp specular arc + wider sheen + weak backscatter
			spec := 0.55*(l16*l8*l4) + 0.28*(l4*l2) + 0.10*d12

			// broad "wet" sheen sweeping with the spin: sells rotation
			sh := math.Cos(thR - lightDir + hR)
			if sh < 0 {
				sh = 0
			}
			sh2 := sh * sh
			sh8 := sh2 * sh2 * sh2 * sh2
			sweep := 0.20 * sh8 * sh

			// radial shading: darker hub, sheen mid-radius, rim shadow
			rr := r / radF
			radial := 1.10 - 0.35*rr - 0.30*math.Pow(rr, 6)
			rimLit := 0.0
			if r < L+1.5 { // label bezel: small ridge casts a shadow line
				radial += 0.25
			}
			if rim := r - (radF - 2.0); rim > 0 {
				radial += rim / 2.0 * (0.5 + 0.6*cosL) // rim catches light
				rimLit = rim / 2.0 * cosL
			}

			bright := rough*radial*(1+glintStrength*groove) + spec + rimLit*0.3

			var cr, cg, cb float64
			if r < L { // rotating cover art, center-cropped to a circle
				mx := co*dx + si*dy
				my := -si*dx + co*dy
				if artPix != nil && artPix.Bounds().Dx() > 0 {
					b := artPix.Bounds()
					// unit label disc -> inscribed circle of the cover
					Rin := math.Min(float64(b.Dx()), float64(b.Dy())) * 0.5 * 0.985
					acx := float64(b.Min.X) + float64(b.Dx())/2
					acy := float64(b.Min.Y) + float64(b.Dy())/2
					ax := int(acx + mx/L*Rin)
					ay := int(acy + my/L*Rin)
					c := artPix.RGBAAt(ax, ay)
					cr, cg, cb = float64(c.R), float64(c.G), float64(c.B)
				} else {
					cr, cg, cb = 0x30, 0x6a, 0xbe
				}
				// glossy label: lighting + embossed ring + paper grain
				// (grain/ink rotate with the label, they're printed on it)
				lb := 0.80 + 0.28*cosL + 0.20*grainDev
				emb := math.Exp(-math.Pow((r-(L-3.5))/1.1, 2))
				lb += 0.45 * emb * cosL
				lb -= 0.28 * math.Pow(1-rr, 2) // ink pools toward spindle
				lb += 0.30 * (l16 * l4) * (1 - 0.5*r/L)
				// hover overlay over the lit label: the rasterized
				// Material play/pause glyph in near-white
				if hoverA > 0.01 {
					if o.glyphPlay == nil {
						o.glyphPlay = buildGlyphMask(glyphUnitPlay)
						o.glyphPause = buildGlyphMask(glyphUnitPause)
					}
					gH := L * glyphHeightFrac // glyph half-height, px
					// rectangular gate: the pause bars are wider
					// (0.857*gH) than tall, a circular r-gate would
					// crop their outer ends
					if dx > -gH && dx < gH && dy > -gH-2 && dy < gH+2 {
						mask := o.glyphPlay
						if playing {
							mask = o.glyphPause
						}
						if ga := sampleGlyphMask(mask, dx/(2*gH), dy/(2*gH)); ga > 0 {
							ia := 0.92 * hoverA
							cr += (240 - cr) * ia * ga
							cg += (240 - cg) * ia * ga
							cb += (242 - cb) * ia * ga
						}
					}
				}
				cr, cg, cb = cr*lb, cg*lb, cb*lb
			} else {
				// near-black vinyl: disc-locked wear texture rides on the
				// screen-static micrograin, so the surface visibly turns
				b := bright + grainDev*0.12*(0.5+1.8*cosL)
				b += sweep
				// scratches & dust: dark gouge that streaks brightly when
				// it crosses the light arc
				b += feat * (0.10 - 1.6*(l16*l8*l2))
				vinyl := 0x10 * b
				vinyl2 := 0x0e * b
				cr, cg, cb = vinyl, vinyl, vinyl2
				// cool ambient tint on the shadow side
				if cosL < 0.05 {
					f := -math.Cos(theta - lightDir) // 0..1
					cb += 5 * f
					cr -= 2 * f
				}
				// pressure-mark darker ring where label meets vinyl
				if b2 := math.Abs(r - (L + 1.5)); b2 < 1.0 {
					d := 1 - 0.35*(1-b2)
					cr, cg, cb = cr*d, cg*d, cb*d
				}
			}

			i := x * 4
			va := a * o.visA               // global visibility folds into the premultiplied alpha
			row[i] = byte(clampF(cb) * va) // premultiplied BGRA
			row[i+1] = byte(clampF(cg) * va)
			row[i+2] = byte(clampF(cr) * va)
			row[i+3] = byte(255 * va)
		}
	}

	// hover hint: title/artist under the play glyph; while the deck is
	// open it is suppressed entirely (card hints replace it)
	if hoverA > 0.01 && o.cardHov < 0 && o.deckA < 0.5 && o.opts.Track != nil {
		title, artist := o.opts.Track()
		o.drawHint(ctr, ctrY, L, 0.92*hoverA, title, artist, 0)
	}
	if hvi := o.cardHov; hvi >= 0 && hvi < len(cardPos) &&
		hvi < len(o.queue) && o.cardH[hvi] > 0.05 {
		cp := cardPos[hvi]
		o.drawHint(cp[0], cp[1], L, 0.9*o.cardH[hvi], o.queue[hvi].Title, o.queue[hvi].Artist, cp[2])
	}

	// push uploads the composed frame to the compositor; kept separate
	// so the hidden state can push a transparent frame while idle.
	o.push()
}

func (o *overlay) push() {
	hdcScreen, _, _ := procGetDC.Call(0)
	if hdcScreen == 0 {
		return
	}
	defer procRelDC.Call(0, hdcScreen)

	psize := size{CX: int32(o.w), CY: int32(o.h)}
	ptSrc := point{X: 0, Y: 0}
	bf := blendFunc{BlendOp: acrSrcOver, SourceConstantAlpha: 255, AlphaFormat: acrSrcAlpha}
	ret, _, _ := procULW.Call(
		o.win, hdcScreen, 0, /*pptDst=nil: position unchanged*/
		uintptr(unsafe.Pointer(&psize)), o.hdcMem,
		uintptr(unsafe.Pointer(&ptSrc)), 0,
		uintptr(unsafe.Pointer(&bf)), ulwAlpha)
	if ret == 0 && o.ulwFails < 3 {
		o.ulwFails++
		logFail(fmt.Sprintf("_UpdateLayeredWindow ret=0 (frame %d)", o.frames), nil)
	}
}

func convertRGBA(src image.Image) *image.RGBA {
	if src == nil {
		return nil
	}
	if r, ok := src.(*image.RGBA); ok {
		return r
	}
	b := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(dst, dst.Bounds(), src, b.Min, draw.Src)
	return dst
}

// noise2 returns a cheap deterministic pseudo-random 0..1 for a pixel.
func noise2(x, y int) float64 {
	h := uint32(x*73856093) ^ uint32(y*19349663)
	h ^= h >> 13
	h *= 1274126177
	h ^= h >> 16
	return float64(h&0xFFFF) / 65535.0
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// vnoise is smooth value noise: noise2 on a grid, bilinearly interpolated
// with smoothstep handles — used for continuous surface texture.
func vnoise(x, y float64) float64 {
	ix, iy := math.Floor(x), math.Floor(y)
	fx, fy := x-ix, y-iy
	sx := fx * fx * (3 - 2*fx)
	sy := fy * fy * (3 - 2*fy)
	n00 := noise2(int(ix), int(iy))
	n10 := noise2(int(ix)+1, int(iy))
	n01 := noise2(int(ix), int(iy)+1)
	n11 := noise2(int(ix)+1, int(iy)+1)
	top := n00 + (n10-n00)*sx
	bottom := n01 + (n11-n01)*sx
	return top + (bottom-top)*sy
}

// buildDust precomputes the disc-locked surface layer in disc
// coordinates: low-frequency wear blotches, faint scratches (thin arcs)
// and dust specks. Because it is sampled rotated with the record, the
// whole surface visibly turns. Returns signed grain deviation (about
// -0.5..0.5), feature intensity (0..1), the canvas width and the disc
// center position within the canvas.
func buildDust(radF, labelR float64) (grain, feat []float32, W, R int) {
	R = int(math.Ceil(radF)) + 2
	W = 2*R + 2
	grain = make([]float32, W*W)
	feat = make([]float32, W*W)

	// wear blotches: two octaves of smooth noise
	for y := 0; y < W; y++ {
		for x := 0; x < W; x++ {
			fx, fy := float64(x), float64(y)
			d := vnoise(fx*0.55, fy*0.55) - 0.5
			d += (vnoise(fx*1.7+9.2, fy*1.7+3.1) - 0.5) * 0.6
			grain[y*W+x] = float32(d * 0.5)
		}
	}

	// scratches: thin arcs between the label and the rim
	rnd := rand.New(rand.NewSource(1)) // deterministic wear pattern
	for s := 0; s < 9; s++ {
		rs := labelR + 10 + rnd.Float64()*(radF-labelR-16)
		if rs >= radF-4 || rs <= labelR+2 {
			continue
		}
		a0 := rnd.Float64() * 2 * math.Pi
		arcLen := 0.25 + rnd.Float64()*1.1
		strength := 0.35 + rnd.Float64()*0.65
		for t := 0.0; t < arcLen; t += 0.006 {
			a := a0 + t
			px := int(radF + rs*math.Cos(a))
			py := int(radF + rs*math.Sin(a))
			if px < 0 || py < 0 || px >= W || py >= W {
				continue
			}
			v := float32(strength * (0.55 + 0.45*rnd.Float64()))
			if feat[py*W+px] < v {
				feat[py*W+px] = v
			}
		}
	}

	// dust specks scattered 1px at a time
	for d := 0; d < 110; d++ {
		a := rnd.Float64() * 2 * math.Pi
		r := math.Sqrt(rnd.Float64()) * (radF - 5)
		px := int(radF + r*math.Cos(a))
		py := int(radF + r*math.Sin(a))
		if px < 0 || py < 0 || px >= W || py >= W {
			continue
		}
		feat[py*W+px] = float32(0.35 + 0.65*rnd.Float64())
	}
	return grain, feat, W, R
}

// loadHintFont loads a UI font for the track hint (best effort; a nil
// font just disables the hint text).
func (o *overlay) loadHintFont() {
	cands := []string{
		`C:\Windows\Fonts\segoeuib.ttf`,
		`C:\Windows\Fonts\segoeui.ttf`,
		`C:\Windows\Fonts\arialbd.ttf`,
		`C:\Windows\Fonts\arial.ttf`,
	}
	for _, p := range cands {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		f, err := truetype.Parse(data)
		if err != nil {
			continue
		}
		o.font = f
		break
	}
	if o.font == nil {
		return
	}
	o.ctx = freetype.NewContext()
	o.ctx.SetDPI(96)
	o.ctx.SetFont(o.font)
	o.ctx.SetSrc(&image.Uniform{color.NRGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff}})
	o.ctx.SetHinting(font.HintingFull)
	o.measure = image.NewGray(image.Rect(0, 0, 1, 1))
}

// drawHint letters "Title" over "Artist" onto the black vinyl just
// below a record's label — title larger and brighter, artist smaller
// and dim (visual hierarchy). A line longer than the label band
// marquees. aboveRf > 0 places the band floating just above a disc of
// that radius instead (used for deck-card hints).
func (o *overlay) drawHint(cx, cy, L, strength float64, title, artist string, aboveRf float64) {
	if title == "" && artist == "" {
		return
	}
	if key := title + "\x00" + artist; key != o.hintKey {
		o.hintKey = key
		o.marqX = 0
		o.marqT = time.Time{}
	}
	if o.ctx == nil {
		o.loadHintFont()
		if o.ctx == nil {
			return
		}
	}

	tF := L * 0.17 // title font px (scales with the record)
	if tF < 8 {
		return
	}
	aF := tF * 0.72
	bw := L * 0.72 // text band half-width around the label center
	cw := int(2*bw) + 2
	hTitle := int(1.35 * tF)
	hArtist := int(1.35 * aF)
	H := hTitle + hArtist + 2
	if o.hintA == nil || o.hintA.Bounds().Dx() != cw || o.hintA.Bounds().Dy() != H {
		o.hintA = image.NewGray(image.Rect(0, 0, cw, H))
		o.hintB = image.NewGray(image.Rect(0, 0, cw, H))
	}
	for i := range o.hintA.Pix {
		o.hintA.Pix[i] = 0
	}
	for i := range o.hintB.Pix {
		o.hintB.Pix[i] = 0
	}

	// shared marquee phase; only ticks while hovered (stale dt clamped)
	t := time.Now()
	if !o.marqT.IsZero() {
		if d := t.Sub(o.marqT).Seconds(); d > 0 && d < 0.5 {
			o.marqX += d * 30
		}
	}
	o.marqT = t

	o.ctx.SetDst(o.hintA)
	o.drawHintLine(title, o.hintA, cw, tF, 0)
	o.ctx.SetDst(o.hintB)
	o.drawHintLine(artist, o.hintB, cw, aF, hTitle+2)

	// composite both masks into the premultiplied panel: on the black
	// vinyl below the label (past its 0.44*radF edge), or floating just
	// above a deck card's disc
	top := 1.16 * L // first line's top, center-relative (below label)
	if aboveRf > 0 {
		top = -(aboveRf + float64(H) + 10)
	}
	for row := 0; row < H; row++ {
		by := int(cy+top) + row
		if by < 0 || by >= o.h {
			continue
		}
		brow := o.bits[by*o.w*4:]
		grA := o.hintA.Pix[row*o.hintA.Stride:]
		grB := o.hintB.Pix[row*o.hintB.Stride:]
		for col := 0; col < cw; col++ {
			bx := int(cx) - cw/2 + col
			if bx < 0 || bx >= o.w {
				continue
			}
			avA := float64(grA[col]) / 255 * (0.92 * strength)
			avB := float64(grB[col]) / 255 * (0.55 * strength)
			av := math.Max(avA, avB)
			if av <= 0 {
				continue
			}
			i := bx * 4
			// soft dark ground so ink reads on any cover art
			k := 1 - av*0.55
			brow[i] = byte(float64(brow[i]) * k)
			brow[i+1] = byte(float64(brow[i+1]) * k)
			brow[i+2] = byte(float64(brow[i+2]) * k)
			// ink: title near-white, artist warm gray (hierarchy)
			brow[i] = byte(math.Min(255, float64(brow[i])+avA*225+avB*150))
			brow[i+1] = byte(math.Min(255, float64(brow[i+1])+avA*225+avB*150))
			brow[i+2] = byte(math.Min(255, float64(brow[i+2])+avA*228+avB*155))
		}
	}
}

// drawHintLine renders one centered (or marqueeing) text line into the
// mask dst, baseline at top+0.78*fs; fs is the font size in px.
func (o *overlay) drawHintLine(s string, dst *image.Gray, w int, fs float64, top int) {
	if s == "" {
		return
	}
	o.ctx.SetFontSize(fs)
	o.ctx.SetDst(o.measure)
	adv, _ := o.ctx.DrawString(s, fixed.Point26_6{})
	o.ctx.SetDst(dst)
	sw := (int(adv.X) + 32) / 64
	base := fixed.I(top + int(1.0*fs)) // full em above baseline: no clipped ascenders
	o.ctx.SetClip(dst.Bounds())
	if sw <= w-4 {
		o.ctx.DrawString(s, fixed.Point26_6{X: fixed.I((w - sw) / 2), Y: base})
	} else {
		// marquee loop, second copy trailing for a seamless pass
		loopPx := sw + 40
		x0 := fixed.I(w) - fixed.I(int(o.marqX)%loopPx)
		o.ctx.DrawString(s, fixed.Point26_6{X: x0, Y: base})
		o.ctx.DrawString(s, fixed.Point26_6{X: x0 + fixed.I(loopPx), Y: base})
	}
	o.ctx.SetClip(dst.Bounds())
}

// SetVisible shows or hides the record with the appear/disappear
// animation (fade + sink into the corner). Safe from any goroutine; the
// Run loop picks the latest request up per frame.
var visCh chan bool

func SetVisible(v bool) {
	if visCh != nil {
		select {
		case visCh <- v:
		default:
		}
	}
}

func clampF(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return v
}

// Material Design icon geometry, normalized from the 24x24 viewport:
// bounding box centered at 0, height scaled to 1. Play is the
// play_arrow triangle "M8 5v14l11-7z"; pause is its two bars
// ("M6 19h4V5H6v14z" / "M14 5h4v14h-4z").
var (
	glyphUnitPlay = [][][2]float64{
		{{-0.2857, -0.5}, {-0.2857, 0.5}, {0.5, 0}},
	}
	glyphUnitPause = [][][2]float64{
		{{-0.4286, -0.5}, {-0.1429, -0.5}, {-0.1429, 0.5}, {-0.4286, 0.5}},
		{{0.1429, -0.5}, {0.4286, -0.5}, {0.4286, 0.5}, {0.1429, 0.5}},
	}
)

const (
	glyphHeightFrac = 0.40 // hover glyph height as a fraction of the label radius
	glyphRes        = 160  // mask resolution
	glyphSpan       = 0.55 // half-extent of the mask frame, unit space
	glyphSS         = 8    // supersampling factor per axis
)

// buildGlyphMask rasterizes the given normalized polygon(s) into a
// res x res alpha mask via even-odd fill with 8x8 supersampling.
func buildGlyphMask(polys [][][2]float64) []uint8 {
	m := make([]uint8, glyphRes*glyphRes)
	step := 2 * glyphSpan / glyphRes
	for y := 0; y < glyphRes; y++ {
		uy := -glyphSpan + (float64(y)+0.5)*step
		for x := 0; x < glyphRes; x++ {
			ux := -glyphSpan + (float64(x)+0.5)*step
			hit := 0
			for sy := 0; sy < glyphSS; sy++ {
				py := uy + (float64(sy)+0.5)*step/glyphSS
				for sx := 0; sx < glyphSS; sx++ {
					px := ux + (float64(sx)+0.5)*step/glyphSS
					for _, poly := range polys {
						if pointInPoly(px, py, poly) {
							hit++
							break
						}
					}
				}
			}
			m[y*glyphRes+x] = uint8(hit * 255 / (glyphSS * glyphSS))
		}
	}
	return m
}

func pointInPoly(x, y float64, poly [][2]float64) bool {
	in := false
	j := len(poly) - 1
	for i := 0; i < len(poly); i++ {
		xi, yi := poly[i][0], poly[i][1]
		xj, yj := poly[j][0], poly[j][1]
		if (yi > y) != (yj > y) && x < (xj-xi)*(y-yi)/(yj-yi)+xi {
			in = !in
		}
		j = i
	}
	return in
}

// sampleGlyphMask bilinearly samples the mask; u,v are in unit space
// where the glyph height spans [0,-0.5]..[0,0.5] and the frame covers
// [-glyphSpan, +glyphSpan].
func sampleGlyphMask(m []uint8, u, v float64) float64 {
	fx := (u/(2*glyphSpan) + 0.5) * float64(glyphRes-1)
	fy := (v/(2*glyphSpan) + 0.5) * float64(glyphRes-1)
	x0, y0 := int(fx), int(fy)
	if x0 < 0 || y0 < 0 || x0 >= glyphRes-1 || y0 >= glyphRes-1 {
		return 0
	}
	tx, ty := fx-float64(x0), fy-float64(y0)
	x1, y1 := x0+1, y0+1
	top := float64(m[y0*glyphRes+x0])*(1-tx) + float64(m[y0*glyphRes+x1])*tx
	bot := float64(m[y1*glyphRes+x0])*(1-tx) + float64(m[y1*glyphRes+x1])*tx
	return ((top*(1-ty) + bot*ty) / 255)
}

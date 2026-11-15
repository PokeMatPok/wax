//go:build windows

// Package lyricswin shows the currently sung lyric line in a small
// glass-style always-on-top overlay above the vinyl widget. Words appear
// one by one as they are sung (times interpolated across the line's
// timestamp interval). The window is fully click-through: it's a display,
// not a control.
package lyricswin

import (
	"image"
	"image/color"
	"math"
	"os"
	"runtime"
	"syscall"

	"unsafe"

	"github.com/golang/freetype"
	"github.com/golang/freetype/truetype"
	"golang.org/x/image/font"
	"golang.org/x/image/math/fixed"
)

// Word is one word of the currently displayed line.
type Word struct {
	Text       string
	Start, End float64 // seconds, absolute track time
}

// Options configure the overlay (all callbacks run on the overlay thread).
type Options struct {
	Width, Height int // panel size in physical px
	FromRight     int // gap between panel right edge and screen right edge
	FromBottom    int // gap between panel bottom edge and screen bottom edge
	FontSize      int

	// GetWords returns the current line's words and the playback position
	// in seconds. Empty slice/absent line = panel hidden (fades out).
	GetWords func() ([]Word, float64)

	// Show is polled each frame; false hides the overlay window entirely
	// (used by the settings toggle). nil = always shown.
	Show func() bool
}

var (
	user32      = syscall.NewLazyDLL("user32.dll")
	procRegCls  = user32.NewProc("RegisterClassExW")
	procCreateW = user32.NewProc("CreateWindowExW")
	procDefWnd  = user32.NewProc("DefWindowProcW")
	procMsgWait = user32.NewProc("MsgWaitForMultipleObjects")
	procPeekMsg = user32.NewProc("PeekMessageW")
	procTrans   = user32.NewProc("TranslateMessage")
	procDisp    = user32.NewProc("DispatchMessageW")
	procPostQ   = user32.NewProc("PostQuitMessage")
	procGetDC   = user32.NewProc("GetDC")
	procRelDC   = user32.NewProc("ReleaseDC")
	procSysMet  = user32.NewProc("GetSystemMetrics")
	procULW     = user32.NewProc("UpdateLayeredWindow")
	procShowW   = user32.NewProc("ShowWindow")
	procSetPos  = user32.NewProc("SetWindowPos")
	procGetMod  = syscall.NewLazyDLL("kernel32.dll").NewProc("GetModuleHandleW")
	gdi32       = syscall.NewLazyDLL("gdi32.dll")
	procCDC     = gdi32.NewProc("CreateCompatibleDC")
	procDIB     = gdi32.NewProc("CreateDIBSection")
	procSel     = gdi32.NewProc("SelectObject")
	procDelObj  = gdi32.NewProc("DeleteObject")
	procDelDC   = gdi32.NewProc("DeleteDC")
)

const (
	wsPopup        = 0x80000000
	wsVisible      = 0x10000000
	wsExLayered    = 0x00080000
	wsExTopmost    = 0x00000008
	wsExToolWindow = 0x00000080
	wsExNoActivate = 0x08000000
	wsExTransp     = 0x00000020 // fully mouse-transparent
	wmDestroy      = 0x0002
	wmQuit         = 0x0012
	ulwAlpha       = 2
	qsAllInput     = 0x04FF
	mwmoInputAvail = 0x0001
)

type point struct{ X, Y int32 }
type size struct{ CX, CY int32 }
type msg struct {
	HWnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      point
	_       uintptr
}
type rect struct{ Left, Top, Right, Bottom int32 }

type bitmapInfoHeader struct {
	Size        uint32
	Width       int32
	Height      int32 // negative = top-down
	Planes      uint16
	BitCount    uint16
	Compression uint32
	SizeImage   uint32
	_           int32
	_           int32
	_           uint32
	_           uint32
}

type bitmapInfo struct {
	Header bitmapInfoHeader
	Colors [1]uint32
}

type wndClassEx struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	_          int32
	_          int32
	Instance   uintptr
	Icon       uintptr
	Cursor     uintptr
	Background uintptr
	MenuName   uintptr
	ClassName  uintptr
	IconSm     uintptr
}

type blendFunc struct {
	BlendOp, BlendFlags, SourceConstantAlpha, AlphaFormat byte
}

// Run creates and services the lyrics overlay. Owns its OS thread.
func Run(opts Options) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if opts.Width < 100 {
		opts.Width = 560
	}
	if opts.Height < 40 {
		opts.Height = 84
	}
	if opts.FontSize <= 0 {
		opts.FontSize = 23
	}
	o := &panel{opts: opts, alpha: 0}

	cname, _ := syscall.UTF16PtrFromString("ytmLyricsOverlay")
	cls := wndClassEx{
		Size:      uint32(unsafe.Sizeof(wndClassEx{})),
		WndProc:   syscall.NewCallback(o.wndProc),
		Instance:  hInstance(),
		ClassName: uintptr(unsafe.Pointer(cname)),
	}
	if ret, _, _ := procRegCls.Call(uintptr(unsafe.Pointer(&cls))); ret == 0 {
		return
	}

	// anchor to bottom-right of the primary screen, above the vinyl
	w, h := int32(opts.Width), int32(opts.Height)
	cx, _, _ := procSysMet.Call(0)
	cyy, _, _ := procSysMet.Call(1)
	fr := int32(opts.FromRight)
	fb := int32(opts.FromBottom)

	name, _ := syscall.UTF16PtrFromString("")
	o.win, _, _ = procCreateW.Call(
		uintptr(wsExLayered|wsExTopmost|wsExToolWindow|wsExNoActivate|wsExTransp),
		uintptr(unsafe.Pointer(cname)), uintptr(unsafe.Pointer(name)),
		uintptr(wsPopup|wsVisible),
		uintptr(int32(cx)-w-fr), uintptr(int32(cyy)-h-fb), uintptr(w), uintptr(h),
		0, 0, cls.Instance, 0)
	if o.win == 0 {
		return
	}

	o.w, o.h = int(w), int(h)
	o.anchX = int32(cx) - w - fr
	o.anchBase = fb
	nudgeCh = make(chan int, 64)
	if !o.initSurface() {
		return
	}
	defer o.freeSurface()

	o.loadFont()

	shown := true
	for {
		if opts.Show != nil {
			want := opts.Show()
			if want != shown {
				shown = want
				code := uintptr(0) // SW_HIDE
				if want {
					code = 4 // SW_SHOWNOACTIVATE
				}
				procShowW.Call(o.win, code)
			}
		}
		if shown {
			// live vertical tracking (vinyl hover lean)
		drain:
			for {
				select {
				case p := <-nudgeCh:
					if p != o.extraBtm {
						o.extraBtm = p
						procSetPos.Call(o.win, 0,
							uintptr(o.anchX),
							uintptr(int32(cyy)-h-o.anchBase-int32(p)),
							uintptr(w), uintptr(h),
							0x1|0x4|0x10) // NOSIZE|NOZORDER|NOACTIVATE
					}
				default:
					break drain
				}
			}
			o.render()
		}

		procMsgWait.Call(0, 0, 0, uintptr(33), uintptr(qsAllInput|mwmoInputAvail))
		for {
			var m msg
			ret, _, _ := procPeekMsg.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0, 1)
			if ret == 0 {
				break
			}
			if m.Message == wmQuit {
				return
			}
			procTrans.Call(uintptr(unsafe.Pointer(&m)))
			procDisp.Call(uintptr(unsafe.Pointer(&m)))
			if m.Message == wmDestroy {
				return
			}
		}
	}
}

// nudgeCh carries live upward-shift requests into the panel loop.
var nudgeCh chan int

// NudgeBottom shifts the lyrics panel up from its Options-anchored
// position by the given pixels (0 = the anchored spot). It exists so
// companion overlays (the vinyl disc) can pull the panel along with
// their hover animation. Safe from any goroutine; non-blocking; the
// panel applies the latest value on every frame.
func NudgeBottom(px int) {
	if nudgeCh != nil {
		select {
		case nudgeCh <- px:
		default:
		}
	}
}

type panel struct {
	opts     Options
	win      uintptr
	hdcMem   uintptr
	hbmp     uintptr
	oldBmp   uintptr
	bits     []byte
	w, h     int
	anchX    int32   // anchored left position (screen coords)
	anchBase int32   // anchored bottom offset (screen bottom -> panel bottom)
	extraBtm int     // live upward shift, driven by NudgeBottom
	alpha    float64 // smoothed visibility 0..1
	font     *truetype.Font
	ctx      *freetype.Context
	grayW    int
	gray     *image.Gray // scratch for word masks
	measure  *image.Gray // 1x1 dst for advance measuring
	haveLine bool
}

func (o *panel) initSurface() bool {
	hdcScreen, _, _ := procGetDC.Call(0)
	if hdcScreen == 0 {
		return false
	}
	defer procRelDC.Call(0, hdcScreen)

	hdcMem, _, _ := procCDC.Call(hdcScreen)
	if hdcMem == 0 {
		return false
	}
	o.hdcMem = hdcMem

	bi := bitmapInfo{}
	bi.Header.Size = uint32(unsafe.Sizeof(bi.Header))
	bi.Header.Width = int32(o.w)
	bi.Header.Height = -int32(o.h)
	bi.Header.Planes = 1
	bi.Header.BitCount = 32

	var bitsPtr unsafe.Pointer
	ret, _, _ := procDIB.Call(hdcMem, uintptr(unsafe.Pointer(&bi)), 0, uintptr(unsafe.Pointer(&bitsPtr)), 0, 0)
	if ret == 0 || bitsPtr == nil {
		return false
	}
	o.hbmp = ret
	o.bits = unsafe.Slice((*byte)(bitsPtr), o.w*o.h*4)
	o.oldBmp, _, _ = procSel.Call(hdcMem, ret)
	return true
}

func (o *panel) freeSurface() {
	if o.hdcMem != 0 {
		if o.hbmp != 0 {
			procSel.Call(o.hdcMem, o.oldBmp)
			procDelObj.Call(o.hbmp)
		}
		procDelDC.Call(o.hdcMem)
	}
}

func hInstance() uintptr {
	h, _, _ := procGetMod.Call(0)
	return h
}

func (o *panel) wndProc(hwnd uintptr, m uintptr, wp, lp uintptr) uintptr {
	if m == wmDestroy {
		procPostQ.Call(0)
		return 0
	}
	ret, _, _ := procDefWnd.Call(hwnd, m, wp, lp)
	return ret
}

// loadFont picks the first usable system font for the caption.
func (o *panel) loadFont() {
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
		font, err := truetype.Parse(data)
		if err != nil {
			continue
		}
		o.font = font
		break
	}
	if o.font == nil {
		return
	}
	o.ctx = freetype.NewContext()
	o.ctx.SetDPI(96)
	o.ctx.SetFontSize(float64(o.opts.FontSize))
	o.ctx.SetFont(o.font)
	o.ctx.SetSrc(&image.Uniform{color.NRGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff}})
	o.ctx.SetHinting(font.HintingFull)
	o.grayW = o.w - 48
	o.gray = image.NewGray(image.Rect(0, 0, o.grayW, o.h))
	o.measure = image.NewGray(image.Rect(0, 0, 1, 1))
}

// sdRoundedRect: signed distance to a rounded rectangle (negative inside),
// p relative to panel center. r = corner radius.
func sdRoundedRect(x, y, hw, hh, r float64) float64 {
	ax, ay := math.Abs(x), math.Abs(y)
	qx, qy := ax-(hw-r), ay-(hh-r)
	ox, oy := math.Max(qx, 0), math.Max(qy, 0)
	out := math.Hypot(ox, oy)
	in := math.Min(math.Max(qx, qy), 0)
	return out + in - r
}

// render paints the glass panel + words into the BGRA buffer.
func (o *panel) render() {
	w, h := o.w, o.h
	bits := o.bits
	for i := range bits {
		bits[i] = 0
	}

	words, now := []Word(nil), 0.0
	if o.opts.GetWords != nil {
		words, now = o.opts.GetWords()
	}

	// target visibility: 1 while a line is in its active window
	target := 0.0
	if len(words) > 0 {
		t0, t1 := words[0].Start, words[len(words)-1].End
		if now >= t0-1.2 && now <= t1+0.45 {
			target = 1
		}
	}
	// exponential ease toward target (~120ms), so lines fade in/out
	dt := 0.033
	if target > o.alpha {
		o.alpha += (target - o.alpha) * (1 - math.Exp(-dt/0.10))
	} else {
		o.alpha += (target - o.alpha) * (1 - math.Exp(-dt/0.22))
	}
	if o.alpha < 0.01 && target == 0 {
		o.push()
		return
	}
	if o.font == nil {
		o.push()
		return
	}
	A := o.alpha

	hw, hh := float64(w)/2-1, float64(h)/2-1
	r := float64(o.opts.FontSize) * 0.9
	sw := float64(10) // soft shadow extent

	for y := 0; y < h; y++ {
		py := float64(y) - hh
		row := bits[y*w*4:]
		for x := 0; x < w; x++ {
			px := float64(x) - hw
			d := sdRoundedRect(px, py, hw, hh, r)
			if d > sw {
				continue
			}

			i := x * 4
			var R, G, B, Aa float64
			if d >= 0 {
				// outside: soft black drop shadow
				s := math.Max(0, 1-d/sw)
				Aa = 55 * s * s * A
			} else {
				cov := math.Min(1, 0.5-d) // antialiased edge coverage
				if cov > 0 {
					// vertical glass gradient
					t := (py + hh) / (2 * hh) // 0 top .. 1 bottom
					R, G, B = 20-3*t, 21-3*t, 27-4*t
					// top sheen
					if py < -hh*0.5 {
						s := (-hh*0.5 - py) / (hh * 0.5)
						R += 20 * s
						G += 20 * s
						B += 24 * s
					}
					// frost noise: subtle alpha variation
					op := 0.72 - 0.05*float64(fnoise(x, y)&3)/3
					// inner light border ring
					bd := -d
					if bd < 1.8 {
						k := (1.8 - bd) / 1.8 * 0.5
						R += (240 - R) * k
						G += (240 - G) * k
						B += (245 - B) * k
					}
					Aa = op * cov * A
					R *= Aa // premultiplied
					G *= Aa
					B *= Aa
				}
			}
			row[i] = byte(math.Min(255, B))
			row[i+1] = byte(math.Min(255, G))
			row[i+2] = byte(math.Min(255, R))
			row[i+3] = byte(math.Min(255, Aa))
		}
	}

	o.drawText(words, now)
	o.push()
}

// placedT is one word at its final layout position.
type placedT struct {
	i          int
	x, w, base fixed.Int26_6
}

// drawText lays out the line centered, wrapping it onto up to two rows
// (shrinking the font until it fits), reveals each word at its start time
// (fade), composites glyphs into the panel buffer.
func (o *panel) drawText(words []Word, now float64) {
	if len(words) == 0 || o.ctx == nil {
		return
	}
	w, h := o.w, o.h
	o.ctx.SetDst(o.measure)

	// row wrap: try successively smaller font sizes until the line fits
	// the panel width in few enough rows.
	type rowT struct {
		idxs []int
		w    fixed.Int26_6
	}
	fs := o.opts.FontSize
	var widths []fixed.Int26_6
	var spaceW fixed.Int26_6
	var rows []rowT
	overflow := false
	for {
		o.ctx.SetFontSize(float64(fs))
		spaceW = fixed.I(0)
		if adv, _ := o.ctx.DrawString(" ", fixed.Point26_6{}); adv.X > 0 {
			spaceW = adv.X
		}
		widths = make([]fixed.Int26_6, len(words))
		for i, wd := range words {
			adv, _ := o.ctx.DrawString(wd.Text, fixed.Point26_6{})
			widths[i] = adv.X
		}
		maxW := fixed.I(o.grayW)
		rows = rows[:0]
		cur := rowT{}
		overflow = false
		for i := range words {
			ww := widths[i]
			if ww > maxW {
				overflow = true
			}
			if len(cur.idxs) > 0 && cur.w+spaceW+ww > maxW {
				rows = append(rows, cur)
				cur = rowT{w: ww}
			} else if len(cur.idxs) > 0 {
				cur.w += spaceW + ww
			} else {
				cur.w = ww
			}
			cur.idxs = append(cur.idxs, i)
		}
		rows = append(rows, cur)
		lineH := int(float64(fs) * 1.35)
		maxLines := max(1, (h-12)/lineH)
		if len(rows) <= maxLines && !overflow {
			break
		}
		if fs <= 13 {
			break // keep this size; extra rows just clip
		}
		fs -= 2
	}

	// place every word: rows are centered horizontally and the text
	// block is centered vertically.
	lineH := int(float64(fs) * 1.35)
	blockH := lineH * len(rows)
	top := h/2 - blockH/2
	ascent := int(float64(fs) * 0.78)

	placed := make([]placedT, 0, len(words))
	for r, row := range rows {
		base := fixed.I(top + r*lineH + ascent)
		pen := fixed.I((w - (int(row.w)+32)/64) / 2)
		for _, i := range row.idxs {
			placed = append(placed, placedT{i: i, x: pen, w: widths[i], base: base})
			pen += widths[i] + spaceW
		}
	}

	// per-word reveal alpha (quick ramp at the word's start time)
	wa := make([]float64, len(words))
	for i, wd := range words {
		if now >= wd.Start {
			wa[i] = math.Min(1, (now-wd.Start)/0.10+0.3)
		}
		if now >= wd.End {
			wa[i] = 1
		}
	}

	o.paint(placed, wa, words)
}

// paint renders the placed words into the combined glyph mask, then
// composites the mask into the panel buffer.
func (o *panel) paint(placed []placedT, wa []float64, words []Word) {
	// combined glyph mask, word by word at its layout position
	gray := o.gray
	for i := range gray.Pix {
		gray.Pix[i] = 0
	}
	o.ctx.SetDst(gray)
	for _, pw := range placed {
		if wa[pw.i] <= 0 {
			continue
		}
		xs := max(0, (int(pw.x)+32)/64-2)
		xe := min(o.grayW, (int(pw.x+pw.w)+32)/64+2)
		if xe <= xs {
			continue
		}
		o.ctx.SetClip(image.Rect(xs, 0, xe, gray.Bounds().Dy()))
		o.ctx.DrawString(words[pw.i].Text, fixed.Point26_6{X: pw.x, Y: pw.base})
	}
	o.ctx.SetClip(gray.Bounds())

	// composite into BGRA: alpha = glyph * word reveal * panel alpha
	w, h := o.w, o.h
	bits := o.bits
	for _, pw := range placed {
		if wa[pw.i] <= 0 {
			continue
		}
		xs := max(0, (int(pw.x)+31)/64-2)
		xe := min(o.grayW, (int(pw.x+pw.w)+32)/64+2)
		for y := 0; y < h; y++ {
			row := bits[y*w*4:]
			grow := gray.Pix[y*gray.Stride:]
			for x := xs; x < xe; x++ {
				gv := int(grow[x])
				if gv == 0 {
					continue
				}
				av := float64(gv) / 255 * wa[pw.i] * o.alpha
				i := x * 4
				row[i] = byte(math.Min(255, float64(row[i])+av*238))
				row[i+1] = byte(math.Min(255, float64(row[i+1])+av*238))
				row[i+2] = byte(math.Min(255, float64(row[i+2])+av*242))
				row[i+3] = byte(math.Min(255, float64(row[i+3])+av*255))
			}
		}
	}
}

// push uploads the buffer to the compositor.
func (o *panel) push() {
	hdcScreen, _, _ := procGetDC.Call(0)
	if hdcScreen == 0 {
		return
	}
	defer procRelDC.Call(0, hdcScreen)
	psize := size{CX: int32(o.w), CY: int32(o.h)}
	ptSrc := point{}
	bf := blendFunc{BlendOp: 0, SourceConstantAlpha: 255, AlphaFormat: 1}
	procULW.Call(o.win, hdcScreen, 0,
		uintptr(unsafe.Pointer(&psize)), o.hdcMem,
		uintptr(unsafe.Pointer(&ptSrc)), 0,
		uintptr(unsafe.Pointer(&bf)), ulwAlpha)
}

// fnoise: cheap deterministic per-pixel hash for frost variation.
func fnoise(x, y int) uint32 {
	h := uint32(x*374761393) ^ uint32(y*668265263)
	h ^= h >> 13
	h *= 1274126177
	h ^= h >> 16
	return h
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

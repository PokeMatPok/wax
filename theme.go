package main

// The app's visual kit: palette, easing, interaction states (hover /
// press / selected), and flat geometric icons. Nothing here talks to the
// engine; everything is pure drawing + widgets.

import (
	"image"
	"image/color"
	"time"

	"gio.tools/icons"
	"gioui.org/f32"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"
)

// ---- palette: near-monochrome dark, one warm accent ----

var (
	colBG     = color.NRGBA{R: 0x14, G: 0x14, B: 0x18, A: 0xff} // app background
	colPanel  = color.NRGBA{R: 0x1e, G: 0x1e, B: 0x25, A: 0xff} // raised surfaces
	colField  = color.NRGBA{R: 0x26, G: 0x26, B: 0x2e, A: 0xff} // inputs
	colAmber  = color.NRGBA{R: 0xd9, G: 0xa4, B: 0x41, A: 0xff} // the single accent
	colText   = color.NRGBA{R: 0xe9, G: 0xe6, B: 0xdf, A: 0xff} // primary text
	colSub    = color.NRGBA{R: 0x9b, G: 0x98, B: 0x90, A: 0xff} // secondary text
	colHint   = color.NRGBA{R: 0x67, G: 0x65, B: 0x5e, A: 0xff} // placeholders, timestamps
	colStroke = color.NRGBA{R: 0x35, G: 0x35, B: 0x3e, A: 0xff} // hairlines, field borders
)

// interaction overlays are translucent white on dark; alpha comes from
// the eased hover/press value, the colors below are full-strength bases.
var (
	colHover = color.NRGBA{R: 0xff, G: 0xff, B: 0xff, A: 0x0f}
	colPress = color.NRGBA{R: 0xff, G: 0xff, B: 0xff, A: 0x1d}
)

// ---- easing: strong ease-out curves (cubic-bezier(0.23, 1, 0.32, 1)
// family), so motion starts fast and settles gently ----

func easeOutQuint(t float64) float64 {
	u := 1 - t
	return 1 - u*u*u*u*u
}

func easeOutCubic(t float64) float64 {
	u := 1 - t
	return 1 - u*u*u
}

// anim is a one-shot eased scalar. laid out in UI frames, advanced by
// frame time; refreshLoop just checks settledness to keep frames flowing.
type anim struct {
	from, to float64
	start    time.Time
	dur      time.Duration
	done     bool
}

func newAnim(v float64) anim { return anim{from: v, to: v, done: true} }

// seek retargets the animation; retargeting mid-flight keeps the current
// value (transitions stay interruptible).
func (a *anim) seek(to float64, now time.Time, dur time.Duration) {
	if to == a.to && !a.done {
		return
	}
	a.from = a.val(now)
	a.to = to
	a.start = now
	a.dur = dur
	a.done = false
}

func (a *anim) val(now time.Time) float64 {
	if a.done {
		return a.to
	}
	t := float64(now.Sub(a.start)) / float64(a.dur)
	if t >= 1 {
		a.done = true
		return a.to
	}
	return a.from + (a.to-a.from)*easeOutQuint(t)
}

func (a *anim) settled() bool { return a.done }

// inter is the eased interaction state of one clickable.
type inter struct {
	hover anim // 0..1
	press anim // 0..1
}

func (i *inter) animating() bool {
	return !i.hover.settled() || !i.press.settled()
}

// interState looks up (creating if needed) this clickable's eased state
// and advances it to `now`. Call from the UI goroutine only.
func (ui *appUI) interState(c *widget.Clickable, now time.Time) *inter {
	st := ui.interMap[c]
	if st == nil {
		st = &inter{hover: newAnim(0), press: newAnim(0)}
		if ui.interMap == nil {
			ui.interMap = map[*widget.Clickable]*inter{}
		}
		ui.interMap[c] = st
	}
	h, p := float64(0), float64(0)
	if c.Hovered() {
		h = 1
	}
	if c.Pressed() {
		p = 1
	}
	st.hover.seek(h, now, 150*time.Millisecond)
	st.press.seek(p, now, 120*time.Millisecond)
	return st
}

// uiMicroAnimating reports whether any hover/press fade or the rail
// indicator is still settling; refreshLoop uses it to keep frames coming.
func (ui *appUI) uiMicroAnimating() bool {
	if ui.homeMarqActive.Load() {
		return true
	}
	ui.mu.Lock()
	defer ui.mu.Unlock()
	if !ui.railY.settled() {
		return true
	}
	for c, st := range ui.interMap {
		if st.animating() {
			return true
		}
		// a state flipped since the last paint but no layout advanced it
		if c.Hovered() != (st.hover.to == 1) || c.Pressed() != (st.press.to == 1) {
			return true
		}
	}
	return false
}

// ---- drawing helpers ----

// fillRect paints a rounded rect of the given size and color.
func fillRect(gtx layout.Context, r image.Rectangle, radius int, c color.NRGBA) {
	defer clip.UniformRRect(r, radius).Push(gtx.Ops).Pop()
	paint.FillShape(gtx.Ops, c, clip.Rect(r).Op())
}

// panelRing draws a rounded panel with a hairline border behind w.
func panelRing(gtx layout.Context, radius int, bg, ring color.NRGBA, w layout.Widget) layout.Dimensions {
	macro := op.Record(gtx.Ops)
	dims := w(gtx)
	call := macro.Stop()
	r := image.Rectangle{Max: dims.Size}
	fillRect(gtx, r, radius, ring)
	inner := image.Rectangle{Min: image.Pt(1, 1), Max: dims.Size.Sub(image.Pt(1, 1))}
	if inner.Dx() > 2 && inner.Dy() > 2 {
		fillRect(gtx, inner, radius-1, bg)
	}
	call.Add(gtx.Ops)
	return dims
}

// ---- icons: official Material Design glyphs, packed by gio.tools/icons ----

// drawIcon renders one of the packed Material icons, sized to the
// constraints the caller gives it (icon buttons pass their exact pixel
// size; navRail falls back to the 24dp default of widget.Icon).
func drawIcon(gtx layout.Context, ic *widget.Icon, c color.NRGBA) {
	ic.Layout(gtx, c)
}

func iconSearch(gtx layout.Context, c color.NRGBA) { drawIcon(gtx, icons.ActionSearch, c) }
func iconQueue(gtx layout.Context, c color.NRGBA)  { drawIcon(gtx, icons.AVQueueMusic, c) }
func iconLyrics(gtx layout.Context, c color.NRGBA) { drawIcon(gtx, icons.AVSubtitles, c) }
func iconPlay(gtx layout.Context, c color.NRGBA)   { drawIcon(gtx, icons.AVPlayArrow, c) }
func iconPause(gtx layout.Context, c color.NRGBA)  { drawIcon(gtx, icons.AVPause, c) }
func iconPrev(gtx layout.Context, c color.NRGBA)   { drawIcon(gtx, icons.AVSkipPrevious, c) }
func iconNext(gtx layout.Context, c color.NRGBA)   { drawIcon(gtx, icons.AVSkipNext, c) }
func iconStop(gtx layout.Context, c color.NRGBA)   { drawIcon(gtx, icons.AVStop, c) }
func iconPlus(gtx layout.Context, c color.NRGBA)   { drawIcon(gtx, icons.ContentAdd, c) }
func iconRadio(gtx layout.Context, c color.NRGBA)  { drawIcon(gtx, icons.AVRadio, c) }
func iconHome(gtx layout.Context, c color.NRGBA)   { drawIcon(gtx, icons.ActionHome, c) }

// iconBtn is a flat circular icon button with eased hover/press states
// and a subtle press scale (0.97), the way modern buttons respond.
// draw runs inside; accentCol (may be nil) tints icon + hover ring when
// the control is active.
func (ui *appUI) iconBtn(gtx layout.Context, c *widget.Clickable, size int, draw func(layout.Context, color.NRGBA), accent bool) layout.Dimensions {
	now := gtx.Now
	if now.IsZero() {
		now = time.Now()
	}
	st := ui.interState(c, now)
	sz := gtx.Dp(unit.Dp(size))

	// background: hover tint (and stronger when pressed)
	hv := float32(st.hover.val(now)) * (1 - float32(st.press.val(now))*0.4)
	if hv > 0.01 {
		hbg := colHover
		hbg.A = uint8(float32(colHover.A) * hv)
		fillRect(gtx, image.Rect(0, 0, sz, sz), sz/2, hbg)
	}
	pw := float32(st.press.val(now))
	if pw > 0.01 {
		pbg := colPress
		pbg.A = uint8(float32(colPress.A) * pw)
		fillRect(gtx, image.Rect(0, 0, sz, sz), sz/2, pbg)
	}

	// press feedback: the glyph scales down slightly (never from zero)
	ic := colText
	if accent {
		ic = colAmber
	}
	// the glyph draws at the button's exact pixel size (drawIcon reads the
	// constraints), so no base scaling transform is needed; press feedback
	// applies a tiny scale about the button center.
	return c.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		sc := 1 - 0.03*pw
		off := float32(sz) / 2 * (1 - sc)
		defer op.Affine(f32.NewAffine2D(sc, 0, off, 0, sc, off)).Push(gtx.Ops).Pop()
		gtx.Constraints = layout.Exact(image.Pt(sz, sz))
		draw(gtx, ic)
		return layout.Dimensions{Size: image.Pt(sz, sz)}
	})
}

// navRail draws the left navigation: three tabs, hover states, an
// animated amber selection indicator, and a queue badge. Click handling
// happens in layoutRoot over the same clickables.
func (ui *appUI) navRail(gtx layout.Context) layout.Dimensions {
	now := gtx.Now
	if now.IsZero() {
		now = time.Now()
	}
	w := gtx.Dp(unit.Dp(76))
	tabH := gtx.Dp(unit.Dp(72))

	fillRect(gtx, image.Rect(0, 0, w, gtx.Constraints.Max.Y), 0, colPanel)

	// animated selection indicator (280ms ease-out quint slide); when the
	// settings page is open the indicator slides down to the gear pinned
	// at the rail's bottom edge
	tab := 0
	switch {
	case ui.tab == tabSearch:
		tab = 1
	case ui.tab == tabQueue:
		tab = 2
	case ui.tab == tabLyrics:
		tab = 3
	}
	railH := gtx.Constraints.Max.Y
	gearY := railH - tabH
	target := tab*tabH + tabH/2 - 12
	if ui.tab == tabSettings {
		target = gearY + tabH/2 - 12
	}
	ui.railY.seek(float64(target), now, 280*time.Millisecond)
	y := int(ui.railY.val(now))
	if y >= 0 {
		fillRect(gtx, image.Rect(8, y, 12, y+24), 2, colAmber)
	}

	// app mark
	if y0 := gtx.Dp(unit.Dp(12)); true {
		fillRect(gtx, image.Rect(w/2-7, y0, w/2+7, y0+14), 7, colAmber)
	}

	type tabdef struct {
		c     *widget.Clickable
		draw  func(layout.Context, color.NRGBA)
		badge bool
	}
	ui.mu.Lock()
	haveQueue := len(ui.queue) > 0
	ui.mu.Unlock()
	defs := []tabdef{
		{&ui.homeTabBtn, iconHome, false},
		{&ui.searchTabBtn, iconSearch, false},
		{&ui.queueTabBtn, iconQueue, haveQueue},
		{&ui.lyricsTabBtn, iconLyrics, false},
	}
	for i, d := range defs {
		selected := i == tab
		st := ui.interState(d.c, now)
		if hv := float32(st.hover.val(now)); hv > 0.01 {
			hbg := colHover
			hbg.A = uint8(float32(colHover.A) * hv)
			fillRect(gtx, image.Rect(6, i*tabH+8, w-6, (i+1)*tabH-8), 12, hbg)
		}
		if press := float32(st.press.val(now)); press > 0.01 {
			pbg := colPress
			pbg.A = uint8(float32(colPress.A) * press)
			fillRect(gtx, image.Rect(6, i*tabH+8, w-6, (i+1)*tabH-8), 12, pbg)
		}
		ic := colSub
		if selected {
			ic = colAmber
		}
		draw, cls, badge := d.draw, d.c, d.badge
		// the tab's click area is its whole row
		tr := op.Offset(image.Pt(0, i*tabH)).Push(gtx.Ops)
		cls.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			sz := gtx.Dp(unit.Dp(24))
			defer op.Offset(image.Pt((w-sz)/2, (tabH-sz)/2)).Push(gtx.Ops).Pop()
			draw(gtx, ic)
			return layout.Dimensions{Size: image.Pt(w, tabH)}
		})
		tr.Pop()
		if badge {
			bx, by := w-26, i*tabH+gtx.Dp(unit.Dp(18))
			fillRect(gtx, image.Rect(bx, by, bx+6, by+6), 3, colAmber)
		}
	}

	// settings gear, pinned to the rail's bottom edge
	gearSelected := ui.tab == tabSettings
	gst := ui.interState(&ui.gearBtn, now)
	if hv := float32(gst.hover.val(now)) * (1 - float32(gst.press.val(now))*0.4); hv > 0.01 {
		hbg := colHover
		hbg.A = uint8(float32(colHover.A) * hv)
		fillRect(gtx, image.Rect(6, gearY+8, w-6, gearY+tabH-8), 12, hbg)
	}
	if press := float32(gst.press.val(now)); press > 0.01 {
		pbg := colPress
		pbg.A = uint8(float32(colPress.A) * press)
		fillRect(gtx, image.Rect(6, gearY+8, w-6, gearY+tabH-8), 12, pbg)
	}
	gic := colSub
	if gearSelected {
		gic = colAmber
	}
	gr := op.Offset(image.Pt(0, gearY)).Push(gtx.Ops)
	ui.gearBtn.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		sz := gtx.Dp(unit.Dp(24))
		defer op.Offset(image.Pt((w-sz)/2, (tabH-sz)/2)).Push(gtx.Ops).Pop()
		drawIcon(gtx, icons.ActionSettings, gic)
		return layout.Dimensions{Size: image.Pt(w, tabH)}
	})
	gr.Pop()

	return layout.Dimensions{Size: image.Pt(w, railH)}
}

// ---- marquee: horizontally scrolling label for text that overflows ----

const (
	marqueeSpeedPx = 42 // px/s while scrolling
	marqueePause   = 900 * time.Millisecond
)

// marquee is the scroll state of one overflowing label. Touched only from
// the UI goroutine; the refresh loop learns about activity via the
// homeMarqActive atomic instead of reading these fields.
type marquee struct {
	offset  float64
	last    time.Time
	phase   int // 0 = start pause, 1 = scrolling, 2 = end pause
	phaseAt time.Time
}

// advance steps the pause-scroll-pause-reset cycle and returns the current
// pixel offset. textW is the full text width, viewW the visible window.
func (m *marquee) advance(now time.Time, textW, viewW int) float64 {
	if m.last.IsZero() {
		m.last, m.phaseAt = now, now
	}
	dt := now.Sub(m.last).Seconds()
	m.last = now
	max := float64(textW - viewW)
	if max < 0 {
		max = 0
	}
	switch m.phase {
	case 0: // hold at the start so the beginning is readable
		if now.Sub(m.phaseAt) >= marqueePause {
			m.phase, m.phaseAt = 1, now
		}
	case 1:
		m.offset += marqueeSpeedPx * dt
		if m.offset >= max {
			m.offset = max
			m.phase, m.phaseAt = 2, now
		}
	case 2: // hold at the end so the ending is readable
		if now.Sub(m.phaseAt) >= marqueePause {
			m.offset = 0
			m.phase, m.phaseAt = 0, now
		}
	}
	return m.offset
}

// reset parks the marquee back at its start (used when hover ends).
func (m *marquee) reset() {
	m.offset = 0
	m.phase = 0
	m.last = time.Time{}
}

// marqueeLabel renders lbl inside viewW. Text that fits renders
// statically; overflowing text renders ellipsized until animate is true,
// then it scrolls through the full text. Returns the label dimensions.
func (ui *appUI) marqueeLabel(gtx layout.Context, lbl material.LabelStyle, mq *marquee, viewW int, animate bool) layout.Dimensions {
	// natural single-line size, unconstrained width
	mgtx := gtx
	mgtx.Constraints = layout.Constraints{Max: image.Pt(1<<26, gtx.Constraints.Max.Y)}
	macro := op.Record(gtx.Ops)
	dims := lbl.Layout(mgtx)
	call := macro.Stop()
	if dims.Size.X <= viewW {
		call.Add(gtx.Ops)
		return dims
	}
	if !animate {
		mq.reset()
		cgtx := gtx
		cgtx.Constraints.Max.X = viewW
		return lbl.Layout(cgtx) // MaxLines=1 -> ellipsized
	}
	ui.homeMarqActive.Store(true)
	off := mq.advance(gtx.Now, dims.Size.X, viewW)
	h := dims.Size.Y
	cl := clip.Rect{Max: image.Pt(viewW, h)}.Push(gtx.Ops)
	tr := op.Offset(image.Pt(-int(off), 0)).Push(gtx.Ops)
	call.Add(gtx.Ops)
	tr.Pop()
	cl.Pop()
	return layout.Dimensions{Size: image.Pt(viewW, h)}
}

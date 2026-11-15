// vinyl geometry: the overlay disc's center sits exactly on the
// bottom-right corner of the screen's work area, so only its upper-left
// quarter is visible — the disc peeks out from the screen corner itself.
const vinylDp = 220 // dp diameter

var windowBG = color.NRGBA{R: 0xef, G: 0xef, B: 0xf1, A: 0xff}

// startVinylOverlay launches the transparent screen-corner vinyl once the
// window DPI is known. The overlay runs its own OS thread + message loop
// (internal/vinylwin) and talks back through the callbacks below.
func (ui *appUI) startVinylOverlay(gtx layout.Context) {
	radius := int(float32(vinylDp) / 2 * gtx.Metric.PxPerDp)
	go vinylwin.Run(vinylwin.Options{
		RadiusPx: radius,
		Angle: func() float64 {
			ui.mu.Lock()
			defer ui.mu.Unlock()
			return ui.vinylAngle
		},
		State: func() (playing bool, art image.Image) {
			st := player.Status{}
			if ui.engine != nil {
				st = ui.engine.Status()
			}
			ui.mu.Lock()
			t := ui.nowTrack
			ui.mu.Unlock()
			if t.VideoID != "" {
				art, _ = ui.covers.Get(t.VideoID, t.Thumbnail)
			}
			return st.State == player.StatePlaying, art
		},
		Clicked: func() {
			if ui.engine == nil {
				return
			}
			st := ui.engine.Status()
			switch st.State {
			case player.StatePlaying:
				ui.mu.Lock()
				ui.vinylVel -= 700 // scratch kick backwards
				ui.mu.Unlock()
				ui.engine.SetPaused(true)
			case player.StatePaused:
				ui.mu.Lock()
				if ui.vinylVel < 60 {
					ui.vinylVel = 60 // spin-up momentum
				}
				ui.mu.Unlock()
				ui.engine.SetPaused(false)
			}
		},
	})
}

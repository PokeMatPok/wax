package main

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"math"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gioui.org/app"
	"gioui.org/io/key"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"ytm-client/internal/cover"
	"ytm-client/internal/extract"
	"ytm-client/internal/history"
	"ytm-client/internal/lyrics"
	"ytm-client/internal/lyricswin"
	"ytm-client/internal/player"
	"ytm-client/internal/prefs"
	"ytm-client/internal/recommend"
	"ytm-client/internal/smtc"
	"ytm-client/internal/vinylwin"
	"ytm-client/internal/ytm"
)

type searchResult struct {
	query  string
	tracks []ytm.Track
	err    error
}

type appUI struct {
	window    *app.Window
	th        *material.Theme
	editor    widget.Editor
	searchBtn widget.Clickable
	list      widget.List
	rows      []widget.Clickable
	results   []ytm.Track
	status    string
	busy      bool
	ch        chan searchResult

	// playback
	extractor extract.Extractor
	engine    player.Engine
	nowTrack  ytm.Track
	// playingTrack is the track actually (or last) playing, distinct
	// from nowTrack, which is set optimistically before the stream has
	// resolved so the player shows the new pick instantly.
	playingTrack ytm.Track
	mu           sync.Mutex
	nowTitle     string

	// transport controls (icon buttons, styled in theme.go)
	playToggle widget.Clickable
	stopBtn    widget.Clickable
	prevBtn    widget.Clickable
	nextBtn    widget.Clickable
	radioBtn   widget.Clickable
	volume     widget.Float
	// volume as 0..100 int, last one sent to the engine
	volumeSent   int
	muted        bool
	savedVol     int // volume before mute
	seek         widget.Float
	prevDragging bool

	// navigation (left rail) tabs
	tab          int // tabHome / tabSearch / tabQueue / tabLyrics
	homeTabBtn   widget.Clickable
	searchTabBtn widget.Clickable
	queueTabBtn  widget.Clickable
	lyricsTabBtn widget.Clickable
	railY        anim // animated selection indicator position
	interMap     map[*widget.Clickable]*inter
	editFocus    bool // search field focus ring

	// loading indicator state
	loadingIdx int // row currently being resolved, -1 if none
	loadCancel context.CancelFunc

	// covers
	covers *cover.Cache

	// lyrics
	lyrClient *lyrics.Client
	lyrList   widget.List
	curLyrics *lyrics.Lyrics
	lyrEnds   []float64 // seconds: end of each synced line
	lyrErr    error
	lyrBusy   bool

	// vinyl overlay widget: angle + velocity live here, the overlay
	// window itself lives in internal/vinylwin.
	vinylAngle float64 // degrees, grows while playing
	vinylVel   float64 // deg/s; eases between 0 (paused) and vinylSpeed

	// queue & radio
	queue           []ytm.Track // upcoming, consumed from the front
	history         []ytm.Track // recently played, newest last
	autoplay        bool        // radio keeps the queue topped up
	qRows           []widget.Clickable
	rowsPlus        []widget.Clickable
	qlist           widget.List
	stoppedManually bool // set by the Stop button; suppresses auto-advance

	// dedupe for end-of-track events: mpv reports a natural end twice
	// (eof-reached property + end-file event), both as EventStopped.
	lastEndedURL string
	lastEndedAt  time.Time

	// persistent listening history + the Home tab built from it
	hstore         *history.Store
	rstore         *recommend.Store // radio cache behind the recommendation engine
	hits           []ytm.Track      // recently listened, newest first
	picks          []ytm.Track      // "for you": radio-derived, ranked
	homeBusy       bool             // a recommendation refresh is in flight
	homeRows       []widget.Clickable
	homePlus       []widget.Clickable
	homeMarq       []homeMarq  // per-tile marquee states (title, artist)
	homeMarqActive atomic.Bool // any marquee scrolled on the last frame
	hlist          widget.List

	// settings (gear at the rail's bottom)
	gearBtn  widget.Clickable
	clearBtn widget.Clickable
	recOn    widget.Bool
	bubOn    widget.Bool
	hideBtn  widget.Clickable
	// vinyl auto-hide timeout (minutes; -1 = never), synced with prefs
	vinylHideMin atomic.Int32
	prefs        *prefs.Store
	recData      bool // mirrored under mu; read by playTrack
	bubShow      bool // mirrored under mu; polled by the lyrics overlay

	// SMTC (Windows media overlay): commands flow in from media keys via
	// smtcCmds; publisher state is touched only by the refresh loop.
	smtcCmds  chan smtc.Button
	smtcTrack ytm.Track    // last track published to SMTC
	smtcCover string       // last cover path published to SMTC
	smtcState player.State // last playback state published to SMTC
	smtcTlAt  time.Time    // last timeline publish (1 Hz throttle)
	smtcTlPos time.Duration
}

func newAppUI() *appUI {
	th := material.NewTheme()
	th.Fg = colText
	th.Bg = colBG
	ui := &appUI{
		th:         th,
		ch:         make(chan searchResult, 1),
		list:       widget.List{List: layout.List{Axis: layout.Vertical}},
		lyrList:    widget.List{List: layout.List{Axis: layout.Vertical}},
		qlist:      widget.List{List: layout.List{Axis: layout.Vertical}},
		status:     "Search for something, click a result to play.",
		volume:     widget.Float{Value: 1},
		nowTitle:   "Nothing playing",
		loadingIdx: -1,
		autoplay:   true,
		railY:      newAnim(24),
		interMap:   map[*widget.Clickable]*inter{},
		covers:     cover.New(),
		lyrClient:  lyrics.NewClient(),
		smtcCmds:   make(chan smtc.Button, 8),
	}
	ui.hstore = history.Load()
	ui.rstore = recommend.Open()
	ui.hlist = widget.List{List: layout.List{Axis: layout.Vertical}}

	// settings
	ui.prefs = prefs.Load()
	p := ui.prefs.Get()
	ui.recOn = widget.Bool{Value: p.Recording}
	ui.bubOn = widget.Bool{Value: p.LyricsBubble}
	ui.recData = p.Recording
	ui.bubShow = p.LyricsBubble
	ui.vinylHideMin.Store(int32(p.VinylHideMin))

	// restore the persisted queue (dropping any broken entries)
	for _, t := range p.Queue {
		if t.VideoID != "" {
			ui.queue = append(ui.queue, t)
		}
	}
	if len(ui.queue) > 0 {
		ui.status = fmt.Sprintf("Restored %d queued track%s.", len(ui.queue), plural(len(ui.queue)))
	}

	if len(ui.hstore.Recent(1)) > 0 {
		ui.tab = tabHome // returning listener: land on their mixes
	}
	return ui
}

// tab identity for the navigation rail.
const (
	tabHome = iota
	tabSearch
	tabQueue
	tabLyrics
	tabSettings
)

func main() {
	vinylwin.Setup() // per-monitor DPI awareness, before any window exists
	ui := newAppUI()
	go ui.queueSaver()
	if len(ui.hstore.Recent(1)) > 0 {
		go ui.refreshHome() // returning listener: mixes ready on arrival
	}
	go func() {
		w := &app.Window{}
		ui.window = w
		ui.covers.OnLoad = func(string) { w.Invalidate() }
		w.Option(app.Title("Wax"), app.Size(unit.Dp(720), unit.Dp(760)))
		if err := ui.run(w); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}()
	app.Main()
}

func (ui *appUI) run(w *app.Window) error {
	if x, err := extract.NewYtDLP(""); err == nil {
		ui.extractor = x
	}
	if e, err := player.NewMpv(); err == nil {
		if err := e.Start(); err == nil {
			ui.engine = e
		} else {
			ui.status = "mpv failed to start: " + err.Error()
		}
	} else {
		ui.status = err.Error()
	}
	// Windows media overlay: register as the SMTC source so the flyout
	// shows track info + art and media keys drive the same control path
	// as the in-app buttons and shortcuts. Buttons are funneled through
	// a channel and dispatched on the refresh loop (never blocks the
	// WinRT callback thread).
	go func() {
		smtc.Start(func(b smtc.Button) {
			select {
			case ui.smtcCmds <- b:
			default:
			}
		})
	}()

	defer func() {
		// graceful exit: persist the queue one last time
		ui.mu.Lock()
		q := make([]ytm.Track, len(ui.queue))
		copy(q, ui.queue)
		ui.mu.Unlock()
		ui.prefs.SaveQueue(q)
		if ui.engine != nil {
			ui.engine.Close()
		}
	}()

	// Redraw on playback changes: the engine's state (position ticks,
	// pause, track end) updates itself, but Gio only repaints on events.
	// A change-detector poller invalidates the window when the visible
	// status actually differs from the last frame. Idle app = no repaints.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ui.refreshLoop(ctx, w)

	ops := new(op.Ops)
	vinylStarted := false
	for {
		e := w.Event()
		switch e := e.(type) {
		case app.DestroyEvent:
			return e.Err
		case app.FrameEvent:
			ui.drain()
			gtx := app.NewContext(ops, e)
			if !vinylStarted {
				vinylStarted = true
				ui.startVinylOverlay(gtx)
			}
			ui.layoutRoot(gtx)
			e.Frame(gtx.Ops)
		}
	}
}

// refreshLoop is the app's animation clock: it advances vinyl physics and
// invalidates the window whenever anything visible changes (~30 fps while
// playing or spinning, none when fully idle).
func (ui *appUI) refreshLoop(ctx context.Context, w *app.Window) {
	ticker := time.NewTicker(33 * time.Millisecond)
	defer ticker.Stop()
	prevTick := time.Now()
	var last player.Status
	var dt float64
	for {
		select {
		case <-ctx.Done():
			return
		case b := <-ui.smtcCmds:
			ui.dispatchSMTCButton(b)
			continue
		case now := <-ticker.C:
			dt = now.Sub(prevTick).Seconds()
			prevTick = now
		}

		ui.mu.Lock()
		vel := ui.vinylVel
		ui.mu.Unlock()

		st := player.Status{}
		if ui.engine != nil {
			st = ui.engine.Status()
		}
		// vinyl target velocity: full speed playing, stopped paused; a big
		// negative kick on pause reads as DJ scratching (set in handlers).
		const vinylSpeed = 200.0 // deg/s (roughly 33 RPM)
		if st.State == player.StatePlaying {
			vel += (vinylSpeed - vel) * (1 - math.Exp(-8*dt))
		} else {
			vel += (0 - vel) * (1 - math.Exp(-3*dt))
		}
		angle := ui.vinylAngle + vel*dt
		if math.Abs(vel) < 1.5 && st.State != player.StatePlaying {
			vel = 0 // settle exactly, so invalidation stops
		}
		ui.mu.Lock()
		ui.vinylVel = vel
		ui.vinylAngle = math.Mod(angle, 360)
		ui.mu.Unlock()

		spinning := st.State == player.StatePlaying || math.Abs(vel) > 0.5
		changed := st != last
		if changed || spinning {
			last = st
			w.Invalidate()
		}
		// keep frames flowing while hover/press fades or the rail
		// indicator are settling (they advance in layout frames)
		if changed == false && ui.uiMicroAnimating() {
			w.Invalidate()
		}

		ui.syncSMTC(st)
	}
}

// engineStatus reads the engine status (zero status when no engine).
func (ui *appUI) engineStatus() player.Status {
	if ui.engine == nil {
		return player.Status{}
	}
	return ui.engine.Status()
}

// dispatchSMTCButton maps a flyout/media-key button onto the same control
// path as the keyboard shortcuts. Runs on the refresh loop goroutine; the
// toggles and queue ops involved are safe off the UI thread.
func (ui *appUI) dispatchSMTCButton(b smtc.Button) {
	switch b {
	case smtc.BtnPlay, smtc.BtnPause:
		ui.togglePlay()
	case smtc.BtnStop:
		ui.mu.Lock()
		ui.stoppedManually = true
		ui.mu.Unlock()
		if ui.engine != nil {
			ui.engine.Stop()
		}
	case smtc.BtnNext:
		ui.playNext()
	case smtc.BtnPrevious:
		ui.playPrev(ui.engineStatus())
	}
}

// syncSMTC mirrors the optimistic now-track, playback state and timeline
// (1 Hz) into the system media overlay. Runs on the refresh loop only.
func (ui *appUI) syncSMTC(st player.Status) {
	ui.mu.Lock()
	t := ui.nowTrack
	ui.mu.Unlock()

	if t.VideoID == "" {
		if ui.smtcTrack.VideoID != "" {
			ui.smtcTrack = ytm.Track{}
			smtc.SetStopped()
		}
		return
	}
	if t.VideoID != ui.smtcTrack.VideoID || t.Title != ui.smtcTrack.Title || t.Artist != ui.smtcTrack.Artist {
		ui.smtcTrack = t
		ui.smtcCover = "" // art may not be on disk yet; SetCover fills it
		smtc.SetTrack(t.Title, t.Artist, t.Album, "")
	}
	if p := ui.covers.Path(t.VideoID); p != "" && p != ui.smtcCover {
		ui.smtcCover = p
		smtc.SetCover(p)
	}
	if st.State != ui.smtcState {
		ui.smtcState = st.State
		switch st.State {
		case player.StatePlaying:
			smtc.SetPlayback(true)
		case player.StatePaused:
			smtc.SetPlayback(false)
		default:
			smtc.SetStopped()
		}
	}
	// timeline at most once a second, or instantly after a seek jump
	if st.Duration > 0 &&
		(time.Since(ui.smtcTlAt) >= time.Second || st.Position != ui.smtcTlPos) {
		ui.smtcTlAt = time.Now()
		ui.smtcTlPos = st.Position
		smtc.SetTimeline(st.Position, st.Duration)
	}
}

// drain picks up search results and player events without blocking the UI.
func (ui *appUI) drain() {
	select {
	case res := <-ui.ch:
		ui.busy = false
		if res.err != nil {
			ui.status = res.err.Error()
		} else {
			ui.results = res.tracks
			ui.status = fmt.Sprintf("%d results for %q", len(res.tracks), res.query)
		}
	default:
	}
	if ui.engine != nil {
		for {
			select {
			case ev := <-ui.engine.Events():
				switch ev.Kind {
				case player.EventStopped:
					ui.advance(ev.URL)
				default:
				}
			default:
				return
			}
		}
	}
}

// advance plays the next queue entry after the track whose URL is fromURL
// ended; falls back to the radio when autoplay is on and the queue ran
// dry. A manual stop passes "" handled via stoppedManually instead.
func (ui *appUI) advance(fromURL string) {
	ui.mu.Lock()
	if ui.loadingIdx >= 0 {
		ui.loadingIdx = -1
	}
	if ui.stoppedManually {
		ui.mu.Unlock()
		return
	}
	if fromURL != "" && fromURL == ui.lastEndedURL && time.Since(ui.lastEndedAt) < 5*time.Second {
		ui.mu.Unlock()
		return // same end reported twice (property + end-file)
	}
	seed := ui.nowTrack
	var next ytm.Track
	for len(ui.queue) > 0 {
		next = ui.queue[0]
		ui.queue = ui.queue[1:]
		if next.VideoID != "" {
			break
		}
		next = ytm.Track{}
	}
	radio := ui.autoplay && seed.VideoID != ""
	dry := next.VideoID == ""
	ui.lastEndedURL = fromURL
	ui.lastEndedAt = time.Now()
	ui.mu.Unlock()

	if dry && radio {
		go ui.radioAdvance(seed)
		return
	}
	if dry || ui.engine == nil || ui.extractor == nil {
		return
	}
	ctx := ui.newLoadCtx()
	ui.status = "Loading " + next.Title + "..."
	go ui.playTrack(ctx, next, -1)
}

// jumpQueue jumps playback to queue entry i, consuming entries 0..i
// (clicked card first). Called from the vinyl deck overlay.
func (ui *appUI) jumpQueue(i int) {
	if ui.engine == nil || ui.extractor == nil {
		return
	}
	ui.mu.Lock()
	if i < 0 || i >= len(ui.queue) || ui.queue[i].VideoID == "" {
		ui.mu.Unlock()
		return
	}
	next := ui.queue[i]
	ui.queue = append(ui.queue[:0], ui.queue[i+1:]...)
	ui.loadingIdx = -1
	ui.lastEndedURL = "" // the interrupted track's end event is ours: don't gate the jump
	ui.mu.Unlock()
	ctx := ui.newLoadCtx()
	ui.status = "Loading " + next.Title + "..."
	go ui.playTrack(ctx, next, -1)
}

// radioAdvance fetches the seed track's radio and immediately plays the
// first pick, queueing the rest behind it.
func (ui *appUI) radioAdvance(seed ytm.Track) {
	if ui.engine == nil || ui.extractor == nil {
		return
	}
	tracks, err := ytm.Autoplay(seed.VideoID)
	if err != nil || len(tracks) == 0 {
		ui.status = "Radio top-up failed: " + shortErr(err)
		return
	}
	ctx := ui.newLoadCtx()
	ui.status = "Radio: loading " + tracks[0].Title + "..."
	ui.mu.Lock()
	ui.queue = append(ui.queue, tracks[1:min(3, len(tracks))]...)
	ui.mu.Unlock()
	go ui.playTrack(ctx, tracks[0], -1)
}

// topUpQueue fetches radio tracks for seedID and appends up to need of
// them, so the queue never runs dry while autoplay is on.
func (ui *appUI) topUpQueue(seedID string, need int) {
	if need <= 0 {
		return
	}
	tracks, err := ytm.Autoplay(seedID)
	if err != nil {
		return
	}
	ui.mu.Lock()
	for _, t := range tracks {
		if len(ui.queue) >= need {
			break
		}
		if t.VideoID == "" {
			continue
		}
		ui.queue = append(ui.queue, t)
	}
	ui.mu.Unlock()
}

// refreshHome rebuilds the Home tab: the recently-listened tiles straight
// from the persistent history, and the "For you" picks from the
// recommendation engine (seeded radio with a 24h on-disk cache, ranked by
// cross-seed consensus). Safe to call concurrently; runs at most one
// refresh at a time.
func (ui *appUI) refreshHome() {
	ui.mu.Lock()
	if ui.homeBusy {
		ui.mu.Unlock()
		return
	}
	ui.homeBusy = true
	ui.mu.Unlock()
	defer func() {
		ui.mu.Lock()
		ui.homeBusy = false
		ui.mu.Unlock()
		if ui.window != nil {
			ui.window.Invalidate()
		}
	}()

	listens := ui.hstore.Recent(30)
	if len(listens) == 0 {
		ui.mu.Lock()
		ui.hits, ui.picks = nil, nil
		ui.mu.Unlock()
		return
	}

	// recently listened: distinct tracks, newest first (tiles)
	seen := map[string]bool{}
	var recent []ytm.Track
	for _, e := range listens {
		if seen[e.VideoID] {
			continue
		}
		seen[e.VideoID] = true
		recent = append(recent, e.Track)
	}
	if len(recent) > 10 {
		recent = recent[:10]
	}

	// ranked picks from the engine; the radio cache makes this free
	// unless seeds are stale or missing
	picks := ui.rstore.Refresh(listens, func(seedID string) ([]ytm.Track, error) {
		return ytm.Autoplay(seedID)
	}, time.Now())

	ui.mu.Lock()
	ui.hits, ui.picks = recent, picks
	ui.mu.Unlock()
}

// homeTileW is the cover-tile width in dp; the grid derives its column
// count from it.
const homeTileW = 150

// homeMarq holds the marquee scroll states of one tile's two labels.
type homeMarq struct {
	title, artist marquee
}

// homeTile renders one cover tile: square art with a hover-only + button
// (queue without interrupting), then single-line title and artist.
func (ui *appUI) homeTile(gtx layout.Context, t ytm.Track, i int) layout.Dimensions {
	now := gtx.Now
	if now.IsZero() {
		now = time.Now()
	}
	st := ui.interState(&ui.homeRows[i], now)
	hv := float32(st.hover.val(now))
	pw := float32(st.press.val(now))

	ui.mu.Lock()
	playing := ui.nowTrack.VideoID == t.VideoID && t.VideoID != ""
	ui.mu.Unlock()

	macro := op.Record(gtx.Ops)
	dims := ui.homeRows[i].Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.UniformInset(unit.Dp(8)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return layout.Stack{Alignment: layout.SE}.Layout(gtx,
						layout.Expanded(func(gtx layout.Context) layout.Dimensions {
							return ui.coverArea(gtx, t, homeTileW-16)
						}),
						layout.Stacked(func(gtx layout.Context) layout.Dimensions {
							if hv < 0.5 {
								return layout.Dimensions{}
							}
							return ui.iconBtn(gtx, &ui.homePlus[i], 28, iconPlus, false)
						}),
					)
				}),
				layout.Rigid(layout.Spacer{Height: unit.Dp(6)}.Layout),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					title := material.Body1(ui.th, t.Title)
					title.TextSize = unit.Sp(13)
					title.MaxLines = 1
					if playing {
						title.Color = colAmber
					}
					return ui.marqueeLabel(gtx, title, &ui.homeMarq[i].title, gtx.Dp(unit.Dp(homeTileW-16)), hv >= 0.5)
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					meta := material.Body2(ui.th, t.Artist)
					meta.TextSize = unit.Sp(12)
					meta.MaxLines = 1
					meta.Color = colSub
					return ui.marqueeLabel(gtx, meta, &ui.homeMarq[i].artist, gtx.Dp(unit.Dp(homeTileW-16)), hv >= 0.5)
				}),
			)
		})
	})
	call := macro.Stop()
	r := image.Rectangle{Max: dims.Size}
	if playing {
		fillRect(gtx, r, 10, colPanel)
		fillRect(gtx, image.Rect(0, 2, 3, dims.Size.Y-2), 1, colAmber)
	}
	if a := hv*(1-pw) + pw*0.6; a > 0.01 {
		hbg := colHover
		if pw > 0.01 {
			hbg = colPress
		}
		hbg.A = uint8(float32(colHover.A) * a)
		fillRect(gtx, r, 10, hbg)
	}
	call.Add(gtx.Ops)
	return dims
}

// homeTileTrack returns the track shown by flat tile index i (hits first,
// then picks).
func (ui *appUI) homeTileTrack(i int) ytm.Track {
	ui.mu.Lock()
	defer ui.mu.Unlock()
	if i < len(ui.hits) {
		return ui.hits[i]
	}
	if i-len(ui.hits) < len(ui.picks) {
		return ui.picks[i-len(ui.hits)]
	}
	return ytm.Track{}
}

// handleHomeClicks wires the Home tiles: a tile click plays the track now
// (which naturally re-seeds the radio and the recommendations), the +
// overlay appends to the queue without interrupting playback. The + is
// checked first so a click on it never also starts playback.
func (ui *appUI) handleHomeClicks(gtx layout.Context) {
	ui.mu.Lock()
	nTiles := len(ui.hits) + len(ui.picks)
	ui.mu.Unlock()
	for i := 0; i < nTiles; i++ {
		if i >= len(ui.homeRows) {
			break
		}
		plus := i < len(ui.homePlus) && ui.homePlus[i].Clicked(gtx)
		play := !plus && ui.homeRows[i].Clicked(gtx)
		if !plus && !play {
			continue
		}
		t := ui.homeTileTrack(i)
		if t.VideoID == "" {
			continue
		}
		if plus {
			ui.mu.Lock()
			ui.queue = append(ui.queue, t)
			n := len(ui.queue)
			ui.mu.Unlock()
			ui.status = fmt.Sprintf("Queued %s (%d in queue)", t.Title, n)
			continue
		}
		if ui.engine == nil || ui.extractor == nil {
			ui.status = "playback unavailable: " + missingPiece()
			continue
		}
		ctx := ui.newLoadCtx()
		ui.status = "Loading " + t.Title + "..."
		go ui.playTrack(ctx, t, -1)
	}
}

// homeView renders the Home tab as a tiled grid: "Jump back in" tiles from
// the listening history, then "For you" tiles from the recommendation
// engine. Tiles behave like search rows: click plays, + queues.
func (ui *appUI) homeView(gtx layout.Context) layout.Dimensions {
	ui.mu.Lock()
	nHits, nPicks := len(ui.hits), len(ui.picks)
	busy := ui.homeBusy
	ui.mu.Unlock()

	if nHits+nPicks == 0 {
		msg := "Nothing here yet. Play a few tracks — Home learns from what you listen to."
		if busy {
			msg = "Mixing your recommendations..."
		}
		tip := material.Body2(ui.th, msg)
		tip.Color = colHint
		return layout.Center.Layout(gtx, tip.Layout)
	}

	tiles := nHits + nPicks
	if len(ui.homeRows) != tiles {
		ui.homeRows = make([]widget.Clickable, tiles)
		ui.homePlus = make([]widget.Clickable, tiles)
		ui.homeMarq = make([]homeMarq, tiles)
	}
	// marquee activity is re-detected every frame; the refresh loop keeps
	// frames flowing while any tile scrolls
	ui.homeMarqActive.Store(false)

	// grid geometry: as many columns as fit the available width
	tilePx := gtx.Dp(unit.Dp(homeTileW))
	gapPx := gtx.Dp(unit.Dp(14))
	cols := 1
	if tilePx+gapPx > 0 {
		cols = (gtx.Constraints.Max.X + gapPx) / (tilePx + gapPx)
		if cols < 1 {
			cols = 1
		}
	}

	// flat row list: section headers interleaved with grid rows
	type gridRow struct {
		header string
		from   int // tile index range [from, to)
		to     int
	}
	var rows []gridRow
	if nHits > 0 {
		rows = append(rows, gridRow{header: "Jump back in"})
		for i := 0; i < nHits; i += cols {
			rows = append(rows, gridRow{from: i, to: min(i+cols, nHits)})
		}
	}
	if nPicks > 0 {
		rows = append(rows, gridRow{header: "For you"})
		for i := 0; i < nPicks; i += cols {
			rows = append(rows, gridRow{from: nHits + i, to: nHits + min(i+cols, nPicks)})
		}
	}

	return material.List(ui.th, &ui.hlist).Layout(gtx, len(rows), func(gtx layout.Context, i int) layout.Dimensions {
		row := rows[i]
		if row.header != "" {
			h := material.Body1(ui.th, row.header)
			h.Color = colSub
			h.TextSize = unit.Sp(14)
			return layout.Inset{Left: unit.Dp(12), Top: unit.Dp(14), Bottom: unit.Dp(6)}.Layout(gtx, h.Layout)
		}
		var children []layout.FlexChild
		for ti := row.from; ti < row.to; ti++ {
			t := ui.homeTileTrack(ti)
			if t.VideoID == "" {
				continue
			}
			idx := ti
			children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return ui.homeTile(gtx, t, idx)
			}))
			if ti < row.to-1 {
				children = append(children, layout.Rigid(layout.Spacer{Width: unit.Dp(14)}.Layout))
			}
		}
		return layout.Flex{Axis: layout.Horizontal}.Layout(gtx, children...)
	})
}
func (ui *appUI) newLoadCtx() context.Context {
	ui.mu.Lock()
	defer ui.mu.Unlock()
	if ui.loadCancel != nil {
		ui.loadCancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	ui.loadCancel = cancel
	return ctx
}

// queueSaver snapshots the playback queue every few seconds so it
// survives app crashes, forced restarts and hard power-offs.
func (ui *appUI) queueSaver() {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for range t.C {
		ui.mu.Lock()
		q := make([]ytm.Track, len(ui.queue))
		copy(q, ui.queue)
		ui.mu.Unlock()
		ui.prefs.SaveQueue(q)
	}
}

// plural returns "s" for counts != 1.
func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func shortErr(err error) string {
	if err == nil {
		return "unknown"
	}
	s := err.Error()
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 80 {
		s = s[:80] + "..."
	}
	return s
}

func (ui *appUI) startSearch(query string) {
	if ui.busy || query == "" {
		return
	}
	ui.busy = true
	ui.status = "Searching for " + query + "..."
	go func() {
		tracks, err := ytm.SearchYTMusic(query)
		ui.ch <- searchResult{query: query, tracks: tracks, err: err}
	}()
}

// handleRowClick resolves the clicked track and starts playback. Clicking
// the row that is currently loading cancels the load.
func (ui *appUI) handleRowClick(gtx layout.Context) {
	for i := range ui.rows {
		if !ui.rows[i].Clicked(gtx) || i >= len(ui.results) {
			continue
		}
		ui.mu.Lock()
		loading := ui.loadingIdx
		ui.mu.Unlock()
		if loading == i {
			if ui.loadCancel != nil {
				ui.loadCancel()
			}
			ui.mu.Lock()
			ui.loadingIdx = -1
			ui.status = "Load cancelled"
			ui.mu.Unlock()
			continue
		}
		if ui.loadCancel != nil {
			ui.loadCancel()
		}
		if ui.engine == nil || ui.extractor == nil {
			ui.status = "playback unavailable: " + missingPiece()
			continue
		}
		ctx := ui.newLoadCtx()
		t := ui.results[i]
		ui.mu.Lock()
		ui.loadingIdx = i
		ui.status = "Loading " + t.Title + "..."
		ui.mu.Unlock()
		go ui.playTrack(ctx, t, i)
	}
	// "+" appends the row's track to the queue without interrupting
	// playback.
	for i := range ui.rowsPlus {
		if !ui.rowsPlus[i].Clicked(gtx) || i >= len(ui.results) {
			continue
		}
		t := ui.results[i]
		ui.mu.Lock()
		ui.queue = append(ui.queue, t)
		n := len(ui.queue)
		ui.mu.Unlock()
		ui.status = fmt.Sprintf("Queued %s (%d in queue)", t.Title, n)
	}
}

// handleQueueClicks wires the queue view's rows: clicking an upcoming
// track plays it now; earlier queued items before it are dropped.
func (ui *appUI) handleQueueClicks(gtx layout.Context) {
	ui.mu.Lock()
	q := append([]ytm.Track(nil), ui.queue...)
	nq := len(ui.queue)
	ui.mu.Unlock()
	for i := range ui.qRows {
		if nq != len(q) || !ui.qRows[i].Clicked(gtx) || i >= len(q) {
			continue
		}
		t := q[i]
		ui.mu.Lock()
		if i+1 < len(ui.queue) {
			ui.queue = ui.queue[i+1:]
		} else {
			ui.queue = nil
		}
		ui.mu.Unlock()
		if ui.engine == nil || ui.extractor == nil {
			ui.status = "playback unavailable: " + missingPiece()
			continue
		}
		ctx := ui.newLoadCtx()
		ui.status = "Loading " + t.Title + "..."
		go ui.playTrack(ctx, t, -1)
	}
}

// playTrack resolves and starts a track. idx >= 0 points at the search
// row that is loading (shows its spinner); idx < 0 means the play came
// from the queue, the radio or auto-advance (nothing to spin).
func (ui *appUI) playTrack(ctx context.Context, t ytm.Track, idx int) {
	// optimistic UI: the player shows the clicked track immediately,
	// before the stream resolves — no dead 500ms where nothing moves
	ui.mu.Lock()
	ui.nowTrack = t
	ui.nowTitle = dispTitle(t)
	if idx == -1 {
		ui.stoppedManually = false
	}
	ui.mu.Unlock()

	// resilient load: extraction streams expire and YouTube throttles
	// sporadically — retry the whole resolve+start round with backoff
	// (fresh stream URL each attempt), bail out on context cancel
	var s *extract.Stream
	var lastErr error
	var lastErrFromResolve bool
	backoffs := []time.Duration{0, 900 * time.Millisecond, 2500 * time.Millisecond}
	for attempt := 0; attempt < len(backoffs); attempt++ {
		if backoffs[attempt] > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(backoffs[attempt]):
			}
		}
		stream, err := ui.extractor.Resolve(ctx, t.VideoID)
		if err != nil {
			lastErr = err
			lastErrFromResolve = true
			if ctx.Err() != nil {
				break
			}
			continue
		}
		if err := ui.engine.Play(stream.URL); err != nil {
			lastErr = err
			lastErrFromResolve = false
			if ctx.Err() != nil {
				break
			}
			continue
		}
		s, lastErr = stream, nil
		break
	}
	if lastErr != nil {
		ui.finishLoad(lastErr, idx, lastErrFromResolve)
		return
	}
	title := dispTitle(t)
	ui.status = "Playing: " + title

	ui.mu.Lock()
	if ui.loadingIdx == idx {
		ui.loadingIdx = -1
	}
	// the track actually playing until now is a finished listen: move
	// it to history and record it in the persistent store (unless data
	// collection is paused in the settings)
	listened := false
	if ui.playingTrack.VideoID != "" && ui.playingTrack.VideoID != t.VideoID {
		ui.history = append(ui.history, ui.playingTrack)
		if len(ui.history) > 50 {
			ui.history = ui.history[len(ui.history)-50:]
		}
		if ui.recData {
			ui.hstore.Add(ui.playingTrack, time.Now())
		}
		listened = true
	}
	ui.playingTrack = t
	ui.stoppedManually = false
	topUp := 0
	if ui.autoplay && len(ui.queue) < 2 {
		topUp = 3 - len(ui.queue)
	}
	ui.mu.Unlock()

	go ui.fetchLyrics(t, s.Duration)
	if topUp > 0 {
		go ui.topUpQueue(t.VideoID, topUp)
	}
	if listened {
		go ui.refreshHome() // rebuild the Home mixes from the new listen
	}
}

func (ui *appUI) finishLoad(err error, idx int, fromResolve bool) {
	ui.mu.Lock()
	ui.status = "Load failed: " + err.Error()
	if fromResolve {
		// extraction failures are almost always a stale yt-dlp; the
		// bundled binary in bin/ just needs replacing
		ui.status += " (stream extraction failed — yt-dlp may need an update)"
	}
	if ui.loadingIdx == idx {
		ui.loadingIdx = -1
	}
	// roll back the optimistic player update: snap back to whatever is
	// actually playing so the bar never lies about the current track
	if ui.playingTrack.VideoID != "" && ui.nowTrack.VideoID != ui.playingTrack.VideoID {
		ui.nowTrack = ui.playingTrack
		ui.nowTitle = dispTitle(ui.playingTrack)
	}
	ui.mu.Unlock()
}

// dispTitle builds "Title - Artist" (or Album) display text.
func dispTitle(t ytm.Track) string {
	title := t.Title
	if t.Artist != "" {
		title += " - " + t.Artist
	} else if t.Album != "" {
		title += " - " + t.Album
	}
	return title
}

func (ui *appUI) fetchLyrics(t ytm.Track, dur time.Duration) {
	if t.Title == "" {
		return
	}
	ui.mu.Lock()
	ui.lyrBusy = true
	ui.lyrErr = nil
	ui.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	lyr, err := ui.lyrClient.Get(ctx, t.Artist, cleanTitle(t.Title), dur)

	ui.mu.Lock()
	ui.lyrBusy = false
	if err != nil {
		ui.lyrErr = err
		ui.curLyrics = nil
		ui.lyrEnds = nil
	} else {
		ui.lyrErr = nil
		ui.curLyrics = lyr
		// end time of each synced line = next line's start (interlude
		// ends at track end fallback)
		ends := make([]float64, len(lyr.Synced))
		for i, ln := range lyr.Synced {
			if i+1 < len(lyr.Synced) {
				ends[i] = lyr.Synced[i+1].Start.Seconds()
			} else {
				d := dur.Seconds()
				if d < 10 {
					d = 10
				}
				ends[i] = math.Min(d, ln.Start.Seconds()+8)
			}
		}
		ui.lyrEnds = ends
	}
	ui.mu.Unlock()
	if ui.window != nil {
		ui.window.Invalidate()
	}
}

// cleanTitle strips noise from search-result titles that hurts lyric
// lookups: "(feat. X)", "(Official Video)", "[HD]", trailing "- remaster".
func cleanTitle(t string) string {
	s := t
	if i := strings.IndexByte(s, '('); i > 0 {
		s = s[:i]
	}
	if i := strings.IndexByte(s, '['); i > 0 {
		s = s[:i]
	}
	return strings.TrimRight(strings.TrimSpace(s), "- ")
}

func missingPiece() string {
	x, xerr := extract.NewYtDLP("")
	if xerr != nil {
		return xerr.Error()
	}
	_ = x
	e, err := player.NewMpv()
	if err != nil {
		return err.Error()
	}
	_ = e
	return "unknown"
}

// nTabs is the number of navigation tabs (Home, Search, Queue, Lyrics,
// Settings); used by the keyboard shortcuts.
const nTabs = 5

// handleShortcuts implements the global keyboard map:
//
//	Space            play / pause
//	Left / Right     seek -10s / +10s
//	Up / Down        volume +/- 5
//	Ctrl+M           mute / unmute
//	Ctrl+Left        restart track (or the previous one if just started)
//	Ctrl+Right       next track in queue
//	Ctrl+F           focus the search field
//	Ctrl+1..5        jump to Home / Search / Queue / Lyrics / Settings
//	Ctrl+Tab         next tab
//	Shift+Tab        previous tab
//	Escape           leave the search field
//
// Filters are registered with a nil Focus tag, so they match wherever
// focus sits; the text-entry keys (Space, arrows) are suppressed while
// the search editor owns the keyboard.
func (ui *appUI) handleShortcuts(gtx layout.Context) {
	var st player.Status
	if ui.engine != nil {
		st = ui.engine.Status()
	}
	for {
		ev, ok := gtx.Event(
			key.Filter{Name: key.NameSpace},
			key.Filter{Name: key.NameLeftArrow},
			key.Filter{Name: key.NameRightArrow},
			key.Filter{Name: key.NameUpArrow},
			key.Filter{Name: key.NameDownArrow},
			key.Filter{Name: key.NameEscape},
			key.Filter{Name: key.NameTab, Required: key.ModShift},
			key.Filter{Name: key.NameTab, Required: key.ModShortcut},
			key.Filter{Name: "F", Required: key.ModShortcut},
			key.Filter{Name: "M", Required: key.ModShortcut},
			key.Filter{Name: "1", Required: key.ModShortcut},
			key.Filter{Name: "2", Required: key.ModShortcut},
			key.Filter{Name: "3", Required: key.ModShortcut},
			key.Filter{Name: "4", Required: key.ModShortcut},
			key.Filter{Name: "5", Required: key.ModShortcut},
		)
		if !ok {
			break
		}
		ke, is := ev.(key.Event)
		if !is || ke.State != key.Press {
			continue
		}
		typing := gtx.Focused(&ui.editor)

		if ke.Name == key.NameTab {
			d := 1
			if ke.Modifiers.Contain(key.ModShift) {
				d = -1
			}
			ui.jumpTab(d)
			continue
		}
		if ke.Modifiers.Contain(key.ModShortcut) {
			switch ke.Name {
			case "F":
				ui.tab = tabSearch
				gtx.Execute(key.FocusCmd{Tag: &ui.editor})
			case "M":
				ui.toggleMute(st.Volume)
			case "1", "2", "3", "4", "5":
				ui.jumpTab(int(ke.Name[0] - '1'))
			case key.NameLeftArrow:
				ui.playPrev(st)
			case key.NameRightArrow:
				ui.playNext()
			}
			continue
		}
		if typing {
			if ke.Name == key.NameEscape {
				gtx.Execute(key.FocusCmd{Tag: nil})
			}
			continue // let the editor own Space and the arrows
		}
		switch ke.Name {
		case key.NameSpace:
			ui.togglePlay()
		case key.NameLeftArrow:
			if st.Duration > 0 {
				ui.engine.Seek(maxDuration(st.Position-10*time.Second, 0))
			}
		case key.NameRightArrow:
			if st.Duration > 0 {
				ui.engine.Seek(minDuration(st.Position+10*time.Second, st.Duration))
			}
		case key.NameUpArrow:
			ui.setVolume(st.Volume + 5)
		case key.NameDownArrow:
			ui.setVolume(st.Volume - 5)
		}
	}
}

// jumpTab switches to tab offset by delta (wrapping around).
func (ui *appUI) jumpTab(delta int) {
	ui.tab = (ui.tab + delta%nTabs + nTabs) % nTabs
}

// setVolume sets and pushes the volume (0..100), syncing the slider.
func (ui *appUI) setVolume(pct int) {
	if pct < 0 {
		pct = 0
	} else if pct > 100 {
		pct = 100
	}
	ui.mu.Lock()
	ui.volume.Value = float32(pct) / 100
	ui.volumeSent = pct
	ui.mu.Unlock()
	if ui.engine != nil {
		ui.engine.SetVolume(pct)
	}
}

// toggleMute mutes (volume -> 0) or restores the previous volume.
func (ui *appUI) toggleMute(curVol int) {
	if ui.muted && ui.savedVol > 0 {
		ui.muted = false
		ui.setVolume(ui.savedVol)
		return
	}
	if curVol > 0 {
		ui.mu.Lock()
		ui.savedVol = curVol
		ui.muted = true
		ui.mu.Unlock()
		ui.setVolume(0)
	}
}

// togglePlay flips engine pause; mirrors the play/pause button.
func (ui *appUI) togglePlay() {
	if ui.engine == nil {
		return
	}
	switch ui.engine.Status().State {
	case player.StatePlaying:
		ui.engine.SetPaused(true)
	case player.StatePaused:
		ui.engine.SetPaused(false)
	}
}

// playNext jumps to the first queued track (consumes it), if any.
func (ui *appUI) playNext() {
	if ui.engine == nil || ui.extractor == nil {
		return
	}
	ui.mu.Lock()
	if len(ui.queue) == 0 {
		ui.mu.Unlock()
		return
	}
	ui.mu.Unlock()
	ui.jumpQueue(0)
}

// playPrev restarts the current track, or replays the previous one from
// history when the track just started; mirrors the back button.
func (ui *appUI) playPrev(st player.Status) {
	if ui.engine == nil || ui.extractor == nil {
		return
	}
	if st.Position > 3*time.Second {
		ui.engine.Seek(0)
		return
	}
	ui.mu.Lock()
	var prev ytm.Track
	if n := len(ui.history); n > 0 {
		prev = ui.history[n-1]
		ui.history = ui.history[:n-1]
	}
	ui.mu.Unlock()
	if prev.VideoID != "" {
		ctx := ui.newLoadCtx()
		ui.status = "Playing: " + dispTitle(prev)
		go ui.playTrack(ctx, prev, -1)
	}
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func (ui *appUI) layoutRoot(gtx layout.Context) layout.Dimensions {
	for {
		ev, ok := ui.editor.Update(gtx)
		if !ok {
			break
		}
		switch ev.(type) {
		case widget.SubmitEvent:
			ui.startSearch(ui.editor.Text())
		}
		// focus ring: the editor's tag is its own pointer, so we can
		// listen for its focus events alongside the editor itself.
		for {
			ke, ok := gtx.Event(key.FocusFilter{Target: &ui.editor})
			if !ok {
				break
			}
			if fe, is := ke.(key.FocusEvent); is {
				ui.editFocus = fe.Focus
			}
		}
	}
	if !ui.busy && ui.searchBtn.Clicked(gtx) {
		ui.startSearch(ui.editor.Text())
	}
	ui.handleShortcuts(gtx)

	if ui.engine != nil {
		ui.handleTransport(gtx)
	}
	ui.handleRowClick(gtx)
	ui.handleQueueClicks(gtx)
	ui.handleHomeClicks(gtx)
	if ui.homeTabBtn.Clicked(gtx) {
		ui.tab = tabHome
		ui.mu.Lock()
		empty := len(ui.hits)+len(ui.picks) == 0 && !ui.homeBusy
		ui.mu.Unlock()
		if empty {
			go ui.refreshHome()
		}
	}
	if ui.searchTabBtn.Clicked(gtx) {
		ui.tab = tabSearch
	}
	if ui.queueTabBtn.Clicked(gtx) {
		ui.tab = tabQueue
	}
	if ui.lyricsTabBtn.Clicked(gtx) {
		ui.tab = tabLyrics
	}
	if ui.radioBtn.Clicked(gtx) {
		ui.autoplay = !ui.autoplay
	}
	if ui.gearBtn.Clicked(gtx) {
		if ui.tab == tabSettings {
			ui.tab = tabHome
		} else {
			ui.tab = tabSettings
		}
	}

	// settings switches: mirror into guarded state and persist
	if ui.recOn.Update(gtx) {
		ui.mu.Lock()
		ui.recData = ui.recOn.Value
		ui.mu.Unlock()
		ui.savePrefs()
		if ui.recOn.Value {
			go ui.refreshHome()
		}
	}
	if ui.bubOn.Update(gtx) {
		ui.mu.Lock()
		ui.bubShow = ui.bubOn.Value
		ui.mu.Unlock()
		ui.savePrefs()
	}
	if ui.hideBtn.Clicked(gtx) {
		// cycle: off -> 1 -> 3 -> 5 -> 10 -> off
		m := int(ui.vinylHideMin.Load())
		switch m {
		case -1:
			m = 1
		case 1:
			m = 3
		case 3:
			m = 5
		case 5:
			m = 10
		default:
			m = -1
		}
		ui.vinylHideMin.Store(int32(m))
		ui.savePrefs()
	}
	if ui.clearBtn.Clicked(gtx) {
		ui.hstore.Clear()
		ui.mu.Lock()
		ui.history = nil
		ui.mu.Unlock()
		ui.status = "Listening history cleared."
		go ui.refreshHome()
	}

	// opaque window background
	paint.FillShape(gtx.Ops, colBG, clip.Rect{Max: gtx.Constraints.Max}.Op())

	return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
		layout.Rigid(ui.navRail),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
				layout.Rigid(ui.topBar),
				layout.Flexed(1, ui.mainPane),
				layout.Rigid(ui.controlBar),
			)
		}),
	)
}

// topBar hosts the search field (Search tab) or the pane heading.
func (ui *appUI) topBar(gtx layout.Context) layout.Dimensions {
	if ui.tab != tabSearch {
		var head string
		switch ui.tab {
		case tabHome:
			head = "Home"
		case tabQueue:
			head = "Queue"
		case tabLyrics:
			head = "Lyrics"
		case tabSettings:
			head = "Settings"
		}
		hdr := material.Body1(ui.th, head)
		hdr.TextSize = unit.Sp(18)
		return layout.Inset{Top: unit.Dp(16), Left: unit.Dp(16), Right: unit.Dp(16), Bottom: unit.Dp(4)}.Layout(gtx, hdr.Layout)
	}
	return layout.Inset{Top: unit.Dp(12), Left: unit.Dp(12), Right: unit.Dp(12), Bottom: unit.Dp(0)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return ui.searchField(gtx)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				lab := material.Body2(ui.th, ui.status)
				lab.Color = colHint
				lab.TextSize = unit.Sp(12)
				return layout.Inset{Top: unit.Dp(6), Left: unit.Dp(4)}.Layout(gtx, lab.Layout)
			}),
		)
	})
}

// mainPane switches the content area by tab.
func (ui *appUI) mainPane(gtx layout.Context) layout.Dimensions {
	switch ui.tab {
	case tabHome:
		return ui.homeView(gtx)
	case tabQueue:
		return ui.queueView(gtx)
	case tabLyrics:
		return ui.lyricsPane(gtx)
	case tabSettings:
		return ui.settingsView(gtx)
	}
	return ui.resultsList(gtx)
}

// settingsView renders the settings page: preference groups as cards.
func (ui *appUI) savePrefs() {
	ui.prefs.Set(prefs.Prefs{
		Recording:    ui.recOn.Value,
		LyricsBubble: ui.bubOn.Value,
		VinylHideMin: int(ui.vinylHideMin.Load()),
	})
}

// hideLabel renders the auto-hide button text.
func (ui *appUI) hideLabel() string {
	m := int(ui.vinylHideMin.Load())
	if m < 0 {
		return "Off"
	}
	return fmt.Sprintf("%d min", m)
}

func (ui *appUI) settingsView(gtx layout.Context) layout.Dimensions {
	return layout.UniformInset(unit.Dp(16)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return ui.settingsCard(gtx, "Data & privacy", func(gtx layout.Context) layout.Dimensions {
					return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							return ui.settingSwitch(gtx,
								"Record listening history",
								"Saves what you play (on this machine only) to power the Home mixes.",
								&ui.recOn)
						}),
						layout.Rigid(layout.Spacer{Height: unit.Dp(10)}.Layout),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							return ui.settingRow(gtx, "Clear listening history",
								"Deletes the saved history and empties the Home mixes.",
								func(gtx layout.Context) layout.Dimensions {
									btn := material.Button(ui.th, &ui.clearBtn, "Clear")
									btn.Background = color.NRGBA{}
									btn.Color = colText
									btn.TextSize = unit.Sp(13)
									btn.Inset = layout.Inset{Top: unit.Dp(7), Bottom: unit.Dp(7), Left: unit.Dp(14), Right: unit.Dp(14)}
									return panelRing(gtx, 8, colField, colStroke, btn.Layout)
								})
						}),
					)
				})
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(12)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return ui.settingsCard(gtx, "Overlays", func(gtx layout.Context) layout.Dimensions {
					return ui.settingSwitch(gtx,
						"Lyrics bubble",
						"Floating glass caption just above the vinyl record showing the current lyric line.",
						&ui.bubOn)
				})
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(12)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return ui.settingsCard(gtx, "Vinyl record", func(gtx layout.Context) layout.Dimensions {
					return ui.settingRow(gtx, "Auto-hide when idle",
						"Hides the record after a while without playback and shows it again as soon as something plays.",
						func(gtx layout.Context) layout.Dimensions {
							btn := material.Button(ui.th, &ui.hideBtn, ui.hideLabel())
							btn.Background = color.NRGBA{}
							btn.Color = colText
							btn.TextSize = unit.Sp(13)
							btn.Inset = layout.Inset{Top: unit.Dp(7), Bottom: unit.Dp(7), Left: unit.Dp(14), Right: unit.Dp(14)}
							return panelRing(gtx, 8, colField, colStroke, btn.Layout)
						})
				})
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(12)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return ui.settingsCard(gtx, "Keyboard", func(gtx layout.Context) layout.Dimensions {
					rows := [][2]string{
						{"Space", "play / pause"},
						{"← / →", "seek 10 seconds back / forward"},
						{"↑ / ↓", "volume up / down"},
						{"Ctrl+M", "mute / unmute"},
						{"Ctrl+← / Ctrl+→", "previous / next track"},
						{"Ctrl+F", "focus the search field"},
						{"Enter", "run the search"},
						{"Ctrl+Tab / Shift+Tab", "next / previous tab"},
						{"Ctrl+1 … Ctrl+5", "jump to Home, Search, Queue, Lyrics, Settings"},
						{"Esc", "leave the search field"},
					}
					parts := make([]layout.FlexChild, 0, len(rows))
					for _, kv := range rows {
						kv := kv
						parts = append(parts, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
								layout.Rigid(func(gtx layout.Context) layout.Dimensions {
									k := material.Body2(ui.th, kv[0])
									k.Color = colText
									k.TextSize = unit.Sp(12)
									return layout.Inset{Right: unit.Dp(18)}.Layout(gtx, k.Layout)
								}),
								layout.Rigid(func(gtx layout.Context) layout.Dimensions {
									v := material.Body2(ui.th, kv[1])
									v.Color = colSub
									v.TextSize = unit.Sp(12)
									return v.Layout(gtx)
								}),
							)
						}))
					}
					return layout.Flex{Axis: layout.Vertical}.Layout(gtx, parts...)
				})
			}),
		)
	})
}

// settingsCard draws one preference group: title + rows inside a panel.
func (ui *appUI) settingsCard(gtx layout.Context, title string, rows func(layout.Context) layout.Dimensions) layout.Dimensions {
	return panelRing(gtx, 14, colPanel, colStroke, func(gtx layout.Context) layout.Dimensions {
		return layout.Inset{Top: unit.Dp(14), Bottom: unit.Dp(14), Left: unit.Dp(16), Right: unit.Dp(16)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					h := material.Body2(ui.th, title)
					h.Color = colSub
					h.TextSize = unit.Sp(13)
					return layout.Inset{Bottom: unit.Dp(8)}.Layout(gtx, h.Layout)
				}),
				layout.Rigid(rows),
			)
		})
	})
}

// settingSwitch lays out a labeled toggle with a caption line.
func (ui *appUI) settingSwitch(gtx layout.Context, label, caption string, sw *widget.Bool) layout.Dimensions {
	gtx.Constraints.Min.X = gtx.Constraints.Max.X
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
				layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					l := material.Body1(ui.th, label)
					l.TextSize = unit.Sp(15)
					return l.Layout(gtx)
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					st := material.Switch(ui.th, sw, "")
					st.Color.Enabled = colAmber
					return st.Layout(gtx)
				}),
			)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			c := material.Body2(ui.th, caption)
			c.Color = colHint
			c.TextSize = unit.Sp(12)
			return layout.Inset{Top: unit.Dp(2), Right: unit.Dp(50)}.Layout(gtx, c.Layout)
		}),
	)
}

// settingRow lays out a label + caption with a custom control on the right.
func (ui *appUI) settingRow(gtx layout.Context, label, caption string, control func(layout.Context) layout.Dimensions) layout.Dimensions {
	gtx.Constraints.Min.X = gtx.Constraints.Max.X
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
				layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					l := material.Body1(ui.th, label)
					l.TextSize = unit.Sp(15)
					return l.Layout(gtx)
				}),
				layout.Rigid(control),
			)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			c := material.Body2(ui.th, caption)
			c.Color = colHint
			c.TextSize = unit.Sp(12)
			return layout.Inset{Top: unit.Dp(2), Right: unit.Dp(110)}.Layout(gtx, c.Layout)
		}),
	)
}

// vinyl geometry: the overlay disc's center sits exactly on the
// bottom-right corner of the screen's work area, so only its upper-left
// quarter is visible — the disc peeks out from the screen corner itself.
const vinylDp = 220 // dp diameter

// startVinylOverlay launches the transparent screen-corner vinyl once the
// window DPI is known. The overlay runs its own OS thread + message loop
// (internal/vinylwin) and talks back through the callbacks below.
func (ui *appUI) startVinylOverlay(gtx layout.Context) {
	// vinyl lifecycle: hidden until something plays; auto-hides after
	// the configured idle time (settings), reappears on playback
	go func() {
		tick := time.NewTicker(2 * time.Second)
		defer tick.Stop()
		shown := false
		var active time.Time
		for range tick.C {
			if ui.engine == nil {
				continue
			}
			if ui.engine.Status().State == player.StatePlaying {
				active = time.Now()
				if !shown {
					shown = true
					vinylwin.SetVisible(true)
				}
				continue
			}
			// not playing: hide once idle past the timeout, but only
			// if we had actually played (initial state stays hidden)
			if !shown || active.IsZero() {
				continue
			}
			min := int(ui.vinylHideMin.Load())
			if min >= 0 && time.Since(active) > time.Duration(min)*time.Minute {
				shown = false
				vinylwin.SetVisible(false)
			}
		}
	}()
	radius := int(float32(vinylDp) / 2 * gtx.Metric.PxPerDp)
	go vinylwin.Run(vinylwin.Options{
		RadiusPx:        radius,
		OffsetFrac:      0.35, // peek more than a quarter-disc out of the corner
		MainWindowTitle: "Wax",
		Tooltip:         "Wax — YouTube Music",
		OnQuit: func() {
			if ui.engine != nil {
				ui.engine.Close()
			}
			os.Exit(0)
		},
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
		OnLean: func(px float64) {
			lyricswin.NudgeBottom(int(px + 0.5))
		},
		Track: func() (string, string) {
			ui.mu.Lock()
			defer ui.mu.Unlock()
			return ui.nowTrack.Title, ui.nowTrack.Artist
		},
		Queue: func() []vinylwin.QItem {
			ui.mu.Lock()
			defer ui.mu.Unlock()
			n := len(ui.queue)
			if n > 5 {
				n = 5
			}
			items := make([]vinylwin.QItem, n)
			for i := 0; i < n; i++ {
				items[i] = vinylwin.QItem{Title: ui.queue[i].Title, Artist: ui.queue[i].Artist}
				// fifth slot is the "+N more" tail whenever the queue
				// runs beyond the four clickable cards; shown dim,
				// not clickable — jumping consumes it up front
				items[i].Tail = i == 4
			}
			return items
		},
		Jump: func(i int) {
			ui.jumpQueue(i)
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

	// glass lyrics caption just above the vinyl overlay; it tracks the
	// disc's hover lean so the two ride the same spring
	offPx := int(float32(radius) * 0.35)
	go lyricswin.Run(lyricswin.Options{
		Width:     540,
		Height:    84,
		FontSize:  22,
		FromRight: 76,
		// base: ~26px above the resting record edge; NudgeBottom adds
		// the live lean (full lean ≈ 0.85*radius keeps it clear of the
		// 1.10x-grown, fully leaned disc)
		FromBottom: offPx + radius + 26,
		Show: func() bool {
			ui.mu.Lock()
			defer ui.mu.Unlock()
			return ui.bubShow
		},
		GetWords: func() ([]lyricswin.Word, float64) {
			ui.mu.Lock()
			lyr := ui.curLyrics
			ends := ui.lyrEnds
			ui.mu.Unlock()
			if lyr == nil || len(lyr.Synced) == 0 || len(ends) != len(lyr.Synced) {
				return nil, 0
			}
			pos := 0.0
			if ui.engine != nil {
				pos = ui.engine.Status().Position.Seconds()
			}
			// current line = last line started at or before pos
			idx := -1
			for i, ln := range lyr.Synced {
				if ln.Start.Seconds() <= pos+0.02 {
					idx = i
				} else {
					break
				}
			}
			if idx < 0 {
				return nil, pos
			}
			ls := lyr.Synced[idx].Start.Seconds()
			le := ends[idx]
			if le <= ls {
				le = ls + 1
			}
			ws := strings.Fields(lyr.Synced[idx].Text)
			if len(ws) == 0 {
				return nil, pos
			}
			out := make([]lyricswin.Word, len(ws))
			for i, s := range ws {
				f0 := float64(i) / float64(len(ws))
				f1 := float64(i+1) / float64(len(ws))
				out[i] = lyricswin.Word{
					Text:  s,
					Start: ls + (le-ls)*f0,
					End:   ls + (le-ls)*f1,
				}
			}
			return out, pos
		},
	})
}

func (ui *appUI) resultsList(gtx layout.Context) layout.Dimensions {
	if len(ui.results) == 0 {
		return layout.Dimensions{Size: gtx.Constraints.Min}
	}
	if len(ui.rows) != len(ui.results) {
		rows := make([]widget.Clickable, len(ui.results))
		ui.rows = rows
		ui.rowsPlus = make([]widget.Clickable, len(ui.results))
	}
	return material.List(ui.th, &ui.list).Layout(gtx, len(ui.results), func(gtx layout.Context, i int) layout.Dimensions {
		ui.mu.Lock()
		loading := i == ui.loadingIdx
		ui.mu.Unlock()
		return layout.Inset{Right: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
				layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					return ui.trackRow(gtx, ui.results[i], &ui.rows[i], loading)
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return ui.iconBtn(gtx, &ui.rowsPlus[i], 30, iconPlus, false)
				}),
				layout.Rigid(layout.Spacer{Width: unit.Dp(2)}.Layout),
			)
		})
	})
}

// queueView shows the upcoming tracks. Clicking one plays it now.
func (ui *appUI) queueView(gtx layout.Context) layout.Dimensions {
	return layout.UniformInset(unit.Dp(4)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				ui.mu.Lock()
				n, auto := len(ui.queue), ui.autoplay
				ui.mu.Unlock()
				head := fmt.Sprintf("Up next — %d in queue", n)
				if auto {
					head += "  (radio keeps the queue topped up)"
				}
				h := material.Body2(ui.th, head)
				h.Color = colSub
				return layout.Inset{Left: unit.Dp(8)}.Layout(gtx, h.Layout)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(4)}.Layout),
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				ui.mu.Lock()
				tracks := append([]ytm.Track(nil), ui.queue...)
				nq := len(ui.queue)
				ui.mu.Unlock()
				if nq != len(tracks) {
					return layout.Dimensions{Size: gtx.Constraints.Min}
				}
				if len(tracks) == 0 {
					tip := material.Body2(ui.th, "Queue is empty. Add tracks with the + button in the search results; with radio on, the next pick loads automatically.")
					tip.Color = colHint
					return layout.Center.Layout(gtx, tip.Layout)
				}
				if len(ui.qRows) != len(tracks) {
					ui.qRows = make([]widget.Clickable, len(tracks))
				}
				return material.List(ui.th, &ui.qlist).Layout(gtx, len(tracks), func(gtx layout.Context, i int) layout.Dimensions {
					return ui.trackRow(gtx, tracks[i], &ui.qRows[i], false)
				})
			}),
		)
	})
}

func (ui *appUI) lyricsPane(gtx layout.Context) layout.Dimensions {
	ui.mu.Lock()
	lyr := ui.curLyrics
	busy := ui.lyrBusy
	lerr := ui.lyrErr
	ui.mu.Unlock()

	return layout.UniformInset(unit.Dp(8)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		var head string
		switch {
		case busy:
			head = "Fetching lyrics..."
		case lyr == nil && lerr != nil:
			head = "No lyrics: " + lerr.Error()
		case lyr == nil:
			head = "Play a song to fetch lyrics"
		default:
			head = lyr.Track
			if lyr.Artist != "" {
				head += " - " + lyr.Artist
			}
			if lyr.Synced == nil {
				head += "  (no synced version)"
			}
		}
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				h := material.Body1(ui.th, head)
				h.Color = colSub
				h.TextSize = unit.Sp(13)
				return h.Layout(gtx)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(6)}.Layout),
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				if lyr == nil {
					return layout.Dimensions{Size: gtx.Constraints.Min}
				}
				if lyr.Synced == nil {
					// plain lyrics, one block per \n
					return material.Body1(ui.th, lyr.Plain).Layout(gtx)
				}
				pos := time.Duration(0)
				if ui.engine != nil {
					pos = ui.engine.Status().Position
				}
				cur := currentLine(lyr.Synced, pos)
				// keep the current line roughly centered
				if cur >= 0 {
					want := cur - 3
					if want >= 0 {
						ui.lyrList.Position.First = want
					} else {
						ui.lyrList.Position.First = 0
					}
				}
				return material.List(ui.th, &ui.lyrList).Layout(gtx, len(lyr.Synced), func(gtx layout.Context, i int) layout.Dimensions {
					line := lyr.Synced[i]
					lab := material.Body1(ui.th, line.Text)
					if i == cur {
						lab.Color = colAmber
						lab.TextSize = unit.Sp(16)
					} else {
						lab.Color = colSub
					}
					return layout.UniformInset(unit.Dp(2)).Layout(gtx, lab.Layout)
				})
			}),
		)
	})
}

// currentLine returns the index of the line active at pos, or the upcoming
// line before playback reaches the first stamp, or -1.
func currentLine(lines []lyrics.Line, pos time.Duration) int {
	cur := -1
	for i, ln := range lines {
		if ln.Start <= pos {
			cur = i
		} else {
			break
		}
	}
	if cur < 0 && len(lines) > 0 && pos < lines[0].Start {
		return -1
	}
	return cur
}

// handleTransport wires the play/pause/stop buttons, the volume slider and
// the seek bar to the engine, and keeps the seek bar in sync with playback.
func (ui *appUI) handleTransport(gtx layout.Context) {
	st := ui.engine.Status()

	if ui.playToggle.Clicked(gtx) {
		if st.State == player.StatePlaying {
			ui.engine.SetPaused(true)
		} else if st.State == player.StatePaused {
			ui.engine.SetPaused(false)
		}
	}
	if ui.stopBtn.Clicked(gtx) {
		ui.mu.Lock()
		ui.stoppedManually = true
		ui.mu.Unlock()
		ui.engine.Stop()
	}

	if ui.prevBtn.Clicked(gtx) {
		if st.Position > 3*time.Second {
			ui.engine.Seek(0)
		} else {
			ui.mu.Lock()
			var prev ytm.Track
			if n := len(ui.history); n > 0 {
				prev = ui.history[n-1]
				ui.history = ui.history[:n-1]
			}
			ui.mu.Unlock()
			if prev.VideoID != "" && ui.extractor != nil {
				ctx := ui.newLoadCtx()
				ui.status = "Loading " + prev.Title + "..."
				go ui.playTrack(ctx, prev, -1)
			} else {
				ui.engine.Seek(0)
			}
		}
	}
	if ui.nextBtn.Clicked(gtx) {
		ui.advance("")
	}

	// Volume slider -> engine (only when the integer value changes).
	if ui.volume.Update(gtx) {
		this := int(ui.volume.Value*100 + 0.5)
		if this != ui.volumeSent {
			ui.volumeSent = this
			ui.engine.SetVolume(this)
		}
	} else if ui.volumeSent != st.Volume {
		// engine-side volume change (e.g. at Start) -> slider
		ui.volumeSent = st.Volume
		ui.volume.Value = float32(st.Volume) / 100
	}

	// Seek bar: while dragging, let the user move it; on release, seek.
	dragging := ui.seek.Dragging()
	if ui.seek.Update(gtx) && ui.prevDragging && !dragging && st.Duration > 0 {
		ui.engine.Seek(time.Duration(ui.seek.Value * float32(st.Duration)))
	}
	ui.prevDragging = dragging
	if !dragging && st.Duration > 0 && st.State == player.StatePlaying {
		ui.seek.Value = float32(st.Position) / float32(st.Duration)
	}
}

// controlBar: the now-playing strip. Hairline top border, track title +
// amber seek line, icon transport, radio toggle and volume.
func (ui *appUI) controlBar(gtx layout.Context) layout.Dimensions {
	now := gtx.Now
	if now.IsZero() {
		now = time.Now()
	}
	ui.mu.Lock()
	title := ui.nowTitle
	ui.mu.Unlock()

	st := player.Status{}
	if ui.engine != nil {
		st = ui.engine.Status()
	}
	transport := ui.engine != nil
	playing := st.State == player.StatePlaying

	return layout.Inset{Top: unit.Dp(0)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		// hairline on top of the strip
		fillRect(gtx, image.Rect(0, 0, gtx.Constraints.Max.X, 1), 0, colStroke)
		return layout.UniformInset(unit.Dp(12)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					pos := formatTime(st.Position) + "  /  " + formatTime(st.Duration)
					return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							head := material.Body1(ui.th, title)
							head.TextSize = unit.Sp(14)
							head.Color = colText
							return head.Layout(gtx)
						}),
						layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
							return layout.Dimensions{Size: gtx.Constraints.Min}
						}),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							lab := material.Body2(ui.th, pos)
							lab.Color = colHint
							lab.TextSize = unit.Sp(12)
							return lab.Layout(gtx)
						}),
					)
				}),
				layout.Rigid(layout.Spacer{Height: unit.Dp(4)}.Layout),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					slider := material.Slider(ui.th, &ui.seek)
					slider.Color = colAmber
					return slider.Layout(gtx)
				}),
				layout.Rigid(layout.Spacer{Height: unit.Dp(4)}.Layout),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							return ui.iconBtn(gtx, &ui.prevBtn, 36, iconPrev, false)
						}),
						layout.Rigid(layout.Spacer{Width: unit.Dp(6)}.Layout),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							draw := iconPlay
							if st.State == player.StatePlaying {
								draw = iconPause
							}
							if !transport {
								gtx = gtx.Disabled()
							}
							return ui.iconBtn(gtx, &ui.playToggle, 42, draw, playing)
						}),
						layout.Rigid(layout.Spacer{Width: unit.Dp(6)}.Layout),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							if !transport {
								gtx = gtx.Disabled()
							}
							return ui.iconBtn(gtx, &ui.nextBtn, 36, iconNext, false)
						}),
						layout.Rigid(layout.Spacer{Width: unit.Dp(6)}.Layout),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							if !transport {
								gtx = gtx.Disabled()
							}
							return ui.iconBtn(gtx, &ui.stopBtn, 32, iconStop, false)
						}),
						layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
							return layout.Dimensions{Size: gtx.Constraints.Min}
						}),
						layout.Rigid(ui.radioPill),
						layout.Rigid(layout.Spacer{Width: unit.Dp(12)}.Layout),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							lab := material.Body2(ui.th, fmt.Sprintf("%d", ui.volumeSent))
							lab.Color = colHint
							lab.TextSize = unit.Sp(12)
							return lab.Layout(gtx)
						}),
						layout.Rigid(layout.Spacer{Width: unit.Dp(6)}.Layout),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							gtx.Constraints.Max.X = gtx.Dp(unit.Dp(120))
							slider := material.Slider(ui.th, &ui.volume)
							slider.Color = colAmber
							return slider.Layout(gtx)
						}),
					)
				}),
			)
		})
	})
}

// radioPill: flat toggle for the autoplay radio, amber underline when on.
func (ui *appUI) radioPill(gtx layout.Context) layout.Dimensions {
	now := gtx.Now
	if now.IsZero() {
		now = time.Now()
	}
	stInt := ui.interState(&ui.radioBtn, now)
	on := ui.autoplay

	btn := material.Button(ui.th, &ui.radioBtn, "Radio")
	btn.Background = color.NRGBA{}
	btn.Color = colSub
	btn.TextSize = unit.Sp(13)
	btn.CornerRadius = unit.Dp(14)
	btn.Inset = layout.Inset{Top: unit.Dp(6), Bottom: unit.Dp(6), Left: unit.Dp(12), Right: unit.Dp(12)}
	if on {
		btn.Color = colAmber
	}

	// record the label, then paint state layers behind it
	macro := op.Record(gtx.Ops)
	dims := btn.Layout(gtx)
	call := macro.Stop()
	r := image.Rectangle{Max: dims.Size}
	if on {
		hbg := colAmber
		hbg.A = 0x1c
		fillRect(gtx, r, 14, hbg)
	} else if hv := float32(stInt.hover.val(now)); hv > 0.01 {
		hbg := colHover
		hbg.A = uint8(float32(colHover.A) * hv)
		fillRect(gtx, r, 14, hbg)
	}
	if on {
		fillRect(gtx, image.Rect(dims.Size.X/2-14, dims.Size.Y-3, dims.Size.X/2+14, dims.Size.Y-1), 1, colAmber)
	}
	call.Add(gtx.Ops)
	return dims
}

func formatTime(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	min := int(d.Minutes())
	sec := int(d.Seconds()) % 60
	return fmt.Sprintf("%d:%02d", min, sec)
}

func (ui *appUI) searchField(gtx layout.Context) layout.Dimensions {
	border := colStroke
	if ui.editFocus {
		border = colAmber
	}
	return panelRing(gtx, 10, colField, border, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				ed := material.Editor(ui.th, &ui.editor, "Search YouTube Music")
				ed.TextSize = unit.Sp(15)
				return layout.UniformInset(unit.Dp(10)).Layout(gtx, ed.Layout)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				if ui.busy {
					gtx = gtx.Disabled()
				}
				return ui.iconBtn(gtx, &ui.searchBtn, 34, iconSearch, false)
			}),
			layout.Rigid(layout.Spacer{Width: unit.Dp(4)}.Layout),
		)
	})
}

// trackRow renders one search/queue row.

func (ui *appUI) trackRow(gtx layout.Context, t ytm.Track, click *widget.Clickable, loading bool) layout.Dimensions {
	now := gtx.Now
	if now.IsZero() {
		now = time.Now()
	}
	st := ui.interState(click, now)
	hv := float32(st.hover.val(now))
	pw := float32(st.press.val(now))

	ui.mu.Lock()
	playing := ui.nowTrack.VideoID == t.VideoID && t.VideoID != ""
	ui.mu.Unlock()
	if loading {
		playing = false
	}

	// row body, recorded so the hover/press panel slides underneath it
	macro := op.Record(gtx.Ops)
	dims := click.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.UniformInset(unit.Dp(8)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
				// cover art
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return ui.coverArea(gtx, t, 30)
				}),
				layout.Rigid(layout.Spacer{Width: unit.Dp(10)}.Layout),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							title := material.Body1(ui.th, t.Title)
							title.TextSize = unit.Sp(15)
							if playing {
								title.Color = colAmber
							}
							return title.Layout(gtx)
						}),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							meta := trackMeta(t, loading)
							lab := material.Body2(ui.th, meta)
							switch {
							case loading:
								lab.Color = colAmber
							default:
								lab.Color = colSub
							}
							lab.TextSize = unit.Sp(12)
							return lab.Layout(gtx)
						}),
					)
				}),
				layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					return layout.Dimensions{Size: gtx.Constraints.Min}
				}),
				// loading spinner on the right edge of the loading row
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					if !loading {
						return layout.Dimensions{}
					}
					return material.Loader(ui.th).Layout(gtx)
				}),
			)
		})
	})
	call := macro.Stop()
	r := image.Rectangle{Max: dims.Size}
	if playing {
		fillRect(gtx, r, 8, colPanel)
		fillRect(gtx, image.Rect(0, 2, 3, dims.Size.Y-2), 1, colAmber)
	}
	if a := hv*(1-pw) + pw*0.6; a > 0.01 {
		hbg := colHover
		if pw > 0.01 {
			hbg = colPress
		}
		hbg.A = uint8(float32(colHover.A) * a)
		fillRect(gtx, r, 8, hbg)
	}
	call.Add(gtx.Ops)
	return dims
}

// trackMeta builds the muted second line of a track row.
func trackMeta(t ytm.Track, loading bool) string {
	if loading {
		return "resolving stream..."
	}
	meta := t.Artist
	if t.Album != "" {
		if meta != "" {
			meta += "  ·  "
		}
		meta += t.Album
	}
	if t.Duration != "" {
		if meta != "" {
			meta += "   "
		}
		meta += t.Duration
	}
	return meta
}

// coverArea renders cover art at size dp, triggering a background fetch if
// needed. Falls back to a neutral placeholder square.
func (ui *appUI) coverArea(gtx layout.Context, t ytm.Track, size int) layout.Dimensions {
	sz := gtx.Dp(unit.Dp(size))
	img, ok := ui.covers.Get(t.VideoID, t.Thumbnail)
	if !ok {
		ph := clip.Rect{Max: image.Pt(sz, sz)}
		defer ph.Push(gtx.Ops).Pop()
		paint.FillShape(gtx.Ops, color.NRGBA{R: 0xdc, G: 0xdc, B: 0xdc, A: 0xff}, ph.Op())
		return layout.Dimensions{Size: image.Pt(sz, sz)}
	}
	wi := widget.Image{Src: paint.NewImageOp(img), Scale: float32(sz) / float32(img.Bounds().Dx())}
	gtx.Constraints = layout.Constraints{Min: image.Pt(sz, sz), Max: image.Pt(sz, sz)}
	return wi.Layout(gtx)
}

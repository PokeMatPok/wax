//go:build !windows

// Package smtc: no-op fallback for platforms without System Media Transport
// Controls. The Windows twin wire the same API onto WinRT; on Linux/macOS the
// future MPRIS ports (godbus) implement this surface.
package smtc

import "time"

type Button int

const (
	BtnPlay     Button = 0
	BtnPause    Button = 1
	BtnStop     Button = 2
	BtnNext     Button = 6
	BtnPrevious Button = 7
)

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
	return "Button"
}

// Start never fails on non-Windows; there is nothing to integrate yet.
func Start(onButton func(Button)) bool { return false }

func SetTrack(title, artist, album, coverPath string) {}
func SetCover(coverPath string)                       {}
func SetPlayback(playing bool)                        {}
func SetStopped()                                     {}
func SetTimeline(pos, dur time.Duration)              {}
func Enable(on bool)                                  {}

//go:build darwin

// mediakeys_darwin registers a macOS media key controller that integrates
// with MPRemoteCommandCenter (receive play/pause/next/prev/seek commands)
// and MPNowPlayingInfoCenter (show track info in Control Center).

package mediactl

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework MediaPlayer -framework Foundation -framework CoreFoundation
#include "mediakeys_darwin.h"
#include <CoreFoundation/CFRunLoop.h>
#include <stdlib.h>
*/
import "C"

import (
	"runtime"
	"sync"
	"unsafe"

	"cliamp/internal/control"
)

// Package-level send callback. Protected by mu because CGo exported
// functions are called from the Obj-C run loop thread while send is
// set from the Go side during init.
var (
	darwinSend func(interface{})
	darwinMu   sync.Mutex
)

func send(msg interface{}) {
	darwinMu.Lock()
	fn := darwinSend
	darwinMu.Unlock()
	if fn != nil {
		fn(msg)
	}
}

//export goMediaKeyPlay
func goMediaKeyPlay() { send(control.ToggleMsg{}) }

//export goMediaKeyPause
func goMediaKeyPause() { send(control.ToggleMsg{}) }

//export goMediaKeyToggle
func goMediaKeyToggle() { send(control.ToggleMsg{}) }

//export goMediaKeyNext
func goMediaKeyNext() { send(control.NextMsg{}) }

//export goMediaKeyPrev
func goMediaKeyPrev() { send(control.PrevMsg{}) }

//export goMediaKeyStop
func goMediaKeyStop() { send(control.StopMsg{}) }

//export goMediaKeySeekTo
func goMediaKeySeekTo(positionSec C.double) {
	// Convert seconds to microseconds for control.SetPositionMsg.
	us := int64(float64(positionSec) * 1e6)
	send(control.SetPositionMsg{Position: us})
}

// darwinMediaKeys implements Controller and MainLoopRunner.
type darwinMediaKeys struct {
	mu         sync.Mutex
	lastStatus string
	lastTrack  TrackInfo
}

func init() {
	// Pin the main goroutine to OS thread 1 before the Go scheduler
	// can migrate it. macOS requires MPRemoteCommandCenter and
	// CFRunLoop work to happen on the actual main thread.
	runtime.LockOSThread()

	Register(newDarwinMediaKeys)
}

func newDarwinMediaKeys(sendFn func(interface{})) (Controller, error) {
	darwinMu.Lock()
	darwinSend = sendFn
	darwinMu.Unlock()

	// MediaKeysInit is deferred to RunMainLoop so it executes on the
	// main OS thread where the CFRunLoop will run.
	return &darwinMediaKeys{}, nil
}

// Update pushes the current playback state and track info to macOS
// Now Playing info center. Skips the CGo call when only the position
// changed (the per-tick common case).
func (d *darwinMediaKeys) Update(status string, track TrackInfo, volumeDB float64, positionUs int64, canSeek bool) {
	d.mu.Lock()
	defer d.mu.Unlock()

	positionSec := float64(positionUs) / 1e6

	// Fast path: only position changed — lightweight update.
	if status == d.lastStatus && track == d.lastTrack {
		C.MediaKeysUpdatePosition(C.double(positionSec))
		return
	}
	d.lastStatus = status
	d.lastTrack = track

	playing := status == "Playing"

	var cTitle, cArtist, cAlbum *C.char
	if track.Title != "" {
		cTitle = C.CString(track.Title)
		defer C.free(unsafe.Pointer(cTitle))
	}
	if track.Artist != "" {
		cArtist = C.CString(track.Artist)
		defer C.free(unsafe.Pointer(cArtist))
	}
	if track.Album != "" {
		cAlbum = C.CString(track.Album)
		defer C.free(unsafe.Pointer(cAlbum))
	}

	durationSec := float64(track.LengthUs) / 1e6

	C.MediaKeysUpdateNowPlaying(cTitle, cArtist, cAlbum,
		C.double(durationSec), C.double(positionSec), C.bool(playing))
}

// EmitSeeked updates the elapsed position in Now Playing info after a seek.
func (d *darwinMediaKeys) EmitSeeked(positionUs int64) {
	C.MediaKeysUpdatePosition(C.double(float64(positionUs) / 1e6))
}

// Close removes all command targets and clears Now Playing info.
func (d *darwinMediaKeys) Close() {
	C.MediaKeysClose()
	darwinMu.Lock()
	darwinSend = nil
	darwinMu.Unlock()
}

// RunMainLoop registers MPRemoteCommandCenter handlers and pumps the
// CoreFoundation run loop on the main OS thread so that media key
// events are delivered. It blocks until done is closed.
func (d *darwinMediaKeys) RunMainLoop(done <-chan struct{}) {
	// init() already locked this goroutine to the main OS thread.
	C.MediaKeysInit()
	go func() {
		<-done
		C.CFRunLoopStop(C.CFRunLoopGetMain())
	}()
	C.CFRunLoopRun()
}

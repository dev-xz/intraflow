//go:build darwin

// Package runtime — darwin "reopen the running host" bridge.
//
// A second launch of the app (double-click in Finder, Dock icon, Launchpad) does
// NOT start a second process: LaunchServices finds the already-running host by
// bundle identifier, activates it, and delivers the kAEReopenApplication Apple
// event. Before this existed the host had no handler for that event, so a
// relaunch did nothing visible — and because the activation promoted the
// accessory host to a Regular app, a Dock icon appeared for the trayless daemon.
//
// The host cannot implement -applicationShouldHandleReopen:hasVisibleWindows:
// on NSApplicationDelegate, because the systray library (go-systray) installs
// its own TrayIconAppDelegate as NSApp.delegate and owns that class. Verified on
// macOS: adding the delegate method to that foreign class at runtime is NOT
// invoked on a Dock/Finder relaunch, but registering an Apple-event handler with
// NSAppleEventManager DOES fire reliably. So we register a kAEReopenApplication
// handler whose target is our own object (not the delegate) and forward it to Go.
//
// Registration must happen from systray's onReady callback: that is the first
// point at which NSApplication and its event machinery are up. systray invokes
// onReady on a worker goroutine, so the ObjC bridge synchronously registers on
// the main queue before returning. Registering earlier silently never delivers
// the event.
//
// The Objective-C class lives in reopen_darwin.m rather than in this preamble
// because this file uses //export: cgo duplicates a file's preamble into the
// generated export translation unit, so any definition (rather than a mere
// declaration) here would be compiled twice and fail to link.
package runtime

/*
#cgo CFLAGS: -x objective-c -Wno-deprecated-declarations
#cgo LDFLAGS: -framework Cocoa

// Implemented in reopen_darwin.m.
void IntraflowRegisterReopenHandler(void);
*/
import "C"

// goReopenSink receives reopen events from the ObjC handler. It is set once by
// RegisterReopenHandler before synchronous main-queue registration completes;
// the Apple-event handler reads it on the main run loop. Call registration once.
var goReopenSink func()

//export reopenHandlerGo
func reopenHandlerGo() {
	if goReopenSink != nil {
		goReopenSink()
	}
}

// RegisterReopenHandler installs the kAEReopenApplication Apple-event handler
// and invokes onReopen whenever the user re-launches the already-running host
// from Finder, the Dock, or Launchpad. onReopen is invoked on the main thread;
// callers that need to touch host state should hand the work to another
// goroutine so the main run loop is not blocked.
//
// Call once, from systray's worker-goroutine onReady callback (after
// NSApplication is up). The ObjC bridge synchronously hops to the main queue.
// Calling it earlier is a silent no-op: the event is not delivered.
func RegisterReopenHandler(onReopen func()) {
	if onReopen == nil {
		return
	}
	goReopenSink = onReopen
	C.IntraflowRegisterReopenHandler()
}

//go:build darwin

// Package runtime — darwin activation policy bridge for the HOST process.
//
// The host process must NOT show a Dock icon (it's a menu-bar-only daemon).
// Two mechanisms enforce that, and both are needed:
//
//  1. LSUIElement=true in the bundle Info.plist. This makes LaunchServices
//     launch the host as an accessory/agent app, which is also what keeps a
//     *relaunch* (second double-click in Finder / Dock click) from promoting
//     the running host to a Regular app when LaunchServices activates it.
//  2. setHostAccessoryPolicy below, which re-asserts the Accessory policy after
//     systray's own NSApplicationDelegate has come up (systray may force
//     Regular before our policy is applied).
//
// The GUI child process is a normal windowed app (Regular) and does not touch
// this: Wails' AppDelegate calls setActivationPolicy:Regular unconditionally in
// the GUI process, and that per-process call is authoritative.
package runtime

/*
#cgo CFLAGS: -x objective-c -Wno-deprecated-declarations
#cgo LDFLAGS: -framework Cocoa

#import <Cocoa/Cocoa.h>

void setHostAccessoryPolicy(void) {
    if ([NSThread isMainThread]) {
        [NSApp setActivationPolicy:NSApplicationActivationPolicyAccessory];
        return;
    }
    dispatch_sync(dispatch_get_main_queue(), ^{
        [NSApp setActivationPolicy:NSApplicationActivationPolicyAccessory];
    });
}
*/
import "C"

// SetHostAccessoryPolicy hides the host process from the Dock by switching to
// the Accessory activation policy. Call once at host startup, after the
// systray/NSApplication is up. No-op on non-darwin.
func SetHostAccessoryPolicy() {
	C.setHostAccessoryPolicy()
}
//go:build darwin

// Package runtime — darwin activation policy bridge for the HOST process.
//
// The host process must NOT show a Dock icon (it's a menu-bar-only daemon).
// systray's own AppDelegate may force Regular policy; we override to
// Accessory once our host is initialized. The GUI child process is a normal
// windowed app (Regular) and does not touch this.
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
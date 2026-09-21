//go:build darwin

package runtime

/*
#cgo CFLAGS: -x objective-c -fobjc-arc -Wno-deprecated-declarations
#cgo LDFLAGS: -framework ServiceManagement -framework Foundation

#import <Foundation/Foundation.h>
#import <ServiceManagement/ServiceManagement.h>

// SMAppService status codes (mapped from SMAppServiceStatus enum).
enum {
    kSMStatusUnknown          = 0,
    kSMStatusEnabled          = 1,
    kSMStatusRequiresApproval = 2,
    kSMStatusNotRegistered    = 3,
    kSMStatusNotFound         = 4,
    kSMStatusNotBundled       = 5, // caller-side: process not in an .app
    kSMStatusError            = 6,
};

// smCopyNSString copies an NSString into a malloc'd C string (UTF-8). Caller
// frees with smFreeCString. Must be declared BEFORE the functions that use it
// so the C99 compiler sees a proper prototype.
static char *smCopyNSString(NSString *s) {
    if (s == nil) { s = @"(nil)"; }
    const char *utf8 = [s UTF8String];
    size_t len = strlen(utf8) + 1;
    char *out = (char *)malloc(len);
    if (out) { memcpy(out, utf8, len); }
    return out;
}

static void smFreeCString(char *p) {
    free(p);
}

// smBoolToInt normalizes an Objective-C BOOL to a plain int. BOOL is a signed
// char on 64-bit Intel but a C bool on Apple Silicon, so cgo exposes it as
// different Go types per architecture; returning int keeps the Go side portable
// across the darwin/universal build.
static int smBoolToInt(BOOL v) {
    return v ? 1 : 0;
}

// smAppServiceAvailable returns YES if the SMAppService API is present at
// runtime, i.e. the OS is macOS 13.0 (Ventura) or newer.
static BOOL smAppServiceAvailableImpl(void) {
    if (@available(macOS 13.0, *)) {
        return YES;
    }
    return NO;
}

// smAppServiceStatusImpl returns the registration status of the calling app's
// main login item. Returns kSMStatusNotBundled when the process is not running
// from inside an .app bundle (SMAppService cannot operate outside a bundle).
static int smAppServiceStatusImpl(char **outErr) {
    if (@available(macOS 13.0, *)) {
        NSString *bundlePath = [[NSBundle mainBundle] bundlePath];
        if (bundlePath == nil || bundlePath.length == 0) {
            return kSMStatusNotBundled;
        }
        if (![bundlePath hasSuffix:@".app"]) {
            return kSMStatusNotBundled;
        }
        SMAppServiceStatus st = [[SMAppService mainAppService] status];
        switch (st) {
            case SMAppServiceStatusEnabled:
                return kSMStatusEnabled;
            case SMAppServiceStatusRequiresApproval:
                return kSMStatusRequiresApproval;
            case SMAppServiceStatusNotRegistered:
                return kSMStatusNotRegistered;
            case SMAppServiceStatusNotFound:
                return kSMStatusNotFound;
            default:
                return kSMStatusUnknown;
        }
    }
    if (outErr) { *outErr = smCopyNSString(@"SMAppService unavailable"); }
    return kSMStatusNotBundled;
}

// smAppServiceRegisterImpl registers the calling app as a login item.
// Returns YES on success. On failure, copies the NSError localizedDescription
// into *outErr (caller frees with smFreeCString).
static BOOL smAppServiceRegisterImpl(char **outErr) {
    if (@available(macOS 13.0, *)) {
        NSString *bundlePath = [[NSBundle mainBundle] bundlePath];
        if (bundlePath == nil || bundlePath.length == 0 || ![bundlePath hasSuffix:@".app"]) {
            if (outErr) { *outErr = smCopyNSString(@"not bundled in a .app"); }
            return NO;
        }
        NSError *err = nil;
        BOOL ok = [[SMAppService mainAppService] registerAndReturnError:&err];
        if (!ok) {
            if (outErr) { *outErr = smCopyNSString([err localizedDescription]); }
            return NO;
        }
        return YES;
    }
    if (outErr) { *outErr = smCopyNSString(@"SMAppService unavailable"); }
    return NO;
}

// smAppServiceUnregisterImpl unregisters the calling app's login item.
static BOOL smAppServiceUnregisterImpl(char **outErr) {
    if (@available(macOS 13.0, *)) {
        NSString *bundlePath = [[NSBundle mainBundle] bundlePath];
        if (bundlePath == nil || bundlePath.length == 0 || ![bundlePath hasSuffix:@".app"]) {
            if (outErr) { *outErr = smCopyNSString(@"not bundled in a .app"); }
            return NO;
        }
        NSError *err = nil;
        BOOL ok = [[SMAppService mainAppService] unregisterAndReturnError:&err];
        if (!ok) {
            if (outErr) { *outErr = smCopyNSString([err localizedDescription]); }
            return NO;
        }
        return YES;
    }
    if (outErr) { *outErr = smCopyNSString(@"SMAppService unavailable"); }
    return NO;
}
*/
import "C"

import (
	"errors"
	"fmt"
)

// errSMAppNotBundled is returned by the SMAppService bridge when the running
// process is not inside an .app bundle. Callers treat this as a signal to
// fall back to the LaunchAgent code path rather than as a hard error.
var errSMAppNotBundled = errors.New("autostart: process is not bundled in a .app")

// smAppServiceAvailable reports whether the SMAppService API is available at
// runtime (macOS >= 13.0). This is the default implementation of
// darwinSMAppServiceAvailable; tests may swap that var to simulate older OSes.
func smAppServiceAvailable() bool {
	return C.smBoolToInt(C.smAppServiceAvailableImpl()) != 0
}

// smAppServiceStatus queries the live SMAppService registration status of the
// calling main app and maps it to an AutoStartState.
func smAppServiceStatus() (AutoStartState, error) {
	var cerr *C.char
	st := C.smAppServiceStatusImpl(&cerr)
	var msg string
	if cerr != nil {
		msg = C.GoString(cerr)
		C.smFreeCString(cerr)
	}
	switch int(st) {
	case 1: // kSMStatusEnabled
		return AutoStartStateEnabled, nil
	case 2: // kSMStatusRequiresApproval
		return AutoStartStateRequiresApproval, nil
	case 3, 4: // kSMStatusNotRegistered / kSMStatusNotFound
		return AutoStartStateDisabled, nil
	case 5: // kSMStatusNotBundled
		return AutoStartStateDisabled, errSMAppNotBundled
	case 0, 6: // unknown / error
		if msg != "" {
			return AutoStartStateDisabled, fmt.Errorf("autostart: SMAppService status: %s", msg)
		}
		return AutoStartStateDisabled, errors.New("autostart: SMAppService status unknown")
	default:
		return AutoStartStateDisabled, fmt.Errorf("autostart: SMAppService status: unexpected code %d", int(st))
	}
}

// smAppServiceSetEnabled registers or unregisters the calling main app as a
// login item via SMAppService. Returns errSMAppNotBundled when the process is
// not inside an .app bundle, so the caller can fall back to the LaunchAgent
// path.
func smAppServiceSetEnabled(enabled bool) error {
	var cerr *C.char
	var ok C.int
	if enabled {
		ok = C.smBoolToInt(C.smAppServiceRegisterImpl(&cerr))
	} else {
		ok = C.smBoolToInt(C.smAppServiceUnregisterImpl(&cerr))
	}
	var msg string
	if cerr != nil {
		msg = C.GoString(cerr)
		C.smFreeCString(cerr)
	}
	if ok == 0 {
		// Detect the "not bundled" condition from the bridge's own message
		// and translate it to errSMAppNotBundled for clean fallback.
		if msg == "not bundled in a .app" {
			return errSMAppNotBundled
		}
		if msg != "" {
			return fmt.Errorf("autostart: SMAppService register: %s", msg)
		}
		return errors.New("autostart: SMAppService register failed")
	}
	return nil
}
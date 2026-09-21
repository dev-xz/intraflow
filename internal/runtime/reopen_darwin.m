//go:build darwin

// Objective-C support for reopening the already-running host — see
// reopen_darwin.go for the full rationale.
//
// This lives in a .m file (not the cgo preamble) so the class is defined exactly
// once: the accompanying Go file uses //export, and cgo compiles a copy of the
// preamble into the export translation unit, which would otherwise duplicate
// these symbols at link time.

#import <Cocoa/Cocoa.h>
#import <dispatch/dispatch.h>

// reopenHandlerGo is implemented in Go (//export in reopen_darwin.go) and is
// the Go-side receiver for the reopen event.
void reopenHandlerGo(void);

// IntraflowReopenTarget is our own Apple-event target object. Using a dedicated
// target keeps us completely clear of the systray library's delegate object.
@interface IntraflowReopenTarget : NSObject
- (void)handleReopen:(NSAppleEventDescriptor *)event
      withReplyEvent:(NSAppleEventDescriptor *)replyEvent;
@end

@implementation IntraflowReopenTarget

- (void)handleReopen:(NSAppleEventDescriptor *)event
      withReplyEvent:(NSAppleEventDescriptor *)replyEvent {
    (void)event;
    (void)replyEvent;
    reopenHandlerGo();
}

@end

static IntraflowReopenTarget *intraflowReopenTarget = nil;
static BOOL intraflowReopenRegistered = NO;

static void intraflowRegisterReopenOnMain(void *context) {
    (void)context;
    if (intraflowReopenRegistered) {
        return;
    }
    intraflowReopenTarget = [[IntraflowReopenTarget alloc] init];
    [[NSAppleEventManager sharedAppleEventManager]
        setEventHandler:intraflowReopenTarget
            andSelector:@selector(handleReopen:withReplyEvent:)
          forEventClass:kCoreEventClass
          andEventID:kAEReopenApplication];
    intraflowReopenRegistered = YES;
}

void IntraflowRegisterReopenHandler(void) {
    if ([NSThread isMainThread]) {
        intraflowRegisterReopenOnMain(NULL);
    } else {
        dispatch_sync_f(dispatch_get_main_queue(), NULL, intraflowRegisterReopenOnMain);
    }
}

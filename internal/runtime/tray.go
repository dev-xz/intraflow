// Package runtime implements the desktop-runtime layer for IntraFlow: the
// system-tray (menu-bar) icon, OS-native auto-start-on-boot, and a
// single-instance lock. It is consumed by the main package (main.go / app.go).
//
// The tray is built on github.com/cardinalby/go-systray, a fork of
// fyne-io/systray that renamed the macOS ObjC AppDelegate class to
// TrayIconAppDelegate so it does not collide with Wails' own AppDelegate
// symbol (which causes duplicate-symbol linker errors on darwin). The public
// API is identical to fyne-io/getlantern systray.
//
// On macOS the tray's native event loop must run on the main thread.
// systray.Run(onReady, onExit) blocks until systray.Quit() is called. Because
// Wails also needs the main thread for its NSApplication loop on macOS, the
// integration pattern used here is: main() calls RunTray(...) directly
// (blocking the main goroutine), and inside systray's onReady callback Wails
// is launched in a separate goroutine. See main.go for the wiring.
package runtime

import (
	"fmt"
	"sync"

	systray "github.com/cardinalby/go-systray"
)

// TrayState is the minimal snapshot the host pushes to the tray via Refresh.
// It carries exactly what the menu needs to render: the paused flag (drives
// the title and the pause/resume item label) and the counts the title shows
// (domain count and the number of forwards currently listening). Statuses /
// per-record lists are no longer surfaced in the menu (the menu is global-
// only now); the host computes the counts before calling Refresh.
type TrayState struct {
	// Paused is the global paused flag from config.Settings.
	Paused bool
	// DomainCount is the number of configured Domain entries.
	DomainCount int
	// ListeningForwards is the number of effective forwards currently in
	// forwarder.StatusListening (0 when paused).
	ListeningForwards int
}

// TrayCallbacks is the set of host-supplied callbacks invoked when the user
// interacts with the tray menu. The App/main package supplies these so the
// tray layer never imports the orchestrator or Wails runtime directly.
//
// Every callback is invoked from a systray menu-click goroutine. Implementors
// must be safe to call from arbitrary goroutines.
type TrayCallbacks struct {
	// OnOpen is invoked when the user clicks "打开主界面". Typically the host
	// spawns the GUI child process (intraflow --gui).
	OnOpen func()

	// OnOpenSettings is invoked when the user clicks "设置...". The host
	// shows the window (spawning the GUI with --open-settings if none is
	// running, or broadcasting an openSettings IPC event to an already-open
	// GUI) so the frontend opens the settings modal.
	OnOpenSettings func()

	// OnPause is invoked when the user clicks "暂停托管" (label shown when
	// not paused). The host triggers the elevated pause flow (PreparePause →
	// GUI writes hosts → CompletePause). Because the host must never call
	// osascript, the flow is routed through the GUI process — see host.go's
	// onPause for the exact dispatch.
	OnPause func()

	// OnResume is invoked when the user clicks "恢复托管" (label shown when
	// paused). The host triggers the elevated resume flow (PrepareResume →
	// GUI writes hosts → CompleteResume) via the GUI process.
	OnResume func()

	// OnQuit is invoked when the user clicks "退出". The host should tear the
	// app down (kill the GUI child, wailsruntime.Quit + runtime.QuitTray).
	OnQuit func()
}

// Tray wraps the systray menu state. It is safe for concurrent use: the
// systray library itself is goroutine-safe, and buildMenu rebuilds the
// entire menu from scratch under a lock on every refresh.
//
// Menu design note: the cardinalby/go-systray fork (like its fyne-io parent)
// only APPENDS items via AddMenuItem/AddSeparator/ResetMenu; there is no
// insert-at-index or per-item delete. So instead of mutating the live menu on
// each refresh (which left stale appended items after 退出), we rebuild the
// WHOLE menu from scratch via ResetMenu() + re-add every item in the correct
// order. Each rebuild creates fresh MenuItems and re-watches their ClickedCh
// channels; the previous items are discarded by ResetMenu and their click
// goroutines exit when their channels are closed by the library.
type Tray struct {
	cb TrayCallbacks

	mu sync.Mutex

	// menuReady is set true once onReady has fired and the icon/tooltip are
	// configured. buildMenu is a no-op until then (ResetMenu before the
	// systray loop is up is undefined).
	menuReady bool

	// lastState is the most recent state pushed by the host via Refresh.
	// buildMenu reads it so a refresh-only update can rebuild the menu with
	// the correct title and pause/resume label without needing the host to
	// re-push anything else.
	lastState TrayState

	stopCh chan struct{}
	start  func()
	end    func()
}

// RunTray starts the systray event loop and blocks until QuitTray() is
// called. onReady builds the menu and wires click channels to the callbacks;
// it also starts the goroutines that drain each MenuItem.ClickedCh.
//
// IMPORTANT: on macOS this MUST be called from the main goroutine (systray
// locks the OS thread in init()). Because it blocks, it is only suitable
// when the host runs its own UI loop in a goroutine. For Wails (which also
// needs the main thread on macOS), use RegisterTray instead and then call
// wails.Run on the main goroutine.
//
// RunTray returns the *Tray only after systray.Run returns (i.e. after the
// loop has quit), so the caller cannot use the reference to push updates
// while the loop is running. To get the reference before the loop blocks,
// use CreateTray + (*Tray).Run.
func RunTray(cb TrayCallbacks) *Tray {
	t := CreateTray(cb)
	t.Run()
	return t
}

// CreateTray constructs a Tray with the given callbacks WITHOUT starting the
// native event loop. It returns immediately so the caller can hold the
// reference and push updates (Refresh) from other goroutines while the loop
// runs. The caller MUST call (*Tray).Run on the main goroutine (on macOS) to
// actually start the systray loop; until then the menu is not built and the
// icon does not appear.
//
// This is the correct entry point for the dual-process host: the host creates
// the tray, starts its IPC server in a goroutine, then calls Run on the main
// goroutine. IPC handlers can call Refresh on the returned *Tray as soon as
// the menu is ready (Refresh is a no-op before onReady fires).
func CreateTray(cb TrayCallbacks) *Tray {
	return &Tray{
		cb:     cb,
		stopCh: make(chan struct{}),
	}
}

// Run starts the systray native event loop and blocks until QuitTray() is
// called. It must be called on the main goroutine on macOS (systray locks the
// OS thread in init()). The Tray must have been created via CreateTray (or
// returned by RunTray/RegisterTray); calling Run on a Tray whose loop is
// already running is undefined.
func (t *Tray) Run() {
	systray.Run(t.onReady, t.onExit)
}

// RegisterTray registers the systray callbacks WITHOUT running the native
// event loop. The host (Wails' NSApplication loop on macOS) drives the event
// loop. The returned Tray has a Start() method that the host MUST call once
// the native application has started (e.g. from Wails' OnStartup or
// OnDomReady hook) to actually activate the tray icon — without Start(), the
// icon is registered but never appears in the menu bar.
//
// This uses systray.RunWithExternalLoop, which is the correct integration
// path for embedding systray inside another macOS NSApplication host (Wails).
// systray.Register alone does NOT surface the icon on modern macOS because it
// skips the nativeStart step that wires the NSStatusItem into the running
// NSApplication.
func RegisterTray(cb TrayCallbacks) *Tray {
	t := &Tray{
		cb:     cb,
		stopCh: make(chan struct{}),
	}
	t.start, t.end = systray.RunWithExternalLoop(t.onReady, t.onExit)
	return t
}

// Start activates the tray icon. Must be called once the host's native
// application has started (e.g. from Wails' OnStartup hook). Safe to call
// exactly once; calling it before the NSApplication is running is a no-op
// (the icon appears once the app loop spins up).
func (t *Tray) Start() {
	if t.start != nil {
		t.start()
	}
}

// End tears down the tray's native resources. Called by the host on quit
// before QuitTray. Safe to call once.
func (t *Tray) End() {
	if t.end != nil {
		t.end()
	}
}

// onReady builds the initial menu skeleton and marks the tray ready for
// refresh. All menu contents are (re)built by buildMenu; onReady just fires
// it once with the last-pushed state (zero values if nothing pushed yet) so
// the icon appears with a coherent menu the moment it shows up.
func (t *Tray) onReady() {
	// Use an icon image instead of text title for a proper menu-bar item.
	// SetTitle shows literal text in the menu bar (ugly); SetIcon shows a
	// template image that adapts to light/dark mode.
	iconBytes, err := embeddedIcon()
	if err != nil {
		// Fallback: no icon, keep the text title so the item is at least visible.
		systray.SetTitle("IF")
	} else {
		systray.SetIcon(iconBytes)
	}
	systray.SetTooltip("IntraFlow")

	// Ensure the host process does not show a Dock icon — it's a menu-bar-only
	// daemon. This runs inside onReady which fires after NSApplication is up,
	// so setActivationPolicy is safe here.
	SetHostAccessoryPolicy()

	t.mu.Lock()
	t.menuReady = true
	state := t.lastState
	t.mu.Unlock()

	// buildMenu uses the stored state; if the host has not pushed anything
	// yet, the title still renders with zero counts and the pause/resume
	// item reads "暂停托管".
	t.buildMenuLocked(state)
}

// onExit is systray's exit callback. It closes stopCh so any waiter (main)
// knows the loop has terminated.
func (t *Tray) onExit() {
	select {
	case <-t.stopCh:
		// already closed
	default:
		close(t.stopCh)
	}
}

// watch drains a MenuItem's ClickedCh and invokes fn for every click. fn may
// be nil, in which case the click is silently consumed (used for disabled
// header items, though those don't fire anyway). The goroutine exits when
// ResetMenu closes the channel (the library closes ClickedCh on item
// destruction).
func (t *Tray) watch(item *systray.MenuItem, fn func()) {
	for range item.ClickedCh {
		if fn != nil {
			fn()
		}
	}
}

// Refresh stores the latest state and rebuilds the menu. It is a no-op until
// onReady has fired (the menu is not ready before the systray loop is up).
// This is the single entry point the host uses besides the systray callbacks;
// the rebuild-on-refresh design means the title and pause/resume label stay
// in sync with backend state without any per-item mutation.
func (t *Tray) Refresh(state TrayState) {
	t.mu.Lock()
	t.lastState = state
	if !t.menuReady {
		t.mu.Unlock()
		return
	}
	t.mu.Unlock()
	t.buildMenu()
}

// buildMenu rebuilds the entire menu from the last-pushed state. It calls
// ResetMenu() and re-adds every item in the correct order, wiring each
// item's ClickedCh to a fresh goroutine. Callers MUST NOT hold t.mu when
// calling this (buildMenuLocked acquires/releases it internally so the
// systray calls are made without holding the lock, avoiding a deadlock with
// a click handler that may also need the lock).
func (t *Tray) buildMenu() {
	t.mu.Lock()
	state := t.lastState
	t.mu.Unlock()
	t.buildMenuLocked(state)
}

// buildMenuLocked reconstructs the menu from the given state. It is called
// by onReady (with the initial state) and by buildMenu (with the last-pushed
// state). It is the single source of truth for menu layout, so the order is
// guaranteed regardless of how the host pushed updates.
//
// Menu layout (global-only, no per-record section):
//  1. Disabled title: paused ? "IntraFlow · 已暂停" : "IntraFlow · N 域名 · M 转发中"
//  2. 打开主界面
//  3. 设置...
//  4. separator
//  5. 暂停托管 / 恢复托管 (label flips by paused state)
//  6. separator
//  7. 退出
func (t *Tray) buildMenuLocked(state TrayState) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if !t.menuReady {
		return
	}

	// ResetMenu discards every existing item (and closes their ClickedCh,
	// which lets our watch goroutines exit). We then re-add everything in
	// order. This is the only way to keep the pause/resume label above the
	// quit entry, because systray only appends.
	systray.ResetMenu()

	// 1. Disabled title item.
	var title string
	if state.Paused {
		title = "IntraFlow · 已暂停"
	} else {
		title = fmt.Sprintf("IntraFlow · %d 域名 · %d 转发中", state.DomainCount, state.ListeningForwards)
	}
	titleItem := systray.AddMenuItem(title, "")
	titleItem.Disable()

	// 2. 打开主界面
	openItem := systray.AddMenuItem("打开主界面", "打开主界面")
	go t.watch(openItem, t.cb.OnOpen)

	// 3. 设置...
	settingsItem := systray.AddMenuItem("设置...", "打开设置")
	go t.watch(settingsItem, t.cb.OnOpenSettings)

	// 4. Separator
	systray.AddSeparator()

	// 5. 暂停托管 / 恢复托管 (single item, label flips by paused state).
	var pauseLabel, pauseTooltip string
	var pauseFn func()
	if state.Paused {
		pauseLabel = "恢复托管"
		pauseTooltip = "恢复所有转发并写回 hosts"
		pauseFn = t.cb.OnResume
	} else {
		pauseLabel = "暂停托管"
		pauseTooltip = "停止所有转发并清空 hosts 标记区"
		pauseFn = t.cb.OnPause
	}
	pauseItem := systray.AddMenuItem(pauseLabel, pauseTooltip)
	go t.watch(pauseItem, pauseFn)

	// 6. Separator
	systray.AddSeparator()

	// 7. 退出
	quitItem := systray.AddMenuItem("退出", "退出 IntraFlow")
	go t.watch(quitItem, t.cb.OnQuit)
}

// StopCh returns a channel that is closed when systray's onExit callback
// fires (i.e. the tray loop has terminated). main can wait on this after
// calling QuitTray to ensure clean teardown ordering.
func (t *Tray) StopCh() <-chan struct{} {
	return t.stopCh
}

// QuitTray requests the systray loop to terminate. It is safe to call from
// any goroutine; systray.Quit is once-guarded internally.
func QuitTray() {
	systray.Quit()
}
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
// On macOS the tray's native event loop must run on the main thread, while
// systray invokes onReady on a worker goroutine. The host uses RegisterTray
// with systray's external-loop integration to run alongside Wails' separate
// GUI process.
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
	// OnOpen is invoked when the user clicks "打开主界面", and also when the
	// user re-launches the already-running app (Finder double-click / Dock /
	// Launchpad) on macOS — both mean "show me the panel". The host either
	// spawns the GUI child process (intraflow --gui) when none is running, or
	// raises the existing window.
	OnOpen func()

	// OnOpenSettings is invoked when the user clicks "设置...". The host
	// shows the window (spawning the GUI with --open-settings if none is
	// running, or broadcasting an openSettings IPC event to an already-open
	// GUI) so the frontend opens the settings modal.
	OnOpenSettings func()

	// OnReopen is invoked when the macOS user re-launches the already-running
	// app from Finder, the Dock, or Launchpad (the kAEReopenApplication Apple
	// event). It means the same thing as OnOpen — "show me the panel" — but is
	// kept separate because it also fires with no tray interaction at all, and
	// only on macOS. Nil on other platforms.
	OnReopen func()

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
	// iconMu serializes native icon updates separately from menu/state access.
	iconMu sync.Mutex

	// menuReady is set true once onReady has fired and the icon/tooltip are
	// configured. buildMenu is a no-op until then (ResetMenu before the
	// systray loop is up is undefined).
	menuReady bool

	// lastState is the most recent state pushed by the host via Refresh.
	// buildMenu reads it so a refresh-only update can rebuild the menu with
	// the correct title and pause/resume label without needing the host to
	// re-push anything else.
	lastState TrayState

	// lastIconPaused and iconInstalled are protected by iconMu. The latter
	// ensures the initial hosted icon is installed even for the zero state.
	lastIconPaused bool
	iconInstalled  bool

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
// goroutine. IPC handlers can call Refresh on the returned *Tray before the
// menu is ready; onReady applies the last state they pushed.
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
	// Ensure the host process does not show a Dock icon — it's a menu-bar-only
	// daemon. This runs inside onReady which fires after NSApplication is up,
	// so setActivationPolicy is safe here.
	SetHostAccessoryPolicy()

	// Use an icon image instead of a text title for a proper menu-bar item.
	// SetTitle shows literal text in the menu bar (ugly); SetTemplateIcon
	// installs a monochrome template image that macOS re-tints for light/dark
	// menu bars. The hosted/paused asset pair is chosen by the hosting state.
	t.applyIcon()

	systray.SetTooltip("IntraFlow")

	// Register for the macOS "user relaunched the running app" Apple event, so
	// a second launch from Finder/Dock/Launchpad opens the panel instead of
	// silently doing nothing (see reopen_darwin.go). Registration must happen
	// here: NSApp and its event machinery are only up once onReady fires.
	// systray calls onReady from a worker goroutine; registration synchronously
	// switches to the main queue before returning.
	if t.cb.OnReopen != nil {
		RegisterReopenHandler(func() {
			// The Apple-event handler runs on the main thread; hand off so a
			// slow GUI spawn never stalls the native run loop.
			go t.cb.OnReopen()
		})
	}

	t.mu.Lock()
	t.menuReady = true
	t.mu.Unlock()

	// A refresh while menuReady was false may have changed the state after the
	// first icon update. Pick up that state before building the initial menu.
	t.applyIcon()
	t.buildMenu()
}

// applyIcon installs the menu-bar icon for the latest hosting state. The caller
// must not hold t.mu. iconMu keeps concurrent native updates in state order.
//
// On macOS SetTemplateIcon marks the image as a template so it adapts to the
// menu-bar appearance; on Windows/Linux systray ignores the template bytes and
// uses the colour fallback.
func (t *Tray) applyIcon() {
	t.iconMu.Lock()
	defer t.iconMu.Unlock()

	t.mu.Lock()
	paused := t.lastState.Paused
	t.mu.Unlock()
	if t.iconInstalled && t.lastIconPaused == paused {
		return
	}
	template, regular := trayIcon(paused)
	systray.SetTemplateIcon(template, regular)
	t.lastIconPaused = paused
	t.iconInstalled = true
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

// Refresh stores the latest state and rebuilds the menu once onReady has fired.
// Before then, the state is retained for onReady's initial icon and menu.
// This is the single entry point the host uses besides the systray callbacks;
// the rebuild-on-refresh design means the title and pause/resume label stay
// in sync with backend state without any per-item mutation.
func (t *Tray) Refresh(state TrayState) {
	t.mu.Lock()
	t.lastState = state
	ready := t.menuReady
	t.mu.Unlock()
	if !ready {
		return
	}
	t.applyIcon()
	t.buildMenu()
}

// buildMenu rebuilds the entire menu from the last-pushed state. It calls
// ResetMenu() and re-adds every item in the correct order, wiring each
// item's ClickedCh to a fresh goroutine. Callers MUST NOT hold t.mu when
// calling this; the rebuild holds it to keep native menu updates ordered.
func (t *Tray) buildMenu() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.menuReady {
		return
	}
	t.buildMenuLocked(t.lastState)
}

// buildMenuLocked reconstructs the menu from the given state. It is called
// by buildMenu with t.mu held and the last-pushed state. It is the single
// source of truth for menu layout, so the order is guaranteed regardless of
// how the host pushed updates.
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

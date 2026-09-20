package main

import (
	"context"
	"log"
	"time"

	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"intraflow/internal/hosts"
	"intraflow/internal/ipc"
)

// App is the Wails-bound application struct in the GUI process. It does NOT
// own an orchestrator — the GUI connects to the host process over IPC and
// every facade method is a thin shim that calls the corresponding ipc.Client
// method. The host owns the real config/hosts/forwarder state.
//
// For hosts-file writes (SaveAll / Pause / Resume / FixNow), the GUI runs the
// elevated osascript itself: it is the foreground windowed process, so the
// native admin password dialog can attach. The host cannot show the dialog
// (it's a background menu-bar daemon → osascript errors -128 "user canceled"
// because the dialog never appears). The flow is two-phase:
//  1. GUI calls host Prepare* → host validates + builds new hosts content
//  2. GUI runs osascript to write that content (foreground dialog works)
//  3. GUI calls host Complete* → host finalizes (persist config, sync pool)
//
// In bindings mode (main_bindings.go) the App is constructed with a nil IPC
// client; Wails' binding extractor inspects only the method signatures, not
// runtime values, so this is safe.
type App struct {
	ctx context.Context
	ipc *ipc.Client

	// pendingAction carries a one-shot spawn-flag-driven action the
	// frontend should perform once it has registered its event listeners
	// and finished its initial render. It is set by runGUI when the host
	// spawned the GUI with --open-settings ("settings") or --confirm-quit
	// ("confirmQuit") and read (and cleared) by the PendingAction() Wails
	// binding, which the frontend calls from its init sequence.
	//
	// Why pull instead of push: onDomReady fires before the frontend JS
	// has registered its EventsOn handlers, so a Wails EventsEmit from
	// onDomReady would be lost. The frontend instead polls PendingAction()
	// after its init runs, which is strictly after its listeners are up.
	// The already-running-GUI path (IPC event → emitOpenSettings /
	// emitConfirmQuit) is unaffected because by then the listeners are
	// subscribed.
	//
	// The --elevate flow does NOT use this field: its two-phase hosts
	// write is pure Go (Prepare → WriteHostsElevated → Complete) with no
	// JS involvement, so elevateOnReady runs directly from onDomReady.
	pendingAction string

	// elevateOnReady is set when the GUI was spawned with --elevate=pause or
	// --elevate=resume (from the tray's "暂停托管"/"恢复托管" entry while no
	// GUI was running). onDomReady runs the corresponding two-phase flow
	// (Prepare → WriteHostsElevated → Complete) directly and then quits the
	// GUI process so it does not leave a window open. This is the spawned-
	// flag elevation path documented in host.go's onPause/onResume.
	elevateOnReady string
}

// NewApp creates a new App application struct. ipc may be nil in bindings
// generation mode (the methods are never called there; Wails only introspects
// the signatures).
func NewApp(ipcClient *ipc.Client) *App {
	return &App{ipc: ipcClient}
}

// startup is called when the Wails app starts. The context is saved so we
// can call Wails runtime methods (events, window) if needed.
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
}

// onDomReady is called once the frontend has loaded. It handles only the
// --elevate spawn-flag path, whose two-phase hosts write is pure Go
// (Prepare → WriteHostsElevated → Complete) with no JS involvement, so it
// is safe to run here before the frontend's event listeners are up.
//
// The --open-settings and --confirm-quit spawn-flag paths are NOT handled
// here: they would need to emit a Wails event, but onDomReady fires before
// the frontend JS has registered its EventsOn handlers, so the event would
// be lost. Those paths instead stash a pendingAction ("settings" /
// "confirmQuit") which the frontend pulls via PendingAction() after its
// init sequence completes and its listeners are subscribed.
func (a *App) onDomReady(ctx context.Context) {
	if a.elevateOnReady != "" {
		a.runElevateOnReady(a.elevateOnReady)
		return
	}
}

// runElevateOnReady runs the pause/resume two-phase flow in response to a
// --elevate flag, then quits the GUI process. It logs failures but does not
// surface a toast (the spawned GUI window is hidden behind the elevation
// dialog and quits immediately after, so a toast would not be seen).
func (a *App) runElevateOnReady(action string) {
	switch action {
	case "pause":
		_ = a.Pause()
	case "resume":
		_ = a.Resume()
	}
	if a.ctx != nil {
		wailsruntime.Quit(a.ctx)
	}
}

// emitRecordsChanged emits a Wails "recordsChanged" event so the open panel
// refreshes in real time when the backend domain/forward set changes (e.g.
// the user paused/resumed from the tray). The frontend listens for this
// event and re-fetches its lists.
func (a *App) emitRecordsChanged() {
	if a.ctx == nil {
		return
	}
	wailsruntime.EventsEmit(a.ctx, "recordsChanged")
}

// emitOpenSettings emits a Wails "openSettings" event so the frontend opens
// the settings modal. Called from the IPC OnEvent handler (when the user
// clicked "设置..." in the tray while the GUI was already running). The
// spawn-flag path (--open-settings when no GUI was running) does NOT use
// this — it would race the frontend's EventsOn registration — and instead
// stashes a pendingAction the frontend pulls via PendingAction().
func (a *App) emitOpenSettings() {
	if a.ctx == nil {
		return
	}
	wailsruntime.EventsEmit(a.ctx, "openSettings")
}

// emitConfirmQuit emits a Wails "confirmQuit" event so the frontend shows the
// quit-confirmation modal. Called from the IPC OnEvent handler (when the
// user clicked "退出" in the tray while the GUI was already running and
// hosting is active). The spawn-flag path (--confirm-quit when no GUI was
// running) does NOT use this — it would race the frontend's EventsOn
// registration — and instead stashes a pendingAction the frontend pulls via
// PendingAction(). The frontend's confirm/cancel choice drives a subsequent
// QuitHost IPC call (tear down) or no-op (cancel).
func (a *App) emitConfirmQuit() {
	if a.ctx == nil {
		return
	}
	wailsruntime.EventsEmit(a.ctx, "confirmQuit")
}

// PendingAction returns the one-shot spawn-flag-driven action the frontend
// should perform once its init sequence has run and its EventsOn listeners
// are subscribed, then clears it. Values: "settings" (open the settings
// modal), "confirmQuit" (open the quit-confirmation modal), or "" (no
// pending action — normal launch). The frontend calls this once after its
// initial render; a second call always returns "".
//
// This is the pull-side counterpart to the --open-settings / --confirm-quit
// spawn flags: emitting a Wails event from onDomReady races the frontend's
// listener registration (onDomReady fires before the JS EventsOn calls),
// so the host stashes the intent here and lets the frontend pull it after
// it is ready. The already-running-GUI path still uses emitOpenSettings /
// emitConfirmQuit (IPC event → Wails event) because by then the listeners
// are up.
func (a *App) PendingAction() string {
	v := a.pendingAction
	a.pendingAction = ""
	return v
}

// shutdown is called when the GUI window is closing. It closes the IPC
// connection to the host. The host detects the disconnect and marks the GUI
// as closed (its cmd.Wait goroutine fires); the user can reopen the window
// from the tray.
func (a *App) shutdown(ctx context.Context) {
	if a.ipc != nil {
		_ = a.ipc.Close()
	}
}

// onBeforeClose is the Wails OnBeforeClose hook. In the dual-process design
// the GUI does NOT minimize to tray on close — closing the window quits the
// GUI process (the host keeps running with its tray icon). We always allow
// the close to proceed.
func (a *App) onBeforeClose(ctx context.Context) bool {
	return false
}

// --- Facade methods bound to the frontend ---

// ListDomains returns the current domain list (host-side).
func (a *App) ListDomains() []ipc.DomainDTO {
	if a.ipc == nil {
		return []ipc.DomainDTO{}
	}
	domains, err := a.ipc.ListDomains()
	if err != nil {
		log.Printf("ListDomains: %v", err)
		return []ipc.DomainDTO{}
	}
	return domains
}

// ListForwards returns the current forward list paired with forwarder status.
func (a *App) ListForwards() []ipc.ForwardStatusDTO {
	if a.ipc == nil {
		return []ipc.ForwardStatusDTO{}
	}
	forwards, err := a.ipc.ListForwards()
	if err != nil {
		log.Printf("ListForwards: %v", err)
		return []ipc.ForwardStatusDTO{}
	}
	return forwards
}

// SaveAll applies a full staged domain + forward set with at most ONE
// osascript elevation prompt. Three-phase:
//  1. Host validates the whole set + builds the new hosts content (no
//     elevation). New entries get IDs assigned and returned.
//  2. If the host content changed, the GUI runs the elevated osascript write
//     (foreground dialog → one password prompt for the whole batch). If the
//     content is unchanged, the write is skipped entirely — no prompt.
//  3. Host finalizes: reconciles config to exactly the staged set and syncs
//     forwarders (start effective, stop non-effective).
//
// The returned SaveResultDTO carries the assigned sets on success and an
// error envelope on failure (authCancelled / validationError / otherError).
// The frontend refetches via ListDomains/ListForwards regardless.
func (a *App) SaveAll(payload ipc.SaveAllPayload) ipc.SaveResultDTO {
	if a.ipc == nil {
		return ipc.SaveResultDTO{OtherError: "ipc not connected"}
	}
	// Phase 1: host validates + builds hosts content.
	prep, err := a.ipc.PrepareApplyAll(payload.Domains, payload.Forwards)
	if err != nil {
		if prep.OtherError == "" {
			prep.OtherError = err.Error()
		}
		log.Printf("PrepareApplyAll: %v", err)
		return prepareApplyAllResultToSaveResult(prep)
	}
	// If the host reported an error (validation/other), return it — no
	// elevation attempt.
	if prep.OtherError != "" || prep.ValidationError != "" {
		return prepareApplyAllResultToSaveResult(prep)
	}
	// Phase 2: GUI runs the elevated osascript write ONLY when the hosts
	// content actually changed. When unchanged, skip osascript entirely so
	// there is no password prompt for an internal-only edit.
	if prep.Changed {
		if err := hosts.WriteHostsElevated(prep.HostsContent); err != nil {
			if hosts.IsCancellation(err) {
				return ipc.SaveResultDTO{AuthCancelled: true}
			}
			return ipc.SaveResultDTO{OtherError: "write hosts: " + err.Error()}
		}
	}
	// Phase 3: host finalizes (reconcile config + forwarders).
	res, err := a.ipc.CompleteApplyAll(prep.AssignedDomains, prep.AssignedForwards)
	if err != nil {
		if res.Error == "" {
			res.Error = err.Error()
		}
		log.Printf("CompleteApplyAll: %v", err)
		return ipc.SaveResultDTO{OtherError: res.Error}
	}
	if res.Error != "" {
		return ipc.SaveResultDTO{OtherError: res.Error}
	}
	return ipc.SaveResultDTO{
		SavedDomains:  prep.AssignedDomains,
		SavedForwards: prep.AssignedForwards,
	}
}

// prepareApplyAllResultToSaveResult converts a PrepareApplyAllResult error
// envelope into a SaveResultDTO. The Saved fields are left nil (the frontend
// refetches via ListDomains/ListForwards on success); the error fields carry
// any failure.
func prepareApplyAllResultToSaveResult(p ipc.PrepareApplyAllResult) ipc.SaveResultDTO {
	return ipc.SaveResultDTO{
		ValidationError: p.ValidationError,
		OtherError:      p.OtherError,
	}
}

// Pause suspends all forwarding globally and clears the hosts zone. Two-
// phase: host prepares empty-zone content → GUI writes elevated (only when
// changed) → host completes (Paused=true, stop all). The frontend invokes
// this from the "暂停托管" button; the tray's pause entry routes through the
// same flow via the GUI-spawn / IPC-event path (see host.go onPause).
func (a *App) Pause() ipc.SimpleResult {
	if a.ipc == nil {
		return ipc.SimpleResult{Error: "ipc not connected"}
	}
	prep, err := a.ipc.PreparePause()
	if err != nil {
		if prep.OtherError == "" {
			prep.OtherError = err.Error()
		}
		log.Printf("PreparePause: %v", err)
		return ipc.SimpleResult{Error: prep.OtherError}
	}
	if prep.OtherError != "" {
		return ipc.SimpleResult{Error: prep.OtherError}
	}
	if prep.Changed {
		if err := hosts.WriteHostsElevated(prep.HostsContent); err != nil {
			if hosts.IsCancellation(err) {
				return ipc.SimpleResult{AuthCancelled: true}
			}
			return ipc.SimpleResult{Error: "write hosts: " + err.Error()}
		}
	}
	res, err := a.ipc.CompletePause()
	if err != nil {
		if res.Error == "" {
			res.Error = err.Error()
		}
		log.Printf("CompletePause: %v", err)
	}
	return res
}

// Resume reactivates all forwarding and rebuilds the hosts zone. Two-phase:
// host prepares one-entry-per-domain content → GUI writes elevated (only
// when changed) → host completes (Paused=false, start effective forwards).
func (a *App) Resume() ipc.SimpleResult {
	if a.ipc == nil {
		return ipc.SimpleResult{Error: "ipc not connected"}
	}
	prep, err := a.ipc.PrepareResume()
	if err != nil {
		if prep.OtherError == "" {
			prep.OtherError = err.Error()
		}
		log.Printf("PrepareResume: %v", err)
		return ipc.SimpleResult{Error: prep.OtherError}
	}
	if prep.OtherError != "" {
		return ipc.SimpleResult{Error: prep.OtherError}
	}
	if prep.Changed {
		if err := hosts.WriteHostsElevated(prep.HostsContent); err != nil {
			if hosts.IsCancellation(err) {
				return ipc.SimpleResult{AuthCancelled: true}
			}
			return ipc.SimpleResult{Error: "write hosts: " + err.Error()}
		}
	}
	res, err := a.ipc.CompleteResume()
	if err != nil {
		if res.Error == "" {
			res.Error = err.Error()
		}
		log.Printf("CompleteResume: %v", err)
	}
	return res
}

// GetSettings returns the current application settings (host-side). The
// returned DTO carries the live OS autostart state in AutoStartState (read-
// only; toggling autostart goes through SetAutoStart).
func (a *App) GetSettings() ipc.SettingsDTO {
	if a.ipc == nil {
		return ipc.SettingsDTO{DNSRefreshMinutes: 5}
	}
	s, err := a.ipc.GetSettings()
	if err != nil {
		log.Printf("GetSettings: %v", err)
		return ipc.SettingsDTO{DNSRefreshMinutes: 5}
	}
	return s
}

// SetSettings persists settings (host-side). AutoStartState is read-only and
// ignored by the host; toggling autostart uses SetAutoStart. The frontend
// sends {paused, dnsRefreshMinutes} only.
func (a *App) SetSettings(s ipc.SettingsDTO) ipc.SimpleResult {
	if a.ipc == nil {
		return ipc.SimpleResult{Error: "ipc not connected"}
	}
	res, err := a.ipc.SetSettings(s)
	if err != nil {
		if res.Error == "" {
			res.Error = err.Error()
		}
		log.Printf("SetSettings: %v", err)
	}
	return res
}

// SetAutoStart toggles the OS-native login-item / LaunchAgent / Run-key
// registration. Unlike SetSettings (app preferences), this operates directly
// on the OS autostart registration and reports success/failure
// synchronously. The returned SimpleResult carries OK=true on success or an
// Error string the frontend surfaces via toast.
func (a *App) SetAutoStart(enabled bool) ipc.SimpleResult {
	if a.ipc == nil {
		return ipc.SimpleResult{Error: "ipc not connected"}
	}
	res, err := a.ipc.SetAutoStart(enabled)
	if err != nil {
		if res.Error == "" {
			res.Error = err.Error()
		}
		log.Printf("SetAutoStart: %v", err)
	}
	return res
}

// FixNow rebuilds the hosts zone to match the effective state and reconciles
// the forwarder pool. Two-phase: host prepares → GUI writes elevated (only
// when changed) → host completes. Called from the startup inconsistency
// banner and the settings "清理残留" action.
func (a *App) FixNow() ipc.SimpleResult {
	if a.ipc == nil {
		return ipc.SimpleResult{Error: "ipc not connected"}
	}
	prep, err := a.ipc.PrepareFixNow()
	if err != nil {
		if prep.OtherError == "" {
			prep.OtherError = err.Error()
		}
		log.Printf("PrepareFixNow: %v", err)
		return ipc.SimpleResult{Error: prep.OtherError}
	}
	if prep.OtherError != "" {
		return ipc.SimpleResult{Error: prep.OtherError}
	}
	if prep.Changed {
		if err := hosts.WriteHostsElevated(prep.HostsContent); err != nil {
			if hosts.IsCancellation(err) {
				return ipc.SimpleResult{AuthCancelled: true}
			}
			return ipc.SimpleResult{Error: "write hosts: " + err.Error()}
		}
	}
	res, err := a.ipc.CompleteFixNow()
	if err != nil {
		if res.Error == "" {
			res.Error = err.Error()
		}
		log.Printf("CompleteFixNow: %v", err)
	}
	return res
}

// GetStartupDiff returns the cached startup consistency diff, or nil if the
// hosts file was consistent on startup. The frontend uses this to decide
// whether to show the "Fix now" banner.
func (a *App) GetStartupDiff() *ipc.DiffDTO {
	if a.ipc == nil {
		return nil
	}
	d, err := a.ipc.GetStartupDiff()
	if err != nil {
		log.Printf("GetStartupDiff: %v", err)
		return nil
	}
	return d
}

// QuitHost tears down the host process (kills any running GUI child and
// quits the systray loop). Called by the frontend's quit-confirmation modal
// when the user confirms quitting while hosting is active. The host returns
// SimpleResult{OK:true} and then exits shortly after as the deferred cleanup
// in runHost runs; a connection-closed error on the client side is therefore
// an expected, successful-teardown signal.
func (a *App) QuitHost() ipc.SimpleResult {
	if a.ipc == nil {
		return ipc.SimpleResult{Error: "ipc not connected"}
	}
	res, err := a.ipc.QuitHost()
	if err != nil {
		// The host tore down (or is tearing down) the connection as part
		// of quitting — a connection-closed error here is the expected
		// successful-teardown signal, not a failure. Surface OK so the
		// frontend treats it as success.
		log.Printf("QuitHost: %v (host likely already exiting)", err)
		return ipc.SimpleResult{OK: true}
	}
	return res
}

// CurrentHostsZone returns the IntraFlow marker zone text for the settings
// page preview (host-side).
func (a *App) CurrentHostsZone() string {
	if a.ipc == nil {
		return ""
	}
	zone, err := a.ipc.CurrentHostsZone()
	if err != nil {
		log.Printf("CurrentHostsZone: %v", err)
		return ""
	}
	return zone
}

// CheckPortAvailable reports whether a local port is free (host-side). Used
// by the form on blur to preflight before the user clicks save.
func (a *App) CheckPortAvailable(port int) bool {
	if a.ipc == nil {
		return false
	}
	ok, err := a.ipc.CheckPortAvailable(port)
	if err != nil {
		log.Printf("CheckPortAvailable: %v", err)
		return false
	}
	return ok
}

// SuggestFreePort returns the first free port at or after the given one
// (host-side).
func (a *App) SuggestFreePort(from int) int {
	if a.ipc == nil {
		return from
	}
	p, err := a.ipc.SuggestFreePort(from)
	if err != nil {
		log.Printf("SuggestFreePort: %v", err)
		return from
	}
	return p
}

// IsPrivilegedPort reports whether a port is < 1024 (requires root to bind on
// unix). The frontend uses this to show a warning without branching on magic
// numbers.
func (a *App) IsPrivilegedPort(port int) bool {
	if a.ipc == nil {
		return port > 0 && port < 1024
	}
	ok, err := a.ipc.IsPrivilegedPort(port)
	if err != nil {
		// Fall back to the local computation so the frontend still gets a
		// reasonable answer on a transient IPC error.
		return port > 0 && port < 1024
	}
	return ok
}

// ipcDialTimeout is the time the GUI waits for the host's IPC socket to
// become reachable at launch. The host is normally already running (it
// spawned the GUI), but the socket may not be bound yet if the GUI was
// launched in a narrow race; a retry window covers that.
const ipcDialTimeout = 5 * time.Second
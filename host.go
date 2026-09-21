//go:build !bindings

package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"

	"intraflow/internal/config"
	"intraflow/internal/forwarder"
	"intraflow/internal/hosts"
	"intraflow/internal/ipc"
	"intraflow/internal/orchestrator"
	intraruntime "intraflow/internal/runtime"
)

// runHost is the host-process entry point. The host is a menu-bar-resident
// daemon: it owns the systray icon, the orchestrator, the IPC server, the
// single-instance lock, and autostart. It has no window. The main goroutine
// drives the systray native loop (which blocks); the orchestrator and IPC
// server run in goroutines started before the tray loop.
func runHost() {
	// --- Single-instance lock ---
	// Acquire before anything else so a second host launch exits quickly
	// without touching the hosts file or starting forwarders. The GUI
	// process (intraflow --gui) intentionally does NOT acquire this lock —
	// it is a child of the host.
	releaseLock, err := intraruntime.AcquireLock()
	if err != nil {
		if err == intraruntime.ErrAlreadyRunning {
			log.Println("intraflow: another instance is already running; exiting")
			return
		}
		log.Printf("intraflow: acquire single-instance lock: %v", err)
		// Non-fatal: continue without the lock rather than refusing to start.
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// --- Orchestrator ---
	o, err := orchestrator.New(ctx)
	if err != nil {
		log.Printf("orchestrator: init failed: %v", err)
		// Without the orchestrator the host is useless; release the lock and
		// exit so the user sees a clean failure rather than a silent trayless
		// process.
		if releaseLock != nil {
			releaseLock()
		}
		os.Exit(1)
	}
	diff, err := o.StartupCheck()
	if err != nil {
		log.Printf("orchestrator: startup check failed: %v", err)
	}
	if !diff.Consistent {
		log.Printf("orchestrator: hosts inconsistent on startup: missingInHosts=%v missingInConfig=%v",
			diff.MissingInHosts, diff.MissingInConfig)
	}

	// --- Legacy LaunchAgent cleanup ---
	// The old autostart implementation registered a LaunchAgent at
	// ~/Library/LaunchAgents/com.intraflow.app.plist. The new macOS
	// login-item flow supersedes it; remove the stale agent once at startup
	// so it does not respawn IntraFlow behind the user's back. Non-fatal.
	if err := intraruntime.CleanupLegacyLaunchAgent(); err != nil {
		log.Printf("autostart: cleanup legacy launch agent: %v", err)
	}

	// Shared host state: the cached startup diff (cleared on FixNow), the GUI
	// spawn tracker, and the tray reference used to refresh the menu after
	// mutations.
	hs := &hostState{
		orch:        o,
		startupDiff: &diff,
		gui:         &guiState{},
	}

	// --- Tray ---
	// CreateTray returns the *Tray immediately without blocking; the IPC
	// handlers (and the startup refresh below) can push updates to it. Run
	// blocks the main goroutine on the systray native loop.
	tray := intraruntime.CreateTray(intraruntime.TrayCallbacks{
		OnOpen:         hs.onOpen,
		OnOpenSettings: hs.onOpenSettings,
		OnPause:        hs.onPause,
		OnResume:       hs.onResume,
		OnQuit:         hs.onQuit,
	})
	hs.tray = tray

	// Push the initial state to the tray menu so the icon shows the right
	// counts the moment it appears. Refresh is a no-op until onReady fires,
	// so this is also safe to call again from onReady if needed.
	hs.refreshTray()

	// --- IPC server ---
	// Runs in a goroutine; the host's main goroutine is reserved for
	// systray.Run. IPC handlers call hs.refreshTray() after mutating state so
	// the tray menu stays in sync with the backend.
	ipcServer, err := ipc.ListenAndServe(makeHostHandler(hs))
	if err != nil {
		log.Printf("ipc: listen failed: %v", err)
		// Non-fatal but the GUI won't be able to connect. Continue so the tray
		// still works (the user can quit from the tray).
	} else {
		hs.ipcServer = ipcServer
		defer ipcServer.Close()
	}

	// --- Tray loop (blocks on main goroutine) ---
	tray.Run()

	// --- Teardown ---
	// Reached after systray.Run returns (QuitTray was called). Stop the
	// forwarders, cancel the orchestrator context, release the lock, and
	// exit. The IPC server is closed by the deferred Close above.
	o.StopAll()
	cancel()
	if releaseLock != nil {
		releaseLock()
	}
}

// hostState holds the host process's mutable shared state: the orchestrator,
// the cached startup diff, the tray reference, and the GUI spawn tracker. It
// is touched from the systray menu-click goroutines and the IPC handler
// goroutines.
type hostState struct {
	orch *orchestrator.Orchestrator
	tray *intraruntime.Tray

	// ipcServer is the running IPC server, used to broadcast push events
	// (recordsChanged / openSettings / elevatePause / elevateResume) to
	// connected GUI clients. May be nil if the server failed to start;
	// BroadcastEvent callers guard nil.
	ipcServer *ipc.Server

	// startupDiffMu guards startupDiff, which is written by FixNow (clearing
	// it) and read by the GetStartupDiff IPC method.
	startupDiffMu sync.RWMutex
	startupDiff   *hosts.Diff

	gui *guiState
}

// guiState tracks whether a GUI child process is currently running so the
// host does not spawn a second one when "打开主界面" is clicked while the
// window is already open. The cmd is stored so onQuit can kill the GUI when
// the user quits the host from the tray while the panel is open.
//
// Note: the elevate-spawn path (--elevate=pause|resume) intentionally spawns
// a GUI process even when a normal GUI is NOT running, and that process is
// also tracked here so onQuit can kill it. The single-instance model on the
// GUI side is not enforced (the GUI does not acquire the lock), but a normal
// GUI and an elevate-spawn GUI do not coexist because the elevate path is
// only taken when no GUI is running.
type guiState struct {
	mu      sync.Mutex
	running bool
	cmd     *exec.Cmd
}

// onOpen is the tray "打开主界面" handler. If no GUI is running it spawns one
// (whose window shows itself normally). If a GUI is already running, it
// broadcasts a focusWindow IPC event so the existing window is raised and
// focused instead of silently doing nothing — the host cannot touch another
// process's window directly, so the running GUI performs the raise itself.
func (h *hostState) onOpen() {
	h.gui.mu.Lock()
	running := h.gui.running
	h.gui.mu.Unlock()
	if running {
		if h.ipcServer != nil {
			if err := h.ipcServer.BroadcastEvent(ipc.EventFocusWindow, nil); err != nil {
				log.Printf("host: broadcast focusWindow: %v", err)
			}
		}
		return
	}
	h.spawnGUI()
}

// onOpenSettings is the tray "设置..." handler. If a GUI is already running,
// it broadcasts an "openSettings" push event over IPC; the GUI emits a Wails
// "open-settings" event so the frontend opens the settings modal directly.
// If no GUI is running, it spawns one with the --open-settings flag so the
// GUI emits the open-settings event itself once the frontend is ready.
func (h *hostState) onOpenSettings() {
	h.gui.mu.Lock()
	running := h.gui.running
	h.gui.mu.Unlock()
	if running {
		if h.ipcServer != nil {
			if err := h.ipcServer.BroadcastEvent(ipc.EventOpenSettings, nil); err != nil {
				log.Printf("host: broadcast openSettings: %v", err)
			}
		}
		return
	}
	h.spawnGUIWithFlags([]string{openSettingsFlag})
}

// broadcastRecordsChanged notifies any connected GUI that the backend domain
// / forward set or statuses changed, so an open panel refreshes in real
// time. It is a no-op when no IPC server is up (e.g. listen failed) or no
// GUI is connected. Best-effort: a write error to one GUI is logged and
// skipped.
func (h *hostState) broadcastRecordsChanged() {
	if h.ipcServer == nil {
		return
	}
	if err := h.ipcServer.BroadcastEvent(ipc.EventRecordsChanged, nil); err != nil {
		log.Printf("host: broadcast recordsChanged: %v", err)
	}
}

// onQuit is the tray "退出" handler. It gates teardown on whether hosting is
// active:
//
//   - If settings.Paused is true OR the config has zero domains AND zero
//     forwards, there is nothing to lose — tear down directly (kill any
//     running GUI child, then quit the systray loop).
//   - Otherwise hosting is active (forwards are running and the hosts zone
//     points public-tunnel domains at the LAN). Quitting would silently break
//     that. Instead, route through the GUI's confirmation modal:
//     - If a GUI is already running: broadcast an EventConfirmQuit IPC event
//       so the running GUI re-emits a Wails "confirmQuit" event and the
//       frontend shows the modal.
//     - If no GUI is running: spawn a short-lived GUI with --confirm-quit so
//       it emits the same event from onDomReady and shows the modal on a
//       freshly opened window.
//     In both cases the host does NOT quit yet — the user's choice in the
//     frontend modal drives a subsequent QuitHost IPC call (tear down) or a
//     cancel no-op.
func (h *hostState) onQuit() {
	if h.shouldQuitDirectly() {
		h.teardownAndQuit()
		return
	}
	// Hosting is active: route through the GUI's confirmation modal.
	h.gui.mu.Lock()
	running := h.gui.running
	h.gui.mu.Unlock()
	if running {
		if h.ipcServer != nil {
			if err := h.ipcServer.BroadcastEvent(ipc.EventConfirmQuit, nil); err != nil {
				log.Printf("host: broadcast confirmQuit: %v", err)
			}
		}
		return
	}
	h.spawnGUIWithFlags([]string{confirmQuitFlag})
}

// shouldQuitDirectly reports whether the tray's 退出 click can bypass the
// confirmation modal: when hosting is paused or there are no domains and no
// forwards configured, quitting tears down nothing the user would miss.
func (h *hostState) shouldQuitDirectly() bool {
	if h.orch == nil {
		return true
	}
	settings := h.orch.GetSettings()
	if settings.Paused {
		return true
	}
	return len(h.orch.ListDomains()) == 0 && len(h.orch.ListForwards()) == 0
}

// teardownAndQuit performs the host teardown sequence: kill any running GUI
// child process (so the panel does not outlive the host), then quit the
// systray loop so runHost's main goroutine returns and runs the deferred
// cleanup (stop forwarders, close IPC server, release lock). It is the shared
// teardown path for both the gated direct-quit tray branch and the QuitHost
// IPC handler triggered by the frontend's confirmation modal.
func (h *hostState) teardownAndQuit() {
	// Kill the GUI child if it's running. Otherwise the panel window stays
	// open after the host exits, leaving an orphaned webview.
	h.gui.mu.Lock()
	if h.gui.running && h.gui.cmd != nil && h.gui.cmd.Process != nil {
		_ = h.gui.cmd.Process.Kill()
	}
	h.gui.mu.Unlock()
	intraruntime.QuitTray()
}

// onPause is the tray "暂停托管" handler. It triggers the elevated pause
// flow (PreparePause → GUI writes hosts → CompletePause). Because the host
// must never call osascript, the elevated write is routed through the GUI
// process via one of two paths:
//
//  1. A normal GUI is already running: broadcast an elevatePause IPC event.
//     The running GUI's OnEvent handler calls its own App.Pause() facade
//     method, which runs the two-phase flow and the host's CompletePause
//     handler refreshes the tray + broadcasts recordsChanged when done. No
//     new process is spawned and no window flashes.
//
//  2. No GUI is running: spawn a short-lived GUI with --elevate=pause. That
//     process dials IPC, runs App.Pause() from onDomReady (Prepare →
//     WriteHostsElevated → Complete), then quits. The spawned window is
//     created with StartHidden=true (see gui.go) so the tray click does not
//     flash a panel; Wails still activates the app as a Regular actor, so the
//     osascript admin dialog still surfaces.
//
// In both paths the actual hosts write happens inside the GUI process, so the
// osascript admin dialog attaches to a foreground windowed app as required.
// After CompletePause the host refreshes the tray so the menu label flips to
// "恢复托管".
func (h *hostState) onPause() {
	if h.orch == nil {
		return
	}
	h.runElevatedFromTray(ipc.EventElevatePause, "pause")
}

// onResume is the tray "恢复托管" handler. It mirrors onPause for the resume
// flow (PrepareResume → GUI writes hosts → CompleteResume). See onPause for
// the two dispatch paths (elevateResume event vs. --elevate=resume spawn).
func (h *hostState) onResume() {
	if h.orch == nil {
		return
	}
	h.runElevatedFromTray(ipc.EventElevateResume, "resume")
}

// runElevatedFromTray is the shared pause/resume dispatch. If a GUI is
// already running, it broadcasts the corresponding IPC event so the running
// GUI runs the two-phase flow itself. Otherwise it spawns a short-lived GUI
// with --elevate=<action> that runs the flow and exits.
func (h *hostState) runElevatedFromTray(event, action string) {
	h.gui.mu.Lock()
	running := h.gui.running
	h.gui.mu.Unlock()
	if running {
		if h.ipcServer != nil {
			if err := h.ipcServer.BroadcastEvent(event, nil); err != nil {
				log.Printf("host: broadcast %s: %v", event, err)
			}
		}
		return
	}
	h.spawnGUIWithFlags([]string{elevateFlag + "=" + action})
}

// spawnGUI launches the GUI child process (intraflow --gui) if one is not
// already running. It tracks the running state so repeated "打开主界面"
// clicks do not spawn multiple windows. A goroutine waits for the child to
// exit, clears the running flag, and refreshes the tray.
func (h *hostState) spawnGUI() {
	h.spawnGUIWithFlags(nil)
}

// spawnGUIWithFlags is the shared spawn logic. extraArgs are appended after
// "--gui". The running-guard is taken before resolving the executable so
// concurrent clicks serialize on h.gui.mu.
func (h *hostState) spawnGUIWithFlags(extraArgs []string) {
	h.gui.mu.Lock()
	if h.gui.running {
		h.gui.mu.Unlock()
		return
	}
	h.gui.running = true
	h.gui.mu.Unlock()

	exe, err := os.Executable()
	if err != nil {
		log.Printf("host: spawn GUI: resolve executable: %v", err)
		h.gui.mu.Lock()
		h.gui.running = false
		h.gui.mu.Unlock()
		return
	}
	args := append([]string{"--gui"}, extraArgs...)
	cmd := exec.Command(exe, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		log.Printf("host: spawn GUI: %v", err)
		h.gui.mu.Lock()
		h.gui.running = false
		h.gui.mu.Unlock()
		return
	}
	// Store the cmd so onQuit can kill the GUI if the user quits the host
	// while the panel is open.
	h.gui.mu.Lock()
	h.gui.cmd = cmd
	h.gui.mu.Unlock()
	go func() {
		_ = cmd.Wait()
		h.gui.mu.Lock()
		h.gui.running = false
		h.gui.cmd = nil
		h.gui.mu.Unlock()
		// The GUI closed; refresh the tray in case state changed while it
		// was open (the elevate path finalizes via Complete handlers, which
		// already refresh, but this keeps the menu honest in all cases).
		h.refreshTray()
	}()
}

// refreshTray pushes the current orchestrator state (paused flag + domain
// count + listening-forward count) to the tray menu in a single rebuild. It
// is a no-op before the tray's onReady has fired (Refresh guards against a
// not-ready menu) and when the tray is nil.
func (h *hostState) refreshTray() {
	if h.tray == nil || h.orch == nil {
		return
	}
	settings := h.orch.GetSettings()
	domains := h.orch.ListDomains()
	statuses := h.orch.Statuses()

	// Count effective listening forwards. When paused, the pool is empty
	// (CompletePause stops all), so the count is naturally 0; we still
	// compute it from the pool so a stale pool state is reflected honestly.
	listening := 0
	for _, st := range statuses {
		if st.State == forwarder.StatusListening {
			listening++
		}
	}

	h.tray.Refresh(intraruntime.TrayState{
		Paused:            settings.Paused,
		DomainCount:       len(domains),
		ListeningForwards: listening,
	})
}

// autostartStateString returns the live OS autostart state as the string
// value the IPC SettingsDTO.AutoStartState field carries ("enabled" /
// "disabled" / "requires_approval"). On lookup error it logs and falls back
// to "disabled" so the frontend shows a safe default.
func autostartStateString() string {
	state, err := intraruntime.GetAutoStartState()
	if err != nil {
		log.Printf("autostart: get state: %v", err)
		return string(intraruntime.AutoStartStateDisabled)
	}
	return string(state)
}

// getStartupDiff returns the cached startup diff (or nil if the hosts state
// is consistent). Guarded so FixNow and GetStartupDiff don't race.
func (h *hostState) getStartupDiff() *hosts.Diff {
	h.startupDiffMu.RLock()
	defer h.startupDiffMu.RUnlock()
	return h.startupDiff
}

// setStartupDiff replaces the cached startup diff. Pass nil to clear it
// (after FixNow resolves an inconsistency).
func (h *hostState) setStartupDiff(d *hosts.Diff) {
	h.startupDiffMu.Lock()
	defer h.startupDiffMu.Unlock()
	h.startupDiff = d
}

// --- IPC handler factory ---

// makeHostHandler returns an ipc.Handler that dispatches RPC method names to
// orchestrator calls. It is the bridge between the GUI process (over the
// socket) and the host's orchestrator. Each method produces the DTO shape the
// frontend expects (see the ipc package and the des-1 frontend contract).
//
// After any Complete* method (CompleteApplyAll, CompletePause, CompleteResume
// , CompleteFixNow), the tray menu is refreshed and a recordsChanged event
// is broadcast so connected GUIs refetch.
func makeHostHandler(h *hostState) ipc.Handler {
	o := h.orch
	return func(method string, params json.RawMessage) (interface{}, error) {
		switch method {
		// --- Reads ---

		case ipc.MethodListDomains:
			domains := o.ListDomains()
			out := make([]ipc.DomainDTO, len(domains))
			for i, d := range domains {
				out[i] = toIPCDomainDTO(d)
			}
			return out, nil

		case ipc.MethodListForwards:
			forwards := o.ListForwards()
			statuses := o.Statuses()
			out := make([]ipc.ForwardStatusDTO, 0, len(forwards))
			for _, f := range forwards {
				st := statuses[f.ID]
				out = append(out, ipc.ForwardStatusDTO{
					Forward: toIPCForwardDTO(f),
					Status:  toIPCStatusInfoDTO(st),
				})
			}
			return out, nil

		// --- Settings + autostart ---

		case ipc.MethodGetSettings:
			s := o.GetSettings()
			return ipc.SettingsDTO{
				Paused:            s.Paused,
				DNSRefreshMinutes: s.DNSRefreshMinutes,
				AutoStartState:    autostartStateString(),
			}, nil

		case ipc.MethodSetSettings:
			var p ipc.SetSettingsParams
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, err
			}
			// AutoStartState is read-only; ignore any value the client sent.
			// Only Paused + DNSRefreshMinutes are persisted. Paused is
			// normally flipped via Pause/Resume (which also rewrite the
			// hosts zone); SetSettings is for the DNS refresh interval.
			// However, to keep SetSettings semantics coherent we honor an
			// explicit Paused value too — the host-side orchestrator clamps
			// DNSRefreshMinutes and persists.
			if err := o.SetSettings(fromIPCSettingsDTO(p.Settings)); err != nil {
				return ipc.SimpleResult{Error: err.Error()}, nil
			}
			h.refreshTray()
			return ipc.SimpleResult{OK: true}, nil

		case ipc.MethodSetAutoStart:
			var p ipc.SetAutoStartParams
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, err
			}
			if err := intraruntime.SetAutoStart(p.Enabled); err != nil {
				log.Printf("autostart: set (enabled=%v): %v", p.Enabled, err)
				return ipc.SimpleResult{Error: err.Error()}, nil
			}
			h.refreshTray()
			return ipc.SimpleResult{OK: true}, nil

		// --- Diagnostics ---

		case ipc.MethodGetStartupDiff:
			d := h.getStartupDiff()
			if d == nil {
				return (*ipc.DiffDTO)(nil), nil
			}
			return &ipc.DiffDTO{
				MissingInHosts:  d.MissingInHosts,
				MissingInConfig: d.MissingInConfig,
				Consistent:      d.Consistent,
			}, nil

		case ipc.MethodCurrentHostsZone:
			zone, err := o.CurrentHostsZone()
			if err != nil {
				log.Printf("CurrentHostsZone: %v", err)
				return "", nil
			}
			return zone, nil

		case ipc.MethodCheckPortAvailable:
			var p ipc.CheckPortAvailableParams
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, err
			}
			return forwarder.IsPortAvailable(p.Port), nil

		case ipc.MethodSuggestFreePort:
			var p ipc.SuggestFreePortParams
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, err
			}
			return forwarder.SuggestFreePort(p.From), nil

		case ipc.MethodIsPrivilegedPort:
			var p ipc.IsPrivilegedPortParams
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, err
			}
			return p.Port > 0 && p.Port < 1024, nil

		// --- Two-phase hosts-write methods (GUI-driven elevation) ---
		// The GUI calls Prepare* to get the hosts content (no elevation on
		// the host), runs osascript itself (foreground process → dialog
		// works), then calls Complete* to finalize. The host never calls
		// osascript — it's a background menu-bar daemon.

		case ipc.MethodPrepareApplyAll:
			var p ipc.SaveAllParams
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, err
			}
			domains := make([]config.Domain, len(p.Domains))
			for i, d := range p.Domains {
				domains[i] = fromIPCDomainDTO(d)
			}
			forwards := make([]config.Forward, len(p.Forwards))
			for i, f := range p.Forwards {
				forwards[i] = fromIPCForwardDTO(f)
			}
			content, changed, assignedDomains, assignedForwards, err := o.PrepareApplyAll(domains, forwards)
			if err != nil {
				return prepareApplyAllResultFromError(err), nil
			}
			assignedDomainDTOs := make([]ipc.DomainDTO, len(assignedDomains))
			for i, d := range assignedDomains {
				assignedDomainDTOs[i] = toIPCDomainDTO(d)
			}
			assignedForwardDTOs := make([]ipc.ForwardDTO, len(assignedForwards))
			for i, f := range assignedForwards {
				assignedForwardDTOs[i] = toIPCForwardDTO(f)
			}
			return ipc.PrepareApplyAllResult{
				HostsContent:     content,
				Changed:          changed,
				AssignedDomains:  assignedDomainDTOs,
				AssignedForwards: assignedForwardDTOs,
			}, nil

		case ipc.MethodCompleteApplyAll:
			var p ipc.SaveAllParams
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, err
			}
			domains := make([]config.Domain, len(p.Domains))
			for i, d := range p.Domains {
				domains[i] = fromIPCDomainDTO(d)
			}
			forwards := make([]config.Forward, len(p.Forwards))
			for i, f := range p.Forwards {
				forwards[i] = fromIPCForwardDTO(f)
			}
			if err := o.CompleteApplyAll(domains, forwards); err != nil {
				return ipc.SimpleResult{Error: err.Error()}, nil
			}
			h.refreshTray()
			h.broadcastRecordsChanged()
			return ipc.SimpleResult{OK: true}, nil

		case ipc.MethodPreparePause:
			content, changed, err := o.PreparePause()
			if err != nil {
				return ipc.PrepareResult{OtherError: err.Error()}, nil
			}
			return ipc.PrepareResult{HostsContent: content, Changed: changed}, nil

		case ipc.MethodCompletePause:
			if err := o.CompletePause(); err != nil {
				return ipc.SimpleResult{Error: err.Error()}, nil
			}
			h.refreshTray()
			h.broadcastRecordsChanged()
			return ipc.SimpleResult{OK: true}, nil

		case ipc.MethodPrepareResume:
			content, changed, err := o.PrepareResume()
			if err != nil {
				return ipc.PrepareResult{OtherError: err.Error()}, nil
			}
			return ipc.PrepareResult{HostsContent: content, Changed: changed}, nil

		case ipc.MethodCompleteResume:
			if err := o.CompleteResume(); err != nil {
				return ipc.SimpleResult{Error: err.Error()}, nil
			}
			// Resume may resolve a startup inconsistency (the zone is
			// rebuilt to match config); clear the cached diff so the
			// banner doesn't linger.
			h.setStartupDiff(nil)
			h.refreshTray()
			h.broadcastRecordsChanged()
			return ipc.SimpleResult{OK: true}, nil

		case ipc.MethodPrepareFixNow:
			content, changed, err := o.PrepareFixNow()
			if err != nil {
				return ipc.PrepareResult{OtherError: err.Error()}, nil
			}
			return ipc.PrepareResult{HostsContent: content, Changed: changed}, nil

		case ipc.MethodCompleteFixNow:
			if err := o.CompleteFixNow(); err != nil {
				return ipc.SimpleResult{Error: err.Error()}, nil
			}
			h.setStartupDiff(nil)
			h.refreshTray()
			h.broadcastRecordsChanged()
			return ipc.SimpleResult{OK: true}, nil

		// --- Quit ---
		// QuitHost is invoked by the frontend's quit-confirmation modal
		// after the user confirms quitting while hosting is active. It
		// performs the same teardown as the gated direct-quit tray branch
		// (kill GUI child, quit systray loop) and returns OK before the
		// host process exits. The deferred cleanup in runHost then runs
		// (stop forwarders, close IPC, release lock).
		case ipc.MethodQuitHost:
			go h.teardownAndQuit()
			return ipc.SimpleResult{OK: true}, nil
		}
		return nil, errors.New("unknown method: " + method)
	}
}

// prepareApplyAllResultFromError translates an orchestrator error from
// PrepareApplyAll into a PrepareApplyAllResult error envelope (no hosts
// content). Validation errors from config.ValidateDomains / ValidateForwards
// are surfaced as ValidationError (matched by message substring); all other
// failures as OtherError. There is no separate port-error envelope in the
// split model — port range / duplicate-port failures are validation errors.
func prepareApplyAllResultFromError(err error) ipc.PrepareApplyAllResult {
	if isValidationError(err) {
		return ipc.PrepareApplyAllResult{ValidationError: err.Error()}
	}
	return ipc.PrepareApplyAllResult{OtherError: err.Error()}
}

// --- DTO helpers ---
// Convert between the orchestrator's config types and the IPC DTOs. The IPC
// package's DTOs use the same JSON field names as the frontend contract.

func toIPCDomainDTO(d config.Domain) ipc.DomainDTO {
	return ipc.DomainDTO{
		ID:     d.ID,
		Domain: d.Domain,
	}
}

func fromIPCDomainDTO(d ipc.DomainDTO) config.Domain {
	return config.Domain{
		ID:     d.ID,
		Domain: d.Domain,
	}
}

func toIPCForwardDTO(f config.Forward) ipc.ForwardDTO {
	return ipc.ForwardDTO{
		ID:         f.ID,
		ListenPort: f.ListenPort,
		TargetHost: f.TargetHost,
		TargetPort: f.TargetPort,
		Enabled:    f.Enabled,
	}
}

func fromIPCForwardDTO(d ipc.ForwardDTO) config.Forward {
	return config.Forward{
		ID:         d.ID,
		ListenPort: d.ListenPort,
		TargetHost: d.TargetHost,
		TargetPort: d.TargetPort,
		Enabled:    d.Enabled,
	}
}

func toIPCStatusInfoDTO(st forwarder.StatusInfo) ipc.StatusInfoDTO {
	return ipc.StatusInfoDTO{
		State:      st.State.String(),
		Reason:     st.Reason,
		ResolvedIP: st.ResolvedIP,
	}
}

func fromIPCSettingsDTO(d ipc.SettingsDTO) config.Settings {
	return config.Settings{
		Paused:            d.Paused,
		DNSRefreshMinutes: d.DNSRefreshMinutes,
	}
}

// isValidationError returns true for errors produced by
// config.ValidateDomains / config.ValidateForwards (and config.ValidatePort,
// which those wrappers invoke). The orchestrator returns those errors
// unwrapped, so we match on the known message substrings the validators emit
// ("must not be empty", "duplicate", "port must be between"). This matches
// the spec's guidance: validation errors flow from config.Validate* as plain
// errors — match them by message substring for the DTO's validationError
// field.
func isValidationError(err error) bool {
	if errors.Is(err, config.ErrInvalidPort) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "must not be empty") ||
		strings.Contains(msg, "duplicate") ||
		strings.Contains(msg, "port must be between")
}
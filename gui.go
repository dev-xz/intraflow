//go:build !bindings

package main

import (
	"embed"
	"encoding/json"
	"log"
	"os"
	"strings"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"

	"intraflow/internal/ipc"
)

//go:embed all:frontend/dist
var assets embed.FS

// openSettingsFlag is the CLI flag the host appends when spawning the GUI from
// the tray's "设置..." entry while no GUI is running. The GUI detects it in
// os.Args and emits an "open-settings" Wails event to the frontend once the
// DOM is ready, so the settings modal opens directly on launch.
const openSettingsFlag = "--open-settings"

// elevateFlag is the CLI flag prefix the host appends when spawning a
// short-lived GUI to perform an elevated hosts write on behalf of the tray
// (the host must never call osascript). The full flag is "--elevate=pause" or
// "--elevate=resume". The spawned GUI dials IPC, runs the corresponding
// two-phase flow (Prepare → WriteHostsElevated → Complete) from onDomReady,
// and then quits so no window lingers. A brief window flash on this path is
// acceptable and documented in host.go's onPause/onResume.
const elevateFlag = "--elevate"

// confirmQuitFlag is the CLI flag the host appends when spawning a GUI from
// the tray's "退出" entry while no GUI is running and hosting is active
// (forwards listening + hosts zone populated). The spawned GUI detects it in
// os.Args and emits a "confirmQuit" Wails event to the frontend on DOM ready
// so the confirmation modal opens directly on launch. The user's choice
// drives a subsequent QuitHost IPC call (tear down) or no-op (cancel).
const confirmQuitFlag = "--confirm-quit"

// runGUI is the GUI-process entry point (intraflow --gui). It connects to the
// host over IPC and runs Wails with the existing frontend. The GUI does NOT
// acquire the single-instance lock (the host owns it) and does NOT set up a
// tray (the host owns the tray icon). Closing the window quits the GUI
// process; the host detects the IPC disconnect and marks the GUI as closed,
// so the user can reopen it from the tray.
//
// The GUI is a normal windowed app: it shows in the Dock while running and
// disappears when closed. No activation-policy toggling is needed (that was a
// single-process workaround for hiding the Dock icon while the window was
// minimized to the tray).
//
// Spawn flags consumed here:
//   - --open-settings: stash pendingAction="settings" so the frontend opens
//     the settings modal on launch. The intent is pulled via the
//     PendingAction() binding after the frontend's init sequence (emitting a
//     Wails event from onDomReady would race the frontend's EventsOn
//     registration).
//   - --elevate=pause | --elevate=resume: run the two-phase pause/resume flow
//     on DOM ready and then quit (the host spawned us specifically because no
//     normal GUI was running and the tray click needs an elevated hosts
//     write that the host cannot perform itself). This path is pure Go (no
//     JS involvement) so it does not race.
//   - --confirm-quit: stash pendingAction="confirmQuit" so the frontend opens
//     the quit-confirmation modal on launch (same pull model as
//     --open-settings).
func runGUI() {
	// Detect spawn flags. They are consumed here (removed from os.Args
	// would be racy with Wails' own arg parsing, so we just remember them).
	pendingAction := ""
	elevateAction := ""
	for _, a := range os.Args[1:] {
		if a == openSettingsFlag {
			pendingAction = "settings"
			continue
		}
		if a == confirmQuitFlag {
			pendingAction = "confirmQuit"
			continue
		}
		if strings.HasPrefix(a, elevateFlag+"=") {
			elevateAction = strings.TrimPrefix(a, elevateFlag+"=")
			continue
		}
	}

	// --- IPC client ---
	// Dial the host. The host spawned us, so its socket should already be
	// listening; the retry window covers a narrow race at startup.
	ipcClient, err := ipc.Dial(ipcDialTimeout)
	if err != nil {
		// Without a host connection the GUI is useless (every facade method
		// would return empty/error). Log and exit so the user sees the window
		// flash and the host's tray remains the recovery path.
		log.Printf("gui: dial host IPC: %v", err)
		return
	}

	app := NewApp(ipcClient)
	app.pendingAction = pendingAction
	app.elevateOnReady = elevateAction

	// Subscribe to host push events:
	//   - recordsChanged → re-emit a Wails event so the open panel refreshes.
	//   - openSettings → re-emit a Wails event so the frontend opens the
	//     settings modal (tray "设置..." while GUI already running).
	//   - elevatePause / elevateResume → run the corresponding two-phase
	//     flow on the already-running GUI (the tray click needs an elevated
	//     hosts write and the host cannot call osascript). This path is the
	//     no-spawn counterpart to the --elevate spawn flag.
	//   - confirmQuit → re-emit a Wails "confirmQuit" event so the frontend
	//     shows the quit-confirmation modal (tray "退出" while GUI already
	//     running and hosting is active). This path is the no-spawn
	//     counterpart to the --confirm-quit spawn flag.
	ipcClient.OnEvent(func(name string, data json.RawMessage) {
		switch name {
		case ipc.EventRecordsChanged:
			app.emitRecordsChanged()
		case ipc.EventOpenSettings:
			app.emitOpenSettings()
		case ipc.EventElevatePause:
			go func() {
				_ = app.Pause()
			}()
		case ipc.EventElevateResume:
			go func() {
				_ = app.Resume()
			}()
		case ipc.EventConfirmQuit:
			app.emitConfirmQuit()
		}
	})

	// --- Wails ---
	// Runs on the main goroutine and blocks until the window is closed (or
	// runtime.Quit is called). OnBeforeClose always returns false (allow
	// close), so closing the window quits the GUI process; OnShutdown closes
	// the IPC client. The host's cmd.Wait goroutine then fires and clears
	// the GUI-running flag.
	err = wails.Run(&options.App{
		Title:  "intraflow",
		Width:  1024,
		Height: 768,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		BackgroundColour: &options.RGBA{R: 246, G: 246, B: 246, A: 1},
		OnStartup:        app.startup,
		OnDomReady:       app.onDomReady,
		OnShutdown:       app.shutdown,
		OnBeforeClose:    app.onBeforeClose,
		Bind: []interface{}{
			app,
		},
	})
	if err != nil {
		log.Println("gui: wails error:", err.Error())
	}
}
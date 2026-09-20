//go:build !bindings

package main

import (
	"os"
)

// main is the dispatch entry point for both IntraFlow processes.
//
//   - No arguments: the host process. Owns the systray menu-bar icon, the
//     orchestrator (config + hosts + forwarders), the IPC server, the single-
//     instance lock, and OS-native autostart. It has no Wails/webview. The
//     host's main goroutine drives the systray native loop (systray.Run),
//     which blocks until QuitTray; the orchestrator and IPC server run in
//     goroutines.
//   - "--gui": the GUI process. Spawned on demand by the host when the user
//     clicks "打开主界面" in the tray. Runs Wails with the existing frontend
//     and connects to the host over IPC. Closing the window quits the GUI
//     process; the host keeps running and the user can reopen the GUI from
//     the tray.
//
// See host.go (runHost) and gui.go (runGUI) for the per-process logic.
func main() {
	if len(os.Args) > 1 && os.Args[1] == "--gui" {
		runGUI()
		return
	}
	runHost()
}
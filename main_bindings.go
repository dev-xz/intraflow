//go:build bindings

// Binding-generation entry point. Wails' build process compiles the project
// with the `bindings` tag and runs the resulting binary to introspect the
// bound methods and emit frontend/wailsjs/*. The full runtime bootstrap
// (single-instance lock, system tray, wails.Run) must NOT execute in this
// mode — it would acquire the instance lock and block forever, stalling the
// build. This minimal main just constructs the App (so its bound methods are
// discoverable) and hands control to Wails' binding-extraction App.Run.
package main

import (
	"embed"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"

	"intraflow/internal/ipc"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	// In bindings mode the App is constructed with a nil IPC client. Wails'
	// binding extractor inspects only the App's method signatures, not
	// runtime values, so the methods are never called here — the nil is safe.
	app := NewApp((*ipc.Client)(nil))
	// Wails' bindings-mode App.Run (internal/app/app_bindings.go) intercepts
	// this and emits the JS bindings, then exits. It does not start a window,
	// event loop, or any of our runtime layer.
	_ = wails.Run(&options.App{
		Title:  "intraflow",
		Width:  1024,
		Height: 768,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		Bind: []interface{}{
			app,
		},
	})
}
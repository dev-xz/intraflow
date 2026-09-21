package runtime

import _ "embed"

// Tray icon assets.
//
// On macOS the menu-bar icon must be a TEMPLATE image: a pure-black glyph with
// an alpha channel that macOS re-tints for light and dark menu bars. The
// hosted/paused pair below encodes the two hosting states (see tray.go).
//
// Windows and Linux have no template-image concept, so systray falls back to
// its `regularIconBytes` argument there; we hand it the full-colour app icon so
// the tray stays visible on those platforms.
var (
	//go:embed icon.png
	colorIcon []byte

	//go:embed tray-hosted.png
	trayHostedIcon []byte

	//go:embed tray-paused.png
	trayPausedIcon []byte
)

// trayIcon returns the (template, regular) icon byte pair for a hosting state.
//
// macOS uses the template bytes (monochrome, alpha-only); Windows/Linux ignore
// them and use the colour bytes. Passing both keeps the platform conditional
// out of the caller.
func trayIcon(paused bool) (template, regular []byte) {
	if paused {
		return trayPausedIcon, colorIcon
	}
	return trayHostedIcon, colorIcon
}

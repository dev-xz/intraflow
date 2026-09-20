package runtime

import _ "embed"

//go:embed icon.png
var iconBytes []byte

// embeddedIcon returns the embedded PNG bytes for the menu-bar icon.
func embeddedIcon() ([]byte, error) {
	return iconBytes, nil
}
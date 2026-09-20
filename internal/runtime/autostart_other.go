//go:build !darwin

package runtime

import "errors"

// Non-darwin stubs for the darwin dispatchers referenced by the
// platform-agnostic switch in autostart.go. These are never reached on
// non-darwin platforms (the switch in GetAutoStartState/SetAutoStart/
// CleanupLegacyLaunchAgent routes to the windows/linux implementations
// instead), but Go requires every referenced symbol to be defined at compile
// time, so we provide trivial unreachable stubs that return an error.

func getAutoStartStateDarwin() (AutoStartState, error) {
	return AutoStartStateDisabled, errors.New("autostart: darwin path reached on non-darwin (unreachable)")
}

func setAutoStartDarwin(bool) error {
	return errors.New("autostart: darwin path reached on non-darwin (unreachable)")
}

func cleanupLegacyLaunchAgentDarwin() error {
	return errors.New("autostart: darwin path reached on non-darwin (unreachable)")
}
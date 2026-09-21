//go:build !darwin

package runtime

// RegisterReopenHandler is a no-op on non-darwin platforms. The
// "relaunch shows the panel" behaviour it provides is specific to macOS
// LaunchServices; on Windows and Linux a second launch of an already-running
// app is handled by the single-instance lock instead.
func RegisterReopenHandler(onReopen func()) {}

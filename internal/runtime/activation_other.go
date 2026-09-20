//go:build !darwin

package runtime

// SetHostAccessoryPolicy is a no-op on non-darwin platforms.
func SetHostAccessoryPolicy() {}
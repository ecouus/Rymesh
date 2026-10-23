//go:build !darwin

package sys

// platformBootID is only needed where /proc has no boot_id.
func platformBootID() string { return "" }

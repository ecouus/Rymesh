//go:build !darwin

package main

// platformBootID is only needed where /proc has no boot_id.
func platformBootID() string { return "" }

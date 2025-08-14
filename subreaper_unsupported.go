//go:build !linux

package main

// On non-linux systems, this is a no-op.
func setSubreaper() {
	// The concept of a subreaper is specific to Linux.
	// On other systems, we do nothing. The C code has a similar conditional compilation logic.
}

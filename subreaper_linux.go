//go:build linux

package main

import (
	"fmt"
	"golang.org/x/sys/unix"
)

func setSubreaper() {
	// Attempt to become a subreaper.
	err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0)
	if err != nil {
		if verbose {
			fmt.Println("multirun: failed to register as subreaper, subchildren exit status might be ignored.")
		}
	} else {
		if verbose {
			fmt.Println("multirun: successfully registered as subreaper.")
		}
	}
}

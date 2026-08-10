//go:build !windows

// Command swe-explore-dashboard is a native Win32 development surface. Other
// platforms retain the cross-platform benchmark CLI and artifact schemas.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "swe-explore-dashboard is available only on Windows")
	os.Exit(1)
}

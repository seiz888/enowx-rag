//go:build !windows && !linux

package main

import (
	"fmt"
	"os"
)

// The collector needs a local transport the operating system puts an access
// control on and a key store the operating system holds; see
// pkg/memgw/collector. Windows and Linux have both. On other platforms the
// command exists so the failure is a sentence rather than an unknown
// subcommand.
func runMemgwCollector([]string) {
	fmt.Fprintln(os.Stderr, "memgw collector: the collector is implemented for Windows and Linux only")
	os.Exit(2)
}

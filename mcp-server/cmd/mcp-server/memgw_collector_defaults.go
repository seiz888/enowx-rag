//go:build windows || linux

package main

// collectorDefault holds the handful of things that differ between the hosts
// the collector runs on. Keeping them in one struct means the command itself
// contains no build tags and no platform conditionals.
type collectorDefault struct {
	// dir is where the spool and, on Windows, the wrapped key live.
	dir string
	// endpointFlag is what the local endpoint is called on the command line.
	// It is not "endpoint": an operator on Windows types --pipe and an
	// operator on Linux types --socket, and each of them is right.
	endpointFlag  string
	endpointUsage string
}

//go:build windows

package main

// Defaults put the spool on D: rather than beside the binary, because the spool
// is the one file on this machine that must survive a full C: drive, and C: on
// this host fills up.
func collectorDefaults() collectorDefault {
	return collectorDefault{
		dir:           `D:\memgw-collector`,
		endpointFlag:  "pipe",
		endpointUsage: "named pipe to listen on",
	}
}

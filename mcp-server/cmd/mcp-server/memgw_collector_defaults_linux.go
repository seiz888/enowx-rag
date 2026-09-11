//go:build linux

package main

// The spool lives under /var/lib because it is state that must survive a
// reboot; the socket lives under /run because it must not. StateDirectory= and
// RuntimeDirectory= in the unit produce exactly these two paths with the right
// ownership, which is why they are the defaults rather than something under
// the operator's home.
func collectorDefaults() collectorDefault {
	return collectorDefault{
		dir:           "/var/lib/memgw-collector",
		endpointFlag:  "socket",
		endpointUsage: "unix socket to listen on",
	}
}

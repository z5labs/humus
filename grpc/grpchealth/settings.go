// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package grpchealth

import (
	"fmt"
	"net"
	"strconv"
)

// Settings configures a Service. Its one field has no default, so the end
// user always sources it.
type Settings struct {
	// Addr is the TCP address the Service listens on through the Host, as
	// host:port. It is required: humus imposes no port. An empty host
	// listens on every name, and port 0 picks a free port, which
	// Service.Addr reports.
	//
	// Default: none. Valid: host:port, with a numeric port from 0 to 65535.
	// Check: CheckAddr.
	Addr string
}

// CheckAddr rejects an address that Host.Listen can't listen on, without
// naming the field: one that is not host:port, such as "" or an address
// with no port, or one whose port is not a number from 0 to 65535, a
// service name such as "grpc" included. An empty host and port 0 are
// valid. It accepts and rejects what grpcserver.CheckAddr does. It checks
// the address's form, not that the host resolves or that the port is free:
// Host.Listen reports those when the Service listens.
func CheckAddr(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%q is not host:port", addr)
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return fmt.Errorf("%q has no port from 0 to 65535", addr)
	}
	return nil
}

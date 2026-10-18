// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package grpcclient

import "google.golang.org/grpc/connectivity"

// State returns the state of c's connection, which the Client hides.
func State(c *Client) connectivity.State {
	return c.cc.GetState()
}

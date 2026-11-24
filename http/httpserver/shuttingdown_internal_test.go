// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package httpserver

import (
	"context"
	"net/http"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
)

// net/http reads no new request once Shutdown has begun: HTTP/1.1 closes a
// connection whose request it reads after, and HTTP/2's GOAWAY discards a
// stream opened after. A request that starts after Shutdown began only
// slips in between, so this test checks the context every request's
// context derives from, the server's BaseContext, rather than racing a
// request against Shutdown.
func TestShuttingDown_is_closed_already_for_a_request_that_starts_after_Shutdown_began(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv := &http.Server{}
		watchShutdown(srv)

		before := ShuttingDown(srv.BaseContext(nil))
		require.NotNil(t, before)
		select {
		case <-before:
			t.Fatal("the channel is closed before Shutdown")
		default:
		}

		require.NoError(t, srv.Shutdown(context.Background()))
		synctest.Wait()

		after := ShuttingDown(srv.BaseContext(nil))
		select {
		case <-after:
		default:
			t.Fatal("a request that starts after Shutdown began got a channel still open")
		}
		require.Equal(t, before, after, "every request of the server gets the one channel")
		require.NoError(t, srv.BaseContext(nil).Err(), "Shutdown cancels no request's context")
	})
}

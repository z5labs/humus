// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package httpserver

import (
	"context"
	"net"
	"net/http"
	"sync"
)

// shuttingDownKey is the context key of the channel ShuttingDown returns.
type shuttingDownKey struct{}

// ShuttingDown returns a channel that is closed when the Service serving
// ctx's request begins its Shutdown, after the App's drain. A handler
// whose response runs until its client leaves, such as an event stream,
// a streamed page or a long poll, selects on it and returns, so Shutdown
// isn't held to the end of the grace period. Every other request finishes
// within the grace period, as today. A request that starts after Shutdown
// began gets a channel already closed. For a ctx no Service serves, it
// returns nil, which is never closed.
func ShuttingDown(ctx context.Context) <-chan struct{} {
	ch, _ := ctx.Value(shuttingDownKey{}).(<-chan struct{})
	return ch
}

// watchShutdown makes srv's requests carry a channel that srv's Shutdown
// closes, for ShuttingDown. The channel is a value on srv's BaseContext,
// which net/http calls once per listener, so no request allocates for it,
// and it is closed by a function registered with srv.RegisterOnShutdown,
// which net/http runs when Shutdown begins. Nothing else changes: no
// request's context is cancelled by it.
//
// It returns that function, which closes the channel once however often it
// is called, for a shutdown that closes srv without calling its Shutdown.
func watchShutdown(srv *http.Server) (closeChannel func()) {
	done := make(chan struct{})
	base := context.WithValue(context.Background(), shuttingDownKey{}, (<-chan struct{})(done))
	srv.BaseContext = func(net.Listener) context.Context { return base }
	closeChannel = sync.OnceFunc(func() { close(done) })
	srv.RegisterOnShutdown(closeChannel)
	return closeChannel
}

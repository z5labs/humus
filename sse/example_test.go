// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package sse_test

import (
	"bufio"
	"fmt"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/z5labs/humus/http/httpserver"
	"github.com/z5labs/humus/sse"
)

// quote is one event of the feed: a price, and its sequence number.
type quote struct {
	Seq   string
	Price string
}

// feed returns a feed's handler: an event stream of the quotes it receives,
// with a keep-alive comment every 15s, until quotes is closed, the Service's
// Shutdown begins or the client goes away. In the end user's Init,
// writeTimeout is the httpserver.Settings' WriteTimeout.
func feed(quotes <-chan quote, writeTimeout time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		stream, err := sse.NewStream(w, writeTimeout)
		if err != nil {
			return
		}
		keepAlive := time.NewTicker(15 * time.Second)
		defer keepAlive.Stop()
		for {
			select {
			case <-httpserver.ShuttingDown(r.Context()):
				return
			case <-r.Context().Done():
				return
			case q, ok := <-quotes:
				if !ok {
					return
				}
				if err := stream.Send(sse.Event{Name: "quote", Data: q.Price, ID: q.Seq}); err != nil {
					return
				}
			case <-keepAlive.C:
				if err := stream.Comment("keep-alive"); err != nil {
					return
				}
			}
		}
	}
}

// A feed's handler opens a Stream, sends its events and keep-alive
// comments, and returns when the Service's Shutdown begins or its client
// goes away. Here it is served by an httptest.Server rather than by an
// httpserver Service, so ShuttingDown is nil, and the client going away
// ends it.
func ExampleNewStream() {
	quotes := make(chan quote)
	srv := httptest.NewServer(feed(quotes, httpserver.DefaultSettings().WriteTimeout))
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL)
	if err != nil {
		fmt.Println(err)
		return
	}
	defer func() { _ = resp.Body.Close() }() // the client goes away, so feed returns
	fmt.Println(resp.Header.Get("Content-Type"), resp.Header.Get("Cache-Control"))

	quotes <- quote{Seq: "1", Price: "101.25"}
	quotes <- quote{Seq: "2", Price: "101.50"}
	lines := bufio.NewScanner(resp.Body)
	for events := 0; events < 2 && lines.Scan(); {
		fmt.Printf("%q\n", lines.Text())
		if lines.Text() == "" {
			events++
		}
	}

	// Output: text/event-stream no-store
	// "event: quote"
	// "data: 101.25"
	// "id: 1"
	// ""
	// "event: quote"
	// "data: 101.50"
	// "id: 2"
	// ""
}

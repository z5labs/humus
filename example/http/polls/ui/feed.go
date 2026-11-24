// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package ui

import (
	"net/http"
	"strings"
	"time"

	"github.com/z5labs/humus/example/http/polls/store"
	"github.com/z5labs/humus/http/httpserver"
	"github.com/z5labs/humus/sse"
)

const (
	resultsEvent      = "results"
	keepAliveInterval = 15 * time.Second
)

func (ui *UI) feed(w http.ResponseWriter, r *http.Request) error {
	id, err := pollFrom(r)
	if err != nil {
		return err
	}
	changed, unsubscribe := ui.results.Subscribe(id)
	defer unsubscribe()
	p, err := ui.readPoll(r, id)
	if err != nil {
		return err
	}
	w.Header().Set("Cache-Control", "no-store")
	stream, err := sse.NewStream(w, ui.writeTimeout)
	if err != nil {
		return err
	}
	if r.Method == http.MethodHead {
		return nil
	}
	if err := ui.sendResults(stream, p); err != nil {
		return err
	}
	stop := httpserver.ShuttingDown(r.Context())
	keepAlive := time.NewTicker(keepAliveInterval)
	defer keepAlive.Stop()
	for {
		select {
		case <-stop:
			return nil
		case <-r.Context().Done():
			return nil
		case p := <-changed:
			if err := ui.sendResults(stream, p); err != nil {
				return err
			}
			keepAlive.Reset(keepAliveInterval)
		case <-keepAlive.C:
			if err := stream.Comment("keep-alive"); err != nil {
				return err
			}
		}
	}
}

func (ui *UI) sendResults(stream *sse.Stream, p store.Poll) error {
	b, err := ui.execute("poll.html", "options", p.Options)
	if err != nil {
		return err
	}
	return stream.Send(sse.Event{Name: resultsEvent, Data: strings.TrimSuffix(b.String(), "\n")})
}

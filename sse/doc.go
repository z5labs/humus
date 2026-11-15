// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package sse writes Server-Sent Events, in the text/event-stream format of
// the WHATWG HTML standard, section 9.2.
//
// A handler starts a [Stream] on its response with [NewStream], then sends
// each [Event] with [Stream.Send] and each keep-alive with
// [Stream.Comment]:
//
//	func feed(w http.ResponseWriter, r *http.Request) {
//		w.Header().Set("Cache-Control", "no-store")
//		stream, err := sse.NewStream(w, 10*time.Second)
//		if err != nil {
//			return
//		}
//		for {
//			select {
//			case <-httpserver.ShuttingDown(r.Context()):
//				return
//			case <-r.Context().Done():
//				return
//			case p := <-prices:
//				if err := stream.Send(sse.Event{Name: "price", Data: p.String(), ID: p.Seq}); err != nil {
//					return
//				}
//			case <-keepAlive.C:
//				if err := stream.Comment("keep-alive"); err != nil {
//					return
//				}
//			}
//		}
//	}
//
// # Events
//
// An Event's Name is the event field, and "" writes none, so the client
// dispatches a "message" event. Its Data is split at CRLF, LF or CR, one
// data field per line, so the client reads it back joined with LF. Its ID
// is the id field, and "" writes none. Its Retry is the retry field, in
// whole milliseconds, and 0 writes none, as does a negative Retry, which no
// client would read. Send ends the event with a blank line, which is what
// dispatches it, and flushes it.
//
// A field can't hold a line break, since the line break would end it and
// start another, so a Name that holds CR or LF is refused, and so is an ID
// that holds CR, LF or NUL, which a client ignores. Send returns an error
// for them and writes nothing, so a name or an id taken from a request
// can't inject an event. Data needs no such check, since each of its lines
// is a field of its own. A comment's text is refused for CR or LF in the
// same way.
//
// # The response
//
// NewStream sets Content-Type text/event-stream, writes 200 and flushes,
// so the client sees the stream open before the first event. It sets no
// other header: Cache-Control, and any other header the stream needs, is
// the caller's, set before NewStream.
//
// # The write deadline
//
// A stream's time between events is unbounded: a feed may sit idle for
// minutes. A server's write timeout, such as httpserver's
// Settings.WriteTimeout, bounds the whole response instead, so it would
// cut the stream. NewStream takes a writeTimeout, which bounds each write
// instead: before every write, NewStream's included, the write deadline is
// set writeTimeout ahead, and once the write is flushed, it is cleared. A
// writeTimeout of 0 leaves the deadline alone, as the server set it.
//
// The deadline is cleared, rather than only moved forward before each
// write, because of HTTP/2. Over HTTP/1.1 a write deadline is the
// connection's, and fails only a write made after it, so a deadline passed
// while the stream is idle is moved forward again before the next write.
// Over HTTP/2 the write deadline is a timer on the stream, and when it
// fires, the server resets the stream, whether anything is being written or
// not. A deadline moved forward only before each write would still fire
// during the idle time after the last one, and reset an idle stream.
//
// The deadline is set through http.ResponseController, so a writer that
// can't set one is an error wrapping http.ErrNotSupported, from NewStream,
// before anything is written.
package sse

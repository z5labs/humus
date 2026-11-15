// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package sse

import (
	"bytes"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Event is one event of the WHATWG text/event-stream format.
type Event struct {
	Name  string        // the event field; "" dispatches a "message" event
	Data  string        // split at CRLF, LF or CR, one data field per line
	ID    string        // the id field; "" writes none
	Retry time.Duration // the retry field, in whole milliseconds; 0 writes none
}

// Stream is an event stream on one response. It isn't safe for concurrent
// use, and starts no goroutine.
type Stream struct {
	w            http.ResponseWriter
	rc           *http.ResponseController
	writeTimeout time.Duration
	buf          bytes.Buffer
}

// NewStream sets Content-Type text/event-stream, writes 200 and flushes.
// Every other header, Cache-Control included, is the caller's. Each
// write, this one included, must finish within writeTimeout, and the time
// between writes is unbounded: the write deadline is set writeTimeout
// ahead before a write, and cleared once it is flushed. A writeTimeout of
// 0 leaves the deadline alone. A writer that can't set a deadline is an
// error wrapping http.ErrNotSupported.
func NewStream(w http.ResponseWriter, writeTimeout time.Duration) (*Stream, error) {
	s := &Stream{w: w, rc: http.NewResponseController(w), writeTimeout: writeTimeout}
	err := s.write(func() error {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s, nil
}

// Send writes e and flushes it. A Name that holds CR or LF, or an ID that
// holds CR, LF or NUL, is an error, and nothing is written.
func (s *Stream) Send(e Event) error {
	if strings.ContainsAny(e.Name, "\r\n") {
		return fmt.Errorf("sse: event name %q holds a line break", e.Name)
	}
	if strings.ContainsAny(e.ID, "\r\n\x00") {
		return fmt.Errorf("sse: event id %q holds a line break or NUL", e.ID)
	}
	s.buf.Reset()
	if e.Name != "" {
		s.field("event", e.Name)
	}
	for _, line := range lines(e.Data) {
		s.field("data", line)
	}
	if e.ID != "" {
		s.field("id", e.ID)
	}
	if e.Retry > 0 {
		s.field("retry", strconv.FormatInt(e.Retry.Milliseconds(), 10))
	}
	s.buf.WriteByte('\n')
	return s.write(s.flushBuf)
}

// Comment writes a comment, which clients ignore, such as a keep-alive.
// A text that holds CR or LF is an error.
func (s *Stream) Comment(text string) error {
	if strings.ContainsAny(text, "\r\n") {
		return fmt.Errorf("sse: comment %q holds a line break", text)
	}
	s.buf.Reset()
	s.field("", text)
	return s.write(s.flushBuf)
}

// field adds one line, name: value, to the buffer. A comment's name is "".
func (s *Stream) field(name, value string) {
	s.buf.WriteString(name)
	s.buf.WriteByte(':')
	if value != "" {
		s.buf.WriteByte(' ')
		s.buf.WriteString(value)
	}
	s.buf.WriteByte('\n')
}

// flushBuf writes the buffer to the response.
func (s *Stream) flushBuf() error {
	_, err := s.w.Write(s.buf.Bytes())
	return err
}

// write sets the write deadline, runs do, flushes, and clears the deadline.
func (s *Stream) write(do func() error) error {
	if s.writeTimeout > 0 {
		if err := s.rc.SetWriteDeadline(time.Now().Add(s.writeTimeout)); err != nil {
			return fmt.Errorf("sse: setting the write deadline: %w", err)
		}
	}
	if err := do(); err != nil {
		return fmt.Errorf("sse: writing: %w", err)
	}
	if err := s.rc.Flush(); err != nil {
		return fmt.Errorf("sse: flushing: %w", err)
	}
	if s.writeTimeout > 0 {
		if err := s.rc.SetWriteDeadline(time.Time{}); err != nil {
			return fmt.Errorf("sse: clearing the write deadline: %w", err)
		}
	}
	return nil
}

// lines splits data at CRLF, LF or CR. "" is one empty line.
func lines(data string) []string {
	var out []string
	for {
		i := strings.IndexAny(data, "\r\n")
		if i < 0 {
			return append(out, data)
		}
		out = append(out, data[:i])
		if data[i] == '\r' && i+1 < len(data) && data[i+1] == '\n' {
			i++
		}
		data = data[i+1:]
	}
}

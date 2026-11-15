// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package sse_test

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus/sse"
)

// writer is an http.ResponseWriter that flushes, and logs each call. It
// sets no write deadline.
type writer struct {
	header http.Header
	body   bytes.Buffer
	log    []string
}

func newWriter() *writer { return &writer{header: http.Header{}} }

func (w *writer) Header() http.Header { return w.header }

func (w *writer) WriteHeader(code int) { w.log = append(w.log, fmt.Sprintf("header %d", code)) }

func (w *writer) Write(b []byte) (int, error) {
	w.log = append(w.log, fmt.Sprintf("write %q", b))
	return w.body.Write(b)
}

func (w *writer) Flush() { w.log = append(w.log, "flush") }

// deadlines is a writer that also sets write deadlines, and logs each as
// how far ahead it is, or as cleared.
type deadlines struct{ *writer }

func (w deadlines) SetWriteDeadline(t time.Time) error {
	if t.IsZero() {
		w.log = append(w.log, "deadline cleared")
	} else {
		w.log = append(w.log, fmt.Sprintf("deadline %s ahead", time.Until(t)))
	}
	return nil
}

// streamOn starts a Stream on a new writer with no write timeout, and
// returns them with the writer's log and body emptied.
func streamOn(t *testing.T) (*sse.Stream, *writer) {
	t.Helper()
	w := newWriter()
	s, err := sse.NewStream(w, 0)
	require.NoError(t, err)
	w.body.Reset()
	w.log = nil
	return s, w
}

func TestStream_Send_writes_an_event(t *testing.T) {
	tests := []struct {
		name  string
		event sse.Event
		want  string
	}{
		{
			name:  "with every field",
			event: sse.Event{Name: "price", Data: "42", ID: "7", Retry: 1500 * time.Millisecond},
			want:  "event: price\ndata: 42\nid: 7\nretry: 1500\n\n",
		},
		{
			name:  "with none but Data",
			event: sse.Event{Data: "hello"},
			want:  "data: hello\n\n",
		},
		{
			name:  "with no field at all, as one empty data field",
			event: sse.Event{},
			want:  "data:\n\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, w := streamOn(t)
			require.NoError(t, s.Send(tc.event))
			require.Equal(t, tc.want, w.body.String())
			require.Equal(t, []string{fmt.Sprintf("write %q", tc.want), "flush"}, w.log, "the event is one write, flushed")
		})
	}
}

func TestStream_Comment_writes_a_comment(t *testing.T) {
	s, w := streamOn(t)
	require.NoError(t, s.Comment("keep-alive"))
	require.NoError(t, s.Comment(""))
	require.Equal(t, ": keep-alive\n:\n", w.body.String())
	require.Equal(t, []string{`write ": keep-alive\n"`, "flush", `write ":\n"`, "flush"}, w.log)
}

func TestStream_Send_splits_Data_into_one_data_field_per_line(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{"at CRLF", "a\r\nb\r\nc", "data: a\ndata: b\ndata: c\n\n"},
		{"at LF", "a\nb\nc", "data: a\ndata: b\ndata: c\n\n"},
		{"at CR", "a\rb\rc", "data: a\ndata: b\ndata: c\n\n"},
		{"at each of them", "a\r\nb\nc\rd", "data: a\ndata: b\ndata: c\ndata: d\n\n"},
		{"at LF then CR, as two line breaks", "a\n\rb", "data: a\ndata:\ndata: b\n\n"},
		{"with an empty last line", "a\n", "data: a\ndata:\n\n"},
		{"with a leading space kept", " a", "data:  a\n\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, w := streamOn(t)
			require.NoError(t, s.Send(sse.Event{Data: tc.data}))
			require.Equal(t, tc.want, w.body.String())
		})
	}
}

func TestStream_Send_writes_Retry_in_whole_milliseconds(t *testing.T) {
	tests := []struct {
		retry time.Duration
		want  string
	}{
		{2 * time.Second, "retry: 2000\n"},
		{1500 * time.Millisecond, "retry: 1500\n"},
		{1500*time.Millisecond + 999*time.Microsecond, "retry: 1500\n"},
		{time.Millisecond, "retry: 1\n"},
		{500 * time.Microsecond, "retry: 0\n"},
		{0, ""},
		{-time.Second, ""},
	}
	for _, tc := range tests {
		t.Run(tc.retry.String(), func(t *testing.T) {
			s, w := streamOn(t)
			require.NoError(t, s.Send(sse.Event{Data: "x", Retry: tc.retry}))
			require.Equal(t, "data: x\n"+tc.want+"\n", w.body.String())
		})
	}
}

func TestStream_refuses_a_line_break_in_a_field(t *testing.T) {
	sends := []struct {
		name  string
		event sse.Event
	}{
		{"a Name with LF", sse.Event{Name: "a\nb", Data: "x"}},
		{"a Name with CR", sse.Event{Name: "a\rb", Data: "x"}},
		{"a Name with CRLF", sse.Event{Name: "a\r\nb", Data: "x"}},
		{"an ID with LF", sse.Event{ID: "1\n2", Data: "x"}},
		{"an ID with CR", sse.Event{ID: "1\r2", Data: "x"}},
		{"an ID with NUL", sse.Event{ID: "1\x002", Data: "x"}},
	}
	for _, tc := range sends {
		t.Run("Send, "+tc.name, func(t *testing.T) {
			s, w := streamOn(t)
			require.Error(t, s.Send(tc.event))
			require.Empty(t, w.log, "nothing is written")
		})
	}
	for _, text := range []string{"a\nb", "a\rb", "a\r\nb", "\n"} {
		t.Run(fmt.Sprintf("Comment %q", text), func(t *testing.T) {
			s, w := streamOn(t)
			require.Error(t, s.Comment(text))
			require.Empty(t, w.log, "nothing is written")
		})
	}
}

func TestNewStream_starts_the_response(t *testing.T) {
	t.Run("with the caller's Cache-Control left as it was", func(t *testing.T) {
		w := newWriter()
		w.Header().Set("Cache-Control", "no-store")
		_, err := sse.NewStream(w, 0)
		require.NoError(t, err)
		require.Equal(t, http.Header{
			"Cache-Control": {"no-store"},
			"Content-Type":  {"text/event-stream"},
		}, w.Header())
		require.Equal(t, []string{"header 200", "flush"}, w.log)
		require.Zero(t, w.body.Len())
	})
	t.Run("with no other header when the caller sets none", func(t *testing.T) {
		w := newWriter()
		_, err := sse.NewStream(w, 0)
		require.NoError(t, err)
		require.Equal(t, http.Header{"Content-Type": {"text/event-stream"}}, w.Header())
		require.Equal(t, []string{"header 200", "flush"}, w.log)
	})
}

func TestStream_bounds_each_write_and_clears_the_deadline_between_them(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := deadlines{newWriter()}
		s, err := sse.NewStream(w, 10*time.Second)
		require.NoError(t, err)
		time.Sleep(time.Minute)
		require.NoError(t, s.Send(sse.Event{Data: "x"}))
		time.Sleep(time.Hour)
		require.NoError(t, s.Comment("keep-alive"))
		require.Equal(t, []string{
			"deadline 10s ahead", "header 200", "flush", "deadline cleared",
			"deadline 10s ahead", `write "data: x\n\n"`, "flush", "deadline cleared",
			"deadline 10s ahead", `write ": keep-alive\n"`, "flush", "deadline cleared",
		}, w.log)
	})
}

func TestStream_a_writeTimeout_of_0_leaves_the_deadline_alone(t *testing.T) {
	w := deadlines{newWriter()}
	s, err := sse.NewStream(w, 0)
	require.NoError(t, err)
	require.NoError(t, s.Send(sse.Event{Data: "x"}))
	require.NoError(t, s.Comment("keep-alive"))
	require.Equal(t, []string{
		"header 200", "flush",
		`write "data: x\n\n"`, "flush",
		`write ": keep-alive\n"`, "flush",
	}, w.log)
}

func TestNewStream_a_writer_that_cant_set_a_deadline(t *testing.T) {
	w := newWriter()
	s, err := sse.NewStream(w, 10*time.Second)
	require.ErrorIs(t, err, http.ErrNotSupported)
	require.Nil(t, s)
	require.Empty(t, w.log, "nothing is written")
	require.Empty(t, w.Header(), "no header is set")
}

// failing is a writer whose Write, Flush or SetWriteDeadline fails once
// started.
type failing struct {
	*writer
	started             bool
	write, flush, clear bool
}

var errFailed = errors.New("failed")

func (w *failing) Write(b []byte) (int, error) {
	if w.started && w.write {
		return 0, errFailed
	}
	return w.writer.Write(b)
}

func (w *failing) FlushError() error {
	if w.started && w.flush {
		return errFailed
	}
	w.Flush()
	return nil
}

func (w *failing) SetWriteDeadline(t time.Time) error {
	if w.started && w.clear && t.IsZero() {
		return errFailed
	}
	return nil
}

func TestStream_returns_the_writers_error(t *testing.T) {
	tests := []struct {
		name string
		w    *failing
	}{
		{"from Write", &failing{write: true}},
		{"from Flush", &failing{flush: true}},
		{"from clearing the deadline", &failing{clear: true}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.w.writer = newWriter()
			s, err := sse.NewStream(tc.w, 10*time.Second)
			require.NoError(t, err)
			tc.w.started = true
			require.ErrorIs(t, s.Send(sse.Event{Data: "x"}), errFailed)
			require.ErrorIs(t, s.Comment("x"), errFailed)
		})
	}
	t.Run("from NewStream's flush", func(t *testing.T) {
		w := &failing{writer: newWriter(), started: true, flush: true}
		_, err := sse.NewStream(w, 0)
		require.ErrorIs(t, err, errFailed)
	})
}

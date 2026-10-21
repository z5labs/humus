// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package openapi

import (
	"io"
	"net/http"
)

// Decoder reads request bodies of type T in one media type.
type Decoder[T any] interface {
	// MediaType is the body's media type, such as application/xml. A request
	// whose Content-Type names another media type, parameters aside, or
	// none at all, is answered with a 415. "" is no body: the Content-Type
	// isn't checked.
	MediaType() string

	// Schema describes T's bodies in the document. NewAPI calls it once,
	// and returns its error: a codec that can't handle T says so here.
	Schema() (Schema, error)

	// Decode reads r's body. An error that wraps *http.MaxBytesError is
	// answered with a 413, and any other with a 400. A decoder that streams
	// returns a T that reads the body as the handler reads it.
	Decode(r *http.Request) (T, error)
}

// Encoder writes bodies of type T in one media type.
type Encoder[T any] interface {
	// MediaType is the body's media type. "" is no body: the document lists
	// no content, and nothing is written.
	MediaType() string

	// Schema is as Decoder's.
	Schema() (Schema, error)

	// Encode prepares v to be written, before the status is. Its error is
	// answered by the catch-all. The API then writes the status and calls
	// the WriterTo, which may do more work once it has written the body,
	// such as recording that a download completed. An error from the
	// WriterTo cuts the response short, and is the server's unless the
	// client went away: the request's context is done, or a write to the
	// writer the WriterTo is handed failed because the connection did,
	// rather than because the server refused it. An encoder that buffers returns its
	// encoded bytes, such as a *bytes.Reader. An encoder that streams
	// returns a WriterTo that writes as it goes.
	Encode(v T) (io.WriterTo, error)
}

// Schema is a body's JSON Schema, as JSON, as the document lists it under
// the body's media type. A nil Schema lists the media type alone, as for
// raw bytes.
type Schema []byte

// EmptyBody is the body of a request that has none, and the response of a
// handler that writes none.
type EmptyBody struct{}

// EmptyDecoder and EmptyEncoder are EmptyBody's codecs: no body is read or
// written. A response with no body is a 204, unless Status says otherwise.
func EmptyDecoder() Decoder[EmptyBody] { return emptyCodec{} }

// EmptyEncoder is EmptyDecoder's counterpart: see EmptyDecoder.
func EmptyEncoder() Encoder[EmptyBody] { return emptyCodec{} }

// emptyCodec is EmptyBody's decoder and encoder. Its media type is "", so
// the API checks no Content-Type and writes no Content-Type.
type emptyCodec struct{}

func (emptyCodec) MediaType() string { return "" }

func (emptyCodec) Schema() (Schema, error) { return nil, nil }

func (emptyCodec) Decode(*http.Request) (EmptyBody, error) { return EmptyBody{}, nil }

func (emptyCodec) Encode(EmptyBody) (io.WriterTo, error) { return nothing{}, nil }

// nothing is a WriterTo that writes nothing.
type nothing struct{}

func (nothing) WriteTo(io.Writer) (int64, error) { return 0, nil }

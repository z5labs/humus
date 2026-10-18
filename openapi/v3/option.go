// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package openapi

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
)

// Option is a part of one operation. Its methods are unexported, so only
// this package makes Options.
type Option interface {
	apply(p *parts)
}

// APIOption is a part that NewAPI takes too, for every operation.
type APIOption interface {
	Option
	apiOption()
}

// Fallback is a catch-all, as CatchAll returns it. It is an Option, so an
// operation can give its own.
type Fallback interface {
	Option
	fallback()
}

// RequestErrorAnswer is what RequestErrors returns. It is an Option, as a
// Fallback is.
type RequestErrorAnswer interface {
	Option
	requestErrorAnswer()
}

// parts is what an operation's Options, and NewAPI's, say.
type parts struct {
	status        int
	statusSet     bool
	errors        []errorAnswer
	catchAll      answerer
	requestErrors answerer
}

// answerer is an Error, a CatchAll or a RequestErrors, erased. bind calls
// its encoder function and the encoder's Schema, once.
type answerer interface {
	name() string
	bind() (*answer, error)
}

// errorAnswer is an Error, erased.
type errorAnswer interface {
	answerer
	errType() reflect.Type
}

// answer is a bound answerer: the status it answers with, its encoder's
// media type, and how it encodes the error it answers.
type answer struct {
	name      string
	status    int // 0 for RequestErrors, whose status is the RequestError's
	mediaType string

	// encode returns the encoded answer to err, and false if the answer
	// doesn't match err.
	encode func(err error) (io.WriterTo, bool, error)
}

// Status is the status of the operation's success response. Without it, the
// status is 204 when the operation's encoder has the media type "", and 200
// otherwise.
func Status(code int) Option {
	return statusOption(code)
}

type statusOption int

func (o statusOption) apply(p *parts) {
	p.status = int(o)
	p.statusSet = true
}

// Error answers an error that matches E, with errors.As, with status and E,
// written by enc. Given to NewAPI, it answers on every operation, and every
// operation documents it. An operation's own Error for E replaces it.
//
// An error that only some operations return isn't API-wide. It is a shared
// value, given to those operations:
//
//	var cartNotFound = openapi.Error[NotFound](http.StatusNotFound, problemEncoder)
func Error[E error](status int, enc func() Encoder[E]) APIOption {
	return errorOption[E]{status: status, enc: enc}
}

type errorOption[E error] struct {
	status int
	enc    func() Encoder[E]
}

func (o errorOption[E]) apply(p *parts) { p.errors = append(p.errors, o) }

func (errorOption[E]) apiOption() {}

func (errorOption[E]) errType() reflect.Type { return reflect.TypeFor[E]() }

func (errorOption[E]) name() string { return fmt.Sprintf("Error[%s]", reflect.TypeFor[E]()) }

func (o errorOption[E]) bind() (*answer, error) {
	var errs []error
	if err := checkStatus(o.status); err != nil {
		errs = append(errs, err)
	}
	enc, err := bindEncoder(o.enc)
	if err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return &answer{
		name:      o.name(),
		status:    o.status,
		mediaType: enc.MediaType(),
		encode: func(err error) (io.WriterTo, bool, error) {
			var e E
			if !errors.As(err, &e) {
				return nil, false, nil
			}
			wt, err := enc.Encode(e)
			return wt, true, err
		},
	}, nil
}

// CatchAll answers an error that no Error matched with status and E,
// written by enc: the error itself when it matches E, with errors.As, and
// wrap(err) otherwise. NewAPI requires one, for every operation. An
// operation's own CatchAll replaces it.
//
// An error the catch-all answers is one the end user didn't plan for, so
// the API records it: an exception event on the request's span, and one
// ERROR record, "request failed". wrap decides what the client sees, and
// should not put the error's text in the body: the record has it.
func CatchAll[E error](status int, wrap func(error) E, enc func() Encoder[E]) Fallback {
	return catchAllOption[E]{status: status, wrap: wrap, enc: enc}
}

type catchAllOption[E error] struct {
	status int
	wrap   func(error) E
	enc    func() Encoder[E]
}

func (o catchAllOption[E]) apply(p *parts) { p.catchAll = o }

func (catchAllOption[E]) fallback() {}

func (catchAllOption[E]) name() string { return fmt.Sprintf("CatchAll[%s]", reflect.TypeFor[E]()) }

func (o catchAllOption[E]) bind() (*answer, error) {
	var errs []error
	if err := checkStatus(o.status); err != nil {
		errs = append(errs, err)
	}
	if o.wrap == nil {
		errs = append(errs, errors.New("its wrap function is nil"))
	}
	enc, err := bindEncoder(o.enc)
	if err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return &answer{
		name:      o.name(),
		status:    o.status,
		mediaType: enc.MediaType(),
		encode: func(err error) (io.WriterTo, bool, error) {
			var e E
			if !errors.As(err, &e) {
				e = o.wrap(err)
			}
			wt, err := enc.Encode(e)
			return wt, true, err
		},
	}, nil
}

// RequestError is a request an operation rejected before its handler ran.
type RequestError struct {
	// Status is:
	//   - 400 for a parameter or a body that doesn't decode;
	//   - 413 for a body over http.MaxBytesHandler's limit;
	//   - 415 for a Content-Type the operation's decoder doesn't read, or
	//     none.
	Status int

	// Param names the parameter that doesn't decode, for a 400 about one,
	// and is "" otherwise.
	Param string

	// Err is why: the parameter's error or the decoder's.
	Err error
}

// Error returns the status and Err's text.
func (e *RequestError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("openapi: request rejected with %d %s", e.Status, http.StatusText(e.Status))
	}
	return fmt.Sprintf("openapi: request rejected with %d %s: %v", e.Status, http.StatusText(e.Status), e.Err)
}

// Unwrap returns Err.
func (e *RequestError) Unwrap() error {
	return e.Err
}

// RequestErrors answers every RequestError with its Status and with wrap's
// value, written by enc. Every operation can answer with it: at 400 when
// the operation reads a parameter or a body, and at 413 and 415 when it
// reads a body. NewAPI requires one. Given to an operation, it replaces the
// API's for that operation.
//
// Its answers aren't logged: a request the API rejects is the client's
// mistake.
func RequestErrors[E any](wrap func(*RequestError) E, enc func() Encoder[E]) RequestErrorAnswer {
	return requestErrorsOption[E]{wrap: wrap, enc: enc}
}

type requestErrorsOption[E any] struct {
	wrap func(*RequestError) E
	enc  func() Encoder[E]
}

func (o requestErrorsOption[E]) apply(p *parts) { p.requestErrors = o }

func (requestErrorsOption[E]) requestErrorAnswer() {}

func (requestErrorsOption[E]) name() string {
	return fmt.Sprintf("RequestErrors[%s]", reflect.TypeFor[E]())
}

func (o requestErrorsOption[E]) bind() (*answer, error) {
	var errs []error
	if o.wrap == nil {
		errs = append(errs, errors.New("its wrap function is nil"))
	}
	enc, err := bindEncoder(o.enc)
	if err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return &answer{
		name:      o.name(),
		mediaType: enc.MediaType(),
		encode: func(err error) (io.WriterTo, bool, error) {
			var re *RequestError
			if !errors.As(err, &re) {
				return nil, false, nil
			}
			wt, err := enc.Encode(o.wrap(re))
			return wt, true, err
		},
	}, nil
}

// checkStatus reports a status that net/http can't write.
func checkStatus(status int) error {
	if status < 100 || status > 599 {
		return fmt.Errorf("its status %d isn't an HTTP status", status)
	}
	return nil
}

// bindDecoder calls f, and the decoder's Schema, once.
func bindDecoder[T any](f func() Decoder[T]) (Decoder[T], error) {
	if f == nil {
		return nil, errors.New("its decoder function is nil")
	}
	dec := f()
	if dec == nil {
		return nil, errors.New("its decoder function returned nil")
	}
	if _, err := dec.Schema(); err != nil {
		return nil, fmt.Errorf("its decoder's Schema failed: %w", err)
	}
	return dec, nil
}

// bindEncoder calls f, and the encoder's Schema, once.
func bindEncoder[T any](f func() Encoder[T]) (Encoder[T], error) {
	if f == nil {
		return nil, errors.New("its encoder function is nil")
	}
	enc := f()
	if enc == nil {
		return nil, errors.New("its encoder function returned nil")
	}
	if _, err := enc.Schema(); err != nil {
		return nil, fmt.Errorf("its encoder's Schema failed: %w", err)
	}
	return enc, nil
}

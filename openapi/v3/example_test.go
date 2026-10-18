// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package openapi_test

import (
	"context"
	"io"
	"net/http"
	"regexp"

	openapi "github.com/z5labs/humus/openapi/v3"
)

// The "Serving an API with OpenAPI" page of the docs site quotes the code in
// this file, and TestDocs_code_samples checks that every quote still matches
// it. The tests in files_test.go serve the files API and check what the page
// says about it.

// fileName is a file's name: the path parameter of every operation on a
// file.
var fileName = openapi.PathParam[string]("name").
	Description("The file's name.").
	Pattern(regexp.MustCompile(`^[a-z0-9-]{1,64}$`))

// file is the path of one file: /files/{name}.
var file = openapi.Root().Lit("files").Param(fileName)

// Objects is where the files are kept, such as an S3 bucket.
type Objects interface {
	// Put stores body as the object name, reading it to its end.
	Put(ctx context.Context, name string, body io.Reader) error

	// Get returns the body of the object name, to be read and closed.
	Get(ctx context.Context, name string) (io.ReadCloser, error)

	// MarkDownloaded records that the object name was downloaded in full.
	MarkDownloaded(ctx context.Context, name string) error
}

// Files serves the files API on Objects.
type Files struct {
	Objects Objects
}

// Operations returns the files API's operations: an upload, whose body is
// streamed into Objects as it arrives, and a download, streamed out of
// Objects as it is read, then recorded.
func (f Files) Operations() []openapi.Operation {
	return []openapi.Operation{
		openapi.Put(file, f.putFile, uploadDecoder, openapi.EmptyEncoder,
			openapi.OperationID("putFile"),
			openapi.Summary("Upload a file"),
		),
		openapi.Get(file, f.getFile, downloadEncoder,
			openapi.OperationID("getFile"),
			openapi.Summary("Download a file"),
		),
	}
}

// putFile stores the request's body as it reads it: the decoder handed it
// over unread.
func (f Files) putFile(ctx context.Context, req openapi.Request[io.Reader]) (openapi.EmptyBody, error) {
	name := openapi.ParamFrom(req, fileName)
	return openapi.EmptyBody{}, f.Objects.Put(ctx, name, req.Body())
}

// upload is the decoder of a body that is streamed: Decode hands the
// handler the request's body, unread, as an io.Reader.
type upload struct{}

func uploadDecoder() openapi.Decoder[io.Reader] { return upload{} }

func (upload) MediaType() string { return "application/octet-stream" }

func (upload) Schema() (openapi.Schema, error) { return nil, nil }

func (upload) Decode(r *http.Request) (io.Reader, error) { return r.Body, nil }

// download is what getFile returns: what to copy, and what to do once it
// has been copied.
type download struct {
	body io.ReadCloser // the object's body
	done func() error  // runs after the last byte is written
}

func (f Files) getFile(ctx context.Context, req openapi.Request[openapi.EmptyBody]) (download, error) {
	name := openapi.ParamFrom(req, fileName)
	body, err := f.Objects.Get(ctx, name)
	if err != nil {
		return download{}, err // no status yet: answered by an Error or the catch-all
	}
	return download{body: body, done: func() error {
		return f.Objects.MarkDownloaded(ctx, name) // ctx is still the request's
	}}, nil
}

func downloadEncoder() openapi.Encoder[download] { return downloadCodec{} }

// downloadCodec writes a download as application/octet-stream. Its WriterTo,
// the download itself, has no Len, so the response is chunked, and a
// failure after the last byte is seen by the client.
type downloadCodec struct{}

func (downloadCodec) MediaType() string { return "application/octet-stream" }

func (downloadCodec) Schema() (openapi.Schema, error) { return nil, nil }

func (downloadCodec) Encode(d download) (io.WriterTo, error) { return d, nil }

func (d download) WriteTo(w io.Writer) (int64, error) {
	defer func() { _ = d.body.Close() }()
	n, err := io.Copy(w, d.body)
	if err != nil {
		return n, err // the object failed partway: the response is cut short
	}
	return n, d.done() // a failed update: the same
}

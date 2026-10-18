// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package openapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel/trace"

	"github.com/z5labs/humus/internal/authn"
)

// openAPIVersion is the version of OpenAPI the document follows: the latest
// minor version of OpenAPI 3.
const openAPIVersion = "3.2.0"

// Document returns the operation that serves the API's OpenAPI document at
// p, with GET, as application/json. The document doesn't describe it, the
// requests Unmatched answers, or anything mounted next to the API. An API
// whose operations include no Document serves no document.
//
// NewAPI builds the document from the API's operations and their codecs,
// and encodes it once, so every request for it gets the same bytes. A
// path with a parameter, and a second Document, are NewAPI errors.
//
// The document is secured like the API: a request for it must meet the
// Security given to NewAPI. Of opts, Document takes Security alone, which
// replaces the API's, so Document(p, Security(auth.Disabled())) serves the
// document to anyone. The document doesn't describe itself, so any other
// option is a NewAPI error.
func Document(p Path, opts ...Option) Operation {
	return Operation{op: &operation{method: http.MethodGet, path: p, opts: opts, document: true}}
}

// Summary is the operation's summary in the document: a short sentence.
func Summary(s string) Option {
	return partFunc(func(p *parts) { p.summary = s })
}

// Description is the operation's description in the document, which can be
// longer than its Summary, in CommonMark.
func Description(s string) Option {
	return partFunc(func(p *parts) { p.description = s })
}

// OperationID is the operation's operationId in the document, which client
// generators name the operation's method after. Two operations of an API
// with one OperationID are a NewAPI error.
func OperationID(id string) Option {
	return partFunc(func(p *parts) { p.operationID = id })
}

// Tags adds tags to the operation's tags in the document, which group
// operations.
func Tags(tags ...string) Option {
	return partFunc(func(p *parts) { p.tags = append(p.tags, tags...) })
}

// Deprecated marks the operation as deprecated in the document. The API
// still serves it.
func Deprecated() Option {
	return partFunc(func(p *parts) { p.deprecated = true })
}

// partFunc is an Option that sets a part directly.
type partFunc func(p *parts)

func (f partFunc) apply(p *parts) { f(p) }

// The document, as it is encoded. Maps are encoded with their keys sorted,
// so the document's bytes depend only on the API.
type (
	document struct {
		OpenAPI    string                              `json:"openapi"`
		Info       infoDoc                             `json:"info"`
		Paths      map[string]map[string]*operationDoc `json:"paths"`
		Components *componentsDoc                      `json:"components,omitempty"`
		Security   []requirementDoc                    `json:"security,omitzero"`
	}

	infoDoc struct {
		Title       string `json:"title"`
		Version     string `json:"version"`
		Description string `json:"description,omitempty"`
	}

	operationDoc struct {
		Tags        []string               `json:"tags,omitempty"`
		Summary     string                 `json:"summary,omitempty"`
		Description string                 `json:"description,omitempty"`
		OperationID string                 `json:"operationId,omitempty"`
		Parameters  []paramDoc             `json:"parameters,omitempty"`
		RequestBody *requestBodyDoc        `json:"requestBody,omitempty"`
		Responses   map[string]responseDoc `json:"responses"`
		Deprecated  bool                   `json:"deprecated,omitempty"`

		// Security is nil, and left out, for an operation without a
		// Security of its own, and [] for Security(auth.Disabled()).
		Security []requirementDoc `json:"security,omitzero"`
	}

	paramDoc struct {
		Name        string      `json:"name"`
		In          string      `json:"in"`
		Description string      `json:"description,omitempty"`
		Required    bool        `json:"required,omitempty"`
		Schema      paramSchema `json:"schema"`
		Example     any         `json:"example,omitempty"`
	}

	paramSchema struct {
		Type      string `json:"type"`
		Format    string `json:"format,omitempty"`
		Enum      []any  `json:"enum,omitempty"`
		Default   any    `json:"default,omitempty"`
		Pattern   string `json:"pattern,omitempty"`
		MinLength *int   `json:"minLength,omitempty"`
		MaxLength *int   `json:"maxLength,omitempty"`
		Minimum   any    `json:"minimum,omitempty"`
		Maximum   any    `json:"maximum,omitempty"`
	}

	requestBodyDoc struct {
		Content  map[string]mediaTypeDoc `json:"content"`
		Required bool                    `json:"required"`
	}

	responseDoc struct {
		Description string                  `json:"description,omitempty"`
		Content     map[string]mediaTypeDoc `json:"content,omitempty"`
	}

	mediaTypeDoc struct {
		Schema json.RawMessage `json:"schema,omitempty"`
	}
)

// content lists a body of mediaType, with its schema when it has one. A
// mediaType of "" is no body.
func content(mediaType string, schema Schema) map[string]mediaTypeDoc {
	if mediaType == "" {
		return nil
	}
	return map[string]mediaTypeDoc{mediaType: {Schema: json.RawMessage(schema)}}
}

// response is a response at status, of mediaType and schema.
func response(status int, mediaType string, schema Schema) responseDoc {
	return responseDoc{Description: http.StatusText(status), Content: content(mediaType, schema)}
}

// document returns rt as the document lists it: its metadata, its
// parameters, its request body, and every response it can answer with.
func (rt *route) document() *operationDoc {
	meta := rt.meta
	doc := &operationDoc{
		Tags:        meta.tags,
		Summary:     meta.summary,
		Description: meta.description,
		OperationID: meta.operationID,
		Deprecated:  meta.deprecated,
		Responses:   make(map[string]responseDoc),
	}
	for _, seg := range rt.path.segs {
		if seg.param != nil {
			doc.Parameters = append(doc.Parameters, seg.param.document())
		}
	}
	for _, p := range rt.opParams {
		doc.Parameters = append(doc.Parameters, p.document())
	}

	c := rt.codecs
	readsBody := c.decMediaType != ""
	if readsBody {
		doc.RequestBody = &requestBodyDoc{Content: content(c.decMediaType, c.decSchema), Required: true}
	}

	add := func(status int, mediaType string, schema Schema) {
		doc.Responses[strconv.Itoa(status)] = response(status, mediaType, schema)
	}
	add(rt.status, c.encMediaType, c.encSchema)
	for _, e := range rt.answers.errors {
		add(e.status, e.mediaType, e.schema)
	}
	re := rt.answers.requestErrors
	if readsBody || len(doc.Parameters) > 0 {
		add(http.StatusBadRequest, re.mediaType, re.schema)
	}
	if readsBody {
		add(http.StatusRequestEntityTooLarge, re.mediaType, re.schema)
		add(http.StatusUnsupportedMediaType, re.mediaType, re.schema)
	}
	security := rt.answers.security
	if canAnswer401(security) {
		add(http.StatusUnauthorized, re.mediaType, re.schema)
	}
	if canAnswer403(security) {
		add(http.StatusForbidden, re.mediaType, re.schema)
	}
	if meta.security != nil {
		doc.Security = requirementsDoc(security)
	}
	ca := rt.answers.catchAll
	add(ca.status, ca.mediaType, ca.schema)
	return doc
}

// encodeDocument returns the document of info and routes, encoded as JSON,
// with security, the API's requirements, as its top-level security, and
// schemes, every scheme of every requirement, as its components. NewAPI
// calls it only when it found no mistake, so every route's answers and
// requirements are bound.
func encodeDocument(info Info, routes []*route, security *authn.List, schemes []*authn.Scheme) ([]byte, error) {
	doc := document{
		OpenAPI:    openAPIVersion,
		Info:       infoDoc(info),
		Paths:      make(map[string]map[string]*operationDoc),
		Components: components(schemes),
	}
	if security != nil {
		doc.Security = requirementsDoc(security)
	}
	for _, r := range routes {
		path := r.path.String()
		if doc.Paths[path] == nil {
			doc.Paths[path] = make(map[string]*operationDoc)
		}
		doc.Paths[path][strings.ToLower(r.method)] = r.document()
	}
	return json.Marshal(doc)
}

// documentHandler serves the encoded document.
type documentHandler []byte

func (d documentHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := write(w, http.StatusOK, "application/json", bytes.NewReader(d)); err != nil {
		// The document has a Content-Length, so a failure to write it is
		// the client's, and the client sees it.
		trace.SpanFromContext(r.Context()).RecordError(err)
	}
}

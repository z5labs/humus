// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package hypermedia

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"slices"

	"github.com/z5labs/humus/internal/param"
)

// FormField is a field of an HTML form, such as <input name="email">.
// Required and each constraint return a copy, and leave the field they
// were called on unchanged, as PathParameter's do, so a declaration can be
// shared:
//
//	var Email = hypermedia.Field[string]("email").Required().MaxLength(254)
//
// Enum, Minimum and Maximum take the field's own T, so a value of another
// type doesn't compile. A constraint that doesn't fit T, such as MinLength
// on an int or Minimum on a string, makes ReadForm return an error that
// names it, before it reads the body.
type FormField[T ParamValue] struct {
	name        string
	required    bool
	constraints param.Constraints[T]
}

// Field returns the form field name, the name its control sends.
func Field[T ParamValue](name string) FormField[T] {
	return FormField[T]{name: name}
}

// Required returns f, which the client must send, and not empty. A
// Required field it doesn't send, or sends empty, breaks Missing, as
// HTML's required treats an empty control.
func (f FormField[T]) Required() FormField[T] {
	f.required = true
	return f
}

// Enum returns f, whose value is one of values.
func (f FormField[T]) Enum(values ...T) FormField[T] {
	f.constraints.Enum = slices.Clone(values)
	return f
}

// Pattern returns f, whose value matches re. T is a string type.
func (f FormField[T]) Pattern(re *regexp.Regexp) FormField[T] {
	f.constraints.Pattern = re
	f.constraints.PatternSet = true
	return f
}

// MinLength returns f, whose value is at least n characters long. T is a
// string type.
func (f FormField[T]) MinLength(n int) FormField[T] {
	f.constraints.MinLength = &n
	return f
}

// MaxLength returns f, whose value is at most n characters long. T is a
// string type.
func (f FormField[T]) MaxLength(n int) FormField[T] {
	f.constraints.MaxLength = &n
	return f
}

// Minimum returns f, whose value is at least v. T is a number type.
func (f FormField[T]) Minimum(v T) FormField[T] {
	f.constraints.Minimum = &v
	return f
}

// Maximum returns f, whose value is at most v. T is a number type.
func (f FormField[T]) Maximum(v T) FormField[T] {
	f.constraints.Maximum = &v
	return f
}

// AnyField is any FormField, as ReadForm, Form.Raw and Form.Problem take
// one. Its methods are unexported, so only a FormField is one.
type AnyField interface {
	formField() fieldSpec
}

// fieldSpec is a FormField, erased.
type fieldSpec struct {
	name string

	// typ is the field's T.
	typ reflect.Type

	// check returns the mistakes in the field's declaration.
	check func() []error

	// read parses and checks the values the client sent for the field.
	read func(values []string) (any, *FieldError)
}

func (f FormField[T]) formField() fieldSpec {
	return fieldSpec{
		name:  f.name,
		typ:   reflect.TypeFor[T](),
		check: f.check,
		read: func(values []string) (any, *FieldError) {
			return f.read(values)
		},
	}
}

// describe names f, for an error or a panic.
func (f FormField[T]) describe() string {
	return fmt.Sprintf("form field %q", f.name)
}

// check returns the mistakes in f's declaration: an empty name, a
// constraint that doesn't fit T, and a nil Pattern, each naming f.
func (f FormField[T]) check() []error {
	var errs []error
	if f.name == "" {
		errs = append(errs, errors.New("a form field's name is empty"))
	}
	for _, err := range f.constraints.Check() {
		errs = append(errs, fmt.Errorf("%s: %w", f.describe(), err))
	}
	return errs
}

// read returns f's value from the values the client sent for it, or the
// first rule it breaks, with T's zero value.
func (f FormField[T]) read(values []string) (T, *FieldError) {
	var zero T
	switch {
	case len(values) > 1:
		return zero, f.problem(Repeated, nil, fmt.Errorf("sent %d times", len(values)))
	case len(values) == 0 && f.required:
		return zero, f.problem(Missing, nil, errors.New("not sent"))
	case len(values) == 0:
		return zero, nil
	case values[0] == "" && f.required:
		return zero, f.problem(Missing, nil, errors.New("sent empty"))
	case values[0] == "":
		return zero, nil
	}
	raw := values[0]
	if reflect.TypeFor[T]().Kind() == reflect.Bool {
		// A checkbox sends its value attribute, "on" unless it says
		// otherwise, so a bool is true when present, whatever its value.
		raw = "true"
	}
	v, err := param.Parse[T](raw)
	if err != nil {
		return zero, f.problem(Malformed, nil, err)
	}
	if c, limit, err := f.constraints.Broken(v); c != param.None {
		return zero, f.problem(rules[c], limit, err)
	}
	return v, nil
}

// problem returns the *FieldError of f breaking rule.
func (f FormField[T]) problem(rule Rule, limit any, err error) *FieldError {
	return &FieldError{Field: f.name, Rule: rule, Limit: limit, Err: err}
}

// rules are the Rules of the constraints a value breaks.
var rules = map[param.Constraint]Rule{
	param.Enum:      NotInEnum,
	param.Pattern:   PatternMismatch,
	param.MinLength: TooShort,
	param.MaxLength: TooLong,
	param.Minimum:   TooSmall,
	param.Maximum:   TooLarge,
}

// Rule is the rule a form field broke, so the end user writes the message.
// A value is checked against the rules in the order they are declared, and
// breaks the first it fails: Repeated, then Missing, then Malformed, and
// then its constraints, NotInEnum to TooLarge. A field the client didn't
// send, or sent empty, breaks only Missing, and only when it is Required:
// none of its constraints is checked.
type Rule int

const (
	// Repeated is a field the client sent more than once. Its Limit is
	// nil.
	Repeated Rule = iota + 1

	// Missing is a Required field the client didn't send, or sent
	// empty. Its Limit is nil.
	Missing

	// Malformed is a value that doesn't parse as its field's T. Its
	// Limit is nil, and its Err is the parse error.
	Malformed

	// NotInEnum is a value that isn't one of its field's Enum. Its Limit
	// is the Enum's values, a []T.
	NotInEnum

	// PatternMismatch is a value that doesn't match its field's Pattern.
	// Its Limit is the Pattern, a *regexp.Regexp.
	PatternMismatch

	// TooShort is a value shorter than its field's MinLength. Its Limit
	// is the MinLength, an int.
	TooShort

	// TooLong is a value longer than its field's MaxLength. Its Limit is
	// the MaxLength, an int.
	TooLong

	// TooSmall is a value less than its field's Minimum. Its Limit is the
	// Minimum, a T.
	TooSmall

	// TooLarge is a value greater than its field's Maximum. Its Limit is
	// the Maximum, a T.
	TooLarge
)

// name returns r's name, such as "TooShort", for FieldError's text.
func (r Rule) name() string {
	names := [...]string{"Rule(0)", "Repeated", "Missing", "Malformed", "NotInEnum", "PatternMismatch", "TooShort", "TooLong", "TooSmall", "TooLarge"}
	if r < 0 || int(r) >= len(names) {
		return fmt.Sprintf("Rule(%d)", int(r))
	}
	return names[r]
}

// FieldError is a form field's problem: the rule its value broke. It
// isn't an error ReadForm returns, but the Form's, so the end user
// re-renders the form with a message of their own for it.
type FieldError struct {
	// Field is the field's name.
	Field string

	// Rule is the rule the field's value broke.
	Rule Rule

	// Limit is the value of the constraint the field's value broke, such
	// as 3 for MinLength(3), and nil for Missing, Malformed and Repeated.
	// Each Rule says its type.
	Limit any

	// Err is why, such as the parse error of a Malformed value.
	Err error
}

// Error returns the field's name, the rule and Err's text.
func (e *FieldError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("hypermedia: form field %q: %s", e.Field, e.Rule.name())
	}
	return fmt.Sprintf("hypermedia: form field %q: %s: %v", e.Field, e.Rule.name(), e.Err)
}

// ReadForm reads r's body as a form, application/x-www-form-urlencoded or
// multipart/form-data, whatever r's method, and checks each of fields. So
// a QUERY's body is read as a POST's is, which r.ParseForm doesn't do.
//
// Only the body is read: a value in r's URL query isn't a form field. A
// form already parsed, as by method-override middleware, is read from
// r.PostForm. An empty r.PostForm is a form already parsed only after
// r.ParseMultipartForm, or after r.ParseForm on a urlencoded POST, PUT or
// PATCH: on any other request r.ParseForm leaves it empty without reading
// the body. Otherwise ReadForm reads the body itself, and leaves the form
// in r.PostForm, as r.ParseForm would, so r.PostFormValue and a second
// ReadForm read it again.
//
// A multipart body's file parts, those whose Content-Disposition has a
// filename, even an empty one, are skipped, and no part of it is written
// to disk, so an upload is never spilled outside humus.Host. An end user
// who takes uploads streams them with r.MultipartReader instead.
//
// A body ReadForm can't read is a *RequestError, so a Site answers it with
// its ErrorPage, and doesn't record it:
//   - a body over http.MaxBytesHandler's limit has Status 413;
//   - a body that isn't a form has Status 415, and so does a request with
//     no Content-Type, an empty body included, since a form always says
//     its encoding;
//   - a urlencoded body that doesn't parse, or a multipart body with no
//     boundary or that doesn't parse, has Status 400;
//   - an error reading the body other than its limit, such as a body cut
//     short, has Status 400, and wraps the error.
//
// A field's problem isn't an error: ReadForm returns a nil error and a
// Form whose Valid is false, which holds every field's problem, at most one
// a field. Each Rule says when a value breaks it.
//
// A mistake in fields is the end user's, not the client's, so ReadForm
// returns an error that isn't a *RequestError, names every mistake, and
// reads nothing: a nil field, a field with an empty name, two fields of one
// name, a constraint that doesn't fit its field's type, such as MinLength
// on an int field or Minimum on a string field, and a nil Pattern. A Site
// records it as any handler's error, so the first test that sends the form
// finds it.
//
// ReadForm records nothing: a form with problems is an answer, not a
// failure.
func ReadForm(r *http.Request, fields ...AnyField) (*Form, error) {
	specs := make([]fieldSpec, 0, len(fields))
	var errs []error
	seen := make(map[string]bool)
	for i, field := range fields {
		if isNil(field) {
			errs = append(errs, fmt.Errorf("field %d is nil", i))
			continue
		}
		spec := field.formField()
		errs = append(errs, spec.check()...)
		if spec.name != "" && seen[spec.name] {
			errs = append(errs, fmt.Errorf("form field %q is given more than once", spec.name))
		}
		seen[spec.name] = true
		specs = append(specs, spec)
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("hypermedia: ReadForm: %w", errors.Join(errs...))
	}

	values, err := formValues(r)
	if err != nil {
		return nil, err
	}
	form := &Form{fields: make(map[string]formEntry, len(specs))}
	for _, spec := range specs {
		sent := values[spec.name]
		entry := formEntry{typ: spec.typ}
		if len(sent) > 0 {
			entry.raw = sent[0]
		}
		entry.value, entry.problem = spec.read(sent)
		form.fields[spec.name] = entry
	}
	return form, nil
}

// isNil reports whether field is nil, or a nil *FormField, whose
// formField would dereference it.
func isNil(field AnyField) bool {
	if field == nil {
		return true
	}
	rv := reflect.ValueOf(field)
	return rv.Kind() == reflect.Pointer && rv.IsNil()
}

// formValues returns the form in r's body, from r.PostForm when it is
// already parsed, and otherwise read from the body, and left in
// r.PostForm.
func formValues(r *http.Request) (url.Values, error) {
	if parsed(r) {
		return r.PostForm, nil
	}
	ct := r.Header.Get("Content-Type")
	if ct == "" {
		return nil, &RequestError{Status: http.StatusUnsupportedMediaType, Err: errors.New("the request has no Content-Type, so its body isn't a form")}
	}
	mediaType, params, err := mime.ParseMediaType(ct)
	if err != nil {
		return nil, &RequestError{Status: http.StatusUnsupportedMediaType, Err: fmt.Errorf("Content-Type %q: %w", ct, err)}
	}
	body := r.Body
	if body == nil {
		body = http.NoBody
	}
	var values url.Values
	switch mediaType {
	case "application/x-www-form-urlencoded":
		values, err = readURLEncoded(body)
	case "multipart/form-data":
		values, err = readMultipart(body, params["boundary"])
	default:
		return nil, &RequestError{Status: http.StatusUnsupportedMediaType, Err: fmt.Errorf("a body of %s isn't a form", mediaType)}
	}
	if err != nil {
		return nil, err
	}
	r.PostForm = values
	return values, nil
}

// parsed reports whether r.PostForm holds r's form, already parsed. An
// empty one does only when its body was read: by r.ParseMultipartForm, or
// by r.ParseForm, which reads only a urlencoded body, and only of a POST,
// PUT or PATCH. Otherwise r.ParseForm leaves it empty without reading the
// body.
func parsed(r *http.Request) bool {
	switch {
	case r.PostForm == nil:
		return false
	case len(r.PostForm) > 0, r.MultipartForm != nil:
		return true
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && mediaType == "application/x-www-form-urlencoded" &&
		(r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodPatch)
}

// readURLEncoded reads body as an application/x-www-form-urlencoded form.
func readURLEncoded(body io.Reader) (url.Values, error) {
	b, err := io.ReadAll(body)
	if err != nil {
		return nil, bodyError(err)
	}
	values, err := url.ParseQuery(string(b))
	if err != nil {
		return nil, &RequestError{Status: http.StatusBadRequest, Err: fmt.Errorf("the form doesn't parse: %w", err)}
	}
	return values, nil
}

// readMultipart reads body as a multipart/form-data form, and skips its
// file parts, which it writes nowhere.
func readMultipart(body io.Reader, boundary string) (url.Values, error) {
	if boundary == "" {
		return nil, &RequestError{Status: http.StatusBadRequest, Err: errors.New("the multipart form has no boundary")}
	}
	mr := multipart.NewReader(body, boundary)
	values := make(url.Values)
	for {
		part, err := mr.NextPart()
		// NextPart returns io.EOF itself only at the closing boundary. It
		// wraps an io.EOF that cuts the body short, which is malformed.
		if err == io.EOF {
			return values, nil
		}
		if err != nil {
			return nil, bodyError(err)
		}
		name := part.FormName()
		if name == "" || isFile(part) {
			// NextPart reads past what is left of the part.
			continue
		}
		b, err := io.ReadAll(part)
		if err != nil {
			return nil, bodyError(err)
		}
		values.Add(name, string(b))
	}
}

// isFile reports whether p is a file part: its Content-Disposition has a
// filename, even an empty one, as a file input with no file chosen sends.
func isFile(p *multipart.Part) bool {
	_, params, err := mime.ParseMediaType(p.Header.Get("Content-Disposition"))
	if err != nil {
		return false
	}
	_, ok := params["filename"]
	return ok
}

// bodyError returns err, from reading a form's body, as a *RequestError:
// 413 for a body over its limit, and 400 otherwise.
func bodyError(err error) *RequestError {
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		return &RequestError{Status: http.StatusRequestEntityTooLarge, Err: fmt.Errorf("the form is too large: %w", err)}
	}
	return &RequestError{Status: http.StatusBadRequest, Err: fmt.Errorf("reading the form: %w", err)}
}

// Form is a form ReadForm read: each of its fields' raw value, its value
// parsed, and its problem.
type Form struct {
	fields map[string]formEntry
}

// formEntry is one field of a Form.
type formEntry struct {
	typ     reflect.Type
	raw     string
	value   any
	problem *FieldError
}

// Valid reports whether no field has a problem.
func (f *Form) Valid() bool {
	for _, e := range f.fields {
		if e.problem != nil {
			return false
		}
	}
	return true
}

// Raw returns what the client sent for field, unparsed, so the end user
// re-renders it: "" for a field it didn't send, and the first value of a
// field it sent more than once. A field with a problem has its raw value
// too. Raw panics, naming field, if ReadForm wasn't given it.
func (f *Form) Raw(field AnyField) string {
	return f.entry("Raw", field).raw
}

// Problem returns field's problem, or nil for a field that is fine. It
// panics, naming field, if ReadForm wasn't given it.
func (f *Form) Problem(field AnyField) *FieldError {
	return f.entry("Problem", field).problem
}

// FieldFrom returns field's value, parsed and checked: T's zero value for
// a field the client didn't send, or sent empty, and for a field with a
// problem, so a value that broke a rule is never mistaken for one that
// passed. It panics, naming field, if ReadForm wasn't given it.
func FieldFrom[T ParamValue](f *Form, field FormField[T]) T {
	v, _ := f.entry("FieldFrom", field).value.(T)
	return v
}

// entry returns field's entry, and panics, naming op and field, if
// ReadForm wasn't given it, or was given a field of its name and another
// type.
func (f *Form) entry(op string, field AnyField) formEntry {
	if isNil(field) {
		panic(fmt.Sprintf("hypermedia: %s: the field is nil", op))
	}
	spec := field.formField()
	e, ok := f.fields[spec.name]
	if !ok {
		panic(fmt.Sprintf("hypermedia: %s(%q): ReadForm wasn't given form field %q", op, spec.name, spec.name))
	}
	if e.typ != spec.typ {
		panic(fmt.Sprintf("hypermedia: %s(%q): ReadForm was given form field %q of type %s, but the field has type %s", op, spec.name, spec.name, e.typ, spec.typ))
	}
	return e
}

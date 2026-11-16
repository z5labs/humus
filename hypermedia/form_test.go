// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package hypermedia_test

import (
	"bytes"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/z5labs/humus/hypermedia"
)

// Named types of each kind a form field can have, beside path_test.go's.
type (
	nickname string
	optIn    bool
	quantity int
	cents    int64
	share    float64
)

const urlencodedType = "application/x-www-form-urlencoded"

// urlencoded returns a request of method for target, with body as its
// urlencoded form.
func urlencoded(method, target, body string) *http.Request {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.Header.Set("Content-Type", urlencodedType)
	return r
}

// part is one part of a multipart form: a value, or, with a file name, a
// file.
type part struct {
	name, file, value string
}

// multipartBody returns parts as a multipart/form-data body, and its
// Content-Type.
func multipartBody(t *testing.T, parts ...part) (*bytes.Buffer, string) {
	t.Helper()
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	for _, p := range parts {
		var w io.Writer
		var err error
		if p.file != "" {
			w, err = mw.CreateFormFile(p.name, p.file)
		} else {
			w, err = mw.CreateFormField(p.name)
		}
		require.NoError(t, err)
		_, err = io.WriteString(w, p.value)
		require.NoError(t, err)
	}
	require.NoError(t, mw.Close())
	return &b, mw.FormDataContentType()
}

// multipartRequest returns a request of method for target, with parts as
// its multipart form.
func multipartRequest(t *testing.T, method, target string, parts ...part) *http.Request {
	t.Helper()
	body, ct := multipartBody(t, parts...)
	r := httptest.NewRequest(method, target, body)
	r.Header.Set("Content-Type", ct)
	return r
}

// readForm reads r's form against fields, and fails the test on an error.
func readForm(t *testing.T, r *http.Request, fields ...hypermedia.AnyField) *hypermedia.Form {
	t.Helper()
	form, err := hypermedia.ReadForm(r, fields...)
	require.NoError(t, err)
	return form
}

// requestErrorOf returns err as the *RequestError it must be.
func requestErrorOf(t *testing.T, err error) *hypermedia.RequestError {
	t.Helper()
	re, ok := errors.AsType[*hypermedia.RequestError](err)
	require.True(t, ok, "the error is a %T: %v", err, err)
	return re
}

func TestReadForm_reports_each_rule_with_its_field_limit_and_raw_value(t *testing.T) {
	sku := regexp.MustCompile(`^[A-Z]{3}-[0-9]+$`)
	cases := []struct {
		name  string
		field hypermedia.AnyField
		body  string
		rule  hypermedia.Rule
		limit any
		raw   string
		err   string
	}{
		{"Missing, not sent", hypermedia.Field[string]("nick").Required(), "other=1", hypermedia.Missing, nil, "", `hypermedia: form field "nick": Missing: not sent`},
		{"Missing, sent empty", hypermedia.Field[string]("nick").Required(), "nick=", hypermedia.Missing, nil, "", `hypermedia: form field "nick": Missing: sent empty`},
		{"Missing, a bool not sent", hypermedia.Field[bool]("terms").Required(), "", hypermedia.Missing, nil, "", `hypermedia: form field "terms": Missing: not sent`},
		{"Malformed", hypermedia.Field[int]("age").Required(), "age=ten", hypermedia.Malformed, nil, "ten", `hypermedia: form field "age": Malformed: "ten" isn't an integer of 64 bits`},
		{"Repeated", hypermedia.Field[string]("tag"), "tag=a&tag=b", hypermedia.Repeated, nil, "a", `hypermedia: form field "tag": Repeated: sent 2 times`},
		{"NotInEnum", hypermedia.Field[string]("color").Enum("red", "green"), "color=blue", hypermedia.NotInEnum, []string{"red", "green"}, "blue", `hypermedia: form field "color": NotInEnum: "blue" isn't one of "red", "green"`},
		{"PatternMismatch", hypermedia.Field[string]("sku").Pattern(sku), "sku=abc", hypermedia.PatternMismatch, sku, "abc", `hypermedia: form field "sku": PatternMismatch: "abc" doesn't match the pattern ^[A-Z]{3}-[0-9]+$`},
		{"TooShort", hypermedia.Field[nickname]("name").MinLength(3), "name=ab", hypermedia.TooShort, 3, "ab", `hypermedia: form field "name": TooShort: "ab" is shorter than 3 characters`},
		{"TooLong", hypermedia.Field[string]("bio").MaxLength(5), "bio=abcdef", hypermedia.TooLong, 5, "abcdef", `hypermedia: form field "bio": TooLong: "abcdef" is longer than 5 characters`},
		{"TooSmall", hypermedia.Field[quantity]("qty").Minimum(1), "qty=0", hypermedia.TooSmall, quantity(1), "0", `hypermedia: form field "qty": TooSmall: 0 is less than the minimum, 1`},
		{"TooLarge", hypermedia.Field[float64]("ratio").Maximum(1), "ratio=1.5", hypermedia.TooLarge, 1.0, "1.5", `hypermedia: form field "ratio": TooLarge: 1.5 is greater than the maximum, 1`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			form := readForm(t, urlencoded(http.MethodPost, "/contacts", tc.body), tc.field)
			require.False(t, form.Valid())
			require.Equal(t, tc.raw, form.Raw(tc.field))
			problem := form.Problem(tc.field)
			require.NotNil(t, problem)
			require.Equal(t, tc.rule, problem.Rule)
			require.Equal(t, tc.limit, problem.Limit)
			require.Error(t, problem.Err)
			require.EqualError(t, problem, tc.err)
		})
	}

	t.Run("each Field names its field", func(t *testing.T) {
		for _, tc := range cases {
			form := readForm(t, urlencoded(http.MethodPost, "/contacts", tc.body), tc.field)
			problem := form.Problem(tc.field)
			require.Contains(t, tc.err, `"`+problem.Field+`"`, tc.name)
		}
	})
}

func TestReadForm_reports_a_problem_as_a_valid_answer_not_an_error(t *testing.T) {
	age := hypermedia.Field[int]("age")
	form, err := hypermedia.ReadForm(urlencoded(http.MethodPost, "/contacts", "age=ten"), age)
	require.NoError(t, err)
	require.False(t, form.Valid())
	require.Equal(t, 0, hypermedia.FieldFrom(form, age), "a field with a problem gives T's zero value")
	require.Equal(t, "ten", form.Raw(age), "a field with a problem keeps its raw value")
}

func TestReadForm_reports_one_rule_a_value_breaks_in_the_documented_order(t *testing.T) {
	code := hypermedia.Field[string]("code").Required().Enum("abcdef", "AB").Pattern(regexp.MustCompile(`^[a-z]+$`)).MinLength(3).MaxLength(4)
	limit := hypermedia.Field[int]("limit").Enum(0, 50).Minimum(1).Maximum(10)
	cases := []struct {
		body string
		want hypermedia.Rule
	}{
		{"code=&code=", hypermedia.Repeated},
		{"code=x&code=y", hypermedia.Repeated},
		{"code=", hypermedia.Missing},
		{"code=ZZ", hypermedia.NotInEnum},
		{"code=AB", hypermedia.PatternMismatch},
		{"code=abcdef", hypermedia.TooLong},
		{"limit=x&limit=y", hypermedia.Repeated},
		{"limit=x", hypermedia.Malformed},
		{"limit=7", hypermedia.NotInEnum},
		{"limit=0", hypermedia.TooSmall},
		{"limit=50", hypermedia.TooLarge},
	}
	for _, tc := range cases {
		form := readForm(t, urlencoded(http.MethodPost, "/", tc.body), code, limit)
		var got []hypermedia.Rule
		for _, f := range []hypermedia.AnyField{code, limit} {
			if p := form.Problem(f); p != nil && (strings.HasPrefix(tc.body, p.Field+"=")) {
				got = append(got, p.Rule)
			}
		}
		require.Equal(t, []hypermedia.Rule{tc.want}, got, tc.body)
	}

	short := hypermedia.Field[string]("short").MinLength(3).MaxLength(1)
	form := readForm(t, urlencoded(http.MethodPost, "/", "short=ab"), short)
	require.Equal(t, hypermedia.TooShort, form.Problem(short).Rule, "TooShort comes before TooLong")
}

func TestReadForm_parses_every_field_type(t *testing.T) {
	var (
		s   = hypermedia.Field[string]("s")
		b   = hypermedia.Field[bool]("b")
		i   = hypermedia.Field[int]("i")
		i64 = hypermedia.Field[int64]("i64")
		f   = hypermedia.Field[float64]("f")
		ns  = hypermedia.Field[nickname]("ns")
		nb  = hypermedia.Field[optIn]("nb")
		ni  = hypermedia.Field[quantity]("ni")
		ni6 = hypermedia.Field[cents]("ni64")
		nf  = hypermedia.Field[share]("nf")
	)
	body := url.Values{
		"s": {"green tea"}, "b": {"on"}, "i": {"-3"}, "i64": {"9000000000"}, "f": {"1.5"},
		"ns": {"héllo"}, "nb": {"yes"}, "ni": {"12"}, "ni64": {"-9000000000"}, "nf": {"0.25"},
	}.Encode()
	form := readForm(t, urlencoded(http.MethodPost, "/", body), s, b, i, i64, f, ns, nb, ni, ni6, nf)
	require.True(t, form.Valid())
	require.Equal(t, "green tea", hypermedia.FieldFrom(form, s))
	require.True(t, hypermedia.FieldFrom(form, b))
	require.Equal(t, -3, hypermedia.FieldFrom(form, i))
	require.Equal(t, int64(9000000000), hypermedia.FieldFrom(form, i64))
	require.Equal(t, 1.5, hypermedia.FieldFrom(form, f))
	require.Equal(t, nickname("héllo"), hypermedia.FieldFrom(form, ns))
	require.Equal(t, optIn(true), hypermedia.FieldFrom(form, nb))
	require.Equal(t, quantity(12), hypermedia.FieldFrom(form, ni))
	require.Equal(t, cents(-9000000000), hypermedia.FieldFrom(form, ni6))
	require.Equal(t, share(0.25), hypermedia.FieldFrom(form, nf))
	require.Equal(t, "on", form.Raw(b))
	require.Equal(t, "9000000000", form.Raw(i64))

	for _, bad := range []struct {
		field hypermedia.AnyField
		body  string
	}{
		{i, "i=1.5"},
		{i64, "i64=99999999999999999999"},
		{f, "f=half"},
		{ni, "ni=twelve"},
		{ni6, "ni64=0x10"},
		{nf, "nf=a"},
	} {
		form := readForm(t, urlencoded(http.MethodPost, "/", bad.body), bad.field)
		require.Equal(t, hypermedia.Malformed, form.Problem(bad.field).Rule, bad.body)
	}
}

func TestReadForm_reads_a_checkbox(t *testing.T) {
	subscribe := hypermedia.Field[bool]("subscribe")
	name := hypermedia.Field[string]("name")

	form := readForm(t, urlencoded(http.MethodPost, "/", "name=Ada&subscribe=on"), name, subscribe)
	require.True(t, hypermedia.FieldFrom(form, subscribe), "a checked checkbox sends on")

	form = readForm(t, urlencoded(http.MethodPost, "/", "name=Ada"), name, subscribe)
	require.True(t, form.Valid())
	require.False(t, hypermedia.FieldFrom(form, subscribe), "an unchecked checkbox sends nothing")
	require.Empty(t, form.Raw(subscribe))
	require.Nil(t, form.Problem(subscribe))

	form = readForm(t, urlencoded(http.MethodPost, "/", "subscribe=off"), subscribe)
	require.True(t, hypermedia.FieldFrom(form, subscribe), "a checkbox is true whatever its value attribute")
}

func TestReadForm_checks_no_constraint_of_an_optional_field_not_sent(t *testing.T) {
	nick := hypermedia.Field[string]("nick").MinLength(3).Pattern(regexp.MustCompile(`^[a-z]+$`)).Enum("abc")
	age := hypermedia.Field[int]("age").Minimum(18)
	for _, body := range []string{"", "nick=&age="} {
		form := readForm(t, urlencoded(http.MethodPost, "/", body), nick, age)
		require.True(t, form.Valid(), body)
		require.Nil(t, form.Problem(nick))
		require.Nil(t, form.Problem(age))
		require.Empty(t, hypermedia.FieldFrom(form, nick))
		require.Zero(t, hypermedia.FieldFrom(form, age))
	}
}

func TestReadForm_reports_every_problem_of_a_form(t *testing.T) {
	var (
		name  = hypermedia.Field[string]("name").Required()
		email = hypermedia.Field[string]("email").Required().Pattern(regexp.MustCompile(`^[^@]+@[^@]+$`))
		age   = hypermedia.Field[int]("age").Minimum(18)
		tags  = hypermedia.Field[string]("tag")
		city  = hypermedia.Field[string]("city").MaxLength(20)
	)
	form := readForm(t, urlencoded(http.MethodPost, "/", "email=ada&age=17&tag=a&tag=b&city=London"), name, email, age, tags, city)
	require.False(t, form.Valid())
	got := map[string]hypermedia.Rule{}
	for _, f := range []hypermedia.AnyField{name, email, age, tags, city} {
		if p := form.Problem(f); p != nil {
			got[p.Field] = p.Rule
		}
	}
	require.Equal(t, map[string]hypermedia.Rule{
		"name":  hypermedia.Missing,
		"email": hypermedia.PatternMismatch,
		"age":   hypermedia.TooSmall,
		"tag":   hypermedia.Repeated,
	}, got)
	require.Equal(t, "London", hypermedia.FieldFrom(form, city))
	require.Equal(t, "ada", form.Raw(email))
	require.Equal(t, "17", form.Raw(age))
}

func TestReadForm_reads_the_body_whatever_the_method(t *testing.T) {
	name := hypermedia.Field[string]("name").Required()
	age := hypermedia.Field[int]("age")
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, "QUERY"} {
		t.Run(method+" urlencoded", func(t *testing.T) {
			form := readForm(t, urlencoded(method, "/contacts?name=query", "name=Ada&age=36"), name, age)
			require.True(t, form.Valid())
			require.Equal(t, "Ada", hypermedia.FieldFrom(form, name))
			require.Equal(t, 36, hypermedia.FieldFrom(form, age))
		})
		t.Run(method+" multipart", func(t *testing.T) {
			r := multipartRequest(t, method, "/contacts?name=query", part{name: "name", value: "Ada"}, part{name: "age", value: "36"})
			form := readForm(t, r, name, age)
			require.True(t, form.Valid())
			require.Equal(t, "Ada", hypermedia.FieldFrom(form, name))
			require.Equal(t, 36, hypermedia.FieldFrom(form, age))
		})
	}
}

func TestReadForm_reads_only_the_body_not_the_query(t *testing.T) {
	name := hypermedia.Field[string]("name").Required()
	form := readForm(t, urlencoded("QUERY", "/contacts?name=Ada", ""), name)
	require.Equal(t, hypermedia.Missing, form.Problem(name).Rule)

	form = readForm(t, urlencoded(http.MethodPost, "/contacts?name=Ada", "name=Grace"), name)
	require.Equal(t, "Grace", hypermedia.FieldFrom(form, name), "a query value isn't a second value of the field")
	require.Nil(t, form.Problem(name))
}

func TestReadForm_reads_a_form_already_parsed_from_PostForm(t *testing.T) {
	name := hypermedia.Field[string]("name").Required()

	t.Run("by method-override middleware", func(t *testing.T) {
		r := urlencoded(http.MethodPost, "/contacts/7", "_method=PUT&name=Ada")
		require.NoError(t, r.ParseForm())
		r.Method = r.PostForm.Get("_method")
		form := readForm(t, r, name)
		require.Equal(t, "Ada", hypermedia.FieldFrom(form, name))
	})

	t.Run("by ParseMultipartForm", func(t *testing.T) {
		r := multipartRequest(t, http.MethodPost, "/contacts", part{name: "name", value: "Ada"})
		require.NoError(t, r.ParseMultipartForm(1<<20))
		form := readForm(t, r, name)
		require.Equal(t, "Ada", hypermedia.FieldFrom(form, name))
	})

	t.Run("set by the end user", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPut, "/contacts/7", nil)
		r.PostForm = url.Values{"name": {"Grace"}}
		form := readForm(t, r, name)
		require.Equal(t, "Grace", hypermedia.FieldFrom(form, name))
	})

	t.Run("left empty by ParseForm on a multipart POST, whose body it doesn't read", func(t *testing.T) {
		r := multipartRequest(t, http.MethodPost, "/contacts", part{name: "name", value: "Ada"})
		require.NoError(t, r.ParseForm())
		require.Empty(t, r.PostForm)
		form := readForm(t, r, name)
		require.Equal(t, "Ada", hypermedia.FieldFrom(form, name))
	})

	t.Run("left empty by ParseForm on a POST that isn't a form", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/contacts", strings.NewReader(`{"name":"Ada"}`))
		r.Header.Set("Content-Type", "application/json")
		require.NoError(t, r.ParseForm())
		require.Empty(t, r.PostForm)
		_, err := hypermedia.ReadForm(r, name)
		require.Equal(t, http.StatusUnsupportedMediaType, requestErrorOf(t, err).Status)
	})

	t.Run("left empty by ParseForm on an empty urlencoded POST", func(t *testing.T) {
		r := urlencoded(http.MethodPost, "/contacts", "")
		require.NoError(t, r.ParseForm())
		r.Body = io.NopCloser(iotest.ErrReader(errors.New("the body was already read")))
		form := readForm(t, r, name)
		require.Equal(t, hypermedia.Missing, form.Problem(name).Rule)
	})

	t.Run("left empty by ParseMultipartForm", func(t *testing.T) {
		r := multipartRequest(t, http.MethodPost, "/contacts", part{name: "avatar", file: "a.png", value: "png"})
		require.NoError(t, r.ParseMultipartForm(1<<20))
		require.Empty(t, r.PostForm)
		form := readForm(t, r, name)
		require.Equal(t, hypermedia.Missing, form.Problem(name).Rule)
	})

	t.Run("left empty by ParseForm on a QUERY, whose body it doesn't read", func(t *testing.T) {
		r := urlencoded("QUERY", "/contacts", "name=Ada")
		require.NoError(t, r.ParseForm())
		require.Empty(t, r.PostForm)
		form := readForm(t, r, name)
		require.Equal(t, "Ada", hypermedia.FieldFrom(form, name))
	})

	t.Run("left by ReadForm, for a second ReadForm and PostFormValue", func(t *testing.T) {
		r := urlencoded("QUERY", "/contacts", "name=Ada")
		readForm(t, r, name)
		form := readForm(t, r, name)
		require.Equal(t, "Ada", hypermedia.FieldFrom(form, name))
		require.Equal(t, "Ada", r.PostFormValue("name"))
	})
}

func TestReadForm_skips_an_upload_and_writes_nothing_to_disk(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	require.Equal(t, tmp, os.TempDir())

	name := hypermedia.Field[string]("name").Required()
	avatar := hypermedia.Field[string]("avatar")
	age := hypermedia.Field[int]("age")
	r := multipartRequest(t, http.MethodPost, "/contacts",
		part{name: "name", value: "Ada"},
		part{name: "avatar", file: "ada.png", value: strings.Repeat("\x89PNG", 1<<18)},
		part{name: "age", value: "36"},
	)
	form := readForm(t, r, name, avatar, age)
	require.True(t, form.Valid())
	require.Equal(t, "Ada", hypermedia.FieldFrom(form, name))
	require.Equal(t, 36, hypermedia.FieldFrom(form, age))
	require.Empty(t, form.Raw(avatar), "a file part isn't a field's value")
	require.Nil(t, r.MultipartForm, "the file isn't kept")
	require.Equal(t, url.Values{"name": {"Ada"}, "age": {"36"}}, r.PostForm)

	t.Run("a file input with no file chosen", func(t *testing.T) {
		var b bytes.Buffer
		mw := multipart.NewWriter(&b)
		require.NoError(t, mw.WriteField("name", "Ada"))
		w, err := mw.CreatePart(textproto.MIMEHeader{
			"Content-Disposition": {`form-data; name="avatar"; filename=""`},
			"Content-Type":        {"application/octet-stream"},
		})
		require.NoError(t, err)
		_, err = io.WriteString(w, "")
		require.NoError(t, err)
		require.NoError(t, mw.Close())
		r := httptest.NewRequest(http.MethodPost, "/contacts", &b)
		r.Header.Set("Content-Type", mw.FormDataContentType())

		form := readForm(t, r, name, avatar.Required())
		require.Equal(t, hypermedia.Missing, form.Problem(avatar).Rule, "the part is a file, skipped, not an empty value")
		require.Equal(t, url.Values{"name": {"Ada"}}, r.PostForm)
	})

	entries, err := os.ReadDir(tmp)
	require.NoError(t, err)
	require.Empty(t, entries, "the file isn't written to a temporary file")
}

func TestReadForm_answers_a_body_it_cant_read_with_a_RequestError(t *testing.T) {
	name := hypermedia.Field[string]("name")
	cases := []struct {
		name   string
		r      func() *http.Request
		status int
	}{
		{"no Content-Type", func() *http.Request {
			return httptest.NewRequest(http.MethodPost, "/", strings.NewReader("name=Ada"))
		}, http.StatusUnsupportedMediaType},
		{"no Content-Type and an empty body", func() *http.Request {
			return httptest.NewRequest(http.MethodPost, "/", nil)
		}, http.StatusUnsupportedMediaType},
		{"a body that isn't a form", func() *http.Request {
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":"Ada"}`))
			r.Header.Set("Content-Type", "application/json")
			return r
		}, http.StatusUnsupportedMediaType},
		{"a Content-Type that doesn't parse", func() *http.Request {
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("name=Ada"))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded; =")
			return r
		}, http.StatusUnsupportedMediaType},
		{"a urlencoded body that doesn't parse", func() *http.Request {
			return urlencoded(http.MethodPost, "/", "name=%zz")
		}, http.StatusBadRequest},
		{"a multipart body with no boundary", func() *http.Request {
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("name=Ada"))
			r.Header.Set("Content-Type", "multipart/form-data")
			return r
		}, http.StatusBadRequest},
		{"a multipart body that doesn't parse", func() *http.Request {
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("name=Ada"))
			r.Header.Set("Content-Type", "multipart/form-data; boundary=x")
			return r
		}, http.StatusBadRequest},
		{"an error reading a urlencoded body", func() *http.Request {
			r := httptest.NewRequest(http.MethodPost, "/", io.NopCloser(iotest.ErrReader(io.ErrUnexpectedEOF)))
			r.Header.Set("Content-Type", urlencodedType)
			return r
		}, http.StatusBadRequest},
		{"an error reading a multipart part", func() *http.Request {
			body, ct := multipartBody(t, part{name: "name", value: strings.Repeat("a", 100)})
			r := httptest.NewRequest(http.MethodPost, "/", io.MultiReader(io.LimitReader(body, int64(body.Len()-60)), iotest.ErrReader(io.ErrUnexpectedEOF)))
			r.Header.Set("Content-Type", ct)
			return r
		}, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			form, err := hypermedia.ReadForm(tc.r(), name)
			require.Nil(t, form)
			re := requestErrorOf(t, err)
			require.Equal(t, tc.status, re.Status)
			require.Error(t, re.Err)
		})
	}

	t.Run("an error reading the body wraps it", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/", io.NopCloser(iotest.ErrReader(io.ErrUnexpectedEOF)))
		r.Header.Set("Content-Type", urlencodedType)
		_, err := hypermedia.ReadForm(r, name)
		require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	})

	t.Run("a body over its limit", func(t *testing.T) {
		for _, r := range []*http.Request{
			urlencoded(http.MethodPost, "/", "name="+strings.Repeat("a", 64)),
			multipartRequest(t, http.MethodPost, "/", part{name: "name", value: strings.Repeat("a", 64)}),
			multipartRequest(t, http.MethodPost, "/", part{name: "avatar", file: "a.png", value: strings.Repeat("a", 64)}),
		} {
			r.Body = http.MaxBytesReader(httptest.NewRecorder(), r.Body, 32)
			_, err := hypermedia.ReadForm(r, name)
			require.Equal(t, http.StatusRequestEntityTooLarge, requestErrorOf(t, err).Status)
			var mbe *http.MaxBytesError
			require.ErrorAs(t, err, &mbe)
		}
	})
}

func TestReadForm_reports_a_mistake_in_its_fields_before_reading_the_body(t *testing.T) {
	r := urlencoded(http.MethodPost, "/", "age=7")
	_, err := hypermedia.ReadForm(r,
		hypermedia.Field[int]("age").MinLength(1),
		hypermedia.Field[string]("name").Minimum("a"),
		hypermedia.Field[bool]("vip").Pattern(regexp.MustCompile(`^on$`)).Maximum(true),
		hypermedia.Field[string]("sku").Pattern(nil),
		hypermedia.Field[string](""),
		hypermedia.Field[string]("email"),
		hypermedia.Field[int]("email"),
		nil,
		(*hypermedia.FormField[string])(nil),
	)
	require.Error(t, err)
	_, isRequestError := errors.AsType[*hypermedia.RequestError](err)
	require.False(t, isRequestError, "a mistake is the end user's, not the client's")
	for _, want := range []string{
		"hypermedia: ReadForm: ",
		`form field "age": MinLength needs a string type, but its type is int`,
		`form field "name": Minimum needs a number type, but its type is string`,
		`form field "vip": Pattern needs a string type, but its type is bool`,
		`form field "vip": Maximum needs a number type, but its type is bool`,
		`form field "sku": its Pattern is nil`,
		"a form field's name is empty",
		`form field "email" is given more than once`,
		"field 7 is nil",
		"field 8 is nil",
	} {
		require.ErrorContains(t, err, want)
	}
	require.Nil(t, r.PostForm, "the body isn't read")
}

func TestForm_panics_for_a_field_ReadForm_wasnt_given(t *testing.T) {
	name := hypermedia.Field[string]("name")
	form := readForm(t, urlencoded(http.MethodPost, "/", "name=Ada&age=7"), name)
	other := hypermedia.Field[int]("age")
	retyped := hypermedia.Field[int]("name")

	require.PanicsWithValue(t, `hypermedia: Raw("age"): ReadForm wasn't given form field "age"`, func() { form.Raw(other) })
	require.PanicsWithValue(t, `hypermedia: Problem("age"): ReadForm wasn't given form field "age"`, func() { _ = form.Problem(other) })
	require.PanicsWithValue(t, `hypermedia: FieldFrom("age"): ReadForm wasn't given form field "age"`, func() { hypermedia.FieldFrom(form, other) })
	require.PanicsWithValue(t, `hypermedia: FieldFrom("name"): ReadForm was given form field "name" of type string, but the field has type int`, func() { hypermedia.FieldFrom(form, retyped) })
	require.PanicsWithValue(t, `hypermedia: Raw: the field is nil`, func() { form.Raw(nil) })
	require.PanicsWithValue(t, `hypermedia: Raw: the field is nil`, func() { form.Raw((*hypermedia.FormField[string])(nil)) })

	require.Equal(t, "Ada", form.Raw(name.Required().MaxLength(9)), "a field is its name and type, not its constraints")
}

func TestFormField_constraints_return_a_copy(t *testing.T) {
	base := hypermedia.Field[int]("n")
	values := []int{1, 2}
	small := base.Maximum(10).Enum(values...)
	required := base.Required()
	values[0] = 50 // Enum keeps its own copy of its values.

	form := readForm(t, urlencoded(http.MethodPost, "/", "n=50"), small)
	require.Equal(t, hypermedia.NotInEnum, form.Problem(small).Rule)
	require.Equal(t, []int{1, 2}, form.Problem(small).Limit)

	form = readForm(t, urlencoded(http.MethodPost, "/", "n=50"), base)
	require.Equal(t, 50, hypermedia.FieldFrom(form, base), "base has no constraints")

	form = readForm(t, urlencoded(http.MethodPost, "/", ""), base)
	require.True(t, form.Valid(), "Required on required leaves base optional")
	form = readForm(t, urlencoded(http.MethodPost, "/", ""), required)
	require.Equal(t, hypermedia.Missing, form.Problem(required).Rule)

	name := hypermedia.Field[string]("s")
	for _, f := range []hypermedia.FormField[string]{name.MinLength(3), name.MaxLength(1), name.Pattern(regexp.MustCompile(`^z$`))} {
		form := readForm(t, urlencoded(http.MethodPost, "/", "s=ab"), f)
		require.False(t, form.Valid())
	}
	form = readForm(t, urlencoded(http.MethodPost, "/", "s=ab"), name)
	require.True(t, form.Valid(), "constraints on copies leave name unchanged")
}

func TestFieldError_Error_names_the_field_and_the_rule(t *testing.T) {
	require.EqualError(t, &hypermedia.FieldError{Field: "nick", Rule: hypermedia.TooShort, Limit: 3, Err: errors.New("too short")}, `hypermedia: form field "nick": TooShort: too short`)
	require.EqualError(t, &hypermedia.FieldError{Field: "nick", Rule: hypermedia.Missing}, `hypermedia: form field "nick": Missing`)
	require.EqualError(t, &hypermedia.FieldError{Field: "nick"}, `hypermedia: form field "nick": Rule(0)`)
	require.EqualError(t, &hypermedia.FieldError{Field: "nick", Rule: 42}, `hypermedia: form field "nick": Rule(42)`)
}

func TestSite_answers_a_form_it_cant_read_with_its_ErrorPage_and_records_nothing(t *testing.T) {
	name := hypermedia.Field[string]("name").Required()
	save := func(w http.ResponseWriter, r *http.Request) error {
		form, err := hypermedia.ReadForm(r, name)
		if err != nil {
			return err
		}
		if !form.Valid() {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, err := io.WriteString(w, "fix "+form.Problem(name).Field)
			return err
		}
		_, err = io.WriteString(w, "saved "+hypermedia.FieldFrom(form, name))
		return err
	}
	routes := []hypermedia.Route{
		hypermedia.NewRoute(http.MethodPut, One, save),
		hypermedia.NewRoute("QUERY", List, save),
	}

	serve := func(rs *recorded, r *http.Request) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		http.MaxBytesHandler(http.HandlerFunc(rs.serve), 32).ServeHTTP(w, r)
		return w
	}
	quiet := func(t *testing.T, rs *recorded) {
		t.Helper()
		require.Equal(t, sdktrace.Status{Code: codes.Unset}, rs.span().Status())
		require.Empty(t, rs.span().Events())
		require.Empty(t, rs.records(), "no ERROR record")
	}

	t.Run("a body over http.MaxBytesHandler's limit is a 413", func(t *testing.T) {
		rs := newRecorded(t, nil, routes...)
		w := serve(rs, urlencoded(http.MethodPut, "/contacts/7", "name="+strings.Repeat("a", 64)))
		require.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
		require.Equal(t, "error page", w.Body.String())
		require.Len(t, rs.pages, 1)
		require.Equal(t, http.StatusRequestEntityTooLarge, requestErrorOf(t, rs.pages[0]).Status)
		quiet(t, rs)
	})

	t.Run("a body that isn't a form is a 415", func(t *testing.T) {
		rs := newRecorded(t, nil, routes...)
		r := httptest.NewRequest("QUERY", "/contacts", strings.NewReader(`{}`))
		r.Header.Set("Content-Type", "application/json")
		w := serve(rs, r)
		require.Equal(t, http.StatusUnsupportedMediaType, w.Code)
		require.Equal(t, "error page", w.Body.String())
		require.Len(t, rs.pages, 1)
		require.Equal(t, http.StatusUnsupportedMediaType, requestErrorOf(t, rs.pages[0]).Status)
		quiet(t, rs)
	})

	t.Run("a form with problems is an answer, and isn't recorded", func(t *testing.T) {
		rs := newRecorded(t, nil, routes...)
		w := serve(rs, urlencoded("QUERY", "/contacts", "name="))
		require.Equal(t, http.StatusUnprocessableEntity, w.Code)
		require.Equal(t, "fix name", w.Body.String())
		require.Empty(t, rs.pages)
		quiet(t, rs)
	})

	t.Run("a valid form", func(t *testing.T) {
		rs := newRecorded(t, nil, routes...)
		w := serve(rs, urlencoded(http.MethodPut, "/contacts/7", "name=Ada"))
		require.Equal(t, http.StatusOK, w.Code)
		require.Equal(t, "saved Ada", w.Body.String())
		quiet(t, rs)
	})
}

func TestReadForm_reads_a_nil_body_as_an_empty_form(t *testing.T) {
	name := hypermedia.Field[string]("name").Required()
	r, err := http.NewRequest(http.MethodPost, "/contacts", nil)
	require.NoError(t, err)
	require.Nil(t, r.Body)
	r.Header.Set("Content-Type", urlencodedType)
	form := readForm(t, r, name)
	require.Equal(t, hypermedia.Missing, form.Problem(name).Rule)
}

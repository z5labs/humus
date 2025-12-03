// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rest

import (
	"context"
	"errors"
	"html/template"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/swaggest/openapi-go/openapi3"
)

// Test utilities and mock types

// HTMLData is a simple data structure for HTML templates
type HTMLData struct {
	Title   string
	Message string
}

// NestedHTMLData is a complex nested type for testing
type NestedHTMLData struct {
	Parent string
	Child  struct {
		Name  string
		Value int
	}
}

// PetListHTML is a slice type for testing array responses
type PetListHTML []PetHTML

type PetHTML struct {
	Name string
	Age  int
}

// Priority 1: Core Happy Path Tests

func TestHTMLTemplateResponse_WriteResponse(t *testing.T) {
	t.Run("writes valid HTML with correct content-type header", func(t *testing.T) {
		tmpl := template.Must(template.New("test").Parse("<h1>{{.Title}}</h1><p>{{.Message}}</p>"))

		producer := ProducerFunc[HTMLData](func(ctx context.Context) (*HTMLData, error) {
			return &HTMLData{Title: "Hello", Message: "World"}, nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodGet,
				BasePath("/test"),
				ProduceHTML(producer, tmpl),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		resp, err := http.Get(srv.URL + "/test")
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, "text/html; charset=utf-8", resp.Header.Get("Content-Type"))

		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.Contains(t, string(body), "<h1>Hello</h1>")
		assert.Contains(t, string(body), "<p>World</p>")
	})

	t.Run("renders template with simple data structures", func(t *testing.T) {
		tmpl := template.Must(template.New("simple").Parse("<div>{{.Message}}</div>"))

		producer := ProducerFunc[HTMLData](func(ctx context.Context) (*HTMLData, error) {
			return &HTMLData{Message: "Simple test"}, nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodGet,
				BasePath("/simple"),
				ProduceHTML(producer, tmpl),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		resp, err := http.Get(srv.URL + "/simple")
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)

		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.Equal(t, "<div>Simple test</div>", string(body))
	})

	t.Run("renders template with nested/complex data structures", func(t *testing.T) {
		tmpl := template.Must(template.New("nested").Parse(
			"<div>{{.Parent}}</div><span>{{.Child.Name}}: {{.Child.Value}}</span>",
		))

		producer := ProducerFunc[NestedHTMLData](func(ctx context.Context) (*NestedHTMLData, error) {
			nested := &NestedHTMLData{
				Parent: "parent-value",
			}
			nested.Child.Name = "child-name"
			nested.Child.Value = 42
			return nested, nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodGet,
				BasePath("/nested"),
				ProduceHTML(producer, tmpl),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		resp, err := http.Get(srv.URL + "/nested")
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)

		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.Contains(t, string(body), "<div>parent-value</div>")
		assert.Contains(t, string(body), "<span>child-name: 42</span>")
	})

	t.Run("returns http.StatusOK status code", func(t *testing.T) {
		tmpl := template.Must(template.New("status").Parse("<p>OK</p>"))

		producer := ProducerFunc[HTMLData](func(ctx context.Context) (*HTMLData, error) {
			return &HTMLData{}, nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodGet,
				BasePath("/status"),
				ProduceHTML(producer, tmpl),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		resp, err := http.Get(srv.URL + "/status")
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})

	t.Run("HTML escaping works correctly (XSS protection)", func(t *testing.T) {
		tmpl := template.Must(template.New("xss").Parse("<div>{{.Message}}</div>"))

		producer := ProducerFunc[HTMLData](func(ctx context.Context) (*HTMLData, error) {
			return &HTMLData{Message: "<script>alert('xss')</script>"}, nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodGet,
				BasePath("/xss"),
				ProduceHTML(producer, tmpl),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		resp, err := http.Get(srv.URL + "/xss")
		require.NoError(t, err)
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)

		// html/template should escape the script tag
		assert.Contains(t, string(body), "&lt;script&gt;")
		assert.Contains(t, string(body), "&lt;/script&gt;")
		assert.NotContains(t, string(body), "<script>alert")
	})

	t.Run("handles template execution with missing field", func(t *testing.T) {
		// Template references a field that doesn't exist - html/template renders empty string for missing fields
		tmpl := template.Must(template.New("missing").Parse("<div>{{.NonExistentField}}</div>"))

		producer := ProducerFunc[HTMLData](func(ctx context.Context) (*HTMLData, error) {
			return &HTMLData{Message: "test"}, nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodGet,
				BasePath("/missing-field"),
				ProduceHTML(producer, tmpl),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		resp, err := http.Get(srv.URL + "/missing-field")
		require.NoError(t, err)
		defer resp.Body.Close()

		// Template execution with missing field renders empty string (no error thrown)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		// The field doesn't exist, so it renders as empty
		assert.Contains(t, string(body), "<div>")
	})
}

func TestHTMLTemplateResponse_Spec(t *testing.T) {
	t.Run("generates valid OpenAPI response spec", func(t *testing.T) {
		var resp HTMLTemplateResponse[HTMLData]
		status, spec, err := resp.Spec()

		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, status)
		require.NotNil(t, spec.Response)
		assert.Contains(t, spec.Response.Content, "text/html")
	})

	t.Run("spec contains text/html content type", func(t *testing.T) {
		var resp HTMLTemplateResponse[HTMLData]
		status, spec, err := resp.Spec()

		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, status)
		require.NotNil(t, spec.Response)

		mediaType, exists := spec.Response.Content["text/html"]
		assert.True(t, exists)
		require.NotNil(t, mediaType.Schema)
		require.NotNil(t, mediaType.Schema.Schema)
		require.NotNil(t, mediaType.Schema.Schema.Type)
		assert.Equal(t, openapi3.SchemaTypeString, *mediaType.Schema.Schema.Type)
	})

	t.Run("returns correct status code (200)", func(t *testing.T) {
		var resp HTMLTemplateResponse[HTMLData]
		status, _, err := resp.Spec()

		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, status)
	})
}

func TestReturnHTML(t *testing.T) {
	t.Run("wraps handler and returns HTMLTemplateResponse", func(t *testing.T) {
		tmpl := template.Must(template.New("test").Parse("<p>{{.Message}}</p>"))

		handler := HandlerFunc[EmptyRequest, HTMLData](func(ctx context.Context, req *EmptyRequest) (*HTMLData, error) {
			return &HTMLData{Message: "wrapped"}, nil
		})

		wrapped := ReturnHTML[EmptyRequest, HTMLData](handler, tmpl)

		resp, err := wrapped.Handle(context.Background(), &EmptyRequest{})

		require.NoError(t, err)
		require.NotNil(t, resp)
		require.NotNil(t, resp.Template)
		require.NotNil(t, resp.Data)
		assert.Equal(t, "wrapped", resp.Data.Message)
	})

	t.Run("propagates handler errors correctly", func(t *testing.T) {
		tmpl := template.Must(template.New("test").Parse("<p>{{.Message}}</p>"))

		expectedErr := errors.New("handler error")
		handler := HandlerFunc[EmptyRequest, HTMLData](func(ctx context.Context, req *EmptyRequest) (*HTMLData, error) {
			return nil, expectedErr
		})

		wrapped := ReturnHTML[EmptyRequest, HTMLData](handler, tmpl)

		resp, err := wrapped.Handle(context.Background(), &EmptyRequest{})

		assert.ErrorIs(t, err, expectedErr)
		assert.Nil(t, resp)
	})

	t.Run("template is correctly passed to response", func(t *testing.T) {
		tmpl := template.Must(template.New("test").Parse("<h1>{{.Title}}</h1>"))

		handler := HandlerFunc[EmptyRequest, HTMLData](func(ctx context.Context, req *EmptyRequest) (*HTMLData, error) {
			return &HTMLData{Title: "Test Title"}, nil
		})

		wrapped := ReturnHTML[EmptyRequest, HTMLData](handler, tmpl)

		resp, err := wrapped.Handle(context.Background(), &EmptyRequest{})

		require.NoError(t, err)
		require.NotNil(t, resp)
		assert.Equal(t, tmpl, resp.Template)
	})
}

func TestProduceHTML(t *testing.T) {
	t.Run("creates handler that only produces HTML response", func(t *testing.T) {
		tmpl := template.Must(template.New("produce").Parse("<div>{{.Message}}</div>"))

		producer := ProducerFunc[HTMLData](func(ctx context.Context) (*HTMLData, error) {
			return &HTMLData{Message: "produced content"}, nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodGet,
				BasePath("/produce"),
				ProduceHTML(producer, tmpl),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		resp, err := http.Get(srv.URL + "/produce")
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, "text/html; charset=utf-8", resp.Header.Get("Content-Type"))

		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.Equal(t, "<div>produced content</div>", string(body))
	})

	t.Run("works with GET endpoints (no request body)", func(t *testing.T) {
		tmpl := template.Must(template.New("get").Parse("<p>GET response</p>"))

		producer := ProducerFunc[HTMLData](func(ctx context.Context) (*HTMLData, error) {
			return &HTMLData{}, nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodGet,
				BasePath("/get-endpoint"),
				ProduceHTML(producer, tmpl),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		resp, err := http.Get(srv.URL + "/get-endpoint")
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)

		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.Equal(t, "<p>GET response</p>", string(body))
	})

	t.Run("propagates producer errors", func(t *testing.T) {
		tmpl := template.Must(template.New("error").Parse("<p>Error</p>"))

		producer := ProducerFunc[HTMLData](func(ctx context.Context) (*HTMLData, error) {
			return nil, errors.New("producer error")
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodGet,
				BasePath("/error"),
				ProduceHTML(producer, tmpl),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		resp, err := http.Get(srv.URL + "/error")
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	})

	t.Run("template execution with producer data", func(t *testing.T) {
		tmpl := template.Must(template.New("data").Parse("<h2>{{.Title}}</h2><p>{{.Message}}</p>"))

		producer := ProducerFunc[HTMLData](func(ctx context.Context) (*HTMLData, error) {
			return &HTMLData{
				Title:   "Dynamic Title",
				Message: "Dynamic Message",
			}, nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodGet,
				BasePath("/dynamic"),
				ProduceHTML(producer, tmpl),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		resp, err := http.Get(srv.URL + "/dynamic")
		require.NoError(t, err)
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)

		assert.Contains(t, string(body), "<h2>Dynamic Title</h2>")
		assert.Contains(t, string(body), "<p>Dynamic Message</p>")
	})
}

// Integration tests

func TestHTMLTemplateResponse_Integration(t *testing.T) {
	t.Run("end-to-end HTML rendering via httptest server", func(t *testing.T) {
		tmpl := template.Must(template.New("integration").Parse(`
<!DOCTYPE html>
<html>
<head><title>{{.Title}}</title></head>
<body>
<h1>{{.Title}}</h1>
<p>{{.Message}}</p>
</body>
</html>
`))

		producer := ProducerFunc[HTMLData](func(ctx context.Context) (*HTMLData, error) {
			return &HTMLData{
				Title:   "Integration Test",
				Message: "Full page rendering",
			}, nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodGet,
				BasePath("/page"),
				ProduceHTML(producer, tmpl),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		resp, err := http.Get(srv.URL + "/page")
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, "text/html; charset=utf-8", resp.Header.Get("Content-Type"))

		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)

		htmlContent := string(body)
		assert.Contains(t, htmlContent, "<!DOCTYPE html>")
		assert.Contains(t, htmlContent, "<title>Integration Test</title>")
		assert.Contains(t, htmlContent, "<h1>Integration Test</h1>")
		assert.Contains(t, htmlContent, "<p>Full page rendering</p>")
	})

	t.Run("HTML response can be parsed and validated", func(t *testing.T) {
		tmpl := template.Must(template.New("parse").Parse("<ul>{{range .}}<li>{{.Name}}: {{.Age}}</li>{{end}}</ul>"))

		producer := ProducerFunc[PetListHTML](func(ctx context.Context) (*PetListHTML, error) {
			pets := PetListHTML{
				{Name: "Fluffy", Age: 3},
				{Name: "Spot", Age: 5},
			}
			return &pets, nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodGet,
				BasePath("/pets"),
				ProduceHTML(producer, tmpl),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		resp, err := http.Get(srv.URL + "/pets")
		require.NoError(t, err)
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)

		htmlContent := string(body)
		assert.Contains(t, htmlContent, "<ul>")
		assert.Contains(t, htmlContent, "<li>Fluffy: 3</li>")
		assert.Contains(t, htmlContent, "<li>Spot: 5</li>")
		assert.Contains(t, htmlContent, "</ul>")
	})

	t.Run("multiple concurrent requests (race condition testing)", func(t *testing.T) {
		tmpl := template.Must(template.New("concurrent").Parse("<p>{{.Message}}</p>"))

		producer := ProducerFunc[HTMLData](func(ctx context.Context) (*HTMLData, error) {
			return &HTMLData{Message: "concurrent"}, nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodGet,
				BasePath("/concurrent"),
				ProduceHTML(producer, tmpl),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		// Make multiple concurrent requests
		done := make(chan bool, 10)
		for i := 0; i < 10; i++ {
			go func() {
				resp, err := http.Get(srv.URL + "/concurrent")
				if err == nil {
					resp.Body.Close()
				}
				done <- true
			}()
		}

		// Wait for all requests to complete
		for i := 0; i < 10; i++ {
			<-done
		}

		// If we got here without race detector complaints, test passes
	})
}

func TestHTMLTemplateResponse_EdgeCases(t *testing.T) {
	t.Run("empty template renders empty output", func(t *testing.T) {
		tmpl := template.Must(template.New("empty").Parse(""))

		producer := ProducerFunc[HTMLData](func(ctx context.Context) (*HTMLData, error) {
			return &HTMLData{Message: "ignored"}, nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodGet,
				BasePath("/empty-template"),
				ProduceHTML(producer, tmpl),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		resp, err := http.Get(srv.URL + "/empty-template")
		require.NoError(t, err)
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.Empty(t, string(body))
	})

	t.Run("template with only whitespace", func(t *testing.T) {
		tmpl := template.Must(template.New("whitespace").Parse("   \n\t  "))

		producer := ProducerFunc[HTMLData](func(ctx context.Context) (*HTMLData, error) {
			return &HTMLData{}, nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodGet,
				BasePath("/whitespace"),
				ProduceHTML(producer, tmpl),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		resp, err := http.Get(srv.URL + "/whitespace")
		require.NoError(t, err)
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.True(t, strings.TrimSpace(string(body)) == "")
	})

	t.Run("special HTML characters are escaped", func(t *testing.T) {
		tmpl := template.Must(template.New("special").Parse("<p>{{.Message}}</p>"))

		producer := ProducerFunc[HTMLData](func(ctx context.Context) (*HTMLData, error) {
			return &HTMLData{Message: `<>&"'`}, nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodGet,
				BasePath("/special"),
				ProduceHTML(producer, tmpl),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		resp, err := http.Get(srv.URL + "/special")
		require.NoError(t, err)
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)

		htmlContent := string(body)
		assert.Contains(t, htmlContent, "&lt;")   // <
		assert.Contains(t, htmlContent, "&gt;")   // >
		assert.Contains(t, htmlContent, "&amp;")  // &
		assert.Contains(t, htmlContent, "&#34;")  // "
		assert.Contains(t, htmlContent, "&#39;")  // '
	})
}

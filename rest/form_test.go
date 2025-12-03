// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rest

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type testFormRequest struct {
	Name     string  `form:"name"`
	Email    string  `form:"email"`
	Age      int     `form:"age"`
	Score    float64 `form:"score"`
	Active   bool    `form:"active"`
	NoTag    string
	unexported string
}

func TestFormRequest_Spec(t *testing.T) {
	t.Run("should generate OpenAPI spec with form-urlencoded content type", func(t *testing.T) {
		var fr FormRequest[testFormRequest]

		spec, err := fr.Spec()
		require.NoError(t, err)
		require.NotNil(t, spec.RequestBody)
		require.NotNil(t, spec.RequestBody.Required)
		require.True(t, *spec.RequestBody.Required)

		mediaType, ok := spec.RequestBody.Content["application/x-www-form-urlencoded"]
		require.True(t, ok, "should have application/x-www-form-urlencoded content type")
		require.NotNil(t, mediaType.Schema)
	})
}

func TestFormRequest_ReadRequest(t *testing.T) {
	t.Run("should parse form data successfully", func(t *testing.T) {
		formData := url.Values{}
		formData.Set("name", "John Doe")
		formData.Set("email", "john@example.com")
		formData.Set("age", "30")
		formData.Set("score", "95.5")
		formData.Set("active", "true")

		req := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(formData.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

		var fr FormRequest[testFormRequest]
		err := fr.ReadRequest(context.Background(), req)
		require.NoError(t, err)
		require.Equal(t, "John Doe", fr.inner.Name)
		require.Equal(t, "john@example.com", fr.inner.Email)
		require.Equal(t, 30, fr.inner.Age)
		require.Equal(t, 95.5, fr.inner.Score)
		require.True(t, fr.inner.Active)
	})

	t.Run("should use field name when form tag is missing", func(t *testing.T) {
		formData := url.Values{}
		formData.Set("notag", "test value")

		req := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(formData.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

		var fr FormRequest[testFormRequest]
		err := fr.ReadRequest(context.Background(), req)
		require.NoError(t, err)
		require.Equal(t, "test value", fr.inner.NoTag)
	})

	t.Run("should skip unexported fields", func(t *testing.T) {
		formData := url.Values{}
		formData.Set("unexported", "should be ignored")

		req := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(formData.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

		var fr FormRequest[testFormRequest]
		err := fr.ReadRequest(context.Background(), req)
		require.NoError(t, err)
		require.Empty(t, fr.inner.unexported)
	})

	t.Run("should handle missing form values", func(t *testing.T) {
		formData := url.Values{}
		// Only set name, leave other fields empty

		req := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(formData.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

		var fr FormRequest[testFormRequest]
		err := fr.ReadRequest(context.Background(), req)
		require.NoError(t, err)
		require.Empty(t, fr.inner.Name)
		require.Empty(t, fr.inner.Email)
		require.Zero(t, fr.inner.Age)
	})

	t.Run("should return BadRequestError for form parse failure", func(t *testing.T) {
		// Create a request with invalid form encoding (e.g., malformed percent encoding)
		req := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader("field=%ZZ"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

		var fr FormRequest[testFormRequest]
		err := fr.ReadRequest(context.Background(), req)
		require.Error(t, err)

		var badReqErr BadRequestError
		require.ErrorAs(t, err, &badReqErr)
	})

	t.Run("should return error for invalid int value", func(t *testing.T) {
		formData := url.Values{}
		formData.Set("age", "not a number")

		req := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(formData.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

		var fr FormRequest[testFormRequest]
		err := fr.ReadRequest(context.Background(), req)
		require.Error(t, err)
		require.Contains(t, err.Error(), "failed to set field Age")
	})

	t.Run("should return error for invalid float value", func(t *testing.T) {
		formData := url.Values{}
		formData.Set("score", "not a float")

		req := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(formData.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

		var fr FormRequest[testFormRequest]
		err := fr.ReadRequest(context.Background(), req)
		require.Error(t, err)
		require.Contains(t, err.Error(), "failed to set field Score")
	})

	t.Run("should return error for invalid bool value", func(t *testing.T) {
		formData := url.Values{}
		formData.Set("active", "not a bool")

		req := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(formData.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

		var fr FormRequest[testFormRequest]
		err := fr.ReadRequest(context.Background(), req)
		require.Error(t, err)
		require.Contains(t, err.Error(), "failed to set field Active")
	})
}

func TestConsumeFormHandler_Handle(t *testing.T) {
	t.Run("should pass inner value to handler", func(t *testing.T) {
		called := false
		handler := HandlerFunc[testFormRequest, string](
			func(ctx context.Context, req *testFormRequest) (*string, error) {
				called = true
				require.Equal(t, "test", req.Name)
				result := "success"
				return &result, nil
			},
		)

		consumeHandler := ConsumeForm(handler)

		req := &FormRequest[testFormRequest]{
			inner: testFormRequest{Name: "test"},
		}

		resp, err := consumeHandler.Handle(context.Background(), req)
		require.NoError(t, err)
		require.NotNil(t, resp)
		require.Equal(t, "success", *resp)
		require.True(t, called)
	})
}

func TestConsumeOnlyForm(t *testing.T) {
	t.Run("should create handler that consumes form without response", func(t *testing.T) {
		called := false
		consumer := ConsumerFunc[testFormRequest](
			func(ctx context.Context, req *testFormRequest) error {
				called = true
				require.Equal(t, "test", req.Name)
				return nil
			},
		)

		handler := ConsumeOnlyForm(consumer)

		req := &FormRequest[testFormRequest]{
			inner: testFormRequest{Name: "test"},
		}

		resp, err := handler.Handle(context.Background(), req)
		require.NoError(t, err)
		require.NotNil(t, resp)
		require.True(t, called)
	})
}

func TestHandleForm(t *testing.T) {
	t.Run("should create handler that consumes form and returns JSON", func(t *testing.T) {
		type response struct {
			Message string `json:"message"`
		}

		handler := HandlerFunc[testFormRequest, response](
			func(ctx context.Context, req *testFormRequest) (*response, error) {
				return &response{Message: req.Name}, nil
			},
		)

		formHandler := HandleForm(handler)

		req := &FormRequest[testFormRequest]{
			inner: testFormRequest{Name: "test"},
		}

		resp, err := formHandler.Handle(context.Background(), req)
		require.NoError(t, err)
		require.NotNil(t, resp)
	})
}

type testAllTypesForm struct {
	String  string  `form:"string"`
	Int     int     `form:"int"`
	Int8    int8    `form:"int8"`
	Int16   int16   `form:"int16"`
	Int32   int32   `form:"int32"`
	Int64   int64   `form:"int64"`
	Uint    uint    `form:"uint"`
	Uint8   uint8   `form:"uint8"`
	Uint16  uint16  `form:"uint16"`
	Uint32  uint32  `form:"uint32"`
	Uint64  uint64  `form:"uint64"`
	Bool    bool    `form:"bool"`
	Float32 float32 `form:"float32"`
	Float64 float64 `form:"float64"`
}

func TestDecodeForm_AllTypes(t *testing.T) {
	t.Run("should decode all supported types", func(t *testing.T) {
		formData := map[string][]string{
			"string":  {"hello"},
			"int":     {"-123"},
			"int8":    {"-12"},
			"int16":   {"-1234"},
			"int32":   {"-12345"},
			"int64":   {"-123456"},
			"uint":    {"123"},
			"uint8":   {"12"},
			"uint16":  {"1234"},
			"uint32":  {"12345"},
			"uint64":  {"123456"},
			"bool":    {"true"},
			"float32": {"12.34"},
			"float64": {"123.456"},
		}

		var result testAllTypesForm
		err := decodeForm(formData, &result)
		require.NoError(t, err)
		require.Equal(t, "hello", result.String)
		require.Equal(t, -123, result.Int)
		require.Equal(t, int8(-12), result.Int8)
		require.Equal(t, int16(-1234), result.Int16)
		require.Equal(t, int32(-12345), result.Int32)
		require.Equal(t, int64(-123456), result.Int64)
		require.Equal(t, uint(123), result.Uint)
		require.Equal(t, uint8(12), result.Uint8)
		require.Equal(t, uint16(1234), result.Uint16)
		require.Equal(t, uint32(12345), result.Uint32)
		require.Equal(t, uint64(123456), result.Uint64)
		require.True(t, result.Bool)
		require.Equal(t, float32(12.34), result.Float32)
		require.Equal(t, 123.456, result.Float64)
	})
}

func TestDecodeForm_Errors(t *testing.T) {
	t.Run("should return error when dst is not a pointer", func(t *testing.T) {
		formData := map[string][]string{}
		var result testFormRequest
		err := decodeForm(formData, result)
		require.Error(t, err)
		require.Contains(t, err.Error(), "dst must be a pointer")
	})

	t.Run("should return error when dst is not a pointer to struct", func(t *testing.T) {
		formData := map[string][]string{}
		var result string
		err := decodeForm(formData, &result)
		require.Error(t, err)
		require.Contains(t, err.Error(), "dst must be a pointer to a struct")
	})
}

type testUnsupportedTypeForm struct {
	Unsupported complex64 `form:"unsupported"`
}

func TestSetField_UnsupportedType(t *testing.T) {
	t.Run("should return error for unsupported field types", func(t *testing.T) {
		formData := map[string][]string{
			"unsupported": {"value"},
		}

		var result testUnsupportedTypeForm
		err := decodeForm(formData, &result)
		require.Error(t, err)
		require.Contains(t, err.Error(), "unsupported field type")
	})
}

func TestSetField_IntegerOverflow(t *testing.T) {
	t.Run("should set value within range for int8", func(t *testing.T) {
		type int8Form struct {
			Value int8 `form:"value"`
		}

		formData := map[string][]string{
			"value": {"127"}, // Max value for int8
		}

		var result int8Form
		err := decodeForm(formData, &result)
		require.NoError(t, err)
		require.Equal(t, int8(127), result.Value)
	})

	t.Run("should overflow for values larger than int8 max", func(t *testing.T) {
		// Note: Go's reflect.Value.SetInt will silently truncate values that
		// overflow the target type. This is expected behavior.
		type int8Form struct {
			Value int8 `form:"value"`
		}

		formData := map[string][]string{
			"value": {"1000"}, // Larger than int8 max (127)
		}

		var result int8Form
		err := decodeForm(formData, &result)
		// No error is returned, but value is truncated
		require.NoError(t, err)
		// The value 1000 will be truncated when stored in int8
		// Just verify it's not the max value we'd expect if properly validated
		require.NotEqual(t, int8(127), result.Value)
	})
}

func TestSetField_UintNegative(t *testing.T) {
	t.Run("should return error for negative uint values", func(t *testing.T) {
		type uintForm struct {
			Value uint `form:"value"`
		}

		formData := map[string][]string{
			"value": {"-1"},
		}

		var result uintForm
		err := decodeForm(formData, &result)
		require.Error(t, err)
		require.Contains(t, err.Error(), "failed to set field Value")
	})
}

// Test type aliases
type UserID string
type Age int
type Score float64

func TestSetField_TypeAliases(t *testing.T) {
	t.Run("should decode type aliased primitives", func(t *testing.T) {
		type aliasForm struct {
			UserID UserID `form:"user_id"`
			Age    Age    `form:"age"`
			Score  Score  `form:"score"`
		}

		formData := map[string][]string{
			"user_id": {"user-123"},
			"age":     {"25"},
			"score":   {"98.5"},
		}

		var result aliasForm
		err := decodeForm(formData, &result)
		require.NoError(t, err)
		require.Equal(t, UserID("user-123"), result.UserID)
		require.Equal(t, Age(25), result.Age)
		require.Equal(t, Score(98.5), result.Score)
	})
}

func TestSetField_TimeTime(t *testing.T) {
	t.Run("should decode time.Time from RFC3339 format", func(t *testing.T) {
		type timeForm struct {
			Timestamp time.Time `form:"timestamp"`
		}

		expectedTime := time.Date(2025, 12, 2, 15, 30, 0, 0, time.UTC)
		formData := map[string][]string{
			"timestamp": {expectedTime.Format(time.RFC3339)},
		}

		var result timeForm
		err := decodeForm(formData, &result)
		require.NoError(t, err)
		require.True(t, expectedTime.Equal(result.Timestamp))
	})

	t.Run("should return error for invalid time format", func(t *testing.T) {
		type timeForm struct {
			Timestamp time.Time `form:"timestamp"`
		}

		formData := map[string][]string{
			"timestamp": {"not-a-valid-time"},
		}

		var result timeForm
		err := decodeForm(formData, &result)
		require.Error(t, err)
		require.Contains(t, err.Error(), "failed to set field Timestamp")
	})
}

func TestSetField_TimeDuration(t *testing.T) {
	t.Run("should decode time.Duration from string", func(t *testing.T) {
		type durationForm struct {
			Timeout time.Duration `form:"timeout"`
		}

		formData := map[string][]string{
			"timeout": {"5m30s"},
		}

		var result durationForm
		err := decodeForm(formData, &result)
		require.NoError(t, err)
		require.Equal(t, 5*time.Minute+30*time.Second, result.Timeout)
	})

	t.Run("should decode various duration formats", func(t *testing.T) {
		type durationForm struct {
			D1 time.Duration `form:"d1"`
			D2 time.Duration `form:"d2"`
			D3 time.Duration `form:"d3"`
		}

		formData := map[string][]string{
			"d1": {"1h"},
			"d2": {"500ms"},
			"d3": {"1h30m45s"},
		}

		var result durationForm
		err := decodeForm(formData, &result)
		require.NoError(t, err)
		require.Equal(t, time.Hour, result.D1)
		require.Equal(t, 500*time.Millisecond, result.D2)
		require.Equal(t, time.Hour+30*time.Minute+45*time.Second, result.D3)
	})

	t.Run("should return error for invalid duration format", func(t *testing.T) {
		type durationForm struct {
			Timeout time.Duration `form:"timeout"`
		}

		formData := map[string][]string{
			"timeout": {"invalid-duration"},
		}

		var result durationForm
		err := decodeForm(formData, &result)
		require.Error(t, err)
		require.Contains(t, err.Error(), "failed to set field Timeout")
	})
}

// Custom type implementing encoding.TextUnmarshaler
type CustomID struct {
	Prefix string
	ID     int
}

func (c *CustomID) UnmarshalText(text []byte) error {
	// Parse format: "prefix:123"
	parts := strings.Split(string(text), ":")
	if len(parts) != 2 {
		return fmt.Errorf("invalid CustomID format: %s", text)
	}
	c.Prefix = parts[0]
	var err error
	_, err = fmt.Sscanf(parts[1], "%d", &c.ID)
	return err
}

func TestSetField_TextUnmarshaler(t *testing.T) {
	t.Run("should use TextUnmarshaler interface when available", func(t *testing.T) {
		type customForm struct {
			ID CustomID `form:"id"`
		}

		formData := map[string][]string{
			"id": {"user:12345"},
		}

		var result customForm
		err := decodeForm(formData, &result)
		require.NoError(t, err)
		require.Equal(t, "user", result.ID.Prefix)
		require.Equal(t, 12345, result.ID.ID)
	})

	t.Run("should return error from TextUnmarshaler", func(t *testing.T) {
		type customForm struct {
			ID CustomID `form:"id"`
		}

		formData := map[string][]string{
			"id": {"invalid-format"},
		}

		var result customForm
		err := decodeForm(formData, &result)
		require.Error(t, err)
		require.Contains(t, err.Error(), "failed to set field ID")
	})
}

func TestSetField_MixedAdvancedTypes(t *testing.T) {
	t.Run("should decode form with mixed advanced types", func(t *testing.T) {
		type complexForm struct {
			UserID    UserID        `form:"user_id"`
			CreatedAt time.Time     `form:"created_at"`
			Timeout   time.Duration `form:"timeout"`
			CustomID  CustomID      `form:"custom_id"`
			Name      string        `form:"name"`
			Age       int           `form:"age"`
		}

		timestamp := time.Date(2025, 12, 2, 15, 30, 0, 0, time.UTC)
		formData := map[string][]string{
			"user_id":    {"user-456"},
			"created_at": {timestamp.Format(time.RFC3339)},
			"timeout":    {"10m"},
			"custom_id":  {"order:789"},
			"name":       {"John Doe"},
			"age":        {"30"},
		}

		var result complexForm
		err := decodeForm(formData, &result)
		require.NoError(t, err)
		require.Equal(t, UserID("user-456"), result.UserID)
		require.True(t, timestamp.Equal(result.CreatedAt))
		require.Equal(t, 10*time.Minute, result.Timeout)
		require.Equal(t, "order", result.CustomID.Prefix)
		require.Equal(t, 789, result.CustomID.ID)
		require.Equal(t, "John Doe", result.Name)
		require.Equal(t, 30, result.Age)
	})
}

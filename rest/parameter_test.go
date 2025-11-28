// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPathParamValue(t *testing.T) {
	t.Run("extracts single path parameter", func(t *testing.T) {
		var capturedID string

		producer := ProducerFunc[TestResponse](func(ctx context.Context) (*TestResponse, error) {
			capturedID = PathParamValue(ctx, "id")
			return &TestResponse{Output: capturedID}, nil
		})

		api := NewApi("Test", "v1",
			Operation(http.MethodGet, BasePath("/users").Param("id"), ProduceJson(producer)),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		resp, err := http.Get(srv.URL + "/users/123")
		require.NoError(t, err)
		defer resp.Body.Close()

		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Equal(t, "123", capturedID)

		var body TestResponse
		err = json.NewDecoder(resp.Body).Decode(&body)
		require.NoError(t, err)
		require.Equal(t, "123", body.Output)
	})

	t.Run("extracts multiple path parameters", func(t *testing.T) {
		var capturedOrgID, capturedRepoID string

		producer := ProducerFunc[TestResponse](func(ctx context.Context) (*TestResponse, error) {
			capturedOrgID = PathParamValue(ctx, "orgId")
			capturedRepoID = PathParamValue(ctx, "repoId")
			return &TestResponse{Output: capturedOrgID + "/" + capturedRepoID}, nil
		})

		api := NewApi("Test", "v1",
			Operation(
				http.MethodGet,
				BasePath("/orgs").Param("orgId").Segment("repos").Param("repoId"),
				ProduceJson(producer),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		resp, err := http.Get(srv.URL + "/orgs/myorg/repos/myrepo")
		require.NoError(t, err)
		defer resp.Body.Close()

		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Equal(t, "myorg", capturedOrgID)
		require.Equal(t, "myrepo", capturedRepoID)

		var body TestResponse
		err = json.NewDecoder(resp.Body).Decode(&body)
		require.NoError(t, err)
		require.Equal(t, "myorg/myrepo", body.Output)
	})

	t.Run("extracts parameter at beginning of path", func(t *testing.T) {
		var capturedID string

		producer := ProducerFunc[TestResponse](func(ctx context.Context) (*TestResponse, error) {
			capturedID = PathParamValue(ctx, "id")
			return &TestResponse{Output: capturedID}, nil
		})

		api := NewApi("Test", "v1",
			Operation(
				http.MethodGet,
				BasePath("/").Param("id").Segment("details"),
				ProduceJson(producer),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		resp, err := http.Get(srv.URL + "/user123/details")
		require.NoError(t, err)
		defer resp.Body.Close()

		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Equal(t, "user123", capturedID)
	})

	t.Run("extracts parameter at end of path", func(t *testing.T) {
		var capturedID string

		producer := ProducerFunc[TestResponse](func(ctx context.Context) (*TestResponse, error) {
			capturedID = PathParamValue(ctx, "id")
			return &TestResponse{Output: capturedID}, nil
		})

		api := NewApi("Test", "v1",
			Operation(
				http.MethodGet,
				BasePath("/api/users").Param("id"),
				ProduceJson(producer),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		resp, err := http.Get(srv.URL + "/api/users/456")
		require.NoError(t, err)
		defer resp.Body.Close()

		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Equal(t, "456", capturedID)
	})

	t.Run("works with ProduceJson handler", func(t *testing.T) {
		var capturedID string

		producer := ProducerFunc[TestResponse](func(ctx context.Context) (*TestResponse, error) {
			capturedID = PathParamValue(ctx, "id")
			return &TestResponse{Output: "user-" + capturedID, Result: 42}, nil
		})

		api := NewApi("Test", "v1",
			Operation(http.MethodGet, BasePath("/users").Param("id"), ProduceJson(producer)),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		resp, err := http.Get(srv.URL + "/users/789")
		require.NoError(t, err)
		defer resp.Body.Close()

		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Equal(t, "789", capturedID)

		var body TestResponse
		err = json.NewDecoder(resp.Body).Decode(&body)
		require.NoError(t, err)
		require.Equal(t, "user-789", body.Output)
		require.Equal(t, 42, body.Result)
	})

	t.Run("works with ConsumeOnlyJson handler", func(t *testing.T) {
		var capturedID string
		var capturedInput string

		consumer := ConsumerFunc[TestRequest](func(ctx context.Context, req *TestRequest) error {
			capturedID = PathParamValue(ctx, "id")
			capturedInput = req.Input
			return nil
		})

		api := NewApi("Test", "v1",
			Operation(
				http.MethodPost,
				BasePath("/webhooks").Param("id"),
				ConsumeOnlyJson(consumer),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		body := jsonBody(t, &TestRequest{Input: "webhook-data", Count: 10})
		resp, err := http.Post(srv.URL+"/webhooks/hook123", "application/json", body)
		require.NoError(t, err)
		defer resp.Body.Close()

		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Equal(t, "hook123", capturedID)
		require.Equal(t, "webhook-data", capturedInput)
	})

	t.Run("works with HandleJson handler", func(t *testing.T) {
		var capturedID string

		handler := HandlerFunc[TestRequest, TestResponse](func(ctx context.Context, req *TestRequest) (*TestResponse, error) {
			capturedID = PathParamValue(ctx, "id")
			return &TestResponse{Output: capturedID + "-" + req.Input, Result: req.Count}, nil
		})

		api := NewApi("Test", "v1",
			Operation(
				http.MethodPost,
				BasePath("/items").Param("id"),
				HandleJson(handler),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		body := jsonBody(t, &TestRequest{Input: "test", Count: 5})
		resp, err := http.Post(srv.URL+"/items/item999", "application/json", body)
		require.NoError(t, err)
		defer resp.Body.Close()

		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Equal(t, "item999", capturedID)

		var respBody TestResponse
		err = json.NewDecoder(resp.Body).Decode(&respBody)
		require.NoError(t, err)
		require.Equal(t, "item999-test", respBody.Output)
		require.Equal(t, 5, respBody.Result)
	})

	t.Run("handles nested resources with multiple parameters", func(t *testing.T) {
		var capturedUserID, capturedPostID, capturedCommentID string

		producer := ProducerFunc[TestResponse](func(ctx context.Context) (*TestResponse, error) {
			capturedUserID = PathParamValue(ctx, "userId")
			capturedPostID = PathParamValue(ctx, "postId")
			capturedCommentID = PathParamValue(ctx, "commentId")
			return &TestResponse{Output: capturedUserID + "/" + capturedPostID + "/" + capturedCommentID}, nil
		})

		api := NewApi("Test", "v1",
			Operation(
				http.MethodGet,
				BasePath("/users").Param("userId").Segment("posts").Param("postId").Segment("comments").Param("commentId"),
				ProduceJson(producer),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		resp, err := http.Get(srv.URL + "/users/u1/posts/p2/comments/c3")
		require.NoError(t, err)
		defer resp.Body.Close()

		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Equal(t, "u1", capturedUserID)
		require.Equal(t, "p2", capturedPostID)
		require.Equal(t, "c3", capturedCommentID)
	})

	t.Run("extracts parameters with special characters", func(t *testing.T) {
		testCases := []struct {
			name      string
			paramName string
			urlValue  string
		}{
			{
				name:      "hyphenated parameter name",
				paramName: "user-id",
				urlValue:  "abc-123",
			},
			{
				name:      "underscored parameter name",
				paramName: "user_id",
				urlValue:  "def_456",
			},
			{
				name:      "numeric value",
				paramName: "id",
				urlValue:  "999",
			},
			{
				name:      "alphanumeric value",
				paramName: "id",
				urlValue:  "user123abc",
			},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				var capturedValue string

				producer := ProducerFunc[TestResponse](func(ctx context.Context) (*TestResponse, error) {
					capturedValue = PathParamValue(ctx, tc.paramName)
					return &TestResponse{Output: capturedValue}, nil
				})

				api := NewApi("Test", "v1",
					Operation(
						http.MethodGet,
						BasePath("/items").Param(tc.paramName),
						ProduceJson(producer),
					),
				)

				srv := httptest.NewServer(api)
				defer srv.Close()

				resp, err := http.Get(srv.URL + "/items/" + tc.urlValue)
				require.NoError(t, err)
				defer resp.Body.Close()

				require.Equal(t, http.StatusOK, resp.StatusCode)
				require.Equal(t, tc.urlValue, capturedValue)
			})
		}
	})

	t.Run("isolates parameters across multiple handlers", func(t *testing.T) {
		var capturedUserID, capturedItemID string

		userProducer := ProducerFunc[TestResponse](func(ctx context.Context) (*TestResponse, error) {
			capturedUserID = PathParamValue(ctx, "id")
			return &TestResponse{Output: "user-" + capturedUserID}, nil
		})

		itemProducer := ProducerFunc[TestResponse](func(ctx context.Context) (*TestResponse, error) {
			capturedItemID = PathParamValue(ctx, "id")
			return &TestResponse{Output: "item-" + capturedItemID}, nil
		})

		api := NewApi("Test", "v1",
			Operation(http.MethodGet, BasePath("/users").Param("id"), ProduceJson(userProducer)),
			Operation(http.MethodGet, BasePath("/items").Param("id"), ProduceJson(itemProducer)),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		resp1, err := http.Get(srv.URL + "/users/user123")
		require.NoError(t, err)
		defer resp1.Body.Close()
		require.Equal(t, http.StatusOK, resp1.StatusCode)
		require.Equal(t, "user123", capturedUserID)

		capturedUserID = ""

		resp2, err := http.Get(srv.URL + "/items/item456")
		require.NoError(t, err)
		defer resp2.Body.Close()
		require.Equal(t, http.StatusOK, resp2.StatusCode)
		require.Equal(t, "item456", capturedItemID)

		require.Empty(t, capturedUserID)
	})

	t.Run("works independently from query parameters", func(t *testing.T) {
		var capturedPathID string
		var capturedQueryParam string

		producer := ProducerFunc[TestResponse](func(ctx context.Context) (*TestResponse, error) {
			capturedPathID = PathParamValue(ctx, "id")
			return &TestResponse{Output: capturedPathID}, nil
		})

		api := NewApi("Test", "v1",
			Operation(
				http.MethodGet,
				BasePath("/users").Param("id"),
				ProduceJson(producer),
				QueryParam("format", Required()),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		resp, err := http.Get(srv.URL + "/users/user789?format=json")
		require.NoError(t, err)
		defer resp.Body.Close()

		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Equal(t, "user789", capturedPathID)
		require.Empty(t, capturedQueryParam)
	})
}

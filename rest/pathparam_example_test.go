// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rest_test

import (
	"context"
	"fmt"
	"net/http"

	"github.com/z5labs/humus/rest"
	"github.com/z5labs/humus/rest/rpc"
)

// GetUserHandler demonstrates using PathParamValue with rpc.ProduceJson
type GetUserHandler struct{}

type GetUserResponse struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (h *GetUserHandler) Produce(ctx context.Context) (*GetUserResponse, error) {
	// Extract the user ID from the path parameter
	userID := rest.PathParamValue(ctx, "id")

	// In a real application, you would fetch the user from a database
	return &GetUserResponse{
		ID:   userID,
		Name: fmt.Sprintf("User %s", userID),
	}, nil
}

// ExamplePathParamValue demonstrates how to use PathParamValue to extract path parameters
// from context when using rpc.ProduceJson handlers.
func ExamplePathParamValue() {
	handler := &GetUserHandler{}

	// Register a GET endpoint with a path parameter
	apiOpt := rest.Handle(
		http.MethodGet,
		rest.BasePath("/users").Param("id", rest.Required()),
		rpc.ProduceJson(handler),
	)

	api := rest.NewApi("User API", "v1.0.0", apiOpt)

	// In a real application, you would start the server with:
	// http.ListenAndServe(":8080", api)

	fmt.Println(api != nil)
	// Output: true
}

// UpdateUserHandler demonstrates using PathParamValue with rpc.HandleJson
type UpdateUserHandler struct{}

type UpdateUserRequest struct {
	Name string `json:"name"`
}

type UpdateUserResponse struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (h *UpdateUserHandler) Handle(ctx context.Context, req *UpdateUserRequest) (*UpdateUserResponse, error) {
	// Extract the user ID from the path parameter
	userID := rest.PathParamValue(ctx, "id")

	// In a real application, you would update the user in a database
	return &UpdateUserResponse{
		ID:   userID,
		Name: req.Name,
	}, nil
}

// ExamplePathParamValue_withMultipleParams demonstrates using PathParamValue
// to extract multiple path parameters from a nested resource URL.
func ExamplePathParamValue_withMultipleParams() {
	type GetPostHandler struct{}

	type GetPostResponse struct {
		UserID string `json:"user_id"`
		PostID string `json:"post_id"`
		Title  string `json:"title"`
	}

	handler := rpc.ProducerFunc[GetPostResponse](func(ctx context.Context) (*GetPostResponse, error) {
		// Extract both path parameters
		userID := rest.PathParamValue(ctx, "userId")
		postID := rest.PathParamValue(ctx, "postId")

		return &GetPostResponse{
			UserID: userID,
			PostID: postID,
			Title:  fmt.Sprintf("Post %s by User %s", postID, userID),
		}, nil
	})

	// Register endpoint with multiple path parameters
	apiOpt := rest.Handle(
		http.MethodGet,
		rest.BasePath("/users").Param("userId").Segment("posts").Param("postId"),
		rpc.ProduceJson(handler),
	)

	api := rest.NewApi("Blog API", "v1.0.0", apiOpt)

	fmt.Println(api != nil)
	// Output: true
}

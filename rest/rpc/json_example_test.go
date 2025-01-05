// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rpc

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
)

type echoHandler struct{}

type echoRequest struct {
	Msg string `json:"msg"`
}

type echoResponse struct {
	Msg string `json:"msg"`
}

func (h *echoHandler) Handle(ctx context.Context, req *echoRequest) (*echoResponse, error) {
	return &echoResponse{Msg: req.Msg}, nil
}

func Example_consuming_and_producing_json() {
	op := NewOperation(
		ConsumesJson(
			ProducesJson(
				&echoHandler{},
			),
		),
	)

	s := httptest.NewServer(op)
	defer s.Close()

	resp, err := http.Post(s.URL, "application/json", strings.NewReader(`{"msg":"hello world"}`))
	if err != nil {
		fmt.Println(err)
		return
	}
	defer resp.Body.Close()

	contentType := resp.Header.Get("Content-Type")
	fmt.Println(contentType)

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Println(err)
		return
	}

	fmt.Println(string(b))
	// Output: application/json
	// {"msg":"hello world"}
}

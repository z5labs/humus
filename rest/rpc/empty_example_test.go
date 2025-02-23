// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rpc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/swaggest/openapi-go/openapi3"
)

type msgRequest struct {
	Msg string `json:"msg"`
}

func (*msgRequest) Spec() (*openapi3.RequestBody, error) {
	return &openapi3.RequestBody{}, nil
}

func (mr *msgRequest) ReadRequest(ctx context.Context, r *http.Request) error {
	defer r.Body.Close()

	dec := json.NewDecoder(r.Body)
	return dec.Decode(mr)
}

func ExampleReturnNothing() {
	c := ConsumerFunc[msgRequest](func(_ context.Context, req *msgRequest) error {
		fmt.Println(req.Msg)
		return nil
	})

	h := ReturnNothing(c)

	op := NewOperation(h)

	srv := httptest.NewServer(op)
	defer srv.Close()

	resp, err := http.Post(srv.URL, "application/json", strings.NewReader(`{"msg":"hello world"}`))
	if err != nil {
		fmt.Println(err)
		return
	}
	if resp.StatusCode != http.StatusOK {
		fmt.Println("expected HTTP 200 status code but got", resp.StatusCode)
		return
	}

	// Output: hello world
}

type msgResponse struct {
	Msg string `json:"msg"`
}

func (*msgResponse) Spec() (int, *openapi3.Response, error) {
	return http.StatusOK, &openapi3.Response{}, nil
}

func (mr *msgResponse) WriteResponse(ctx context.Context, w http.ResponseWriter) error {
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	return enc.Encode(mr)
}

func ExampleConsumeNothing() {
	p := ProducerFunc[msgResponse](func(_ context.Context) (*msgResponse, error) {
		return &msgResponse{Msg: "hello world"}, nil
	})

	h := ConsumeNothing(p)

	op := NewOperation(h)

	srv := httptest.NewServer(op)
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		fmt.Println(err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Println("expected HTTP 200 status code but got", resp.StatusCode)
		return
	}

	var mr msgResponse
	dec := json.NewDecoder(resp.Body)
	err = dec.Decode(&mr)
	if err != nil {
		fmt.Println(err)
		return
	}

	fmt.Println(mr.Msg)
	// Output: hello world
}

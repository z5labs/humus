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
)

func ExampleReturnJson() {
	p := ProducerFunc[msgResponse](func(_ context.Context) (*msgResponse, error) {
		return &msgResponse{Msg: "hello world"}, nil
	})

	h := ReturnJson(ConsumeNothing(p))

	srv := httptest.NewServer(h)
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

	contentType := resp.Header.Get("Content-Type")
	if contentType != "application/json" {
		fmt.Println("expected Content-Type to be set to application/json instead of:", contentType)
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

func ExampleConsumeJson() {
	c := ConsumerFunc[msgRequest](func(_ context.Context, req *msgRequest) error {
		fmt.Println(req.Msg)
		return nil
	})

	h := ConsumeJson(ReturnNothing(c))

	srv := httptest.NewServer(h)
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
	if resp.Header.Get("Content-Type") != "" {
		return
	}

	// Output: hello world
}

// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
)

func ExampleConsumeNothing() {
	type MyResponse struct {
		Message string `json:"message"`
	}

	producer := ProducerFunc[MyResponse](func(ctx context.Context) (*MyResponse, error) {
		return &MyResponse{Message: "hello"}, nil
	})

	api := NewApi(
		"example",
		"v0.0.0",
		Operation(
			http.MethodGet,
			BasePath("/"),
			ReturnJson(ConsumeNothing(producer)),
		),
	)

	srv := httptest.NewServer(api)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		fmt.Println(err)
		return
	}
	defer resp.Body.Close()

	dec := json.NewDecoder(resp.Body)

	var myResp MyResponse
	err = dec.Decode(&myResp)
	if err != nil {
		fmt.Println(err)
		return
	}

	fmt.Println(myResp.Message)
	// Output: hello
}

func ExampleProduceNothing() {
	type MyResponse struct {
		Message string `json:"message"`
	}

	consumer := ConsumerFunc[MyResponse](func(ctx context.Context, req *MyResponse) error {
		fmt.Println(req.Message)
		return nil
	})

	api := NewApi(
		"example",
		"v0.0.0",
		Operation(
			http.MethodPost,
			BasePath("/"),
			ConsumeJson(ProduceNothing(consumer)),
		),
	)

	srv := httptest.NewServer(api)
	defer srv.Close()

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	err := enc.Encode(&MyResponse{Message: "hello"})
	if err != nil {
		fmt.Println(err)
		return
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/", &buf)
	if err != nil {
		fmt.Println(err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Println(err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Println("unexpected status code:", resp.StatusCode)
		return
	}

	// Output: hello
}

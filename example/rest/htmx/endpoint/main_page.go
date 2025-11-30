// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package endpoint

import (
	"context"
	"html/template"
	"log/slog"
	"net/http"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/rest"
)

var mainTemplate = template.Must(template.New("main").Parse(`
<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>HTMX Todo List</title>
    <script src="https://unpkg.com/htmx.org@1.9.10"></script>
    <style>
        body {
            font-family: Arial, sans-serif;
            max-width: 600px;
            margin: 50px auto;
            padding: 20px;
        }
        h1 {
            color: #333;
        }
        form {
            margin-bottom: 20px;
        }
        input[type="text"] {
            padding: 10px;
            width: 70%;
            border: 1px solid #ddd;
            border-radius: 4px;
        }
        button {
            padding: 10px 20px;
            background-color: #007bff;
            color: white;
            border: none;
            border-radius: 4px;
            cursor: pointer;
        }
        button:hover {
            background-color: #0056b3;
        }
        #item-list {
            list-style-type: none;
            padding: 0;
        }
        .item {
            padding: 10px;
            margin: 5px 0;
            background-color: #f8f9fa;
            border-left: 3px solid #007bff;
            border-radius: 4px;
        }
    </style>
</head>
<body>
    <h1>HTMX Todo List</h1>
    <form hx-post="/add" hx-target="#item-list" hx-swap="beforeend" hx-on::after-request="this.reset()">
        <input type="text" name="text" placeholder="Enter a new item..." required>
        <button type="submit">Add Item</button>
    </form>
    <ul id="item-list">
        {{range .Items}}
        <li class="item">{{.}}</li>
        {{end}}
    </ul>
</body>
</html>
`))

type mainPageHandler struct {
	log   *slog.Logger
	store *ItemStore
}

type PageData struct {
	Items []string
}

func MainPage(ctx context.Context, store *ItemStore) rest.ApiOption {
	h := &mainPageHandler{
		log:   humus.Logger("github.com/z5labs/humus/example/rest/htmx/endpoint"),
		store: store,
	}

	return rest.Operation(
		http.MethodGet,
		rest.BasePath("/"),
		rest.ProduceHTML(h, mainTemplate),
	)
}

func (h *mainPageHandler) Produce(ctx context.Context) (*PageData, error) {
	return &PageData{
		Items: h.store.GetAll(),
	}, nil
}

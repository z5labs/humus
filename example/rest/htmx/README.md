# HTMX Todo List Example

This example demonstrates using Humus REST API HTML response helpers with [HTMX](https://htmx.org/) to build interactive web applications.

## Overview

This is a simple todo list application that showcases:

- **HTML template responses** using `rest.ProduceHTML` and `rest.ReturnHTML`
- **HTMX integration** for dynamic content updates without page reloads
- **Form data handling** with custom `TypedRequest` implementation
- **In-memory data store** for demonstration purposes

## Running the Example

```bash
# From this directory
go run .
```

The application will start on port 8080 (or the port configured in config.yaml).

Open your browser to `http://localhost:8080` to see the interactive todo list.

## How It Works

### HTMX Basics

HTMX allows you to access modern browser features directly from HTML using attributes:

- `hx-post="/add"` - Makes a POST request to `/add` when the form is submitted
- `hx-target="#item-list"` - Specifies where to insert the response HTML
- `hx-swap="beforeend"` - Appends the response before the end of the target element
- `hx-on::after-request="this.reset()"` - Resets the form after a successful request

### Main Page Endpoint

**Endpoint:** `GET /`

Renders the full HTML page with the current list of items:

```go
rest.Operation(
    http.MethodGet,
    rest.BasePath("/"),
    rest.ProduceHTML(handler, mainTemplate),
)
```

The handler implements the `Producer[PageData]` interface:

```go
func (h *mainPageHandler) Produce(ctx context.Context) (*PageData, error) {
    return &PageData{
        Items: h.store.GetAll(),
    }, nil
}
```

### Add Item Endpoint

**Endpoint:** `POST /add`

Accepts form data and returns an HTML fragment for a single item:

```go
rest.Operation(
    http.MethodPost,
    rest.BasePath("/add"),
    rest.ReturnHTML(handler, itemTemplate),
)
```

The handler implements the `Handler[FormRequest, ItemResponse]` interface:

```go
func (h *addItemHandler) Handle(ctx context.Context, req *FormRequest) (*ItemResponse, error) {
    h.store.Add(req.Text)
    return &ItemResponse{Text: req.Text}, nil
}
```

### Form Data Handling

The `FormRequest` type implements the `TypedRequest` interface to parse form data:

```go
type FormRequest struct {
    Text string
}

func (fr *FormRequest) ReadRequest(ctx context.Context, r *http.Request) error {
    if err := r.ParseForm(); err != nil {
        return rest.NewBadRequestError("failed to parse form data")
    }
    fr.Text = r.FormValue("text")
    if fr.Text == "" {
        return rest.NewBadRequestError("text field is required")
    }
    return nil
}
```

## User Flow

1. User opens the page (`GET /`)
2. Server returns full HTML page with existing items
3. User types text and clicks "Add Item"
4. HTMX intercepts the form submission and sends `POST /add` with form data
5. Server returns HTML fragment: `<li class="item">User's text</li>`
6. HTMX appends the fragment to the `#item-list` element
7. Form is reset, ready for the next item

## Key Implementation Details

### HTML Templates

Templates are defined using Go's `html/template` package:

```go
var mainTemplate = template.Must(template.New("main").Parse(`...`))
var itemTemplate = template.Must(template.New("item").Parse(`<li class="item">{{.Text}}</li>`))
```

### Response Helpers

- `rest.ProduceHTML[T](producer, template)` - For GET endpoints that produce data
- `rest.ReturnHTML[Req, Resp](handler, template)` - For endpoints that consume and produce data

Both helpers wrap your business logic and handle template execution automatically.

### OpenAPI Specification

The HTML endpoints are included in the OpenAPI spec at:
```
http://localhost:8080/openapi.json
```

Note that HTML responses use a simple string schema in the OpenAPI spec.

## Project Structure

```
example/rest/htmx/
├── app/
│   └── app.go          # Application initialization
├── endpoint/
│   ├── store.go        # In-memory item store
│   ├── main_page.go    # Main page handler with full HTML
│   └── add_item.go     # Add item handler with HTML fragment
├── config.yaml         # Configuration file
├── main.go            # Entry point
└── README.md          # This file
```

## Production Considerations

1. **Persistent Storage**: Replace the in-memory store with a database
2. **Authentication**: Add user authentication to associate items with users
3. **Delete/Edit**: Extend functionality to delete and edit items
4. **Template Files**: Move templates to separate files for easier editing
5. **CSS Framework**: Consider using a CSS framework like Tailwind or Bootstrap
6. **HTMX Extensions**: Explore HTMX extensions for enhanced functionality

## See Also

- [HTMX Documentation](https://htmx.org/docs/)
- [Humus REST HTML Helpers](https://z5labs.dev/humus/features/rest/handler-helpers/)
- [Humus REST Documentation](https://z5labs.dev/humus/features/rest/)

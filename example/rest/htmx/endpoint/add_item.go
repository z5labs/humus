// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package endpoint

import (
	"context"
	"errors"
	"html/template"
	"log/slog"
	"net/http"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/rest"
)

var ErrTextRequired = errors.New("text field is required")

var itemTemplate = template.Must(template.New("item").Parse(`<li class="item">{{.Text}}</li>`))

// AddItemRequest represents the form data for adding an item.
type AddItemRequest struct {
	Text string `form:"text"`
}

type addItemHandler struct {
	log   *slog.Logger
	store *ItemStore
}

type ItemResponse struct {
	Text string
}

func AddItem(ctx context.Context, store *ItemStore) rest.ApiOption {
	h := &addItemHandler{
		log:   humus.Logger("github.com/z5labs/humus/example/rest/htmx/endpoint"),
		store: store,
	}

	return rest.Operation(
		http.MethodPost,
		rest.BasePath("/add"),
		rest.ConsumeForm(rest.ReturnHTML(h, itemTemplate)),
	)
}

func (h *addItemHandler) Handle(ctx context.Context, req *AddItemRequest) (*ItemResponse, error) {
	if req.Text == "" {
		return nil, rest.BadRequestError{
			Cause: ErrTextRequired,
		}
	}

	h.store.Add(req.Text)

	return &ItemResponse{
		Text: req.Text,
	}, nil
}

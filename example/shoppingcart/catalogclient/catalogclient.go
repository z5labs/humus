// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package catalogclient is the shoppingcart's Catalog: the catalog's prices, over HTTP.
package catalogclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/z5labs/humus/example/shoppingcart/endpoint"
)

type Client struct {
	http *http.Client
	base *url.URL
}

func New(client *http.Client, baseURL *url.URL) *Client {
	return &Client{http: client, base: baseURL}
}

type product struct {
	UnitPrice *float64 `json:"unitPrice"`
}

const maxBody = 64 << 10

func (c *Client) Price(ctx context.Context, productID string) (float64, error) {
	u := c.base.JoinPath("products")
	u.RawPath = u.EscapedPath() + "/" + url.PathEscape(productID)
	u.Path += "/" + productID

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return 0, fmt.Errorf("getting product %s: %w", productID, err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("getting product %s: %w: %w", productID, endpoint.ErrCatalogUnavailable, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
		_ = resp.Body.Close()
	}()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return 0, fmt.Errorf("getting product %s: %w", productID, endpoint.ErrProductNotFound)
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return 0, fmt.Errorf("getting product %s: %w: the catalog answered %d", productID, endpoint.ErrCatalogUnavailable, resp.StatusCode)
	default:
		return 0, fmt.Errorf("getting product %s: the catalog answered %d", productID, resp.StatusCode)
	}

	var p product
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&p); err != nil {
		return 0, fmt.Errorf("getting product %s: decoding the catalog's answer: %w", productID, err)
	}
	switch {
	case p.UnitPrice == nil:
		return 0, fmt.Errorf("getting product %s: the catalog answered with no unitPrice", productID)
	case *p.UnitPrice < 0:
		return 0, fmt.Errorf("getting product %s: the catalog answered with a negative unitPrice", productID)
	}
	return *p.UnitPrice, nil
}

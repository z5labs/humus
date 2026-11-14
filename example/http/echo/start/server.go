// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package main

import (
	"errors"
	"net/http"

	"github.com/go-playground/validator/v10"
	"github.com/labstack/echo/v5"
	"github.com/labstack/echo/v5/middleware"
)

const maxBodyBytes = 1 << 20

func newServer(store *store) *echo.Echo {
	e := echo.New()
	e.Validator = &customValidator{validator: validator.New()}
	e.HTTPErrorHandler = handleError

	e.Use(middleware.RequestLogger())
	e.Use(middleware.Recover())
	e.Use(middleware.BodyLimit(maxBodyBytes))

	v1 := e.Group("/v1")
	users := v1.Group("/users")

	h := &handlers{store: store}
	users.GET("", h.listUsers)
	users.POST("", h.createUser)
	users.GET("/:id", h.getUser)
	users.PUT("/:id", h.updateUser)
	users.DELETE("/:id", h.deleteUser)
	return e
}

type customValidator struct {
	validator *validator.Validate
}

func (cv *customValidator) Validate(i any) error {
	if err := cv.validator.Struct(i); err != nil {
		return echo.NewHTTPError(http.StatusUnprocessableEntity, err.Error())
	}
	return nil
}

type problemDetails struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
}

func handleError(c *echo.Context, err error) {
	if r, _ := echo.UnwrapResponse(c.Response()); r != nil && r.Committed {
		return
	}
	p := problemDetails{Type: "about:blank", Status: http.StatusInternalServerError}
	var he *echo.HTTPError
	switch {
	case errors.As(err, &he):
		p.Status, p.Detail = he.Code, he.Message
	case echo.StatusCode(err) != 0:
		p.Status = echo.StatusCode(err)
	default:
		c.Logger().ErrorContext(c.Request().Context(), "request failed", "error", err)
	}
	p.Title = http.StatusText(p.Status)
	c.Response().Header().Set(echo.HeaderContentType, "application/problem+json")
	if err := c.JSON(p.Status, p); err != nil {
		c.Logger().ErrorContext(c.Request().Context(), "writing the problem failed", "error", err)
	}
}

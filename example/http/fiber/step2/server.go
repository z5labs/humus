// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package main

import (
	"errors"
	"io"
	"net/http"

	"github.com/go-playground/validator/v10"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/logger"
	"github.com/gofiber/fiber/v3/middleware/recover"
)

const maxBodyBytes = 1 << 20

func newServer(store *store, stdout io.Writer, stopped <-chan struct{}) *fiber.App {
	app := fiber.New(fiber.Config{
		BodyLimit:       maxBodyBytes,
		ErrorHandler:    handleError,
		StructValidator: &structValidator{validate: validator.New()},
	})

	app.Use(logger.New(logger.Config{Stream: stdout, TimeDone: stopped}))
	app.Use(recover.New())

	v1 := app.Group("/v1")
	users := v1.Group("/users")

	h := &handlers{store: store}
	users.Get("", h.listUsers)
	users.Post("", h.createUser)
	return app
}

type structValidator struct {
	validate *validator.Validate
}

func (v *structValidator) Validate(out any) error {
	return v.validate.Struct(out)
}

type problemDetails struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
}

func handleError(c fiber.Ctx, err error) error {
	p := problemDetails{Type: "about:blank", Status: fiber.StatusInternalServerError}
	var e *fiber.Error
	if errors.As(err, &e) {
		p.Status, p.Detail = e.Code, e.Message
	}
	p.Title = http.StatusText(p.Status)
	return c.Status(p.Status).JSON(p, "application/problem+json")
}

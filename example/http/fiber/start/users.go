// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package main

import (
	"errors"
	"fmt"

	"github.com/go-playground/validator/v10"
	"github.com/gofiber/fiber/v3"
)

type user struct {
	ID    int    `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

type userInput struct {
	Name  string `json:"name" validate:"required"`
	Email string `json:"email" validate:"required,email"`
}

type userURI struct {
	ID int `uri:"id"`
}

type page struct {
	Limit  int `query:"limit,default:20" validate:"min=1,max=100"`
	Offset int `query:"offset" validate:"min=0"`
}

type handlers struct {
	store *store
}

func (h *handlers) listUsers(c fiber.Ctx) error {
	var p page
	if err := c.Bind().Query(&p); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	return c.JSON(h.store.list(p.Offset, p.Limit))
}

func (h *handlers) createUser(c fiber.Ctx) error {
	var in userInput
	if err := c.Bind().JSON(&in); err != nil {
		return bodyError(err)
	}
	return c.Status(fiber.StatusCreated).JSON(h.store.create(in))
}

func (h *handlers) getUser(c fiber.Ctx) error {
	var uri userURI
	if err := c.Bind().URI(&uri); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	u, ok := h.store.get(uri.ID)
	if !ok {
		return userNotFound(uri.ID)
	}
	return c.JSON(u)
}

func (h *handlers) updateUser(c fiber.Ctx) error {
	var uri userURI
	if err := c.Bind().URI(&uri); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	var in userInput
	if err := c.Bind().JSON(&in); err != nil {
		return bodyError(err)
	}
	u, ok := h.store.update(uri.ID, in)
	if !ok {
		return userNotFound(uri.ID)
	}
	return c.JSON(u)
}

func (h *handlers) deleteUser(c fiber.Ctx) error {
	var uri userURI
	if err := c.Bind().URI(&uri); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	if !h.store.delete(uri.ID) {
		return userNotFound(uri.ID)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

func bodyError(err error) error {
	var invalid validator.ValidationErrors
	if errors.As(err, &invalid) {
		return fiber.NewError(fiber.StatusUnprocessableEntity, err.Error())
	}
	return fiber.NewError(fiber.StatusBadRequest, err.Error())
}

func userNotFound(id int) error {
	return fiber.NewError(fiber.StatusNotFound, fmt.Sprintf("There is no user %d.", id))
}

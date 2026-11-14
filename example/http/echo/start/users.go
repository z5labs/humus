// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package main

import (
	"fmt"
	"net/http"

	"github.com/labstack/echo/v5"
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

type handlers struct {
	store *store
}

func (h *handlers) listUsers(c *echo.Context) error {
	limit, err := echo.QueryParamOr(c, "limit", 20)
	if err != nil {
		return err
	}
	if limit < 1 || limit > 100 {
		return echo.NewHTTPError(http.StatusBadRequest, "limit must be from 1 to 100")
	}
	offset, err := echo.QueryParamOr(c, "offset", 0)
	if err != nil {
		return err
	}
	if offset < 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "offset must not be negative")
	}
	return c.JSON(http.StatusOK, h.store.list(offset, limit))
}

func (h *handlers) createUser(c *echo.Context) error {
	var in userInput
	if err := c.Bind(&in); err != nil {
		return err
	}
	if err := c.Validate(&in); err != nil {
		return err
	}
	return c.JSON(http.StatusCreated, h.store.create(in))
}

func (h *handlers) getUser(c *echo.Context) error {
	id, err := echo.PathParam[int](c, "id")
	if err != nil {
		return err
	}
	u, ok := h.store.get(id)
	if !ok {
		return userNotFound(id)
	}
	return c.JSON(http.StatusOK, u)
}

func (h *handlers) updateUser(c *echo.Context) error {
	id, err := echo.PathParam[int](c, "id")
	if err != nil {
		return err
	}
	var in userInput
	if err := c.Bind(&in); err != nil {
		return err
	}
	if err := c.Validate(&in); err != nil {
		return err
	}
	u, ok := h.store.update(id, in)
	if !ok {
		return userNotFound(id)
	}
	return c.JSON(http.StatusOK, u)
}

func (h *handlers) deleteUser(c *echo.Context) error {
	id, err := echo.PathParam[int](c, "id")
	if err != nil {
		return err
	}
	if !h.store.delete(id) {
		return userNotFound(id)
	}
	return c.NoContent(http.StatusNoContent)
}

func userNotFound(id int) error {
	return echo.NewHTTPError(http.StatusNotFound, fmt.Sprintf("There is no user %d.", id))
}

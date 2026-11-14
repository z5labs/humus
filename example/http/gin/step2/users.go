// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package main

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/go-playground/validator/v10"
)

type user struct {
	ID    int    `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

type userInput struct {
	Name  string `json:"name" binding:"required"`
	Email string `json:"email" binding:"required,email"`
}

type page struct {
	Limit  int `form:"limit,default=20" binding:"min=1,max=100"`
	Offset int `form:"offset" binding:"min=0"`
}

type handlers struct {
	store *store
}

func (h *handlers) listUsers(c *gin.Context) {
	var p page
	if err := c.ShouldBindQuery(&p); err != nil {
		abort(c, http.StatusBadRequest, err.Error())
		return
	}
	c.JSON(http.StatusOK, h.store.list(p.Offset, p.Limit))
}

func (h *handlers) createUser(c *gin.Context) {
	var in userInput
	if err := c.ShouldBindJSON(&in); err != nil {
		abortBinding(c, err)
		return
	}
	c.JSON(http.StatusCreated, h.store.create(in))
}

func abortBinding(c *gin.Context, err error) {
	var tooLarge *http.MaxBytesError
	var invalid validator.ValidationErrors
	switch {
	case errors.As(err, &tooLarge):
		abort(c, http.StatusRequestEntityTooLarge, err.Error())
	case errors.As(err, &invalid):
		abort(c, http.StatusUnprocessableEntity, err.Error())
	default:
		abort(c, http.StatusBadRequest, err.Error())
	}
}

// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package main

import (
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
)

const maxBodyBytes = 1 << 20

func newServer(store *store, stdout, stderr io.Writer) *gin.Engine {
	r := gin.New()
	r.HandleMethodNotAllowed = true

	r.Use(gin.LoggerWithWriter(stdout))
	r.Use(gin.CustomRecoveryWithWriter(stderr, handlePanic))
	r.Use(limitBody)
	r.Use(route)

	r.NoRoute(noRoute)
	r.NoMethod(noMethod)

	v1 := r.Group("/v1")
	users := v1.Group("/users")

	h := &handlers{store: store}
	users.GET("", h.listUsers)
	users.POST("", h.createUser)
	users.GET("/:id", h.getUser)
	users.PUT("/:id", h.updateUser)
	users.DELETE("/:id", h.deleteUser)
	return r
}

func limitBody(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes)
	c.Next()
}

type problemDetails struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
}

func abort(c *gin.Context, status int, detail string) {
	c.Header("Content-Type", "application/problem+json")
	c.AbortWithStatusJSON(status, problemDetails{
		Type:   "about:blank",
		Title:  http.StatusText(status),
		Status: status,
		Detail: detail,
	})
}

func noRoute(c *gin.Context) {
	abort(c, http.StatusNotFound, "")
}

func noMethod(c *gin.Context) {
	abort(c, http.StatusMethodNotAllowed, "")
}

func handlePanic(c *gin.Context, _ any) {
	abort(c, http.StatusInternalServerError, "")
}

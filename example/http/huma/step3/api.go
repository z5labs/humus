// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package main

import (
	"context"
	"fmt"
	"net/http"
	"net/mail"

	"github.com/z5labs/humus/openapi/v3"
	"github.com/z5labs/humus/openapi/v3/jsoncodec"
	"github.com/z5labs/humus/openapi/v3/problem"
)

var userID = openapi.PathParam[int]("id").Description("The user's ID.")

var (
	pageLimit  = openapi.QueryParam[int]("limit").Description("The most users to list.").Minimum(1).Maximum(100).Default(20)
	pageOffset = openapi.QueryParam[int]("offset").Description("How many users to skip.").Minimum(0)
)

var idempotencyKey = openapi.HeaderParam[string]("Idempotency-Key").
	Description("Sent again with the same key, the request creates no second user.").
	MaxLength(64)

var (
	v1        = openapi.Root().Lit("v1")
	usersPath = v1.Lit("users")
	userPath  = usersPath.Param(userID)
)

type notFound struct{ problem.Details }

type unprocessable struct{ problem.Details }

var (
	userNotFoundError = openapi.Error[notFound](http.StatusNotFound, jsoncodec.Problem)
	invalidUserError  = openapi.Error[unprocessable](http.StatusUnprocessableEntity, jsoncodec.Problem)
)

type users struct {
	store *store
}

func (u *users) operations() []openapi.Operation {
	return []openapi.Operation{
		openapi.Get(usersPath, u.listUsers, jsoncodec.Encoder,
			openapi.OperationID("listUsers"),
			openapi.Summary("List the users"),
			openapi.Query(pageLimit),
			openapi.Query(pageOffset),
		),
		openapi.Post(usersPath, u.createUser, jsoncodec.Decoder, jsoncodec.Encoder,
			openapi.OperationID("createUser"),
			openapi.Summary("Create a user"),
			openapi.Status(http.StatusCreated),
			openapi.Header(idempotencyKey),
			invalidUserError,
		),
		openapi.Get(userPath, u.getUser, jsoncodec.Encoder,
			openapi.OperationID("getUser"),
			openapi.Summary("Get a user"),
			userNotFoundError,
		),
		openapi.Put(userPath, u.updateUser, jsoncodec.Decoder, jsoncodec.Encoder,
			openapi.OperationID("updateUser"),
			openapi.Summary("Update a user"),
			userNotFoundError,
			invalidUserError,
		),
		openapi.Delete(userPath, u.deleteUser, openapi.EmptyEncoder,
			openapi.OperationID("deleteUser"),
			openapi.Summary("Delete a user"),
			userNotFoundError,
		),
	}
}

func (u *users) listUsers(_ context.Context, req openapi.Request[openapi.EmptyBody]) ([]user, error) {
	return u.store.list(openapi.ParamFrom(req, pageOffset), openapi.ParamFrom(req, pageLimit)), nil
}

func (u *users) createUser(_ context.Context, req openapi.Request[userInput]) (user, error) {
	in := req.Body()
	if err := validate(in); err != nil {
		return user{}, err
	}
	return u.store.create(openapi.ParamFrom(req, idempotencyKey), in), nil
}

func (u *users) getUser(_ context.Context, req openapi.Request[openapi.EmptyBody]) (user, error) {
	id := openapi.ParamFrom(req, userID)
	found, ok := u.store.get(id)
	if !ok {
		return user{}, userNotFound(id)
	}
	return found, nil
}

func (u *users) updateUser(_ context.Context, req openapi.Request[userInput]) (user, error) {
	id := openapi.ParamFrom(req, userID)
	in := req.Body()
	if err := validate(in); err != nil {
		return user{}, err
	}
	updated, ok := u.store.update(id, in)
	if !ok {
		return user{}, userNotFound(id)
	}
	return updated, nil
}

func (u *users) deleteUser(_ context.Context, req openapi.Request[openapi.EmptyBody]) (openapi.EmptyBody, error) {
	id := openapi.ParamFrom(req, userID)
	if !u.store.delete(id) {
		return openapi.EmptyBody{}, userNotFound(id)
	}
	return openapi.EmptyBody{}, nil
}

func validate(in userInput) error {
	if in.Name == "" {
		return invalidUser("The name is empty.")
	}
	if addr, err := mail.ParseAddress(in.Email); err != nil || addr.Address != in.Email {
		return invalidUser(fmt.Sprintf("%q isn't an email address.", in.Email))
	}
	return nil
}

func userNotFound(id int) error {
	return notFound{problem.Details{
		Type:   "about:blank",
		Title:  http.StatusText(http.StatusNotFound),
		Status: http.StatusNotFound,
		Detail: fmt.Sprintf("There is no user %d.", id),
	}}
}

func invalidUser(detail string) error {
	return unprocessable{problem.Details{
		Type:   "about:blank",
		Title:  http.StatusText(http.StatusUnprocessableEntity),
		Status: http.StatusUnprocessableEntity,
		Detail: detail,
	}}
}

func serverError(error) problem.Details {
	return problem.Details{
		Type:   "about:blank",
		Title:  http.StatusText(http.StatusInternalServerError),
		Status: http.StatusInternalServerError,
	}
}

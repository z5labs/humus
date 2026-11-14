// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package main

import (
	"context"
	"fmt"
	"net/http"

	"github.com/go-playground/validator/v10"

	"github.com/z5labs/humus/openapi/v3"
	"github.com/z5labs/humus/openapi/v3/jsoncodec"
	"github.com/z5labs/humus/openapi/v3/problem"
)

var userID = openapi.PathParam[int]("id").Description("The user's ID.")

var (
	pageLimit  = openapi.QueryParam[int]("limit").Description("The most users to list.").Minimum(1).Maximum(100).Default(20)
	pageOffset = openapi.QueryParam[int]("offset").Description("How many users to skip.").Minimum(0)
)

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
	store    *store
	validate *validator.Validate
}

func (u *users) operations() []openapi.Operation {
	return []openapi.Operation{
		openapi.Get(usersPath, u.listUsers, jsoncodec.Encoder,
			openapi.OperationID("listUsers"),
			openapi.Query(pageLimit),
			openapi.Query(pageOffset),
		),
		openapi.Post(usersPath, u.createUser, jsoncodec.Decoder, jsoncodec.Encoder,
			openapi.OperationID("createUser"),
			openapi.Status(http.StatusCreated),
			invalidUserError,
		),
		openapi.Get(userPath, u.getUser, jsoncodec.Encoder,
			openapi.OperationID("getUser"),
			userNotFoundError,
		),
		openapi.Put(userPath, u.updateUser, jsoncodec.Decoder, jsoncodec.Encoder,
			openapi.OperationID("updateUser"),
			userNotFoundError,
			invalidUserError,
		),
		openapi.Delete(userPath, u.deleteUser, openapi.EmptyEncoder,
			openapi.OperationID("deleteUser"),
			userNotFoundError,
		),
	}
}

func (u *users) listUsers(_ context.Context, req openapi.Request[openapi.EmptyBody]) ([]user, error) {
	return u.store.list(openapi.ParamFrom(req, pageOffset), openapi.ParamFrom(req, pageLimit)), nil
}

func (u *users) createUser(_ context.Context, req openapi.Request[userInput]) (user, error) {
	in := req.Body()
	if err := u.validate.Struct(in); err != nil {
		return user{}, invalidUser(err)
	}
	return u.store.create(in), nil
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
	if err := u.validate.Struct(in); err != nil {
		return user{}, invalidUser(err)
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

func newValidator() *validator.Validate {
	v := validator.New()
	v.SetTagName("binding")
	return v
}

func userNotFound(id int) error {
	return notFound{problem.Details{
		Type:   "about:blank",
		Title:  http.StatusText(http.StatusNotFound),
		Status: http.StatusNotFound,
		Detail: fmt.Sprintf("There is no user %d.", id),
	}}
}

func invalidUser(err error) error {
	return unprocessable{problem.Details{
		Type:   "about:blank",
		Title:  http.StatusText(http.StatusUnprocessableEntity),
		Status: http.StatusUnprocessableEntity,
		Detail: err.Error(),
	}}
}

func serverError(error) problem.Details {
	return problem.Details{
		Type:   "about:blank",
		Title:  http.StatusText(http.StatusInternalServerError),
		Status: http.StatusInternalServerError,
	}
}

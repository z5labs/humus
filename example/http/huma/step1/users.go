// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package main

import (
	"context"
	"fmt"

	"github.com/danielgtaylor/huma/v2"
)

type user struct {
	ID    int    `json:"id" doc:"The user's ID." example:"1"`
	Name  string `json:"name" doc:"The user's name." example:"Ada"`
	Email string `json:"email" format:"email" doc:"The user's email address." example:"ada@example.com"`
}

type userInput struct {
	Name  string `json:"name" minLength:"1" doc:"The user's name." example:"Ada"`
	Email string `json:"email" format:"email" doc:"The user's email address." example:"ada@example.com"`
}

type listUsersInput struct {
	Limit  int `query:"limit" minimum:"1" maximum:"100" default:"20" doc:"The most users to list."`
	Offset int `query:"offset" minimum:"0" doc:"How many users to skip."`
}

type createUserInput struct {
	IdempotencyKey string `header:"Idempotency-Key" maxLength:"64" doc:"Sent again with the same key, the request creates no second user."`
	Body           userInput
}

type userIDInput struct {
	ID int `path:"id" doc:"The user's ID."`
}

type updateUserInput struct {
	ID   int `path:"id" doc:"The user's ID."`
	Body userInput
}

type usersOutput struct {
	Body []user
}

type userOutput struct {
	Body user
}

type handlers struct {
	store *store
}

func (h *handlers) listUsers(_ context.Context, in *listUsersInput) (*usersOutput, error) {
	return &usersOutput{Body: h.store.list(in.Offset, in.Limit)}, nil
}

func (h *handlers) createUser(_ context.Context, in *createUserInput) (*userOutput, error) {
	return &userOutput{Body: h.store.create(in.IdempotencyKey, in.Body)}, nil
}

func (h *handlers) getUser(_ context.Context, in *userIDInput) (*userOutput, error) {
	found, ok := h.store.get(in.ID)
	if !ok {
		return nil, huma.Error404NotFound(fmt.Sprintf("There is no user %d.", in.ID))
	}
	return &userOutput{Body: found}, nil
}

func (h *handlers) updateUser(_ context.Context, in *updateUserInput) (*userOutput, error) {
	updated, ok := h.store.update(in.ID, in.Body)
	if !ok {
		return nil, huma.Error404NotFound(fmt.Sprintf("There is no user %d.", in.ID))
	}
	return &userOutput{Body: updated}, nil
}

func (h *handlers) deleteUser(_ context.Context, in *userIDInput) (*struct{}, error) {
	if !h.store.delete(in.ID) {
		return nil, huma.Error404NotFound(fmt.Sprintf("There is no user %d.", in.ID))
	}
	return nil, nil
}

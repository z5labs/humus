// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package main

type user struct {
	ID    int    `json:"id" required:"true" description:"The user's ID." example:"1"`
	Name  string `json:"name" required:"true" description:"The user's name." example:"Ada"`
	Email string `json:"email" required:"true" format:"email" description:"The user's email address." example:"ada@example.com"`
}

type userInput struct {
	Name  string `json:"name" required:"true" minLength:"1" description:"The user's name." example:"Ada"`
	Email string `json:"email" required:"true" format:"email" description:"The user's email address." example:"ada@example.com"`
}

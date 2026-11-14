// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package main

type user struct {
	ID    int    `json:"id" required:"true"`
	Name  string `json:"name" required:"true"`
	Email string `json:"email" required:"true" format:"email"`
}

type userInput struct {
	Name  string `json:"name" binding:"required" required:"true" minLength:"1"`
	Email string `json:"email" binding:"required,email" required:"true" format:"email"`
}

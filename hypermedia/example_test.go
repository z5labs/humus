// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package hypermedia_test

import (
	"fmt"

	"github.com/z5labs/humus/hypermedia"
)

// ID is a contact's ID, a named type, so a value of another type isn't an
// Arg for its parameter.
type ID int

// The paths of a contacts UI, declared once as package-level variables. Each
// routes its requests and builds the links to them.
var (
	ContactID = hypermedia.PathParam[ID]("contactId").Minimum(1)
	List      = hypermedia.Root().Lit("contacts")
	One       = List.Param(ContactID)
	Edit      = One.Lit("edit")
	Asset     = hypermedia.PathParam[string]("file")
	Static    = hypermedia.Root().Lit("static").Rest(Asset)
	Page      = hypermedia.QueryParam[int]("page")
)

// A path's String is its pattern, and its URL is a link to it, with one Arg
// for each of its parameters.
func ExamplePath_URL() {
	fmt.Println(Edit)
	fmt.Println(Edit.URL(hypermedia.Arg(ContactID, 7)))

	// Output: /contacts/{contactId}/edit
	// /contacts/7/edit
}

// A Rest parameter takes the rest of the path, so a link keeps the slashes
// in its value, and escapes each segment between them.
func ExamplePath_Rest() {
	fmt.Println(Static)
	fmt.Println(Static.URL(hypermedia.Arg(Asset, "css/main page.css")))

	// Output: /static/{file...}
	// /static/css/main%20page.css
}

// With adds a query parameter's value to a link, escaped.
func ExampleURL_With() {
	fmt.Println(List.URL().With(Page, 2))

	// Output: /contacts?page=2
}

// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package hypermedia_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/hypermedia"
	"github.com/z5labs/humus/telemetry"
)

// contactPage writes the contact whose ID is in the path, which the Site
// has already parsed and checked against ContactID's Minimum.
func contactPage(w http.ResponseWriter, r *http.Request) error {
	page, err := hypermedia.QueryFrom(r, Page)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "contact %d, page %d", hypermedia.ParamFrom(r, ContactID), page)
	return err
}

// errorPage answers the errors the Site answers, with the status of a
// *RequestError.
func errorPage(w http.ResponseWriter, _ *http.Request, err error) error {
	status := http.StatusInternalServerError
	if re, ok := errors.AsType[*hypermedia.RequestError](err); ok {
		status = re.Status
	}
	w.WriteHeader(status)
	_, err = fmt.Fprintf(w, "<p>%s</p>", http.StatusText(status))
	return err
}

// NewSite serves routes on Paths, and answers a path or a method none of
// them serves with the end user's error page.
func ExampleNewSite() {
	site, err := hypermedia.NewSite(&humus.Setup{Telemetry: telemetry.Noop()}, errorPage, []hypermedia.Route{
		hypermedia.NewRoute(http.MethodGet, One, contactPage),
		hypermedia.NewRoute(http.MethodPut, One, contactPage),
	})
	if err != nil {
		fmt.Println(err)
		return
	}

	for _, req := range []struct{ method, target string }{
		{http.MethodGet, One.URL(hypermedia.Arg(ContactID, 7)).With(Page, 2).String()},
		{http.MethodGet, "/contacts/0"},
		{http.MethodDelete, "/contacts/7"},
	} {
		w := httptest.NewRecorder()
		site.ServeHTTP(w, httptest.NewRequest(req.method, req.target, nil))
		fmt.Println(req.method, req.target, w.Code, w.Header().Get("Allow"), w.Body)
	}

	// Output: GET /contacts/7?page=2 200  contact 7, page 2
	// GET /contacts/0 404  <p>Not Found</p>
	// DELETE /contacts/7 405 GET, HEAD, PUT <p>Method Not Allowed</p>
}

// NewSite names every mistake in its routes in one error.
func ExampleNewSite_mistakes() {
	_, err := hypermedia.NewSite(&humus.Setup{Telemetry: telemetry.Noop()}, errorPage, []hypermedia.Route{
		hypermedia.NewRoute("GET POST", List, contactPage),
		hypermedia.NewRoute(http.MethodGet, One, contactPage),
		hypermedia.NewRoute(http.MethodGet, List.Param(hypermedia.PathParam[ID]("id")), contactPage),
	})
	fmt.Println(err)

	// Output: hypermedia: NewSite: route GET POST /contacts: method "GET POST" isn't a token
	// paths /contacts/{contactId} and /contacts/{id} differ only in their parameters' names
}

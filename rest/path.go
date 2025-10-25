// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rest

import (
	"path"
)

// PathElement represents a component of a URL path.
// It can be either a static path segment or a dynamic path parameter.
type PathElement interface {
	pathElement() string
}

// PathSegment is a static component of a URL path.
type PathSegment string

func (s PathSegment) pathElement() string {
	return string(s)
}

type pathParam struct {
	name string
	opts []ParameterOption
}

// PathParam creates a dynamic path parameter element.
// Path parameters are extracted from the URL and can be validated using [ParameterOption].
//
// Example:
//
//	rest.PathParam("userId", rest.Required(), rest.Regex(regexp.MustCompile(`^\d+$`)))
func PathParam(name string, opts ...ParameterOption) PathElement {
	return pathParam{
		name: name,
		opts: opts,
	}
}

func (p pathParam) pathElement() string {
	return "{" + p.name + "}"
}

// StaticPath creates a static path segment element.
// This is equivalent to using PathSegment directly.
//
// Example:
//
//	rest.StaticPath("users")  // equivalent to rest.PathSegment("users")
func StaticPath(s string) PathElement {
	return PathSegment(s)
}

// Path represents a URL path composed of static segments and dynamic parameters.
// Paths are built using [BasePath] and extended with [Path.Segment] and [Path.Param].
type Path []PathElement

// BasePath creates a new path starting with the given segment.
// This is the starting point for building paths used with [Handle].
//
// Example:
//
//	path := rest.BasePath("/api/v1")
//	// Results in: /api/v1
func BasePath(s string) Path {
	return []PathElement{PathSegment(s)}
}

// Segment appends a static path segment to the path.
//
// Example:
//
//	path := rest.BasePath("/api").Segment("users").Segment("profile")
//	// Results in: /api/users/profile
func (p Path) Segment(s string) Path {
	return append(p, PathSegment(s))
}

// Param appends a dynamic path parameter to the path.
// The parameter value will be extracted from the URL at request time.
//
// Example:
//
//	path := rest.BasePath("/users").Param("userId").Segment("posts").Param("postId")
//	// Results in: /users/{userId}/posts/{postId}
func (p Path) Param(name string) Path {
	return append(p, PathParam(name))
}

// String converts the path to its string representation.
// Static segments are joined with slashes, and parameters are formatted as {name}.
func (p Path) String() string {
	ss := make([]string, len(p))
	for i, el := range p {
		ss[i] = el.pathElement()
	}
	return path.Join(ss...)
}

// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rest

import (
	"path"
)

type PathElement interface {
	pathElement() string
}

type PathSegment string

func (s PathSegment) pathElement() string {
	return string(s)
}

type pathParam struct {
	name string
	opts []ParameterOption
}

func PathParam(name string, opts ...ParameterOption) PathElement {
	return pathParam{
		name: name,
		opts: opts,
	}
}

func (p pathParam) pathElement() string {
	return "{" + p.name + "}"
}

func StaticPath(s string) PathElement {
	return PathSegment(s)
}

type Path []PathElement

func BasePath(s string) Path {
	return []PathElement{PathSegment(s)}
}

func (p Path) Segment(s string) Path {
	return append(p, PathSegment(s))
}

func (p Path) Param(name string) Path {
	return append(p, PathParam(name))
}

func (p Path) String() string {
	ss := make([]string, len(p))
	for i, el := range p {
		ss[i] = el.pathElement()
	}
	return path.Join(ss...)
}

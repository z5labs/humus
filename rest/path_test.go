// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rest

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBasePath(t *testing.T) {
	t.Run("creates a path with single segment", func(t *testing.T) {
		path := BasePath("/api")
		assert.Equal(t, "/api", path.String())
	})

	t.Run("creates a path without leading slash", func(t *testing.T) {
		path := BasePath("api")
		assert.Equal(t, "api", path.String())
	})
}

func TestPath_Segment(t *testing.T) {
	t.Run("appends a single segment", func(t *testing.T) {
		path := BasePath("/api").Segment("users")
		assert.Equal(t, "/api/users", path.String())
	})

	t.Run("appends multiple segments", func(t *testing.T) {
		path := BasePath("/api").Segment("v1").Segment("users")
		assert.Equal(t, "/api/v1/users", path.String())
	})

	t.Run("handles segments with slashes", func(t *testing.T) {
		path := BasePath("/api").Segment("users/profile")
		assert.Equal(t, "/api/users/profile", path.String())
	})
}

func TestPath_Param(t *testing.T) {
	t.Run("appends a single parameter", func(t *testing.T) {
		path := BasePath("/users").Param("id")
		assert.Equal(t, "/users/{id}", path.String())
	})

	t.Run("appends multiple parameters", func(t *testing.T) {
		path := BasePath("/users").Param("userId").Segment("posts").Param("postId")
		assert.Equal(t, "/users/{userId}/posts/{postId}", path.String())
	})

	t.Run("creates parameter at end of path", func(t *testing.T) {
		path := BasePath("/api/v1/items").Param("itemId")
		assert.Equal(t, "/api/v1/items/{itemId}", path.String())
	})
}

func TestPath_String(t *testing.T) {
	t.Run("formats empty path", func(t *testing.T) {
		var path Path
		// Empty path results in empty string after path.Join
		assert.Equal(t, "", path.String())
	})

	t.Run("formats complex path", func(t *testing.T) {
		path := BasePath("/api").
			Segment("v2").
			Segment("organizations").
			Param("orgId").
			Segment("repositories").
			Param("repoId").
			Segment("issues")
		assert.Equal(t, "/api/v2/organizations/{orgId}/repositories/{repoId}/issues", path.String())
	})

	t.Run("handles path with only parameters", func(t *testing.T) {
		path := Path([]PathElement{PathParam("id")})
		assert.Equal(t, "{id}", path.String())
	})
}

func TestStaticPath(t *testing.T) {
	t.Run("creates a static path element", func(t *testing.T) {
		element := StaticPath("users")
		assert.Equal(t, "users", element.pathElement())
	})

	t.Run("is equivalent to PathSegment", func(t *testing.T) {
		static := StaticPath("test")
		segment := PathSegment("test")
		assert.Equal(t, segment.pathElement(), static.pathElement())
	})
}

func TestPathParam(t *testing.T) {
	t.Run("creates a path parameter element", func(t *testing.T) {
		param := PathParam("userId")
		assert.Equal(t, "{userId}", param.pathElement())
	})

	t.Run("formats parameter name with braces", func(t *testing.T) {
		param := PathParam("id")
		assert.Equal(t, "{id}", param.pathElement())
	})
}

func TestPathSegment_pathElement(t *testing.T) {
	t.Run("returns segment as string", func(t *testing.T) {
		segment := PathSegment("users")
		assert.Equal(t, "users", segment.pathElement())
	})

	t.Run("preserves slashes", func(t *testing.T) {
		segment := PathSegment("api/v1")
		assert.Equal(t, "api/v1", segment.pathElement())
	})
}

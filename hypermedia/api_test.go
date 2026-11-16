// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package hypermedia_test

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestPackage_exports_only_its_API checks the package's own source, test
// files excluded: it exports its paths, parameters and links, its routes
// and its Site, its forms, and nothing else.
func TestPackage_exports_only_its_API(t *testing.T) {
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	fset := token.NewFileSet()
	var exported []string
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		require.NoError(t, err)
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if !d.Name.IsExported() {
					continue
				}
				if d.Recv == nil {
					exported = append(exported, d.Name.Name)
					continue
				}
				recv := d.Recv.List[0].Type
				if star, ok := recv.(*ast.StarExpr); ok {
					recv = star.X
				}
				if index, ok := recv.(*ast.IndexExpr); ok {
					recv = index.X
				}
				// A method of an unexported type, such as an
				// http.Handler's, isn't part of the package's API.
				if recvName := recv.(*ast.Ident).Name; ast.IsExported(recvName) {
					exported = append(exported, recvName+"."+d.Name.Name)
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						if s.Name.IsExported() {
							exported = append(exported, s.Name.Name)
						}
					case *ast.ValueSpec:
						for _, n := range s.Names {
							if n.IsExported() {
								exported = append(exported, n.Name)
							}
						}
					}
				}
			}
		}
	}
	sort.Strings(exported)
	require.Equal(t, []string{
		"AnyField",
		"Arg",
		"ErrorPage",
		"Field",
		"FieldError",
		"FieldError.Error",
		"FieldFrom",
		"Form",
		"Form.Problem",
		"Form.Raw",
		"Form.Valid",
		"FormField",
		"FormField.Enum",
		"FormField.MaxLength",
		"FormField.Maximum",
		"FormField.MinLength",
		"FormField.Minimum",
		"FormField.Pattern",
		"FormField.Required",
		"Handler",
		"Malformed",
		"Missing",
		"NewRoute",
		"NewSite",
		"NotInEnum",
		"ParamFrom",
		"ParamValue",
		"Path",
		"Path.Lit",
		"Path.Param",
		"Path.Rest",
		"Path.String",
		"Path.URL",
		"PathArg",
		"PathParam",
		"PathParameter",
		"PathParameter.Enum",
		"PathParameter.MaxLength",
		"PathParameter.Maximum",
		"PathParameter.MinLength",
		"PathParameter.Minimum",
		"PathParameter.Pattern",
		"PatternMismatch",
		"QueryFrom",
		"QueryParam",
		"QueryParameter",
		"ReadForm",
		"Repeated",
		"RequestError",
		"RequestError.Error",
		"RequestError.Unwrap",
		"Root",
		"Route",
		"Rule",
		"Site",
		"Site.ServeHTTP",
		"TooLarge",
		"TooLong",
		"TooShort",
		"TooSmall",
		"URL",
		"URL.String",
		"URL.With",
	}, exported)
}

// TestLinks_with_a_value_of_the_wrong_type_dont_compile type-checks end
// user code against the package's source: an Arg, a Rest or a With whose
// value or parameter has the wrong type is a compile error.
func TestLinks_with_a_value_of_the_wrong_type_dont_compile(t *testing.T) {
	const header = `package enduser

import "github.com/z5labs/humus/hypermedia"

type ID int

var (
	contactID = hypermedia.PathParam[ID]("contactId")
	count     = hypermedia.PathParam[int]("count")
	file      = hypermedia.PathParam[string]("file")
	page      = hypermedia.QueryParam[int]("page")
	one       = hypermedia.Root().Lit("contacts").Param(contactID)
)
`
	cases := map[string]struct {
		body string
		want string // a fragment of the error, or "" to compile
	}{
		"an Arg of its type": {
			body: `var _ = one.URL(hypermedia.Arg(contactID, ID(7))).With(page, 2).String()`,
		},
		"a Rest of a string type": {
			body: `var _ = hypermedia.Root().Rest(file)`,
		},
		"a Rest of a named string type": {
			body: `type name string
var _ = hypermedia.Root().Rest(hypermedia.PathParam[name]("page"))`,
		},
		"an Arg of another type": {
			body: `var _ = one.URL(hypermedia.Arg(contactID, "7"))`,
			want: `cannot use "7"`,
		},
		"an Arg of another named type": {
			body: `var n int = 7
var _ = one.URL(hypermedia.Arg(contactID, n))`,
			want: "cannot use n",
		},
		"a Rest of an int": {
			body: `var _ = hypermedia.Root().Rest(count)`,
			want: "does not satisfy ~string",
		},
		"a With of another type": {
			body: `var _ = one.URL(hypermedia.Arg(contactID, 1)).With(page, "2")`,
			want: `cannot use "2"`,
		},
		"a Minimum of another type": {
			body: `var _ = contactID.Minimum("1")`,
			want: `cannot use "1"`,
		},
		"a form field of its type": {
			body: `var age = hypermedia.Field[ID]("age").Required().Minimum(18).Enum(18, 21)
func read(f *hypermedia.Form) ID { return hypermedia.FieldFrom(f, age) }`,
		},
		"a form field's Minimum of another type": {
			body: `var _ = hypermedia.Field[ID]("age").Minimum("18")`,
			want: `cannot use "18"`,
		},
		"a form field's value read as another type": {
			body: `var age = hypermedia.Field[ID]("age")
func read(f *hypermedia.Form) int { return hypermedia.FieldFrom(f, age) }`,
			want: "cannot use hypermedia.FieldFrom(f, age)",
		},
		"an AnyField that isn't a FormField": {
			body: `type mine struct{}
var _ hypermedia.AnyField = mine{}`,
			want: "does not implement hypermedia.AnyField",
		},
	}
	imp := importer.ForCompiler(token.NewFileSet(), "source", nil)
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, "enduser.go", header+tc.body+"\n", 0)
			require.NoError(t, err)
			_, err = (&types.Config{Importer: imp}).Check("enduser", fset, []*ast.File{f}, nil)
			if tc.want == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.want)
		})
	}
}

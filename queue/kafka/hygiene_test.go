// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kafka_test

import (
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus/queue/kafka"
)

// variableName matches a string that reads as an environment variable's
// name, such as "BROKERS" or "KAFKA_BROKERS".
var variableName = regexp.MustCompile(`^[A-Z][A-Z0-9]*(?:_[A-Z0-9]+)*$`)

// TestPackage_reads_no_environment_and_no_file checks the package's own
// source, test files excluded: it imports neither bedrock/config, nor os,
// os/exec, syscall, io/fs or io/ioutil, so it can read no environment
// variable and no file, and it never names an Environment. No string in it
// reads as a variable's name: the end user names every variable.
func TestPackage_reads_no_environment_and_no_file(t *testing.T) {
	t.Parallel()

	banned := map[string]string{
		"os":        "the package reads no environment variable and no file",
		"os/exec":   "the package reads no environment variable and no file",
		"syscall":   "the package reads no environment variable and no file",
		"io/fs":     "the package reads no file",
		"io/ioutil": "the package reads no file",
	}
	// bedrock/config and every package under it source settings, which is
	// the end user's to do.
	const config = "github.com/z5labs/bedrock/config"

	names, err := filepath.Glob("*.go")
	require.NoError(t, err)
	fset := token.NewFileSet()
	var checked []string
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		require.NoError(t, err)
		checked = append(checked, name)

		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			require.NoError(t, err)
			if why := banned[path]; why != "" {
				t.Errorf("%s: imports %s, but %s", fset.Position(imp.Pos()), path, why)
			}
			if path == config || strings.HasPrefix(path, config+"/") {
				t.Errorf("%s: imports %s, but the end user sources the settings", fset.Position(imp.Pos()), path)
			}
		}

		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.SelectorExpr:
				if n.Sel.Name == "Environment" {
					t.Errorf("%s: names an Environment", fset.Position(n.Pos()))
				}
			case *ast.BasicLit:
				if n.Kind != token.STRING {
					return true
				}
				s, err := strconv.Unquote(n.Value)
				require.NoError(t, err)
				for _, word := range strings.FieldsFunc(s, func(r rune) bool { return r == ' ' || r == ',' || r == ':' }) {
					if variableName.MatchString(word) && strings.Contains(word, "_") {
						t.Errorf("%s: names a variable, %s", fset.Position(n.Pos()), word)
					}
				}
				if variableName.MatchString(s) {
					t.Errorf("%s: names a variable, %s", fset.Position(n.Pos()), s)
				}
			}
			return true
		})
	}
	// The consumer's files and the producer's are all checked.
	require.Subset(t, checked, []string{"service.go", "settings.go", "dial.go", "logger.go", "producer.go", "breaker.go", "sasl.go"})
}

// TestPackage_exports_no_franz_go_type type-checks the package, test files
// excluded, and walks its exported API: every exported constant, variable,
// function and type, and through the types, every exported field and method,
// the package's unexported types included when the exported API shows them.
// No type in it is from a github.com/twmb/franz-go package, so an end user's
// code never builds or matches one of the client's types.
func TestPackage_exports_no_franz_go_type(t *testing.T) {
	t.Parallel()

	imp, _ := exportData(t)
	pkg, err := imp.Import(self)
	require.NoError(t, err)

	w := apiWalker{self: pkg, seen: map[types.Type]bool{}}
	scope := pkg.Scope()
	for _, name := range scope.Names() {
		if obj := scope.Lookup(name); obj.Exported() {
			w.walk(obj.Name(), obj.Type())
		}
	}
	require.Contains(t, w.named, self+".Settings", "the walk reached the package's types")
	require.Contains(t, w.named, self+".SASLMechanism")
	for _, use := range w.franzGo {
		t.Errorf("%s: a type from franz-go", use)
	}
}

// self is the package's import path.
const self = "github.com/z5labs/humus/queue/kafka"

// exportData returns an importer of the package and of every package it
// depends on, and those packages' import paths. They are read from the
// export data the go command builds, which is quicker than type-checking
// every one of them from source.
func exportData(t *testing.T) (types.Importer, []string) {
	t.Helper()
	out, err := exec.Command("go", "list", "-export", "-deps", "-f", "{{.ImportPath}}={{.Export}}", self).Output()
	require.NoError(t, err)
	exports := map[string]string{}
	var paths []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		path, file, _ := strings.Cut(line, "=")
		exports[path] = file
		paths = append(paths, path)
	}
	return importer.ForCompiler(token.NewFileSet(), "gc", func(path string) (io.ReadCloser, error) {
		file, ok := exports[path]
		if !ok || file == "" {
			return nil, fmt.Errorf("no export data for %s", path)
		}
		return os.Open(file)
	}), paths
}

// isFranzGo reports whether path is the import path of one of franz-go's
// packages.
func isFranzGo(path string) bool {
	return path == "github.com/twmb/franz-go" || strings.HasPrefix(path, "github.com/twmb/franz-go/")
}

// TestFranzGoValues_lists_every_error_value_franz_go_exports type-checks
// every franz-go package the package depends on, internal ones aside, which
// no code outside franz-go can name, and finds each exported variable whose
// type is error. kafka.FranzGoValues, which the tests walk errors for, names
// every one of them, so an upgrade of franz-go that adds one fails here.
func TestFranzGoValues_lists_every_error_value_franz_go_exports(t *testing.T) {
	t.Parallel()

	imp, paths := exportData(t)
	errorType := types.Universe.Lookup("error").Type()
	var found []string
	for _, path := range paths {
		if !isFranzGo(path) || strings.Contains(path, "/internal/") {
			continue
		}
		pkg, err := imp.Import(path)
		require.NoError(t, err)
		scope := pkg.Scope()
		for _, name := range scope.Names() {
			if v, ok := scope.Lookup(name).(*types.Var); ok && v.Exported() && types.Identical(v.Type(), errorType) {
				found = append(found, path+"."+name)
			}
		}
	}
	require.Contains(t, found, "github.com/twmb/franz-go/pkg/kgo.ErrClientClosed", "the walk reached franz-go's packages")
	require.ElementsMatch(t, found, slices.Collect(maps.Keys(kafka.FranzGoValues)))
}

// apiWalker walks the types an exported API shows.
type apiWalker struct {
	self    *types.Package
	seen    map[types.Type]bool
	named   []string // every named type reached, by its full name
	franzGo []string // where a franz-go type was reached
}

func (w *apiWalker) walk(at string, typ types.Type) {
	if w.seen[typ] {
		return
	}
	w.seen[typ] = true

	switch t := typ.(type) {
	case *types.Alias:
		w.named = append(w.named, t.Obj().Pkg().Path()+"."+t.Obj().Name())
		w.walk(at, types.Unalias(t))
	case *types.Named:
		obj := t.Obj()
		if obj.Pkg() == nil {
			return // error, comparable
		}
		path := obj.Pkg().Path()
		w.named = append(w.named, path+"."+obj.Name())
		if isFranzGo(path) {
			w.franzGo = append(w.franzGo, at+" is "+path+"."+obj.Name())
			return
		}
		for i := range t.TypeArgs().Len() {
			w.walk(at, t.TypeArgs().At(i))
		}
		if obj.Pkg() != w.self {
			return // another package's API is its own
		}
		for i := range t.NumMethods() {
			if m := t.Method(i); m.Exported() {
				w.walk(obj.Name()+"."+m.Name(), m.Type())
			}
		}
		w.walk(obj.Name(), t.Underlying())
	case *types.Pointer:
		w.walk(at, t.Elem())
	case *types.Slice:
		w.walk(at, t.Elem())
	case *types.Array:
		w.walk(at, t.Elem())
	case *types.Map:
		w.walk(at, t.Key())
		w.walk(at, t.Elem())
	case *types.Chan:
		w.walk(at, t.Elem())
	case *types.Signature:
		for _, tuple := range []*types.Tuple{t.Params(), t.Results()} {
			for i := range tuple.Len() {
				w.walk(at, tuple.At(i).Type())
			}
		}
		for i := range t.TypeParams().Len() {
			w.walk(at, t.TypeParams().At(i).Constraint())
		}
	case *types.Struct:
		for i := range t.NumFields() {
			if f := t.Field(i); f.Exported() || f.Embedded() {
				w.walk(at+"."+f.Name(), f.Type())
			}
		}
	case *types.Interface:
		for i := range t.NumMethods() {
			if m := t.Method(i); m.Exported() {
				w.walk(at+"."+m.Name(), m.Type())
			}
		}
		for i := range t.NumEmbeddeds() {
			w.walk(at, t.EmbeddedType(i))
		}
	case *types.TypeParam:
		w.walk(at, t.Constraint())
	}
}

// TestPackage_uses_kotels_Meter_alone checks the package's own source, test
// files excluded: of kotel, it uses NewMeter and MeterProvider and nothing
// else. kotel's tracer and NewKotel are not used: the record's span and the
// propagation through headers are humus's own.
func TestPackage_uses_kotels_Meter_alone(t *testing.T) {
	t.Parallel()

	allowed := map[string]bool{"NewMeter": true, "MeterProvider": true, "Meter": true}
	names, err := filepath.Glob("*.go")
	require.NoError(t, err)
	fset := token.NewFileSet()
	used := map[string]bool{}
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		require.NoError(t, err)

		local := ""
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			require.NoError(t, err)
			if path != "github.com/twmb/franz-go/plugin/kotel" {
				continue
			}
			local = "kotel"
			if imp.Name != nil {
				local = imp.Name.Name
			}
		}
		if local == "" {
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if x, ok := sel.X.(*ast.Ident); ok && x.Name == local {
				used[sel.Sel.Name] = true
				if !allowed[sel.Sel.Name] {
					t.Errorf("%s: uses kotel.%s, but only kotel's Meter is used", fset.Position(sel.Pos()), sel.Sel.Name)
				}
			}
			return true
		})
	}
	require.True(t, used["NewMeter"], "the package builds kotel's Meter")
}

// TestModule_imports_no_logging_bridge checks that franz-go's logs reach
// the Setup's LoggerProvider through the package's own adapter: the module
// requires neither franz-go's kslog plugin nor any of OpenTelemetry's
// logging bridges, and the package's own source imports neither.
func TestModule_imports_no_logging_bridge(t *testing.T) {
	t.Parallel()

	bridge := func(path string) bool {
		return path == "github.com/twmb/franz-go/plugin/kslog" ||
			strings.HasPrefix(path, "go.opentelemetry.io/contrib/bridges/")
	}

	mod, err := os.ReadFile("go.mod")
	require.NoError(t, err)
	for _, line := range strings.Split(string(mod), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if fields[0] == "require" && len(fields) > 1 {
			fields = fields[1:]
		}
		if bridge(fields[0]) {
			t.Errorf("go.mod requires %s", fields[0])
		}
	}

	names, err := filepath.Glob("*.go")
	require.NoError(t, err)
	fset := token.NewFileSet()
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		require.NoError(t, err)
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			require.NoError(t, err)
			if bridge(path) {
				t.Errorf("%s: imports %s", fset.Position(imp.Pos()), path)
			}
		}
	}
}

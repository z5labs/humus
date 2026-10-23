// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package humus_test

import (
	"bytes"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestModule_sets_the_OpenTelemetry_error_handler_once checks the module's
// own source, test files excluded: App.Run's is the only call to
// otel.SetErrorHandler in humus, and no package imports a logging bridge,
// since the lifecycle logs with the OpenTelemetry Logs API directly.
func TestModule_sets_the_OpenTelemetry_error_handler_once(t *testing.T) {
	fset := token.NewFileSet()
	var calls []string
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && path != "." && (strings.HasPrefix(d.Name(), ".") || d.Name() == "docs") {
			return filepath.SkipDir
		}
		if d.IsDir() || filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		imports := map[string]string{} // local name to path
		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return err
			}
			if strings.HasPrefix(p, "go.opentelemetry.io/contrib/bridges/") {
				t.Errorf("%s imports the logging bridge %s", path, p)
			}
			local := filepath.Base(p)
			if imp.Name != nil {
				local = imp.Name.Name
			}
			imports[local] = p
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "SetErrorHandler" {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); ok && imports[pkg.Name] == "go.opentelemetry.io/otel" {
				calls = append(calls, fset.Position(sel.Pos()).String())
			}
			return true
		})
		return nil
	})
	require.NoError(t, err)
	require.Len(t, calls, 1, "otel.SetErrorHandler is called at %v", calls)
	require.True(t, strings.HasPrefix(calls[0], "app.go:"), "otel.SetErrorHandler is called at %s, want App.Run", calls[0])
}

// TestRepository_holds_no_compiled_executables checks every file git tracks,
// and every untracked file it does not ignore, so every file git add -A would
// commit: none is a compiled executable. go build ./... writes a module's
// executable into the module's directory when main is the module's only
// package, so the executable of a module like that has to be ignored, or
// verifying a change leaves it behind to be committed.
func TestRepository_holds_no_compiled_executables(t *testing.T) {
	out, err := exec.CommandContext(t.Context(), "git", "ls-files", "-z", "--cached", "--others", "--exclude-standard").Output()
	require.NoError(t, err)

	magics := []struct {
		format string
		prefix []byte
	}{
		{"ELF", []byte("\x7fELF")},
		{"Mach-O", []byte{0xfe, 0xed, 0xfa, 0xce}},
		{"Mach-O", []byte{0xfe, 0xed, 0xfa, 0xcf}},
		{"Mach-O", []byte{0xce, 0xfa, 0xed, 0xfe}},
		{"Mach-O", []byte{0xcf, 0xfa, 0xed, 0xfe}},
		{"Mach-O universal", []byte{0xca, 0xfe, 0xba, 0xbe}},
		{"Mach-O universal", []byte{0xca, 0xfe, 0xba, 0xbf}},
		{"PE", []byte("MZ")},
		{"WebAssembly", []byte("\x00asm")},
	}
	for _, path := range strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00") {
		head, err := readHead(path, 4)
		if errors.Is(err, fs.ErrNotExist) {
			// Deleted from the working tree but not yet from the index.
			continue
		}
		require.NoError(t, err)
		for _, m := range magics {
			if bytes.HasPrefix(head, m.prefix) {
				t.Errorf("%s is a compiled %s executable", path, m.format)
				break
			}
		}
	}
}

// readHead returns up to the first n bytes of the regular file at path, and
// nothing for anything else, such as a directory holding a nested repository.
func readHead(path string, n int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, nil
	}
	head := make([]byte, n)
	k, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return head[:k], nil
}

// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package release

import (
	"regexp"
	"strings"
)

// docLine is a line of a doc that names a version of a released module other
// than the release's.
type docLine struct {
	// file is the doc's path from the repository's root, and n the line's
	// number in it, from 1.
	file string
	n    int
	// new is the line as the release sets it, and old the line as it is.
	new, old string
}

// outdated returns every line of the docs a release of the version changes.
// The docs are every tracked Markdown file, except any under a testdata
// directory, which holds fixtures, or under a directory whose name begins
// with ., such as .github and .claude, which hold the repository's tooling
// rather than anything its readers read.
func (r *runner) outdated(set moduleSet, tracked []string) ([]docLine, error) {
	// Every module path, and whether the release tags it.
	mods := map[string]bool{}
	for _, dir := range set.dirs {
		p, err := r.modulePath(dir)
		if err != nil {
			return nil, err
		}
		if p != "" {
			mods[p] = set.paths[p]
		}
	}
	d := newDocs(set.root, r.version, mods)

	var out []docLine
	for _, file := range tracked {
		if !isDoc(file) {
			continue
		}
		doc, err := r.show(file)
		if err != nil {
			return nil, err
		}
		for i, line := range lines(doc) {
			if n := d.rewrite(line); n != line {
				out = append(out, docLine{file: file, n: i + 1, new: n, old: line})
			}
		}
	}
	return out, nil
}

func isDoc(file string) bool {
	if !strings.HasSuffix(file, ".md") {
		return false
	}
	dir, _ := splitLast(file)
	if dir == "" {
		return true
	}
	for part := range strings.SplitSeq(dir, "/") {
		if part == "testdata" || strings.HasPrefix(part, ".") {
			return false
		}
	}
	return true
}

func splitLast(file string) (dir, name string) {
	i := strings.LastIndex(file, "/")
	if i < 0 {
		return "", file
	}
	return file[:i], file[i+1:]
}

// lines returns the lines of doc, without their newlines. A final line with
// no newline is a line; the empty string after a final newline is not.
func lines(doc string) []string {
	if doc == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(doc, "\n"), "\n")
}

// docs rewrites the lines of a doc that name a version of a released module
// other than the release's.
//
// A doc names a version in two forms: <path>@<ref>, as go get and go install
// take it, where <ref> may be a version, a branch such as main, or a query
// such as latest; and a line of a go.mod, <path> <version>, alone or after
// require. Only a path in a released module counts, a package path as well
// as a module path, and every line in either form counts, prose that names a
// past release that way included. A placeholder that names no version, such
// as vX.Y.Z, a v and a version core that holds no digit and no lowercase
// letter, is left as it is, and so is @none, which removes a requirement
// rather than naming one. A ref without the v, such as HEAD, is a branch.
type docs struct {
	version string
	// mods is every module path, and whether the release tags it.
	mods map[string]bool
	// path matches a path under the root module's, req a go.mod line that
	// requires one, and at one followed by @ and a ref. Each is leftmost
	// longest, as POSIX regular expressions are.
	path, req, at *regexp.Regexp
}

func newDocs(root, version string, mods map[string]bool) *docs {
	path := regexp.QuoteMeta(root) + `(/[A-Za-z0-9._~-]+)*`
	d := &docs{
		version: version,
		mods:    mods,
		path:    regexp.MustCompile(path),
		req:     regexp.MustCompile(`^[ \t]*(require[ \t]+)?` + path + `[ \t]+v[0-9][-A-Za-z0-9.+]*([ \t]*//.*)?[ \t\r]*$`),
		at:      regexp.MustCompile(path + `@[A-Za-z0-9._+/-]+`),
	}
	d.path.Longest()
	d.req.Longest()
	d.at.Longest()
	return d
}

// rewrite returns s as the release sets it.
func (d *docs) rewrite(s string) string {
	if d.req.MatchString(s) {
		loc := d.path.FindStringIndex(s)
		pre, p, rest := s[:loc[0]], s[loc[0]:loc[1]], s[loc[1]:]
		ws := rest[:len(rest)-len(strings.TrimLeft(rest, " \t"))]
		rest = rest[len(ws):]
		v := rest
		if i := strings.IndexAny(rest, " \t\r"); i >= 0 {
			v = rest[:i]
		}
		rest = rest[len(v):]
		if d.released(p) && v != d.version {
			return pre + p + ws + d.version + rest
		}
		return s
	}

	var b strings.Builder
	for {
		loc := d.at.FindStringIndex(s)
		if loc == nil {
			break
		}
		pre, tok := s[:loc[0]], s[loc[0]:loc[1]]
		s = s[loc[1]:]
		p, ref, _ := strings.Cut(tok, "@")
		trail := ""
		for strings.HasSuffix(ref, ".") || strings.HasSuffix(ref, "/") {
			trail = ref[len(ref)-1:] + trail
			ref = ref[:len(ref)-1]
		}
		if insideWord(pre) || ref == "" || ref == d.version || ref == "none" || placeholder(ref) || !d.released(p) {
			b.WriteString(pre)
			b.WriteString(tok)
			continue
		}
		for _, part := range []string{pre, p, "@", d.version, trail} {
			b.WriteString(part)
		}
	}
	return b.String() + s
}

// insideWord reports whether pre, the text before a path, ends in a
// character a path can hold, so that the path is the end of another, such
// as example.com/github.com/z5labs/humus.
func insideWord(pre string) bool {
	if pre == "" {
		return false
	}
	c := pre[len(pre)-1]
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || strings.IndexByte("._/-", c) >= 0
}

// placeholder reports whether ref is a v and a version core that holds no
// digit and no lowercase letter, such as vX.Y.Z, which names no version.
func placeholder(ref string) bool {
	core, ok := strings.CutPrefix(ref, "v")
	if !ok {
		return false
	}
	core, _, _ = strings.Cut(core, "-")
	return core != "" && !strings.ContainsAny(core, "abcdefghijklmnopqrstuvwxyz0123456789")
}

// released reports whether p is in a module the release tags: the module
// whose path is p's longest prefix, so that a tool's or an example's path
// under the root module's is never the root module's.
func (d *docs) released(p string) bool {
	best := ""
	for m := range d.mods {
		if (p == m || strings.HasPrefix(p, m+"/")) && len(m) > len(best) {
			best = m
		}
	}
	return best != "" && d.mods[best]
}

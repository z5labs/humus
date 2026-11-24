// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package inlineedit_test

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type library struct {
	name    string
	pattern *regexp.Regexp
}

var libraries = []library{
	{"htmx", regexp.MustCompile(`(?i)\bhtmx`)},
	{"htmx's hx-* attributes and HX-* headers", regexp.MustCompile(`(?i)\bhx-[a-z]`)},
	{"Datastar", regexp.MustCompile(`(?i)datastar`)},
	{"Datastar's event names", regexp.MustCompile(`(?i)\bdatastar-(patch-elements|patch-signals|merge-fragments|merge-signals|remove-fragments|remove-signals|execute-script)\b`)},
	{"Hotwire's @hotwired packages", regexp.MustCompile(`(?i)@hotwired\b`)},
	{"Turbo's frames", regexp.MustCompile(`(?i)\bturbo-frame\b`)},
	{"Turbo's streams", regexp.MustCompile(`(?i)\bturbo-stream\b`)},
	{"Turbo's media type", regexp.MustCompile(`(?i)\btext/vnd\.turbo-stream\.html\b`)},
	{"Unpoly", regexp.MustCompile(`(?i)\bunpoly\b`)},
	{"Unpoly's up-* attributes", regexp.MustCompile(`\bup-(accept|alias|align|animation|autosubmit|background|cache|clickable|confirm|defer|dismiss|dismissable|emit|expand|fail-[a-z]+|feedback|flashes|focus|follow|fragment|hungry|href|instant|interval|keep|layer|main|method|mode|nav|navigate|on-[a-z]+|params|peel|placement|poll|preload|reveal|scroll|source|submit|switch|target|transition|validate|viewport|watch|wrapper)\b`)},
	{"Unpoly's X-Up-* headers", regexp.MustCompile(`(?i)\bx-up-[a-z]`)},
	{"Unpoly's _up_method parameter", regexp.MustCompile(`(?i)\b_up_method\b`)},
	{"fixi", regexp.MustCompile(`(?i)\bfixi\b`)},
	{"fixi's fx-* attributes", regexp.MustCompile(`(?i)\bfx-[a-z]`)},
	{"Alpine AJAX", regexp.MustCompile(`(?i)\balpine[ -]ajax\b`)},
	{"Alpine AJAX's x-target attribute", regexp.MustCompile(`\bx-target\b`)},
	{"Alpine AJAX's X-Alpine-* headers", regexp.MustCompile(`(?i)\bx-alpine-[a-z]`)},
	{"Hyperview", regexp.MustCompile(`(?i)hyperview`)},
	{"idiomorph", regexp.MustCompile(`(?i)idiomorph`)},
}

func TestRepository_names_no_hypermedia_library_outside_its_examples_and_docs(t *testing.T) {
	root := filepath.Join("..", "..", "..")

	prefix, err := exec.CommandContext(t.Context(), "git", "-C", root, "rev-parse", "--show-prefix").Output()
	require.NoError(t, err)
	require.Empty(t, strings.TrimSpace(string(prefix)), "%s isn't the top of the working tree", root)

	out, err := exec.CommandContext(t.Context(), "git", "-C", root, "ls-files", "-z", "--cached", "--others", "--exclude-standard", "--", "*.go").Output()
	require.NoError(t, err)

	var read int
	for _, path := range strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00") {
		if path == "" || strings.HasPrefix(path, "example/") || strings.HasPrefix(path, "docs/") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		require.NoError(t, err)
		read++
		for _, lib := range libraries {
			for _, m := range lib.pattern.FindAllIndex(b, -1) {
				start := bytes.LastIndexByte(b[:m[0]], '\n') + 1
				end := len(b)
				if i := bytes.IndexByte(b[m[0]:], '\n'); i >= 0 {
					end = m[0] + i
				}
				t.Errorf("%s:%d names %s: %s", path, bytes.Count(b[:start], []byte("\n"))+1, lib.name, bytes.TrimSpace(b[start:end]))
			}
		}
	}
	require.NotZero(t, read, "no .go file outside example/ and docs/ was read")
}

func TestLibraries_find_each_library_and_nothing_else(t *testing.T) {
	samples := map[string][]string{
		"htmx": {`<script src="/static/htmx.min.js"></script>`, `import "github.com/angelofallars/htmx-go"`},
		"htmx's hx-* attributes and HX-* headers": {`<button hx-get="/contacts/1/edit">`, `r.Header.Get("HX-Request")`, `w.Header().Set("HX-Trigger", "contactSaved")`},
		"Datastar":                         {`r.Header.Get("Datastar-Request")`, `import "github.com/starfederation/datastar-go/datastar"`},
		"Datastar's event names":           {`event: datastar-patch-elements`, `event: datastar-merge-fragments`},
		"Hotwire's @hotwired packages":     {`import "@hotwired/turbo"`},
		"Turbo's frames":                   {`<turbo-frame id="contact-1">`, `r.Header.Get("Turbo-Frame")`},
		"Turbo's streams":                  {`<turbo-stream action="replace" target="contact-1">`},
		"Turbo's media type":               {`w.Header().Set("Content-Type", "text/vnd.turbo-stream.html")`},
		"Unpoly":                           {`<script src="/static/unpoly.min.js"></script>`},
		"Unpoly's up-* attributes":         {`<a href="/contacts/1/edit" up-target="#contact-1">`, `<form up-submit up-validate>`, `<a up-follow up-layer="new">`},
		"Unpoly's X-Up-* headers":          {`r.Header.Get("X-Up-Target")`, `w.Header().Set("X-Up-Events", "[]")`},
		"Unpoly's _up_method parameter":    {`r.PostFormValue("_up_method")`},
		"fixi":                             {`<script src="/static/fixi.js"></script>`},
		"fixi's fx-* attributes":           {`<button fx-action="/contacts/1" fx-method="put" fx-target="#contact-1">`},
		"Alpine AJAX":                      {`import "@imacrayon/alpine-ajax"`, `// Alpine AJAX swaps the row`},
		"Alpine AJAX's x-target attribute": {`<form x-target="contact-1" method="put">`},
		"Alpine AJAX's X-Alpine-* headers": {`r.Header.Get("X-Alpine-Request")`, `r.Header.Get("X-Alpine-Target")`},
		"Hyperview":                        {`w.Header().Set("Content-Type", "application/vnd.hyperview+xml")`},
		"idiomorph":                        {`<script src="/static/idiomorph.min.js"></script>`},
	}
	require.Len(t, samples, len(libraries))

	for _, lib := range libraries {
		require.Contains(t, samples, lib.name)
		for _, sample := range samples[lib.name] {
			require.Regexp(t, lib.pattern, sample, lib.name)
		}
	}

	for _, s := range []string{
		"prefixing",
		"alpine3.19",
		"turbo",
		"alpine",
		"Turbo",
		"Alpine",
		`mode.Validate("turbo")`,
		`Default("memcached:1.6.26-alpine3.19")`,
		`{"FROM alpine@sha256:ef56", baseImage{"alpine", "", "sha256:ef56"}, true}`,
		`w.Header().Set("Vary", "X-Target")`,
		`file := filepath.Join(dir, fmt.Sprintf("up-%d.txt", len(r.ups)+1))`,
		`a follow-up story keeps the docs up-to-date`,
		`func TestClient_the_caller_giving_up_isnt_counted(t *testing.T)`,
	} {
		for _, lib := range libraries {
			require.NotRegexp(t, lib.pattern, s, lib.name)
		}
	}
}

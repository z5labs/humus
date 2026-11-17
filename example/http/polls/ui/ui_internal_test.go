// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package ui

import (
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"
)

func TestScriptURL_changes_with_the_scripts_content(t *testing.T) {
	urls := make(map[string]bool)
	for _, content := range []string{"htmx 2.0.11", "htmx 2.0.12", "htmx 2.0.12 "} {
		u, err := build(nil, fstest.MapFS{"htmx.min.js": {Data: []byte(content)}}, templates)
		require.NoError(t, err)
		url, err := u.scriptURL("htmx.min.js")
		require.NoError(t, err)
		require.Regexp(t, `^/scripts/[0-9a-f]{64}/htmx\.min\.js$`, url)
		require.False(t, urls[url], "%q has the URL of another file's content", content)
		urls[url] = true
	}
}

func TestScriptURL_is_an_error_for_a_name_that_isnt_a_script(t *testing.T) {
	u, err := New(nil)
	require.NoError(t, err)
	_, err = u.scriptURL("nothing.js")
	require.ErrorContains(t, err, `"nothing.js" isn't a script`)
}

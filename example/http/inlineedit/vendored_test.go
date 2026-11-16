// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package inlineedit_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var update = flag.Bool("update", false, "download each vendored script again from its version")

type script struct {
	path       string
	datasource string
	depName    string
	version    string
	file       string
}

var scripts = []script{
	{path: "htmxui/static/htmx.min.js", datasource: "npm", depName: "htmx.org", version: "2.0.10", file: "dist/htmx.min.js"},
	{path: "htmxui/static/sse.min.js", datasource: "npm", depName: "htmx-ext-sse", version: "2.2.4", file: "dist/sse.min.js"},
	{path: "datastarui/static/datastar.js", datasource: "github-tags", depName: "starfederation/datastar", version: "v1.0.5", file: "bundles/datastar.js"},
}

func TestVendored_scripts_are_their_versions_files(t *testing.T) {
	client := &http.Client{Timeout: time.Minute}

	for _, s := range scripts {
		t.Run(s.path, func(t *testing.T) {
			want, err := download(t, client, s)
			require.NoError(t, err, "%s: downloading %s %s's %s", s.path, s.depName, s.version, s.file)

			path := filepath.FromSlash(s.path)
			if *update {
				require.NoError(t, os.WriteFile(path, want, 0o644))
			}

			got, err := os.ReadFile(path)
			require.NoError(t, err)
			if !bytes.Equal(want, got) {
				t.Errorf("%s isn't %s %s's %s: run go test -run TestVendored . -update to download it again", s.path, s.depName, s.version, s.file)
			}
		})
	}
}

func TestVendored_scripts_are_each_vendored_file(t *testing.T) {
	var vendored []string
	for _, dir := range []string{"htmxui/static", "datastarui/static"} {
		entries, err := os.ReadDir(filepath.FromSlash(dir))
		require.NoError(t, err)
		for _, e := range entries {
			vendored = append(vendored, dir+"/"+e.Name())
		}
	}

	var declared []string
	for _, s := range scripts {
		declared = append(declared, s.path)
	}
	require.ElementsMatch(t, vendored, declared)
}

func download(t *testing.T, client *http.Client, s script) ([]byte, error) {
	switch s.datasource {
	case "npm":
		return downloadNPM(t, client, s)
	case "github-tags":
		return get(t, client, "https://raw.githubusercontent.com/"+s.depName+"/"+s.version+"/"+s.file)
	default:
		return nil, fmt.Errorf("unknown datasource %q", s.datasource)
	}
}

func downloadNPM(t *testing.T, client *http.Client, s script) ([]byte, error) {
	b, err := get(t, client, "https://registry.npmjs.org/"+s.depName+"/"+s.version)
	if err != nil {
		return nil, err
	}
	var manifest struct {
		Dist struct {
			Integrity string `json:"integrity"`
			Tarball   string `json:"tarball"`
		} `json:"dist"`
	}
	if err := json.Unmarshal(b, &manifest); err != nil {
		return nil, fmt.Errorf("reading the registry's manifest: %w", err)
	}

	tarball, err := get(t, client, manifest.Dist.Tarball)
	if err != nil {
		return nil, err
	}
	sum := sha512.Sum512(tarball)
	if integrity := "sha512-" + base64.StdEncoding.EncodeToString(sum[:]); integrity != manifest.Dist.Integrity {
		return nil, fmt.Errorf("%s is %s, not the registry's integrity %s", manifest.Dist.Tarball, integrity, manifest.Dist.Integrity)
	}

	gz, err := gzip.NewReader(bytes.NewReader(tarball))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s has no package/%s", manifest.Dist.Tarball, s.file)
		}
		if err != nil {
			return nil, err
		}
		if h.Name == "package/"+s.file {
			return io.ReadAll(tr)
		}
	}
}

func get(t *testing.T, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	b, err := io.ReadAll(resp.Body)
	err = errors.Join(err, resp.Body.Close())
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return b, nil
}

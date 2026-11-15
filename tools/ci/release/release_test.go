// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package release_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus/tools/ci/internal/gittest"
	"github.com/z5labs/humus/tools/ci/release"
)

const (
	root   = "github.com/z5labs/humus"
	pseudo = "v0.0.0-00010101000000-000000000000"
)

// newRepo creates a repository whose remote origin is a bare repository,
// with one module per <dir>=<module path> argument (. for the root), commits
// it, pushes main and returns its directory. Each module holds one Go file,
// unless its argument starts with -, as the Hugo module under docs/ does not.
func newRepo(t *testing.T, specs ...string) string {
	t.Helper()
	repo := gittest.New(t)
	for _, spec := range specs {
		mod, noGo := strings.CutPrefix(spec, "-")
		dir, path, ok := strings.Cut(mod, "=")
		require.True(t, ok, spec)
		gittest.Write(t, repo, dir+"/go.mod", "module "+path+"\n\ngo 1.27.0\n")
		if !noGo {
			gittest.Write(t, repo, dir+"/p.go", "package p\n")
		}
	}
	gittest.Commit(t, repo, "initial")
	return repo
}

// result is what a run printed and returned.
type result struct {
	stdout, stderr string
	err            error
}

// out returns everything the run printed, and each line of its error, as
// the command prints them.
func (r result) out() string {
	s := r.stdout + r.stderr
	if r.err != nil {
		s += r.err.Error() + "\n"
	}
	return s
}

func run(t *testing.T, dir string, mode release.Mode, version string) result {
	t.Helper()
	return runWith(t, dir, release.Options{Mode: mode, Version: version})
}

func runWith(t *testing.T, dir string, o release.Options) result {
	t.Helper()
	o.Env = gittest.Env()
	var stdout, stderr strings.Builder
	err := release.Run(t.Context(), dir, o, &stdout, &stderr)
	return result{stdout: stdout.String(), stderr: stderr.String(), err: err}
}

func requireOK(t *testing.T, r result) {
	t.Helper()
	require.NoError(t, r.err, "output:\n%s", r.out())
}

func requireFails(t *testing.T, r result) {
	t.Helper()
	require.Error(t, r.err, "output:\n%s", r.out())
}

func requireOutput(t *testing.T, r result, want ...string) {
	t.Helper()
	for _, w := range want {
		require.Contains(t, r.out(), w)
	}
}

func rejectOutput(t *testing.T, r result, pattern string) {
	t.Helper()
	require.NotRegexp(t, regexp.MustCompile(pattern), r.out())
}

// remoteTags returns every tag on repo's remote, sorted.
func remoteTags(t *testing.T, repo string) []string {
	t.Helper()
	var tags []string
	for line := range strings.SplitSeq(gittest.Git(t, repo, "ls-remote", "--tags", "origin"), "\n") {
		if _, ref, ok := strings.Cut(line, "\t"); ok {
			tags = append(tags, ref)
		}
	}
	slices.Sort(tags)
	return tags
}

func status(t *testing.T, repo string) string {
	t.Helper()
	return gittest.Git(t, repo, "status", "--porcelain")
}

// goList lists the packages of the module at dir offline, with the Go
// tool's defaults, and fails the test when go list does.
func goList(t *testing.T, dir string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "go", "list", "./...")
	cmd.Dir = dir
	cmd.Env = append(gittest.Env(), "GOFLAGS=", "GOWORK=off", "GOPROXY=off")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "go list in %s: %s", dir, out)
}

func TestRun_finds_every_module_and_skips_ignored_and_empty_ones(t *testing.T) {
	repo := newRepo(t, ".="+root, "grpc="+root+"/grpc", "queue/kafka="+root+"/queue/kafka", "-docs="+root,
		"grpc/testdata/x=example.com/x", "_tools=example.com/tools", "queue/.hidden/m=example.com/m")

	r := run(t, repo, release.DryRun, "v0.16.0")
	requireOK(t, r)
	requireOutput(t, r,
		"would create v0.16.0",
		"would create grpc/v0.16.0",
		"would create queue/kafka/v0.16.0",
		"skip docs/go.mod: it holds no Go packages",
		"skip grpc/testdata/x/go.mod: under testdata",
		"skip _tools/go.mod: under _tools",
		"skip queue/.hidden/m/go.mod: under .hidden",
		"dry run: changed nothing",
	)
	rejectOutput(t, r, `would create (docs|grpc/testdata|_tools|queue/\.hidden)`)
	require.Empty(t, remoteTags(t, repo), "a dry run pushes no tag")
	require.Empty(t, gittest.Git(t, repo, "tag"), "a dry run creates no tag")

	r = run(t, repo, release.List, "v0.16.0")
	requireOK(t, r)
	require.Equal(t, root+"\n"+root+"/grpc\n"+root+"/queue/kafka\n", r.stdout)
}

func TestRun_a_Go_file_ignored_or_in_a_nested_module_is_no_package_of_its_parent(t *testing.T) {
	repo := newRepo(t, "-.="+root, "-a="+root+"/a", "-b="+root+"/b", "-c="+root+"/c", "a/n="+root+"/a/n")
	gittest.Write(t, repo, "p.go", "package p\n")
	gittest.Write(t, repo, "b/testdata/p.go", "package p\n")
	gittest.Write(t, repo, "c/_x/p.go", "package p\n")
	gittest.Write(t, repo, "c/_p.go", "package p\n")
	gittest.Write(t, repo, "c/.p.go", "package p\n")
	gittest.Commit(t, repo, "Go files")

	r := run(t, repo, release.List, "v0.16.0")
	requireOK(t, r)
	requireOutput(t, r,
		"skip a/go.mod: it holds no Go packages",
		"skip b/go.mod: it holds no Go packages",
		"skip c/go.mod: it holds no Go packages",
	)
	require.Equal(t, root+"\n"+root+"/a/n\n", r.stdout)
}

func TestRun_the_root_module_does_not_claim_a_nested_modules_Go_files(t *testing.T) {
	repo := newRepo(t, "-.="+root, "d="+root+"/d")
	r := run(t, repo, release.List, "v0.16.0")
	requireOK(t, r)
	require.Equal(t, "skip go.mod: it holds no Go packages\n", r.stderr)
	require.Equal(t, root+"/d\n", r.stdout)
}

// No module under example/ or tools/ is released, whatever it requires, and
// the run says so for each one; its Go files are still its own, not its
// parent's. Its requirements on released modules are still set to the
// version, as theirs are, and tagging refuses a commit at which one names
// another version.
func TestRun_never_releases_examples_or_tools_but_pins_their_requirements(t *testing.T) {
	for _, tc := range []struct {
		under string
		dirs  []string
	}{
		{"example", []string{"example", "example/http/shoppingcart", "example/job/billing"}},
		{"tools", []string{"tools/flaky", "tools/other"}},
	} {
		t.Run(tc.under, func(t *testing.T) {
			specs := []string{"-.=" + root, "grpc=" + root + "/grpc"}
			for _, dir := range tc.dirs {
				specs = append(specs, dir+"="+root+"/"+dir)
			}
			repo := newRepo(t, specs...)
			requiring := tc.dirs[len(tc.dirs)-2]
			gittest.Append(t, repo, requiring+"/go.mod", "\nrequire github.com/z5labs/humus/grpc v0.15.0\n")
			gittest.Commit(t, repo, tc.under)

			r := run(t, repo, release.DryRun, "v0.16.0")
			requireOK(t, r)
			for _, dir := range tc.dirs {
				requireOutput(t, r, "skip "+dir+"/go.mod: under "+tc.under+"/, whose modules are never released")
			}
			requireOutput(t, r,
				"skip go.mod: it holds no Go packages",
				"would set "+requiring+"/go.mod: require "+root+"/grpc v0.16.0 (now v0.15.0)",
				"would create grpc/v0.16.0",
			)
			rejectOutput(t, r, "would create "+tc.under)

			r = run(t, repo, release.List, "v0.16.0")
			requireOK(t, r)
			require.Equal(t, root+"/grpc\n", r.stdout)

			r = run(t, repo, release.Tag, "v0.16.0")
			requireFails(t, r)
			requireOutput(t, r, requiring+"/go.mod requires "+root+"/grpc v0.15.0, want v0.16.0")
			require.Empty(t, remoteTags(t, repo))

			r = run(t, repo, release.Bump, "v0.16.0")
			requireOK(t, r)
			requireOutput(t, r, "set "+requiring+"/go.mod: require "+root+"/grpc v0.16.0 (was v0.15.0)")
			gittest.Commit(t, repo, "bump")

			r = run(t, repo, release.Tag, "v0.16.0")
			requireOK(t, r)
			require.Equal(t, []string{"refs/tags/grpc/v0.16.0"}, remoteTags(t, repo))
		})
	}
}

// An example that requires a nested module, which requires the root module,
// builds only while its own requirement on the root names the version the
// nested module requires. Every replace points at the local directories, so
// its build selects the highest version any of them requires, and a go.mod
// that names another is out of date. The bump sets the example's
// requirements with the nested module's, so the example still builds after
// it.
func TestRun_bumps_an_examples_requirements_with_the_nested_modules(t *testing.T) {
	repo := newRepo(t, ".="+root, "grpc="+root+"/grpc", "example/grpc/wallet="+root+"/example/grpc/wallet")
	gittest.Append(t, repo, "grpc/go.mod", "\nrequire github.com/z5labs/humus "+pseudo+"\n\nreplace github.com/z5labs/humus => ../\n")
	gittest.Append(t, repo, "example/grpc/wallet/go.mod", `
require (
	github.com/z5labs/humus v0.15.0
	github.com/z5labs/humus/grpc `+pseudo+`
)

replace (
	github.com/z5labs/humus => ../../..
	github.com/z5labs/humus/grpc => ../../../grpc
)
`)
	gittest.Write(t, repo, "grpc/p.go", "package p\n\nimport _ \"github.com/z5labs/humus\"\n")
	gittest.Write(t, repo, "example/grpc/wallet/p.go", "package p\n\nimport _ \"github.com/z5labs/humus/grpc\"\n")
	gittest.Commit(t, repo, "nested requirement")
	before := gittest.Git(t, repo, "rev-parse", "HEAD")
	goList(t, filepath.Join(repo, "example", "grpc", "wallet"))

	r := run(t, repo, release.DryRun, "v0.16.0-rc.1")
	requireOK(t, r)
	requireOutput(t, r,
		"skip example/grpc/wallet/go.mod: under example/, whose modules are never released",
		"would set grpc/go.mod: require "+root+" v0.16.0-rc.1 (now "+pseudo+")",
		"would set example/grpc/wallet/go.mod: require "+root+" v0.16.0-rc.1 (now v0.15.0)",
		"would set example/grpc/wallet/go.mod: require "+root+"/grpc v0.16.0-rc.1 (now "+pseudo+")",
		"release v0.16.0-rc.1 at the commit that makes these changes on top of "+before,
	)
	rejectOutput(t, r, "would create example")
	require.Empty(t, status(t, repo), "a dry run changes no file")

	r = run(t, repo, release.Bump, "v0.16.0-rc.1")
	requireOK(t, r)
	requireOutput(t, r,
		"set grpc/go.mod: require "+root+" v0.16.0-rc.1 (was "+pseudo+")",
		"set example/grpc/wallet/go.mod: require "+root+" v0.16.0-rc.1 (was v0.15.0)",
		"set example/grpc/wallet/go.mod: require "+root+"/grpc v0.16.0-rc.1 (was "+pseudo+")",
	)
	require.Contains(t, gittest.Read(t, repo, "example/grpc/wallet/go.mod"), "replace (", "the bump keeps the replaces")
	for _, dir := range []string{".", "grpc", "example/grpc/wallet"} {
		goList(t, filepath.Join(repo, filepath.FromSlash(dir)))
	}

	gittest.Commit(t, repo, "bump")
	r = run(t, repo, release.Tag, "v0.16.0-rc.1")
	requireOK(t, r)
	require.Equal(t, []string{"refs/tags/grpc/v0.16.0-rc.1", "refs/tags/v0.16.0-rc.1"}, remoteTags(t, repo))
}

func TestRun_tags_every_module_at_the_release_commit(t *testing.T) {
	repo := newRepo(t, ".="+root, "grpc="+root+"/grpc", "queue/kafka="+root+"/queue/kafka", "-docs="+root)
	want := []string{"refs/tags/grpc/v0.16.0", "refs/tags/queue/kafka/v0.16.0", "refs/tags/v0.16.0"}

	r := run(t, repo, release.Tag, "v0.16.0")
	requireOK(t, r)
	requireOutput(t, r, "created v0.16.0", "created grpc/v0.16.0", "created queue/kafka/v0.16.0")
	require.Equal(t, want, remoteTags(t, repo))
	releaseCommit := gittest.Git(t, repo, "rev-parse", "HEAD")
	for _, tag := range []string{"v0.16.0", "grpc/v0.16.0", "queue/kafka/v0.16.0"} {
		require.Equal(t, releaseCommit, gittest.Git(t, repo+".git", "rev-parse", tag+"^{commit}"), tag)
	}

	t.Log("a re-run against tags already at the release commit changes nothing")
	r = run(t, repo, release.Tag, "v0.16.0")
	requireOK(t, r)
	requireOutput(t, r,
		"exists v0.16.0 at the release commit, skipping",
		"exists grpc/v0.16.0 at the release commit, skipping",
		"nothing to create",
	)
	require.Equal(t, want, remoteTags(t, repo))

	t.Log("the commit names the release commit, and a module added after it is not tagged with it")
	gittest.Write(t, repo, "later/go.mod", "module "+root+"/later\n")
	gittest.Write(t, repo, "later/p.go", "package p\n")
	gittest.Commit(t, repo, "later")
	r = runWith(t, repo, releaseOptions(release.DryRun, "v0.16.0", releaseCommit))
	requireOK(t, r)
	require.NotContains(t, r.out(), "later")

	r = runWith(t, repo, releaseOptions(release.DryRun, "v0.16.0", "nonexistent"))
	requireFails(t, r)
	requireOutput(t, r, "nonexistent does not name a commit")

	t.Log("the release commit defaults to HEAD, so the release tag existing at another commit fails the run")
	r = run(t, repo, release.Tag, "v0.16.0")
	requireFails(t, r)
	requireOutput(t, r, "v0.16.0 already exists at "+releaseCommit+", not at the release commit")
	require.Equal(t, want, remoteTags(t, repo), "nothing is pushed")
}

func releaseOptions(mode release.Mode, version, commit string) release.Options {
	return release.Options{Mode: mode, Version: version, Commit: commit}
}

func TestRun_fails_on_a_nested_tag_at_another_commit(t *testing.T) {
	repo := newRepo(t, ".="+root, "grpc="+root+"/grpc", "job="+root+"/job")
	gittest.Git(t, repo, "commit", "-q", "--allow-empty", "-m", "other")
	gittest.Git(t, repo, "push", "-q", "origin", "HEAD:refs/tags/grpc/v0.16.0")
	gittest.Git(t, repo, "reset", "-q", "--hard", "HEAD~1")

	r := run(t, repo, release.Tag, "v0.16.0")
	requireFails(t, r)
	requireOutput(t, r, "grpc/v0.16.0 already exists at")
	require.Equal(t, []string{"refs/tags/grpc/v0.16.0"}, remoteTags(t, repo), "nothing is pushed")
}

func TestRun_compares_an_annotated_tag_by_its_commit(t *testing.T) {
	repo := newRepo(t, ".="+root, "grpc="+root+"/grpc")
	gittest.Git(t, repo, "tag", "-a", "-m", "annotated", "grpc/v0.16.0")
	gittest.Git(t, repo, "push", "-q", "origin", "grpc/v0.16.0")

	r := run(t, repo, release.Tag, "v0.16.0")
	requireOK(t, r)
	requireOutput(t, r, "exists grpc/v0.16.0 at the release commit, skipping", "created v0.16.0")
}

// A requirement between humus modules that names another version refuses
// the release, listing each such requirement; a dry run shows what a bump
// would set; a bump sets them, leaving every other requirement alone; and
// the release then goes ahead.
func TestRun_refuses_stale_requirements_until_they_are_bumped(t *testing.T) {
	repo := newRepo(t, ".="+root, "grpc="+root+"/grpc", "queue/kafka="+root+"/queue/kafka", "job="+root+"/job")
	gittest.Append(t, repo, "grpc/go.mod", "\nrequire github.com/z5labs/humus "+pseudo+"\n\nreplace github.com/z5labs/humus => ../\n")
	gittest.Append(t, repo, "queue/kafka/go.mod", `
require (
	example.com/other v1.2.3
	github.com/z5labs/humus `+pseudo+` // indirect
)

replace github.com/z5labs/humus => ../..
`)
	gittest.Append(t, repo, "job/go.mod", `
require (
	github.com/z5labs/humus v0.16.0
	github.com/z5labs/humus/grpc v0.15.0
)
`)
	gittest.Commit(t, repo, "requirements")
	before := gittest.Git(t, repo, "rev-parse", "HEAD")

	r := run(t, repo, release.Tag, "v0.16.0")
	requireFails(t, r)
	requireOutput(t, r,
		"grpc/go.mod requires "+root+" "+pseudo+", want v0.16.0",
		"queue/kafka/go.mod requires "+root+" "+pseudo+", want v0.16.0",
		"job/go.mod requires "+root+"/grpc v0.15.0, want v0.16.0",
		release.BumpCommand+" v0.16.0",
	)
	require.NotContains(t, r.out(), "job/go.mod requires "+root+" v0.16.0")
	require.NotContains(t, r.out(), "example.com/other")
	require.Empty(t, remoteTags(t, repo))

	r = run(t, repo, release.DryRun, "v0.16.0")
	requireOK(t, r)
	requireOutput(t, r,
		"would set grpc/go.mod: require "+root+" v0.16.0 (now "+pseudo+")",
		"would set job/go.mod: require "+root+"/grpc v0.16.0 (now v0.15.0)",
		"release v0.16.0 at the commit that makes these changes on top of "+before,
		"would create grpc/v0.16.0",
	)
	require.Empty(t, status(t, repo), "a dry run changes no file")

	r = run(t, repo, release.Bump, "v0.16.0")
	requireOK(t, r)
	requireOutput(t, r,
		"set grpc/go.mod: require "+root+" v0.16.0 (was "+pseudo+")",
		"set queue/kafka/go.mod: require "+root+" v0.16.0 (was "+pseudo+")",
		"set job/go.mod: require "+root+"/grpc v0.16.0 (was v0.15.0)",
	)
	kafka := gittest.Read(t, repo, "queue/kafka/go.mod")
	require.Contains(t, kafka, "github.com/z5labs/humus v0.16.0 // indirect", "the bump keeps the // indirect comment")
	require.Contains(t, kafka, "example.com/other v1.2.3", "the bump changes no other requirement")
	require.Contains(t, kafka, "replace github.com/z5labs/humus => ../..", "the bump keeps the replace")

	r = run(t, repo, release.Bump, "v0.16.0")
	requireOK(t, r)
	requireOutput(t, r, "every requirement on a released humus module already names v0.16.0")

	gittest.Commit(t, repo, "bump")
	r = run(t, repo, release.Tag, "v0.16.0")
	requireOK(t, r)
	requireOutput(t, r, "created v0.16.0", "created job/v0.16.0")
}

// A release sets every humus version the docs tell a reader to install or
// require, at a version or at a branch, in every Markdown file but those
// under testdata or a hidden directory; a dry run prints each line it would
// change, and tagging refuses a commit at which one names another version. A
// pre-release changes none of them, and tagging lets it through.
// Placeholders, @none, an unreleased module's path, and a path inside
// another word are left alone. A doc added after a release is set by the
// next one.
func TestRun_sets_the_humus_versions_the_docs_name(t *testing.T) {
	repo := newRepo(t, ".="+root, "grpc="+root+"/grpc", "tools/flaky="+root+"/tools/flaky")
	gittest.Write(t, repo, "README.md", "# humus\n\n```sh\ngo get "+root+"@main\n```\n\nRequire the same version of every module, `go get "+root+"@vX.Y.Z "+root+"/grpc@vX.Y.Z`.\n")
	index := `Install it with go get ` + root + `/grpc/health@v0.15.0, or with go get ` + root + `@latest.

Remove it with go get ` + root + `/grpc@none. Run go install ` + root + `/tools/flaky@main.
See example.com/` + root + `@main, which is not humus.

` + "```go-mod" + `
require ` + root + ` v0.15.0

require (
	` + root + `/grpc v0.15.0 // indirect
	example.com/other v1.2.3
)
` + "```" + `
` + fmt.Sprintf("\nTrack it with go get %s@HEAD.\n```go-mod\nrequire %s v0.15.0  \nrequire %s/grpc v0.15.0\r\n```\n", root, root, root)
	gittest.Write(t, repo, "docs/content/_index.md", index)
	gittest.Write(t, repo, "docs/testdata/fixture.md", "go get "+root+"@main\n")
	gittest.Write(t, repo, ".github/notes.md", "go get "+root+"@main\n")
	gittest.Commit(t, repo, "docs")
	before := gittest.Git(t, repo, "rev-parse", "HEAD")

	r := run(t, repo, release.DryRun, "v0.16.0-rc.1")
	requireOK(t, r)
	requireOutput(t, r, "leave the docs alone: v0.16.0-rc.1 is a pre-release, so they keep naming the latest release")
	rejectOutput(t, r, `would set [^ ]*\.md`)
	r = run(t, repo, release.Bump, "v0.16.0-rc.1")
	requireOK(t, r)
	requireOutput(t, r, "leave the docs alone: v0.16.0-rc.1 is a pre-release")
	require.Empty(t, status(t, repo), "a pre-release changes no doc")
	r = run(t, repo, release.Tag, "v0.16.0-rc.1")
	requireOK(t, r)
	requireOutput(t, r, "created v0.16.0-rc.1")

	r = run(t, repo, release.Tag, "v0.16.0")
	requireFails(t, r)
	requireOutput(t, r,
		"README.md:4 names a humus version other than v0.16.0: go get "+root+"@main",
		"docs/content/_index.md:1 names a humus version other than v0.16.0",
		release.BumpCommand+" v0.16.0",
	)
	require.Equal(t, []string{"refs/tags/grpc/v0.16.0-rc.1", "refs/tags/v0.16.0-rc.1"}, remoteTags(t, repo), "nothing is pushed")

	r = run(t, repo, release.DryRun, "v0.16.0")
	requireOK(t, r)
	requireOutput(t, r,
		"would set README.md:4: go get "+root+"@v0.16.0 (now go get "+root+"@main)",
		"would set docs/content/_index.md:1: Install it with go get "+root+"/grpc/health@v0.16.0, or with go get "+root+"@v0.16.0. (now Install it with go get "+root+"/grpc/health@v0.15.0, or with go get "+root+"@latest.)",
		"would set docs/content/_index.md:7: require "+root+" v0.16.0 (now require "+root+" v0.15.0)",
		"would set docs/content/_index.md:10: "+root+"/grpc v0.16.0 // indirect (now "+root+"/grpc v0.15.0 // indirect)",
		"would set docs/content/_index.md:15: Track it with go get "+root+"@v0.16.0. (now Track it with go get "+root+"@HEAD.)",
		"would set docs/content/_index.md:17: require "+root+" v0.16.0   (now require "+root+" v0.15.0  )",
		"would set docs/content/_index.md:18: require "+root+"/grpc v0.16.0\r (now require "+root+"/grpc v0.15.0\r)",
		"release v0.16.0 at the commit that makes these changes on top of "+before,
	)
	rejectOutput(t, r, `would set (README\.md:7|docs/content/_index\.md:(3|4|9|11|12)|docs/testdata|\.github)`)
	require.Empty(t, status(t, repo), "a dry run changes no file")

	r = run(t, repo, release.Bump, "v0.16.0")
	requireOK(t, r)
	requireOutput(t, r,
		"set README.md:4: go get "+root+"@v0.16.0 (was go get "+root+"@main)",
		"set docs/content/_index.md:7: require "+root+" v0.16.0 (was require "+root+" v0.15.0)",
	)
	readme := gittest.Read(t, repo, "README.md")
	require.Contains(t, strings.Split(readme, "\n"), "go get "+root+"@v0.16.0")
	require.Contains(t, readme, "`go get "+root+"@vX.Y.Z "+root+"/grpc@vX.Y.Z`", "a placeholder is left as it is")
	wantIndex := strings.NewReplacer(
		"grpc/health@v0.15.0", "grpc/health@v0.16.0",
		root+"@latest", root+"@v0.16.0",
		"\nrequire "+root+" v0.15.0", "\nrequire "+root+" v0.16.0",
		"\nrequire "+root+"/grpc v0.15.0", "\nrequire "+root+"/grpc v0.16.0",
		root+"@HEAD", root+"@v0.16.0",
		"\t"+root+"/grpc v0.15.0", "\t"+root+"/grpc v0.16.0",
	).Replace(index)
	require.Equal(t, wantIndex, gittest.Read(t, repo, "docs/content/_index.md"))
	require.Equal(t, "go get "+root+"@main\n", gittest.Read(t, repo, "docs/testdata/fixture.md"), "a testdata doc is no doc")
	require.Equal(t, "go get "+root+"@main\n", gittest.Read(t, repo, ".github/notes.md"), "a doc under a hidden directory is no doc")

	r = run(t, repo, release.Bump, "v0.16.0")
	requireOK(t, r)
	requireOutput(t, r, "every humus version the docs name is already v0.16.0")

	gittest.Commit(t, repo, "docs bump")
	r = run(t, repo, release.Tag, "v0.16.0")
	requireOK(t, r)
	requireOutput(t, r, "created v0.16.0")
	require.Contains(t, strings.Split(gittest.Git(t, repo+".git", "show", "v0.16.0:README.md"), "\n"), "go get "+root+"@v0.16.0", "the tagged README names the release")

	gittest.Write(t, repo, "docs/content/later.md", "go get "+root+"/grpc@main")
	gittest.Commit(t, repo, "later doc")
	r = run(t, repo, release.Bump, "v0.17.0")
	requireOK(t, r)
	requireOutput(t, r,
		"set docs/content/later.md:1: go get "+root+"/grpc@v0.17.0 (was go get "+root+"/grpc@main)",
		"set README.md:4: go get "+root+"@v0.17.0 (was go get "+root+"@v0.16.0)",
	)
	require.Equal(t, "go get "+root+"/grpc@v0.17.0\n", gittest.Read(t, repo, "docs/content/later.md"), "a line set ends in a newline, as every line the bump writes does")
}

// A dry run before the bump cannot know the commit the tags will go on, so
// any tag that already exists is a conflict.
func TestRun_a_dry_run_with_changes_to_make_conflicts_with_any_existing_tag(t *testing.T) {
	repo := newRepo(t, ".="+root, "grpc="+root+"/grpc")
	gittest.Append(t, repo, "grpc/go.mod", "\nrequire github.com/z5labs/humus "+pseudo+"\n")
	gittest.Commit(t, repo, "requirement")
	gittest.Git(t, repo, "push", "-q", "origin", "HEAD:refs/tags/grpc/v0.16.0")

	r := run(t, repo, release.DryRun, "v0.16.0")
	requireFails(t, r)
	requireOutput(t, r, "grpc/v0.16.0 already exists at")
}

func TestRun_refuses_malformed_and_v2_versions_before_anything_is_created(t *testing.T) {
	repo := newRepo(t, ".="+root, "grpc="+root+"/grpc")
	for _, version := range []string{"0.16.0", "v0.16", "v0.16.0+build", "v01.2.3", "v0.16.0-", "v0.16.0-rc..1", "v0.16.0-rc.", "v0.16.0-01", ""} {
		r := run(t, repo, release.Tag, version)
		requireFails(t, r)
		requireOutput(t, r, version+" is not a Go module version")
	}
	requireOutput(t, run(t, repo, release.Tag, "v0.16.0+build"), "build metadata")
	requireOutput(t, run(t, repo, release.Tag, "v0.16.0-rc..1"), "its pre-release has an empty identifier")
	requireOutput(t, run(t, repo, release.Tag, "v0.16.0-01"), "numeric pre-release identifier 01 has a leading zero")

	for _, version := range []string{"v0.16.0-rc.1", "v1.0.0", "v1.0.0-0.alpha-1"} {
		r := run(t, repo, release.DryRun, version)
		requireOK(t, r)
		requireOutput(t, r, "would create grpc/"+version)
	}
	for _, mode := range []release.Mode{release.Tag, release.Bump} {
		r := run(t, repo, mode, "v2.0.0")
		requireFails(t, r)
		requireOutput(t, r, "v2.0.0 has major version 2")
	}
	require.Empty(t, remoteTags(t, repo))
}

func TestRun_refuses_a_module_path_that_is_not_its_directory(t *testing.T) {
	repo := newRepo(t, ".="+root, "grpc="+root+"/grpc", "wrong="+root+"/right", "-none="+root+"/none")
	gittest.Write(t, repo, "none/go.mod", "go 1.27.0\n")
	gittest.Write(t, repo, "none/p.go", "package p\n")
	gittest.Commit(t, repo, "no module path")
	for _, mode := range []release.Mode{release.Tag, release.Bump} {
		r := run(t, repo, mode, "v0.16.0")
		requireFails(t, r)
		requireOutput(t, r,
			"wrong/go.mod declares module "+root+"/right, want "+root+"/wrong",
			"none/go.mod declares module <none>, want "+root+"/none",
		)
		require.Len(t, strings.Split(r.err.Error(), "\n"), 2, "each mismatch is a line of its own")
	}
	require.Empty(t, remoteTags(t, repo))
}

func TestRun_works_from_any_directory_of_the_repository(t *testing.T) {
	repo := newRepo(t, ".="+root, "grpc="+root+"/grpc")
	r := run(t, filepath.Join(repo, "grpc"), release.DryRun, "v0.16.0")
	requireOK(t, r)
	requireOutput(t, r, "would create grpc/v0.16.0")
}

func TestRun_refuses_a_bump_of_a_commit(t *testing.T) {
	repo := newRepo(t, ".="+root, "grpc="+root+"/grpc")
	r := runWith(t, repo, releaseOptions(release.Bump, "v0.16.0", "HEAD"))
	requireFails(t, r)
	require.Empty(t, status(t, repo))
}

func TestRun_fails_without_a_root_module_or_its_path(t *testing.T) {
	repo := newRepo(t, "grpc="+root+"/grpc")
	r := run(t, repo, release.DryRun, "v0.16.0")
	requireFails(t, r)
	requireOutput(t, r, "there is no go.mod at the root of the repository")

	gittest.Write(t, repo, "go.mod", "go 1.27.0\n")
	gittest.Commit(t, repo, "root")
	r = run(t, repo, release.DryRun, "v0.16.0")
	requireFails(t, r)
	requireOutput(t, r, "go.mod declares no module path")

	gittest.Write(t, repo, "go.mod", "module\n")
	gittest.Commit(t, repo, "root")
	r = run(t, repo, release.DryRun, "v0.16.0")
	requireFails(t, r)
	requireOutput(t, r, "go.mod declares no module path")
}

func TestRun_fails_outside_a_git_repository(t *testing.T) {
	r := run(t, t.TempDir(), release.DryRun, "v0.16.0")
	requireFails(t, r)
	requireOutput(t, r, "git rev-parse --show-toplevel")
}

func TestRun_fails_when_the_tags_of_the_remote_cannot_be_listed(t *testing.T) {
	repo := newRepo(t, ".="+root, "grpc="+root+"/grpc")
	r := runWith(t, repo, release.Options{Mode: release.DryRun, Version: "v0.16.0", Remote: "nosuch"})
	requireFails(t, r)
	requireOutput(t, r, "could not list the tags of nosuch")
}

// The tags are pushed in one atomic push, so a remote that refuses one of
// them gets none.
func TestRun_pushes_no_tag_when_the_remote_refuses_one(t *testing.T) {
	repo := newRepo(t, ".="+root, "grpc="+root+"/grpc")
	hook := filepath.Join(repo+".git", "hooks", "update")
	require.NoError(t, os.WriteFile(hook, []byte("#!/bin/sh\ncase \"$1\" in refs/tags/grpc/*) exit 1 ;; esac\n"), 0o755))

	r := run(t, repo, release.Tag, "v0.16.0")
	requireFails(t, r)
	requireOutput(t, r, "git push --atomic origin")
	require.NotContains(t, r.stdout, "created")
	require.Empty(t, remoteTags(t, repo))
}

func TestRun_fails_when_a_bump_cannot_edit_a_go_mod(t *testing.T) {
	repo := newRepo(t, ".="+root, "grpc="+root+"/grpc")
	gittest.Append(t, repo, "grpc/go.mod", "\nrequire github.com/z5labs/humus "+pseudo+"\n")
	gittest.Commit(t, repo, "requirement")
	gittest.Append(t, repo, "grpc/go.mod", "\nnot a directive\n")

	r := run(t, repo, release.Bump, "v0.16.0")
	requireFails(t, r)
	requireOutput(t, r, "go mod edit")
}

func TestRun_fails_when_a_bump_cannot_read_a_tracked_doc(t *testing.T) {
	repo := newRepo(t, ".="+root, "grpc="+root+"/grpc")
	gittest.Write(t, repo, "README.md", "go get "+root+"@main\n")
	gittest.Commit(t, repo, "readme")
	require.NoError(t, os.Remove(filepath.Join(repo, "README.md")))

	r := run(t, repo, release.Bump, "v0.16.0")
	requireFails(t, r)
}

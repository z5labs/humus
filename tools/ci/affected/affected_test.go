// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package affected_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus/tools/ci/affected"
	"github.com/z5labs/humus/tools/ci/internal/gittest"
)

// owners are the modules of a repository laid out like humus: the root
// module, nested library modules, walkthroughs, a migration guide's modules
// side by side, the Hugo module under docs/, which holds no package, and a
// module nested in the directory of another.
var owners = []string{
	".", "kube", "queue/kafka", "tools/ci", "tools/flaky", "docs",
	"example/kube/databases", "example/http/shoppingcart",
	"example/kube/kubebuilder/cases", "example/kube/kubebuilder/start", "example/kube/kubebuilder/humus",
	"example/kube/databases/nested",
}

var (
	databases = affected.Module{
		Dir: "example/kube/databases",
		Packages: []affected.Package{
			{Dir: "."},
			{Dir: "health"},
			{Dir: "kube"},
		},
		Requires: []string{".", "kube"},
	}
	shoppingcart = affected.Module{
		Dir: "example/http/shoppingcart",
		Packages: []affected.Package{
			{Dir: "."},
			{Dir: "openapi/v3"},
		},
		Requires: []string{"."},
	}
	kubebuilderHumus = affected.Module{
		Dir: "example/kube/kubebuilder/humus",
		Packages: []affected.Package{
			{Dir: "."},
			{Dir: "kube"},
			{Dir: "example/kube/kubebuilder/cases"},
		},
		Requires: []string{".", "example/kube/kubebuilder/cases", "kube"},
	}
	kubebuilderStart = affected.Module{
		Dir:      "example/kube/kubebuilder/start",
		Packages: []affected.Package{{Dir: "example/kube/kubebuilder/cases"}},
		Requires: []string{"example/kube/kubebuilder/cases"},
	}
	flaky = affected.Module{Dir: "tools/flaky"}
	root  = affected.Module{Dir: "."}
	mods  = []affected.Module{root, databases, shoppingcart, kubebuilderHumus, kubebuilderStart, flaky}
)

// selected returns the directories of the modules Select picks for changed.
func selected(t *testing.T, mods []affected.Module, changed ...string) []string {
	t.Helper()
	var dirs []string
	for _, a := range affected.Select(mods, owners, changed) {
		dirs = append(dirs, a.Dir)
	}
	return dirs
}

func TestSelect_a_file_of_a_modules_own_directory_affects_it(t *testing.T) {
	for _, file := range []string{
		"example/kube/databases/controller/controller.go",
		"example/kube/databases/controller/controller_test.go",
		"example/kube/databases/Dockerfile",
		"example/kube/databases/deploy/kind/up.sh",
		"example/kube/databases/go.mod",
		"example/kube/databases/go.sum",
		"example/http/shoppingcart/deploy/kind/wiremock/mappings/keys.json",
	} {
		got := selected(t, []affected.Module{databases, shoppingcart, flaky}, file)
		require.Len(t, got, 1, file)
		require.Contains(t, file, got[0]+"/", file)
	}
}

func TestSelect_a_file_of_a_module_nested_in_its_directory_does_not_affect_it(t *testing.T) {
	require.Empty(t, selected(t, []affected.Module{databases, root}, "example/kube/databases/nested/x.go"))
}

func TestSelect_a_source_file_of_a_package_in_another_module_it_builds_from_affects_it(t *testing.T) {
	require.Equal(t, []string{".", "example/kube/databases", "example/http/shoppingcart", "example/kube/kubebuilder/humus"},
		selected(t, mods, "app.go"), "humus's root package, through each module's replace")
	require.Equal(t, []string{"example/kube/databases", "example/kube/kubebuilder/humus"},
		selected(t, mods, "kube/reconcile.go"))
	require.Equal(t, []string{"example/kube/databases"}, selected(t, mods[1:], "health/asm_amd64.s"))
	require.Equal(t, []string{"example/http/shoppingcart"}, selected(t, mods[1:], "openapi/v3/serve.go"),
		"a package in a module's subdirectory")
	require.Equal(t, []string{"example/kube/kubebuilder/humus", "example/kube/kubebuilder/start"},
		selected(t, mods, "example/kube/kubebuilder/cases/cases.go"),
		"a migration guide's shared cases, in a module of their own")
}

func TestSelect_a_file_a_package_is_not_built_from_does_not_affect_a_module_in_another(t *testing.T) {
	for _, file := range []string{
		"kube/reconcile_test.go",
		"kube/README.md",
		"kube/testdata/crd.yaml",
		"kube/internal/x.go",
		"README.md",
		"renovate.json",
		"httpclient/client.go",
	} {
		require.Empty(t, selected(t, mods[1:], file), file)
	}
}

func TestSelect_a_file_a_package_embeds_affects_a_module_it_builds_from(t *testing.T) {
	m := affected.Module{
		Dir: "example/kube/databases",
		Packages: []affected.Package{
			{Dir: "kube", Embeds: []string{"migrations/*.sql", "static", "all:hidden", "one.txt", ".named", "static/.keep"}},
			{Dir: ".", Embeds: []string{"banner.txt"}},
		},
	}
	for _, file := range []string{
		"kube/migrations/0001.sql",
		"kube/static/css/site.css",
		"kube/hidden/.keep",
		"kube/hidden/_drafts/a.txt",
		"kube/one.txt",
		"kube/.named",
		"kube/static/.keep",
		"banner.txt",
	} {
		require.Equal(t, []string{m.Dir}, selected(t, []affected.Module{m}, file), file)
	}
	for _, file := range []string{
		"kube/migrations/notes.md",
		"kube/static/_drafts/site.css",
		"kube/static/css/.cache",
		"kube/statically.txt",
		"kube/two.txt",
		"README.md",
		"docs/banner.txt",
	} {
		require.Empty(t, selected(t, []affected.Module{m}, file), file)
	}
}

func TestSelect_the_go_mod_or_go_sum_of_a_module_in_its_build_list_affects_it(t *testing.T) {
	require.Equal(t, []string{".", "example/kube/databases", "example/http/shoppingcart", "example/kube/kubebuilder/humus"},
		selected(t, mods, "go.mod"))
	require.Equal(t, []string{"example/kube/databases", "example/kube/kubebuilder/humus"}, selected(t, mods, "kube/go.sum"))
	m := affected.Module{Dir: "example/kube/databases", Requires: []string{"queue/kafka"}}
	require.Equal(t, []string{m.Dir}, selected(t, []affected.Module{m}, "queue/kafka/go.mod"),
		"a module in its build list it builds no package from")
	require.Empty(t, selected(t, []affected.Module{m}, "queue/kafka/internal/go.mod"),
		"a go.mod in no module's directory is no module's")
	require.Empty(t, selected(t, []affected.Module{databases, shoppingcart}, "tools/flaky/go.mod", "docs/go.mod"))
}

func TestSelect_a_file_of_Always_affects_every_module(t *testing.T) {
	all := []string{".", "example/kube/databases", "example/http/shoppingcart", "example/kube/kubebuilder/humus", "example/kube/kubebuilder/start", "tools/flaky"}
	for _, file := range []string{
		".github/workflows/build.yaml",
		".github/actions/restore-go-cache/action.yaml",
		"tools/ci/affected/affected.go",
		"tools/ci/go.sum",
	} {
		require.Equal(t, all, selected(t, mods, file), file)
	}
	require.Equal(t, []string{"."}, selected(t, mods, ".github/workflows/auto-merge.yaml"),
		"another workflow belongs to the root module like any other file")
}

func TestSelect_a_file_of_a_module_with_no_package_affects_no_other(t *testing.T) {
	require.Empty(t, selected(t, mods, "docs/content/walkthroughs/databases.md", "docs/hugo.yaml"))
}

func TestSelect_no_change_affects_no_module(t *testing.T) {
	require.Empty(t, affected.Select(mods, owners, nil))
}

func TestSelect_says_the_first_changed_file_that_affects_each_module(t *testing.T) {
	got := affected.Select([]affected.Module{databases, flaky}, owners, []string{
		"docs/x.md",
		"kube/a.go",
		"tools/flaky/main.go",
		"example/kube/databases/Dockerfile",
	})
	require.Equal(t, []affected.Affected{
		{Dir: "example/kube/databases", Because: "kube/a.go"},
		{Dir: "tools/flaky", Because: "tools/flaky/main.go"},
	}, got)
}

func TestSelect_decides_a_module_it_has_never_seen_by_the_same_rules(t *testing.T) {
	m := affected.Module{
		Dir:      "example/sqs/notifier",
		Packages: []affected.Package{{Dir: "queue/sqs"}},
		Requires: []string{"queue/sqs"},
	}
	o := append([]string{"example/sqs/notifier", "queue/sqs"}, owners...)
	for _, file := range []string{"example/sqs/notifier/deploy/kind/up.sh", "queue/sqs/queue.go", "queue/sqs/go.sum", ".github/workflows/build.yaml"} {
		got := affected.Select([]affected.Module{m}, o, []string{file})
		require.Equal(t, []affected.Affected{{Dir: m.Dir, Because: file}}, got, file)
	}
	require.Empty(t, affected.Select([]affected.Module{m}, o, []string{"queue/sqs/queue_test.go", "queue/kafka/go.mod"}))
}

func TestSelect_a_file_of_Lint_affects_no_module(t *testing.T) {
	require.Empty(t, selected(t, mods, ".golangci.yaml"), "not even the root module, whose directory holds it")
}

// tagged are modules with Go files behind the tags of affected.Tags.
var (
	kafka    = affected.Module{Dir: "queue/kafka", Tags: []string{"testcontainers"}}
	walkthru = affected.Module{
		Dir:      "example/kube/databases",
		Packages: []affected.Package{{Dir: "kube"}},
		Requires: []string{"kube"},
		Tags:     []string{"testcontainers", "functional"},
	}
	tagged = []affected.Module{root, kafka, walkthru, flaky}
)

func TestDecide_lints_and_vets_only_the_modules_the_change_affects(t *testing.T) {
	got := affected.Decide(tagged, owners, []string{"kube/reconcile.go", "tools/flaky/main.go"})
	require.Equal(t, affected.Plan{
		Affected: []affected.Affected{
			{Dir: "example/kube/databases", Because: "kube/reconcile.go"},
			{Dir: "tools/flaky", Because: "tools/flaky/main.go"},
		},
		Linted: []affected.Affected{
			{Dir: "example/kube/databases", Because: "kube/reconcile.go"},
			{Dir: "tools/flaky", Because: "tools/flaky/main.go"},
		},
		Vetted: map[string][]string{
			"testcontainers": {"example/kube/databases"},
			"functional":     {"example/kube/databases"},
		},
	}, got, "tools/flaky is affected but has no file behind either tag")
}

func TestDecide_a_file_of_Lint_lints_every_module_and_affects_none(t *testing.T) {
	got := affected.Decide(tagged, owners, []string{".golangci.yaml", "queue/kafka/consumer.go"})
	require.Equal(t, affected.Plan{
		Affected: []affected.Affected{{Dir: "queue/kafka", Because: "queue/kafka/consumer.go"}},
		Linted: []affected.Affected{
			{Dir: ".", Because: ".golangci.yaml"},
			{Dir: "queue/kafka", Because: ".golangci.yaml"},
			{Dir: "example/kube/databases", Because: ".golangci.yaml"},
			{Dir: "tools/flaky", Because: ".golangci.yaml"},
		},
		Vetted: map[string][]string{"testcontainers": {"queue/kafka"}, "functional": {}},
	}, got)
}

func TestDecide_no_change_runs_nothing(t *testing.T) {
	for _, changed := range [][]string{nil, {"docs/content/walkthroughs/databases.md"}} {
		got := affected.Decide(tagged, owners, changed)
		require.Empty(t, got.Affected, changed)
		require.Empty(t, got.Linted, changed)
		require.Equal(t, map[string][]string{"testcontainers": {}, "functional": {}}, got.Vetted, changed)
	}
}

func TestDecide_a_file_of_Always_runs_everything(t *testing.T) {
	got := affected.Decide(tagged, owners, []string{"tools/ci/main.go"})
	all := []affected.Affected{
		{Dir: ".", Because: "tools/ci/main.go"},
		{Dir: "queue/kafka", Because: "tools/ci/main.go"},
		{Dir: "example/kube/databases", Because: "tools/ci/main.go"},
		{Dir: "tools/flaky", Because: "tools/ci/main.go"},
	}
	require.Equal(t, affected.Plan{
		Affected: all,
		Linted:   all,
		Vetted: map[string][]string{
			"testcontainers": {"queue/kafka", "example/kube/databases"},
			"functional":     {"example/kube/databases"},
		},
	}, got)
}

func TestEverything_lints_every_module_and_vets_every_one_with_tagged_files(t *testing.T) {
	all := []affected.Affected{{Dir: "."}, {Dir: "queue/kafka"}, {Dir: "example/kube/databases"}, {Dir: "tools/flaky"}}
	require.Equal(t, affected.Plan{
		Affected: all,
		Linted:   all,
		Vetted: map[string][]string{
			"testcontainers": {"queue/kafka", "example/kube/databases"},
			"functional":     {"example/kube/databases"},
		},
	}, affected.Everything(tagged))
}

// repository returns a repository with a root module, whose packages are p,
// which has a file behind !functional, q, which embeds a file, and r, which
// nothing imports; a module ex, which replaces the root module and imports
// p, and q only in a test behind the testcontainers tag, and has a file
// behind functional; and a module other, which nothing requires.
func repository(t *testing.T) string {
	t.Helper()
	repo := gittest.New(t)
	gittest.Write(t, repo, "go.mod", "module example.com/repo\n\ngo 1.27.0\n")
	gittest.Write(t, repo, "p/p.go", "package p\n")
	gittest.Write(t, repo, "p/notfunctional.go", "// Copyright\n\n//go:build !functional\n\npackage p\n")
	gittest.Write(t, repo, "q/q.go", "package q\n\nimport _ \"embed\"\n\n//go:embed data.txt\nvar Data string\n")
	gittest.Write(t, repo, "q/data.txt", "data\n")
	gittest.Write(t, repo, "r/r.go", "package r\n")
	gittest.Write(t, repo, "ex/go.mod", "module example.com/repo/ex\n\ngo 1.27.0\n\nrequire example.com/repo v0.0.0\n\nreplace example.com/repo => ../\n")
	gittest.Write(t, repo, "ex/main.go", "package main\n\nimport _ \"example.com/repo/p\"\n\nfunc main() {}\n")
	gittest.Write(t, repo, "ex/main_test.go", "//go:build testcontainers\n\npackage main\n\nimport _ \"example.com/repo/q\"\n")
	gittest.Write(t, repo, "ex/functional/functional_test.go", "//go:build functional && linux\n\npackage functional\n")
	gittest.Write(t, repo, "ex/Dockerfile", "FROM scratch\n")
	gittest.Write(t, repo, "other/go.mod", "module example.com/repo/other\n\ngo 1.27.0\n")
	gittest.Write(t, repo, "other/o.go", "package o\n")
	gittest.Write(t, repo, "docs/go.mod", "module example.com/repo/docs\n\ngo 1.27.0\n")
	gittest.Commit(t, repo, "modules")
	return repo
}

func TestLoad_lists_the_packages_and_modules_of_the_repository_a_module_builds_from(t *testing.T) {
	repo := repository(t)
	root, err := filepath.EvalSymlinks(repo)
	require.NoError(t, err)

	m, err := affected.Load(t.Context(), root, "ex", gittest.Env())
	require.NoError(t, err)
	require.Equal(t, affected.Module{
		Dir: "ex",
		Packages: []affected.Package{
			{Dir: "p"},
			{Dir: "q", Embeds: []string{"data.txt"}},
		},
		Requires: []string{"."},
		Tags:     []string{"testcontainers", "functional"},
	}, m)

	m, err = affected.Load(t.Context(), root, ".", gittest.Env())
	require.NoError(t, err)
	require.Equal(t, affected.Module{Dir: "."}, m,
		"a module's own packages are its directory's files, and a tag under a ! is not one its files are behind")
}

func TestLoad_fails_when_go_list_fails(t *testing.T) {
	repo := repository(t)
	gittest.Write(t, repo, "ex/main.go", "package main\n\nimport _ \"example.com/repo/missing\"\n\nfunc main() {}\n")
	_, err := affected.Load(t.Context(), repo, "ex", gittest.Env())
	require.ErrorContains(t, err, "go list -deps")

	gittest.Write(t, repo, "ex/go.mod", "module example.com/repo/ex\n\nnot a directive\n")
	_, err = affected.Load(t.Context(), repo, "ex", gittest.Env())
	require.ErrorContains(t, err, "go list -m")
}

func TestChanged_lists_every_file_added_deleted_modified_or_renamed(t *testing.T) {
	repo := repository(t)
	base := gittest.Git(t, repo, "rev-parse", "HEAD")
	gittest.Write(t, repo, "p/new.go", "package p\n")
	gittest.Append(t, repo, "q/data.txt", "more\n")
	gittest.Git(t, repo, "rm", "-q", "r/r.go")
	gittest.Git(t, repo, "mv", "ex/Dockerfile", "ex/Containerfile")
	gittest.Commit(t, repo, "change")
	gittest.Write(t, repo, "uncommitted.go", "package p\n")

	files, err := affected.Changed(t.Context(), repo, base, gittest.Env())
	require.NoError(t, err)
	require.Equal(t, []string{"ex/Containerfile", "ex/Dockerfile", "p/new.go", "q/data.txt", "r/r.go"}, files)

	files, err = affected.Changed(t.Context(), repo, "HEAD", gittest.Env())
	require.NoError(t, err)
	require.Empty(t, files)
}

func TestChanged_fails_when_git_cannot_list_the_files(t *testing.T) {
	repo := repository(t)
	_, err := affected.Changed(t.Context(), repo, "0123456789abcdef0123456789abcdef01234567", gittest.Env())
	require.ErrorContains(t, err, "git diff")
}

func TestRun_with_no_base_affects_every_module(t *testing.T) {
	repo := repository(t)
	got, err := affected.Run(t.Context(), filepath.Join(repo, "ex"), "", gittest.Env())
	require.NoError(t, err)
	all := []affected.Affected{{Dir: "."}, {Dir: "ex"}, {Dir: "other"}}
	require.Equal(t, affected.Plan{
		Affected: all,
		Linted:   all,
		Vetted:   map[string][]string{"testcontainers": {"ex"}, "functional": {"ex"}},
	}, got)
}

// touch adds a line to the file name, a slash-separated path under dir,
// creating it when there is none.
func touch(t *testing.T, dir, name string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
	if !errors.Is(err, fs.ErrNotExist) {
		require.NoError(t, err)
	}
	gittest.Write(t, dir, name, string(b)+"\n")
}

func TestRun_picks_the_modules_the_change_from_base_affects(t *testing.T) {
	for _, tc := range []struct {
		file string
		want []string
	}{
		{"p/p.go", []string{".", "ex"}},
		{"q/data.txt", []string{".", "ex"}},
		{"r/r.go", []string{"."}},
		{"go.mod", []string{".", "ex"}},
		{"ex/Dockerfile", []string{"ex"}},
		{"other/o.go", []string{"other"}},
		{"docs/go.mod", nil},
		{"tools/ci/README.md", []string{".", "ex", "other"}},
		{".golangci.yaml", nil},
	} {
		repo := repository(t)
		base := gittest.Git(t, repo, "rev-parse", "HEAD")
		touch(t, repo, tc.file)
		gittest.Commit(t, repo, "change")

		got, err := affected.Run(t.Context(), repo, base, gittest.Env())
		require.NoError(t, err, tc.file)
		var dirs []string
		for _, a := range got.Affected {
			require.Equal(t, tc.file, a.Because, tc.file)
			dirs = append(dirs, a.Dir)
		}
		require.Equal(t, tc.want, dirs, tc.file)
	}
}

func TestRun_plans_lint_and_vet_from_the_change(t *testing.T) {
	repo := repository(t)
	base := gittest.Git(t, repo, "rev-parse", "HEAD")
	touch(t, repo, ".golangci.yaml")
	touch(t, repo, "p/p.go")
	gittest.Commit(t, repo, "change")

	got, err := affected.Run(t.Context(), repo, base, gittest.Env())
	require.NoError(t, err)
	require.Equal(t, affected.Plan{
		Affected: []affected.Affected{{Dir: ".", Because: "p/p.go"}, {Dir: "ex", Because: "p/p.go"}},
		Linted: []affected.Affected{
			{Dir: ".", Because: ".golangci.yaml"},
			{Dir: "ex", Because: ".golangci.yaml"},
			{Dir: "other", Because: ".golangci.yaml"},
		},
		Vetted: map[string][]string{"testcontainers": {"ex"}, "functional": {"ex"}},
	}, got)
}

func TestRun_fails_when_it_cannot_decide(t *testing.T) {
	repo := repository(t)
	_, err := affected.Run(t.Context(), repo, "no-such-revision", gittest.Env())
	require.ErrorContains(t, err, "git diff")

	gittest.Write(t, repo, "ex/main.go", "package main\n\nimport _ \"example.com/repo/missing\"\n\nfunc main() {}\n")
	gittest.Commit(t, repo, "broken")
	_, err = affected.Run(t.Context(), repo, "HEAD^", gittest.Env())
	require.ErrorContains(t, err, "go list -deps in ex")
	_, err = affected.Run(t.Context(), repo, "", gittest.Env())
	require.ErrorContains(t, err, "go list -deps in ex", "with no base too")

	_, err = affected.Run(t.Context(), t.TempDir(), "", gittest.Env())
	require.ErrorContains(t, err, "git rev-parse")
}

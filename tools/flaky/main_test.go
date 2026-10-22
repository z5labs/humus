// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/bedrocktest"

	"github.com/z5labs/humus/tools/flaky/internal/dockertest"
	"github.com/z5labs/humus/tools/flaky/internal/repotest"
	"github.com/z5labs/humus/tools/flaky/results"
	"github.com/z5labs/humus/tools/flaky/run"
)

// flakyEnv is the environment flaky runs with in these tests: the variables
// the go command needs from the real one, and a TMPDIR of the test's own.
func flakyEnv(t *testing.T) map[string]string {
	vars := map[string]string{"TMPDIR": t.TempDir()}
	for _, name := range []string{"PATH", "HOME", "GOROOT", "GOPATH", "GOCACHE", "GOMODCACHE", "GOFLAGS", "GOTOOLCHAIN", "XDG_CACHE_HOME"} {
		if v, err := (bedrock.OSEnvironment{}).Value(name); err == nil {
			vars[name] = v
		}
	}
	return vars
}

// stressIn runs flaky with args in a copy of testdata/repo, and returns its
// exit status and what it wrote.
func stressIn(t *testing.T, repo string, args ...string) (bedrock.ExitStatus, string, string) {
	t.Helper()
	t.Chdir(filepath.Join(repo, "a"))
	p, status := bedrocktest.Start(t, bedrock.AppFunc(flaky), bedrocktest.WithArgs(args...), bedrocktest.WithEnv(flakyEnv(t)))
	st := <-status
	return st, p.Stdout(), p.Stderr()
}

func needCores(t *testing.T, n int) {
	cores, err := run.Affinity()
	require.NoError(t, err)
	if len(cores) < n {
		t.Skipf("this process may run on %d cores, and the test needs %d", len(cores), n)
	}
}

func TestStress_reports_a_test_that_failed_and_exits_1(t *testing.T) {
	needCores(t, 2)
	repo := repotest.New(t, "0.0.1")
	out := filepath.Join(t.TempDir(), "out")
	st, stdout, stderr := stressIn(t, repo, "stress", "-cpu", "1,2", "-count", "2", "-confirm", "3", "-keep", "0", "-o", out)
	require.Equal(t, 1, st.Code(), "stdout:\n%s\nstderr:\n%s", stdout, stderr)

	require.Contains(t, stderr, "but build.yaml sets 0.0.1", "it warns that the local Go isn't CI's")
	require.Contains(t, stderr, "found 3 packages with tests")
	require.Contains(t, stderr, "built 3 test binaries")

	require.Contains(t, stderr, "confirming 1 unit tests, 3 times at each pair of CPU count and load")
	require.Contains(t, stdout, "FAILING: failed or hung every run at some pair of CPU count and load\n\ntwocpus  TestFailsOnTwoCPUs\n")
	require.Contains(t, stdout, "    cpu 1 load 1           0         0         3\n", "the table gives the confirm runs, at every pair")
	require.Contains(t, stdout, "    cpu 2 load 1           3         0         3\n")
	require.Contains(t, stdout, "    cpu 2 load 2           3         0         3\n", "-load is 1 and 2 by default")
	require.Contains(t, stdout, "    6 failures, 0 hangs in 12 confirm runs\n")
	require.Contains(t, stdout, "runtime.NumCPU() is 2")
	require.Contains(t, stdout, "reproduce: flaky stress -tags unit -run '^TestFailsOnTwoCPUs$' -cpu 2 -load 1 -count 3 ./twocpus\n")
	require.Contains(t, stdout, "taskset -c 0-1 go -C . test -race -cover -count=3 -cpu 2 -timeout 2m0s -run '^TestFailsOnTwoCPUs$' ./twocpus\n")
	require.Contains(t, stdout, "3 tests passed every run\n", "TestA, TestAToo and TestB")
	require.Contains(t, stdout, "confirmed 1 test in 12 runs\n")
	require.Contains(t, stdout, "phases: discovery ")
	require.Contains(t, stdout, ", confirm ")
	require.NotContains(t, stdout, "FLAKY")

	saved, err := os.ReadDir(filepath.Join(out, "twocpus", "TestFailsOnTwoCPUs"))
	require.NoError(t, err)
	require.Len(t, saved, 4+6, "each failure's whole output is saved, the sweep's and the confirm runs'")
}

func TestStress_runs_the_tests_that_match_run_count_times_and_confirms_none(t *testing.T) {
	needCores(t, 2)
	repo := repotest.New(t, "0.0.1")
	st, stdout, stderr := stressIn(t, repo, "stress", "-cpu", "1,2", "-load", "1", "-count", "2", "-confirm", "3", "-keep", "0", "-run", "^TestFailsOnTwoCPUs$", "./twocpus")
	require.Equal(t, 1, st.Code(), "stdout:\n%s\nstderr:\n%s", stdout, stderr)
	require.NotContains(t, stderr, "confirming")
	require.Contains(t, stdout, "    cpu 1 load 1           0         0         2\n")
	require.Contains(t, stdout, "    cpu 2 load 1           2         0         2\n")
	require.Contains(t, stdout, "    2 failures, 0 hangs in 4 runs\n")
	require.Contains(t, stdout, "-count 2 ./twocpus\n")
	require.NotContains(t, stdout, "confirm run")
	require.NotContains(t, stdout, "\nconfirmed ")
	require.NotContains(t, stdout, ", confirm ")
}

func TestStress_writes_the_report_as_JSON(t *testing.T) {
	needCores(t, 2)
	repo := repotest.New(t, "0.0.1")
	st, stdout, stderr := stressIn(t, repo, "stress", "-cpu", "2", "-load", "2", "-count", "1", "-confirm", "2", "-keep", "0", "-timeout", "1m", "-json", "./twocpus")
	require.Equal(t, 1, st.Code(), stderr)
	var rep results.Report
	require.NoError(t, json.Unmarshal([]byte(stdout), &rep))
	require.Len(t, rep.Commit, 40)
	require.True(t, strings.HasPrefix(rep.Go, "go1."))
	require.Positive(t, rep.Cores)
	require.Equal(t, []int{2}, rep.CPUs)
	require.Equal(t, []int{2}, rep.Loads)
	require.Len(t, rep.Failing, 1)
	require.Equal(t, "TestFailsOnTwoCPUs", rep.Failing[0].Test)
	require.True(t, rep.Failing[0].Confirmed)
	require.Equal(t, []results.Pair{{CPU: 2, Load: 2, Runs: 2, Failures: 2}}, rep.Failing[0].Pairs)
	require.Equal(t, "flaky stress -tags unit -run '^TestFailsOnTwoCPUs$' -cpu 2 -load 2 -count 2 -timeout 1m0s ./twocpus", rep.Failing[0].Reproduce)
	require.Equal(t, []results.Level{
		{Name: "unit", Count: 1, Confirm: 2, TimeoutSeconds: 60},
		{Name: "testcontainers", Count: 1, Confirm: 2, TimeoutSeconds: 60},
		{Name: "functional", Count: 1, Confirm: 2, TimeoutSeconds: 60, CPU: 4, Load: 1},
	}, rep.Levels, "-count, -confirm and -timeout, given, apply to every level, and the functional level runs at CPU count 4 and load 1 alone")
	require.Equal(t, 1, rep.Confirmed)
	require.Equal(t, 2, rep.ConfirmRuns)
	require.Len(t, rep.Phases, 4)
	require.Equal(t, "confirm", rep.Phases[3].Name)
	require.Regexp(t, `/flaky-\d{8}T\d{6}$`, rep.Output, "the output goes under TMPDIR by default")
}

func TestStress_shuffles_each_run_and_a_failure_s_seed_reproduces_it(t *testing.T) {
	repo := repotest.New(t, "0.0.1")
	order, err := os.ReadFile(filepath.Join("testdata", "runs", "order", "order_test.go"))
	require.NoError(t, err)
	repotest.Write(t, repo, "order/order_test.go", string(order))

	st, stdout, stderr := stressIn(t, repo, "stress", "-shuffle", "-cpu", "1", "-load", "1", "-count", "40", "-confirm", "3", "-keep", "0", "-json", "./order")
	require.Equal(t, 1, st.Code(), "TestFailsAfterTestDirties fails in about half the shuffled runs:\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	var rep results.Report
	require.NoError(t, json.Unmarshal([]byte(stdout), &rep))
	require.Empty(t, rep.Failing)
	require.Len(t, rep.Flaky, 1)
	x := rep.Flaky[0]
	require.Equal(t, "TestFailsAfterTestDirties", x.Test)
	require.Equal(t, []results.Pair{{CPU: 1, Load: 1, Runs: 3}}, x.Pairs, "alone, it passes every confirm run")
	require.NotNil(t, x.FirstFailureSeed)
	require.NotNil(t, x.Seed)
	seed := strconv.FormatInt(*x.Seed, 10)
	require.Equal(t, "flaky stress -tags unit -run . -shuffle="+seed+" -cpu 1 -load 1 -count 3 ./order", x.Reproduce)
	require.Equal(t, "taskset -c 0 go -C . test -race -cover -count=3 -cpu 1 -timeout 2m0s -run . -shuffle="+seed+" ./order", x.ReproduceGoTest)

	args := strings.Fields(x.Reproduce)[1:]
	args = append(args[:len(args)-1:len(args)-1], "-keep", "0", "-json", args[len(args)-1])
	st, stdout, stderr = stressIn(t, repo, args...)
	require.Equal(t, 1, st.Code(), "stdout:\n%s\nstderr:\n%s", stdout, stderr)
	rep = results.Report{}
	require.NoError(t, json.Unmarshal([]byte(stdout), &rep))
	require.Empty(t, rep.Flaky)
	require.Len(t, rep.Failing, 1, "its seed fails it every run")
	require.Equal(t, "TestFailsAfterTestDirties", rep.Failing[0].Test)
	require.Equal(t, []results.Pair{{CPU: 1, Load: 1, Runs: 3, Failures: 3}}, rep.Failing[0].Pairs)
	require.Equal(t, *x.Seed, *rep.Failing[0].Seed)
}

// containersTest is a test behind the testcontainers tag, in package pkg,
// that fails when it runs on 2 CPUs.
func containersTest(pkg, name string) string {
	return "//go:build testcontainers\n\npackage " + pkg + "\n\nimport (\n\t\"runtime\"\n\t\"testing\"\n)\n\n" +
		"func " + name + "(t *testing.T) {\n\tif n := runtime.NumCPU(); n == 2 {\n\t\tt.Errorf(\"runtime.NumCPU() is %d\", n)\n\t}\n}\n"
}

func TestStress_runs_the_testcontainers_level_s_own_tests_with_its_containers_on_the_kept_back_cores(t *testing.T) {
	needCores(t, 3)
	docker := dockertest.Fake(t)
	repo := repotest.New(t, "0.0.1")
	repotest.Write(t, repo, "a/a_containers_test.go", containersTest("a", "TestAContainers"))
	repotest.Write(t, repo, "c/c_test.go", "//go:build testcontainers\n\npackage c\n\nimport \"testing\"\n\nfunc TestC(t *testing.T) {}\n")
	cores, err := run.Affinity()
	require.NoError(t, err)

	st, stdout, stderr := stressIn(t, repo, "stress", "-cpu", "1,2", "-load", "1", "-count", "2", "-confirm", "3", "-keep", "1", "./a", "./c")
	require.Equal(t, 1, st.Code(), "stdout:\n%s\nstderr:\n%s", stdout, stderr)
	require.Contains(t, stderr, "found 3 packages with tests (1 unit, 2 testcontainers, 0 functional)", "c has tests only behind the tag")
	require.Contains(t, stderr, "pinning the testcontainers containers to cores "+strconv.Itoa(cores[0])+"\n", "-keep 1 keeps back the first core")
	require.Contains(t, dockertest.Read(t, docker, "events.args"), "--filter label=org.testcontainers", "it watches Docker's events")
	require.Contains(t, stderr, "built 3 test binaries")
	require.Contains(t, stderr, "confirming 1 testcontainers tests, 3 times at each pair of CPU count and load")

	require.Contains(t, stdout, "FAILING: failed or hung every run at some pair of CPU count and load\n\na  TestAContainers  (testcontainers)\n")
	require.Contains(t, stdout, "    cpu 2 load 1           3         0         3\n")
	require.Contains(t, stdout, "reproduce: flaky stress -tags testcontainers -run '^TestAContainers$' -cpu 2 -load 1 -count 3 ./a\n")
	require.Contains(t, stdout, "taskset -c 0-1 go -C . test -race -cover -tags testcontainers -count=3 -cpu 2 -timeout 10m0s -run '^TestAContainers$' ./a\n", "the level's timeout is 10m")
	require.Contains(t, stdout, "3 tests passed every run\n", "TestA and TestAToo at the unit level, and TestC: no unit test runs at the testcontainers level too")
}

func TestStress_runs_the_testcontainers_level_s_own_tests_that_match_run(t *testing.T) {
	needCores(t, 3)
	dockertest.Fake(t)
	repo := repotest.New(t, "0.0.1")
	repotest.Write(t, repo, "a/a_containers_test.go", containersTest("a", "TestAContainers"))
	repotest.Write(t, repo, "c/c_test.go", containersTest("c", "TestC"))

	st, stdout, stderr := stressIn(t, repo, "stress", "-tags", "testcontainers", "-cpu", "2", "-load", "1", "-count", "2", "-keep", "1", "-run", "Containers$/x?", "-json", "./a", "./c")
	require.Equal(t, 1, st.Code(), "stdout:\n%s\nstderr:\n%s", stdout, stderr)
	require.Contains(t, stderr, "found 2 packages with tests (2 testcontainers)")
	var rep results.Report
	require.NoError(t, json.Unmarshal([]byte(stdout), &rep))
	require.Len(t, rep.Failing, 1, "TestC doesn't match -run, so it doesn't fail")
	require.Equal(t, "TestAContainers", rep.Failing[0].Test)
	require.Equal(t, "testcontainers", rep.Failing[0].Level)
	require.Equal(t, []results.Pair{{CPU: 2, Load: 1, Runs: 2, Failures: 2}}, rep.Failing[0].Pairs)
	require.Zero(t, rep.Passed, "with no untagged binary built beside it, TestA still doesn't run at the testcontainers level")
}

func TestStress_needs_no_Docker_at_the_unit_level(t *testing.T) {
	docker := dockertest.Fake(t)
	dockertest.Touch(t, docker, "down", "")
	repo := repotest.New(t, "0.0.1")
	repotest.Write(t, repo, "a/a_containers_test.go", containersTest("a", "TestAContainers"))
	st, stdout, stderr := stressIn(t, repo, "stress", "-tags", "unit", "-cpu", "1", "-count", "1", "-keep", "0", "./a")
	require.Equal(t, 0, st.Code(), "stdout:\n%s\nstderr:\n%s", stdout, stderr)
	require.Contains(t, stderr, "found 1 packages with tests (1 unit)")
	require.Empty(t, dockertest.Read(t, docker, "events.args"))
	require.Contains(t, stdout, "2 tests passed every run\n")
}

func TestStress_exits_2_when_the_testcontainers_level_cannot_run(t *testing.T) {
	docker := dockertest.Fake(t)
	repo := repotest.New(t, "0.0.1")
	repotest.Write(t, repo, "a/a_containers_test.go", containersTest("a", "TestAContainers"))
	repotest.Commit(t, repo)

	st, _, stderr := stressIn(t, repo, "stress", "-cpu", "1", "-count", "1", "-keep", "0", "./a")
	require.Equal(t, 2, st.Code())
	require.Contains(t, stderr, "-keep 0 keeps back no cores for the testcontainers containers")

	needCores(t, 2)
	dockertest.Touch(t, docker, "down", "")
	st, _, stderr = stressIn(t, repo, "stress", "-cpu", "1", "-count", "1", "-keep", "1", "./a")
	require.Equal(t, 2, st.Code())
	require.Contains(t, stderr, "the testcontainers level needs Docker")
}

func TestOwnPattern_runs_exactly_the_level_s_own_tests_and_the_rest_of_run_in_each(t *testing.T) {
	require.Equal(t, `^(TestA|TestB\.c)$`, ownPattern([]string{"TestA", "TestB.c"}, ""))
	require.Equal(t, `^(TestA)$`, ownPattern([]string{"TestA"}, "TestA"))
	require.Equal(t, `^(TestA)$/case`, ownPattern([]string{"TestA"}, "Test/case"))
	require.Equal(t, `^(TestA)$/x/y`, ownPattern([]string{"TestA"}, "Test(a/b)/x/y"), "a slash in parentheses doesn't split")
	require.Equal(t, `^(TestA)$/y`, ownPattern([]string{"TestA"}, `Test[/]\//y`), "nor does one in brackets or escaped")
}

func TestCompileRun_compiles_every_level_of_the_pattern(t *testing.T) {
	require.NoError(t, compileRun(""))
	require.NoError(t, compileRun("TestA/(a/b)/c"))
	require.ErrorContains(t, compileRun("[/"), "-run: error parsing regexp")
	require.ErrorContains(t, compileRun("TestA/x/("), "-run: error parsing regexp")
}

func TestParseLevels_takes_each_known_level_once(t *testing.T) {
	levels, err := parseLevels("testcontainers, unit,testcontainers")
	require.NoError(t, err)
	require.Equal(t, []string{"testcontainers", "unit"}, levels)
	levels, err = parseLevels("functional")
	require.NoError(t, err)
	require.Equal(t, []string{"functional"}, levels)
	_, err = parseLevels("unit,nope")
	require.ErrorContains(t, err, `"nope" is not a level: the levels are unit, testcontainers and functional`)
}

func TestStress_exits_0_when_every_test_passed_every_run(t *testing.T) {
	repo := repotest.New(t, "0.0.1")
	st, stdout, stderr := stressIn(t, repo, "stress", "-cpu", "1", "-count", "3", "-keep", "0", "-run", "^TestA$", "./a", "./b/...")
	require.Equal(t, 0, st.Code(), stderr)
	require.Equal(t, "1 test passed every run", strings.Split(stdout, "\n")[0], "-run narrows the tests that run")
}

func TestStress_exits_1_when_a_package_does_not_build(t *testing.T) {
	repo := repotest.New(t, "0.0.1")
	repotest.Write(t, repo, "a/broken_test.go", "package a\n\nfunc broken() int { return \"\" }\n")
	st, _, stderr := stressIn(t, repo, "stress", "-cpu", "1", "-count", "1", "-keep", "0", "./a")
	require.Equal(t, 1, st.Code())
	require.Contains(t, stderr, "building example.com/repo/a")
}

func TestStress_exits_2_when_it_cannot_run(t *testing.T) {
	repo := repotest.New(t, "0.0.1")
	testCases := []struct {
		name string
		args []string
		want string
	}{
		{"a flag it doesn't know", []string{"stress", "-nope"}, "flag provided but not defined"},
		{"a count below 1", []string{"stress", "-count", "0"}, "-count must be at least 1"},
		{"a confirm count below 1", []string{"stress", "-confirm", "0"}, "-confirm must be at least 1"},
		{"a timeout that isn't positive", []string{"stress", "-timeout", "0s"}, "-timeout must be positive"},
		{"a level it doesn't know", []string{"stress", "-tags", "unit,nope"}, `"nope" is not a level`},
		{"a containers cap below 1", []string{"stress", "-containers", "0"}, "-containers must be at least 1"},
		{"a clusters cap below 1", []string{"stress", "-clusters", "0"}, "-clusters must be at least 1"},
		{"a -run that doesn't compile", []string{"stress", "-keep", "0", "-cpu", "1", "-run", "(", "./a"}, "-run: error parsing regexp"},
		{"a -run whose subtest part doesn't compile", []string{"stress", "-keep", "0", "-cpu", "1", "-run", "TestA/x/(", "./a"}, "-run: error parsing regexp"},
		{"a CPU count that isn't one", []string{"stress", "-cpu", "1,x"}, `"x" is not a CPU count`},
		{"a load that isn't one", []string{"stress", "-load", "0"}, `"0" is not a load`},
		{"a shuffle that isn't on, off or a seed", []string{"stress", "-shuffle=x"}, `"x" is not on, off or a seed`},
		{"no lane", []string{"stress", "-keep", "100000"}, "leave no lane"},
		{"a pattern that matches nothing", []string{"stress", "-keep", "0", "-cpu", "1", "./nothing"}, "./nothing matched no package with tests"},
		{"no command", nil, "usage: flaky stress"},
		{"a command it doesn't know", []string{"nope"}, `unknown command "nope"`},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			st, _, stderr := stressIn(t, repo, tc.args...)
			require.Equal(t, 2, st.Code())
			require.Contains(t, stderr, tc.want)
		})
	}
}

func TestStress_fails_outside_a_git_repository(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "a"), 0o755))
	st, _, stderr := stressIn(t, dir, "stress", "-keep", "0")
	require.Equal(t, 2, st.Code())
	require.Contains(t, stderr, "git rev-parse")
}

func TestFlaky_prints_its_usage_when_asked(t *testing.T) {
	repo := repotest.New(t, "0.0.1")
	st, stdout, _ := stressIn(t, repo, "help")
	require.Equal(t, 0, st.Code())
	require.Contains(t, stdout, "usage: flaky stress")

	st, _, stderr := stressIn(t, repo, "stress", "-h")
	require.Equal(t, 0, st.Code())
	require.Contains(t, stderr, "-keep int")
}

func TestShuffle_is_off_on_or_a_seed(t *testing.T) {
	var s shuffle
	require.Equal(t, "off", s.String())
	require.Nil(t, s.next(), "off, a run isn't shuffled")

	require.NoError(t, s.Set("true"))
	require.Equal(t, "on", s.String())
	seeds := map[int64]bool{}
	for range 8 {
		seeds[*s.next()] = true
	}
	require.Greater(t, len(seeds), 1, "on, each run has a seed of its own")

	require.NoError(t, s.Set("-3"))
	require.Equal(t, "-3", s.String())
	require.Equal(t, int64(-3), *s.next())
	require.Equal(t, int64(-3), *s.next(), "with a seed, every run has that seed")

	require.NoError(t, s.Set("off"))
	require.Nil(t, s.next())
	require.Error(t, s.Set("sometimes"))
}

// walkthrough writes a module, name, to the repository repo, with a
// Dockerfile, a deploy/kind/up.sh that records the KIND_CLUSTER_NAME it ran
// with beside it and exits with code, and functional tests behind the
// functional tag. TestOnTheCluster passes when it runs on the cluster
// flaky-<name>, on 4 CPUs, and TestFails fails every run.
func walkthrough(t *testing.T, repo, name string, code int) {
	repotest.Write(t, repo, name+"/go.mod", "module example.com/repo/"+name+"\n\ngo 1.27.0\n")
	repotest.Write(t, repo, name+"/main.go", "package main\n\nfunc main() {}\n")
	repotest.Write(t, repo, name+"/Dockerfile", "FROM scratch\n")
	repotest.Write(t, repo, name+"/deploy/kind/up.sh", "#!/usr/bin/env bash\necho \"$KIND_CLUSTER_NAME\" >>\"$(dirname \"$0\")/up.log\"\necho \"deploying "+name+"\"\nexit "+strconv.Itoa(code)+"\n")
	require.NoError(t, os.Chmod(filepath.Join(repo, name, "deploy", "kind", "up.sh"), 0o755))
	repotest.Write(t, repo, name+"/functional/functional_test.go", "//go:build functional\n\npackage functional\n\nimport (\n\t\"os\"\n\t\"runtime\"\n\t\"testing\"\n)\n\n"+
		"func TestOnTheCluster(t *testing.T) {\n\tif got := os.Getenv(\"KIND_CLUSTER_NAME\"); got != \"flaky-"+name+"\" {\n\t\tt.Errorf(\"KIND_CLUSTER_NAME is %q\", got)\n\t}\n"+
		"\tif n := runtime.NumCPU(); n != 4 {\n\t\tt.Errorf(\"runtime.NumCPU() is %d\", n)\n\t}\n}\n\n"+
		"func TestFails(t *testing.T) {\n\tt.Error(\"always\")\n}\n")
	repotest.Commit(t, repo)
}

func TestStress_runs_each_walkthrough_s_functional_tests_on_a_kind_cluster_of_its_own(t *testing.T) {
	needCores(t, 8)
	docker := dockertest.Fake(t)
	repo := repotest.New(t, "0.0.1")
	walkthrough(t, repo, "wallet", 0)
	walkthrough(t, repo, "mailer", 1)

	st, stdout, stderr := stressIn(t, repo, "stress", "-count", "2", "-confirm", "1", "-keep", "0", "./mailer/...", "./wallet/...")
	require.Equal(t, 1, st.Code(), "stdout:\n%s\nstderr:\n%s", stdout, stderr)
	require.Contains(t, stderr, "found 2 packages with tests (0 unit, 0 testcontainers, 2 functional)", "-tags has functional by default")
	require.Contains(t, stderr, "built 2 test binaries")

	builds := dockertest.Read(t, docker, "builds")
	require.Contains(t, builds, "build --file "+repo+"/wallet/Dockerfile --tag wallet . in "+repo+"\n", "each walkthrough's image is built from the repository's root, tagged with its directory's name")
	require.Contains(t, builds, "build --file "+repo+"/mailer/Dockerfile --tag mailer . in "+repo+"\n")

	kind := dockertest.Read(t, docker, "kind.log")
	require.Equal(t, 2, strings.Count(kind, "create flaky-wallet\n"), "the wallet's cluster comes up for the sweep, and afresh for TestFails' confirm runs")
	require.Equal(t, 2, strings.Count(kind, "delete flaky-wallet\n"))
	require.Equal(t, 1, strings.Count(kind, "create flaky-mailer\n"), "the mailer's cluster isn't retried")
	require.Equal(t, 1, strings.Count(kind, "delete flaky-mailer\n"), "the cluster that didn't come up is deleted too")
	require.Regexp(t, `update --cpuset-cpus \d+,\d+,\d+,\d+ flaky-wallet-control-plane\n`, dockertest.Read(t, docker, "updates"), "its node lands on 4 cores of a lane")
	require.Equal(t, "flaky-wallet\nflaky-wallet\n", dockertest.Read(t, filepath.Join(repo, "wallet", "deploy", "kind"), "up.log"), "up.sh runs with KIND_CLUSTER_NAME set")
	require.Contains(t, dockertest.Read(t, docker, "kubectl.log"), "--context kind-flaky-wallet --namespace app rollout status deployment/web --timeout 5m\n", "it waits for the cluster's Deployments")
	require.Contains(t, stderr, "bringing flaky-wallet up for wallet on cores ")

	require.True(t, strings.HasPrefix(stdout, "UP FAILED: a walkthrough's cluster didn't come up, so its tests didn't run on it\n\nmailer  mailer\n"), stdout)
	require.Contains(t, stdout, "deploying mailer")
	require.Contains(t, stdout, "FAILING: failed or hung every run at some pair of CPU count and load\n\nwallet/functional  TestFails  (functional)\n")
	require.Contains(t, stdout, "    cpu 4 load 1           1         0         1\n", "its confirm runs are at CPU count 4 and load 1 alone")
	require.Contains(t, stdout, "reproduce: flaky stress -tags functional -run '^TestFails$' -count 1 ./wallet/functional\n")
	require.Contains(t, stdout, "taskset -c 0-3 go -C wallet test -race -cover -tags functional -count=1 -cpu 4 -timeout 15m0s -run '^TestFails$' ./functional\n", "the level's timeout is 15m")
	require.Contains(t, stdout, "1 test passed every run\n", "TestOnTheCluster ran on its cluster, with KIND_CLUSTER_NAME set and pinned to 4 cores")
}

func TestStress_exits_2_when_a_package_has_functional_tests_outside_a_walkthrough(t *testing.T) {
	dockertest.Fake(t)
	repo := repotest.New(t, "0.0.1")
	walkthrough(t, repo, "wallet", 0)
	require.NoError(t, os.Remove(filepath.Join(repo, "wallet", "Dockerfile")))
	repotest.Commit(t, repo)
	st, _, stderr := stressIn(t, repo, "stress", "-count", "1", "-keep", "0", "./wallet/...")
	require.Equal(t, 2, st.Code())
	require.Contains(t, stderr, "wallet/functional has functional tests, but its module wallet isn't a walkthrough")
}

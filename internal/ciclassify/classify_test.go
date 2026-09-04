package ciclassify

// The classifier is a shell script (scripts/ci-changes.sh); this test is the
// committed automation around it. It builds a throwaway git repo, points the
// script at it, and checks the key=value lines the script appends to the OUT
// file for every classification path.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type flags struct {
	workflow string
	lint     string
	build    string
	test     string
}

func allTrue() flags  { return flags{"true", "true", "true", "true"} }
func allFalse() flags { return flags{"false", "false", "false", "false"} }

// norm treats an empty expected field as "false" so expected values can be
// written concisely as flags{workflow: "true"}.
func norm(f flags) flags {
	if f.workflow == "" {
		f.workflow = "false"
	}
	if f.lint == "" {
		f.lint = "false"
	}
	if f.build == "" {
		f.build = "false"
	}
	if f.test == "" {
		f.test = "false"
	}
	return f
}

// Script path resolved once, relative to the package dir (= the working dir
// of `go test`). classify() points the child at the fixture repo as cwd, so
// the script itself must be addressed by absolute path.
var script = mustAbs(filepath.Join("..", "..", "scripts", "ci-changes.sh"))

func mustAbs(p string) string {
	p, err := filepath.Abs(p)
	if err != nil {
		panic(err)
	}
	return p
}

// gitEnv returns the current environment with git repository-selection and
// config-injection variables stripped. Every child in this test (fixture git
// commands and the classifier's own git calls) must be immune to such
// variables leaking from the environment the tests happen to run in — e.g. a
// pre-commit hook runner, which sets GIT_DIR and friends while invoking
// `go test`. Without this, `git -C <tempdir>` in a fixture would resolve its
// repo from the leaked variables and write into the surrounding repository:
// index, refs, and .git/config have all been corrupted this way.
func gitEnv() []string {
	env := os.Environ()
	out := make([]string, 0, len(env))
	for _, kv := range env {
		key := kv
		if eq := strings.IndexByte(kv, '='); eq >= 0 {
			key = kv[:eq]
		}
		switch {
		case key == "GIT_DIR",
			key == "GIT_COMMON_DIR",
			key == "GIT_WORK_TREE",
			key == "GIT_INDEX_FILE",
			key == "GIT_OBJECT_DIRECTORY",
			key == "GIT_QUARANTINE_PATH",
			key == "GIT_ALTERNATE_OBJECT_DIRECTORIES",
			key == "GIT_CONFIG_GLOBAL",
			key == "GIT_CONFIG_SYSTEM",
			key == "GIT_CONFIG_NOSYSTEM",
			key == "GIT_CONFIG_COUNT":
			continue
		case strings.HasPrefix(key, "GIT_CONFIG_KEY_"),
			strings.HasPrefix(key, "GIT_CONFIG_VALUE_"):
			continue
		}
		out = append(out, kv)
	}
	return out
}

// gitRepo builds a bare-ish fixture repo under a temp dir:
//   - main: root -> base commit (with .github/workflows/ci.yml committed)
//   - branch (default name): base -> head
//
// Returns (repoDir, gitRun) where gitRun executes git -C repoDir with args.
func gitRepo(t *testing.T, name string, onMain func(dir string, run func(...string))) (string, func(...string)) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = gitEnv()
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "classify@test")
	run("config", "user.name", "classify-test")
	run("config", "commit.gpgsign", "false")

	mustWrite := func(rel, content string) {
		t.Helper()
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(filepath.Join(".github", "workflows", "ci.yml"), "name: CI\n")
	mustWrite("main.go", "package main\n")
	run("add", "-A")
	run("commit", "-q", "-m", "base")
	baseSHA := revRef(t, dir, "main")
	if onMain != nil {
		onMain(dir, run)
	}
	// Branch forks from the original base, even if onMain moved main on.
	run("checkout", "-q", "-b", name, baseSHA)
	return dir, run
}

func writeAndCommit(t *testing.T, run func(...string), dir string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("add", "-A")
	run("commit", "-q", "-m", "change")
}

func classify(t *testing.T, repo, event, base, head string) flags {
	t.Helper()
	outFile := filepath.Join(t.TempDir(), "out")
	cmd := exec.Command("bash", script)
	cmd.Env = append(gitEnv(),
		"CLASSIFY_EVENT="+event,
		"CLASSIFY_BASE="+base,
		"CLASSIFY_HEAD="+head,
		"CLASSIFY_OUT="+outFile,
		"GITHUB_OUTPUT=", // unset in tests; script must not fall back to it
	)
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("classifier failed: %v\n%s", err, out)
	}
	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("no output written: %v\n%s", err, out)
	}
	got := flags{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("unparseable output line %q in %q", line, data)
		}
		switch k {
		case "workflow":
			got.workflow = v
		case "lint":
			got.lint = v
		case "build":
			got.build = v
		case "test":
			got.test = v
		default:
			t.Fatalf("unexpected key %q in %q", k, data)
		}
	}
	return got
}

func TestPullRequestScenarios(t *testing.T) {
	mixedWant := flags{workflow: "true", lint: "true", build: "true", test: "true"}

	cases := []struct {
		name  string
		files map[string]string
		want  flags
	}{
		{name: "workflow-only", files: map[string]string{".github/workflows/ci.yml": "name: CI v2\n"}, want: flags{workflow: "true"}},
		{name: "test-file-only", files: map[string]string{"main_test.go": "package main\n"}, want: flags{lint: "true", test: "true"}},
		{name: "production-go", files: map[string]string{"extra.go": "package main\n"}, want: flags{lint: "true", build: "true", test: "true"}},
		{name: "go-mod", files: map[string]string{"go.mod": "module x\n"}, want: flags{lint: "true", build: "true", test: "true"}},
		{name: "go-sum", files: map[string]string{"go.sum": ""}, want: flags{lint: "true", build: "true", test: "true"}},
		{name: "golangci", files: map[string]string{".golangci.yml": "run:\n"}, want: flags{lint: "true"}},
		{name: "makefile", files: map[string]string{"Makefile": "all:\n"}, want: flags{build: "true"}},
		{name: "inert-docs-only", files: map[string]string{"README.md": "# docs\n"}, want: allFalse()},
		{name: "workflow-yaml-arm", files: map[string]string{".github/workflows/deploy.yaml": "name: deploy\n"}, want: flags{workflow: "true"}},
		{name: "catch-all-other-file", files: map[string]string{"Dockerfile": "FROM scratch\n"}, want: flags{lint: "true", build: "true", test: "true"}},
		{
			name: "mixed-concerns-union",
			files: map[string]string{
				"README.md":                "# docs\n",
				".github/workflows/ci.yml": "name: CI v2\n",
				"main_test.go":             "package main\n",
				"extra.go":                 "package main\n",
				"go.mod":                   "module x\n",
				".golangci.yml":            "run:\n",
				"Makefile":                 "all:\n",
			},
			want: mixedWant,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, run := gitRepo(t, "pr", nil)
			writeAndCommit(t, run, dir, tc.files)
			base := revRef(t, dir, "main")
			head := revRef(t, dir, "pr")
			got := classify(t, dir, "pull_request", base, head)
			if want := norm(tc.want); got != want {
				t.Fatalf("want %+v, got %+v", want, got)
			}
		})
	}
}

// Early-failure contract: with no output file available (CLASSIFY_OUT and
// GITHUB_OUTPUT both unset/empty), the script must abort nonzero BEFORE
// writing any classification output.
func TestMissingOutputFileFailsLoudly(t *testing.T) {
	dir, _ := gitRepo(t, "pr", nil)
	outFile := filepath.Join(t.TempDir(), "out")
	cmd := exec.Command("bash", script)
	cmd.Dir = dir
	cmd.Env = append(gitEnv(),
		"CLASSIFY_EVENT=pull_request",
		"CLASSIFY_BASE=main",
		"CLASSIFY_HEAD=pr",
		"CLASSIFY_OUT=",
		"GITHUB_OUTPUT=",
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("script should have failed, output: %s", out)
	}
	if _, err := os.Stat(outFile); !os.IsNotExist(err) {
		t.Fatalf("script must write no output when no output file is available, stat err: %v", err)
	}
}

func TestPullRequestEmptyDiff(t *testing.T) {
	dir, _ := gitRepo(t, "pr", nil)
	base := revRef(t, dir, "main")
	head := revRef(t, dir, "pr")
	if got := classify(t, dir, "pull_request", base, head); got != allTrue() {
		t.Fatalf("empty diff must fail safe to all true, got %+v", got)
	}
}

// Explicit validation in the pull_request branch: an empty base warns and
// enables all concerns, mirroring the push branch's fail-safe path.
func TestPullRequestEmptyBaseFailsSafe(t *testing.T) {
	dir, _ := gitRepo(t, "pr", nil)
	head := revRef(t, dir, "pr")
	if got := classify(t, dir, "pull_request", "", head); got != allTrue() {
		t.Fatalf("missing pull_request base must fail safe to all true, got %+v", got)
	}
}

// Unresolvable base: merge-base fails, the original base is kept, the diff
// comes up empty, and the empty-diff catch-all enables all concerns.
func TestPullRequestBogusBaseDiffFailsSafe(t *testing.T) {
	dir, _ := gitRepo(t, "pr", nil)
	head := revRef(t, dir, "pr")
	if got := classify(t, dir, "pull_request", "deadbeef", head); got != allTrue() {
		t.Fatalf("unresolvable base must fail safe to all true, got %+v", got)
	}
}

// A PR that is already behind main: main gained a docs-only commit after the
// fork. The classifier must diff against the merge base, so main's own commit
// must not enable any concern.
func TestPullRequestMergeBaseExcludesMainAdvances(t *testing.T) {
	// main gains a docs-only commit AFTER the branch forked.
	dir, run := gitRepo(t, "pr", func(mainDir string, git func(...string)) {
		full := filepath.Join(mainDir, "README.md")
		if err := os.WriteFile(full, []byte("# main moved on\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git("add", "-A")
		git("commit", "-q", "-m", "main: docs move")
	})
	// branch commits only a workflow change.
	writeAndCommit(t, run, dir, map[string]string{".github/workflows/ci.yml": "name: CI v2\n"})
	base := revRef(t, dir, "main") // tip of main (NOT the fork point)
	head := revRef(t, dir, "pr")
	// fork point is the original base commit, strictly below main's tip
	if revRef(t, dir, "HEAD^") == base {
		t.Fatalf("test setup broken: fork point equals main tip")
	}
	got := classify(t, dir, "pull_request", base, head)
	if got != norm(flags{workflow: "true"}) {
		t.Fatalf("main's docs commit leaked into classification: got %+v", got)
	}
}

func TestPushZeroBeforeFailsSafe(t *testing.T) {
	dir, _ := gitRepo(t, "pr", nil)
	head := revRef(t, dir, "pr")
	if got := classify(t, dir, "push", strings.Repeat("0", 40), head); got != allTrue() {
		t.Fatalf("zero before-SHA must fail safe to all true, got %+v", got)
	}
}

func TestPushMissingBeforeFailsSafe(t *testing.T) {
	dir, _ := gitRepo(t, "pr", nil)
	head := revRef(t, dir, "pr")
	if got := classify(t, dir, "push", "", head); got != allTrue() {
		t.Fatalf("missing before-SHA must fail safe to all true, got %+v", got)
	}
}

func TestPushUnresolvableBeforeFailsSafe(t *testing.T) {
	dir, _ := gitRepo(t, "pr", nil)
	head := revRef(t, dir, "pr")
	if got := classify(t, dir, "push", "deadbeef", head); got != allTrue() {
		t.Fatalf("unresolvable before-SHA must fail safe to all true, got %+v", got)
	}
}

func TestPushRealBeforeClassifies(t *testing.T) {
	dir, run := gitRepo(t, "pr", nil)
	writeAndCommit(t, run, dir, map[string]string{"extra.go": "package main\n"})
	base := revRef(t, dir, "main")
	head := revRef(t, dir, "pr")
	if got := classify(t, dir, "push", base, head); got != norm(flags{lint: "true", build: "true", test: "true"}) {
		t.Fatalf("push with real before: want lint/build/test true, got %+v", got)
	}
}

func TestUnrecognizedEventsFailSafe(t *testing.T) {
	for _, event := range []string{"workflow_dispatch", "release", "schedule"} {
		t.Run(event, func(t *testing.T) {
			dir, _ := gitRepo(t, "pr", nil)
			head := revRef(t, dir, "pr")
			if got := classify(t, dir, event, head, head); got != allTrue() {
				t.Fatalf("%s must fail safe to all true, got %+v", event, got)
			}
		})
	}
}

// A push that changes only inert files must enable no concern: the diff is
// non-empty (SAW_PATH is set), so the empty-diff fail-safe does not trigger
// and classification runs over the inert table.
func TestPushInertPaths(t *testing.T) {
	dir, run := gitRepo(t, "pr", nil)
	writeAndCommit(t, run, dir, map[string]string{"README.md": "# docs\n"})
	base := revRef(t, dir, "main")
	head := revRef(t, dir, "pr")
	if got := classify(t, dir, "push", base, head); got != allFalse() {
		t.Fatalf("inert push must enable nothing, got %+v", got)
	}
}

// Empty head on pull_request: merge-base fails, the diff comes up empty, and
// the empty-diff catch-all enables all concerns.
func TestPullRequestEmptyHeadFailsSafe(t *testing.T) {
	dir, _ := gitRepo(t, "pr", nil)
	base := revRef(t, dir, "main")
	if got := classify(t, dir, "pull_request", base, ""); got != allTrue() {
		t.Fatalf("empty pull_request head must fail safe to all true, got %+v", got)
	}
}

// Bad head on push: base passes validation, but git diff fails on the bad
// head ref, leaving an empty path stream — the SAW_PATH=0 catch-all enables
// all concerns.
func TestPushBadHeadFailsSafe(t *testing.T) {
	for _, head := range []string{"", strings.Repeat("0", 40), "deadbeef"} {
		t.Run(head, func(t *testing.T) {
			dir, _ := gitRepo(t, "pr", nil)
			base := revRef(t, dir, "main")
			if got := classify(t, dir, "push", base, head); got != allTrue() {
				t.Fatalf("bad head %q must fail safe to all true, got %+v", head, got)
			}
		})
	}
}

// Push with base == head: genuinely empty diff (SAW_PATH stays 0) — the
// push-side fail-safe enables all concerns.
func TestPushEmptyDiffFailsSafe(t *testing.T) {
	dir, _ := gitRepo(t, "pr", nil)
	base := revRef(t, dir, "main")
	if got := classify(t, dir, "push", base, base); got != allTrue() {
		t.Fatalf("push with empty diff must fail safe to all true, got %+v", got)
	}
}

// A rev-parse-valid ref that is not a commit (a tag pointing at a blob):
// git rev-parse --verify accepts it, but git diff fails on it, so the
// classifier's path stream is empty and the fail-safe enables all concerns.
func blobTag(t *testing.T, run func(...string), dir string) {
	t.Helper()
	blob := revRef(t, dir, "main:main.go")
	run("update-ref", "refs/tags/blobtag", blob)
}

func TestPushGitDiffFailureFailsSafe(t *testing.T) {
	dir, run := gitRepo(t, "pr", nil)
	blobTag(t, run, dir)
	base := revRef(t, dir, "main")
	if got := classify(t, dir, "push", base, "blobtag"); got != allTrue() {
		t.Fatalf("git diff failure must fail safe to all true, got %+v", got)
	}
}

func revRef(t *testing.T, dir, ref string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", dir, "rev-parse", ref)
	cmd.Env = gitEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse %s: %v\n%s", ref, err, out)
	}
	return strings.TrimSpace(string(out))
}

// The full inert path table must stay inert together: any of these changed,
// on its own or combined, enables no concern.
func TestInertPathTable(t *testing.T) {
	inert := []string{
		"README.md", "docs/notes.md", "renovate.json5", ".pre-commit-config.yaml",
		".gitignore", ".editorconfig", "LICENSE", ".env.example",
		".github/CONTRIBUTING.md", "CHANGELOG.md", ".release-please-manifest.json",
		"release-please-config.json",
	}
	t.Run("all-inert", func(t *testing.T) {
		dir, run := gitRepo(t, "pr", nil)
		files := make(map[string]string, len(inert))
		for _, p := range inert {
			files[p] = "content\n"
		}
		writeAndCommit(t, run, dir, files)
		base := revRef(t, dir, "main")
		head := revRef(t, dir, "pr")
		if got := classify(t, dir, "pull_request", base, head); got != allFalse() {
			t.Fatalf("inert table must enable nothing, got %+v", got)
		}
	})
}

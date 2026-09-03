package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const guardGeneratedFilesWorkflowPath = ".github/workflows/guard-generated-files.yml"

// TestGuardGeneratedFilesWorkflowCoversReleasePleaseArtifacts pins the list of
// guarded paths. If release-please starts managing more files, add them here
// and to the workflow together.
func TestGuardGeneratedFilesWorkflowCoversReleasePleaseArtifacts(t *testing.T) {
	data, err := os.ReadFile(".github/workflows/guard-generated-files.yml")
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}
	content := string(data)

	guarded := []string{
		"CHANGELOG.md",
		".release-please-manifest.json",
	}
	for _, path := range guarded {
		if !strings.Contains(content, path) {
			t.Errorf("workflow must guard %q", path)
		}
		if _, err := os.Stat(path); err != nil {
			t.Errorf("guarded path %q not present in repo: %v", path, err)
		}
	}
}

// TestGuardGeneratedFilesWorkflowExemptsReleasePlease ensures the release
// pipeline's own PR (which legitimately modifies the generated files) is
// always allowed through.
func TestGuardGeneratedFilesWorkflowExemptsReleasePlease(t *testing.T) {
	data, err := os.ReadFile(".github/workflows/guard-generated-files.yml")
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}
	content := string(data)

	for _, login := range []string{"github-actions[bot]", "release-please[bot]"} {
		needle := "github.event.pull_request.user.login != '" + login + "'"
		if !strings.Contains(content, needle) {
			t.Errorf("workflow must exempt %q via %q", login, needle)
		}
	}
}

// TestGuardGeneratedFilesWorkflowUsesGitDiffWithFullHistory pins the
// git-based file-diff approach. Using the API would add a permission surface
// (pull-requests: read), rate-limit exposure, and pagination concerns; the
// git three-dot diff matches exactly what GitHub shows in "Files changed".
func TestGuardGeneratedFilesWorkflowUsesGitDiffWithFullHistory(t *testing.T) {
	data, err := os.ReadFile(".github/workflows/guard-generated-files.yml")
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}
	content := string(data)

	if !strings.Contains(content, "actions/checkout") {
		t.Errorf("workflow must check out the repo to run git diff locally")
	}
	if !strings.Contains(content, "fetch-depth: 0") {
		t.Errorf("workflow must use fetch-depth: 0 so merge-base for base...head is available")
	}
	if !strings.Contains(content, `git diff --name-only "${BASE_SHA}...${HEAD_SHA}"`) {
		t.Errorf("workflow must use 'git diff --name-only base...head' (three-dot) for PR file list")
	}
	if strings.Contains(content, "gh api") {
		t.Errorf("workflow must not fall back to the GitHub API for file listing")
	}
	if strings.Contains(content, "pull-requests:") {
		t.Errorf("workflow must not request pull-requests permission once switched to git diff")
	}
}

// TestGuardGeneratedFilesWorkflowTriggersOnPushedCommits ensures the guard
// re-runs when new commits are pushed to a PR (the synchronize event), so a
// contributor cannot open a clean PR then push a commit that edits CHANGELOG.md.
func TestGuardGeneratedFilesWorkflowTriggersOnPushedCommits(t *testing.T) {
	data, err := os.ReadFile(".github/workflows/guard-generated-files.yml")
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}
	content := string(data)

	for _, typ := range []string{"opened", "synchronize", "reopened"} {
		if !strings.Contains(content, typ) {
			t.Errorf("workflow must trigger on pull_request type %q", typ)
		}
	}
}

// guardGeneratedFilesWorkflow is the subset of the guard workflow these tests
// judge: the job condition that decides whether the guard runs at all, and the
// script step that actually judges the diff.
type guardGeneratedFilesWorkflow struct {
	Jobs map[string]guardGeneratedFilesJob `yaml:"jobs"`
}

type guardGeneratedFilesJob struct {
	Name  string                    `yaml:"name"`
	If    string                    `yaml:"if"`
	Steps []guardGeneratedFilesStep `yaml:"steps"`
}

type guardGeneratedFilesStep struct {
	Name string            `yaml:"name"`
	Uses string            `yaml:"uses"`
	Env  map[string]string `yaml:"env"`
	Run  string            `yaml:"run"`
}

func loadGuardGeneratedFilesWorkflow(t *testing.T) guardGeneratedFilesWorkflow {
	t.Helper()
	data, err := os.ReadFile(guardGeneratedFilesWorkflowPath)
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}
	var workflow guardGeneratedFilesWorkflow
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatalf("parse workflow: %v", err)
	}
	return workflow
}

func (w guardGeneratedFilesWorkflow) checkJob(t *testing.T) guardGeneratedFilesJob {
	t.Helper()
	job, ok := w.Jobs["check"]
	if !ok {
		t.Fatal("guard workflow is missing the check job")
	}
	return job
}

// guardGeneratedFilesScriptStep returns the step that judges the PR's file list.
func guardGeneratedFilesScriptStep(t *testing.T) guardGeneratedFilesStep {
	t.Helper()
	for _, step := range loadGuardGeneratedFilesWorkflow(t).checkJob(t).Steps {
		if strings.TrimSpace(step.Run) != "" {
			return step
		}
	}
	t.Fatal("guard workflow has no script step to judge the PR file list")
	return guardGeneratedFilesStep{}
}

// evaluateGuardGeneratedFilesCondition evaluates the guard job's `if:` the way a
// runner would for the two pull-request facts it can read: the author login and
// the head branch. Every term must be a literal `!=` comparison against one of
// those two facts, so an exemption written any other way - a wildcard, another
// context, an `||` - fails here instead of being silently treated as "the guard
// runs".
func evaluateGuardGeneratedFilesCondition(t *testing.T, condition, author, headRef string) bool {
	t.Helper()
	termPattern := regexp.MustCompile(`^github\.event\.pull_request\.(user\.login|head\.ref)\s*!=\s*'([^']+)'$`)
	facts := map[string]string{"user.login": author, "head.ref": headRef}
	for _, term := range strings.Split(condition, "&&") {
		term = strings.TrimSpace(term)
		matches := termPattern.FindStringSubmatch(term)
		if matches == nil {
			t.Fatalf("unsupported guard condition term %q", term)
		}
		if facts[matches[1]] == matches[2] {
			return false
		}
	}
	return true
}

// TestGuardGeneratedFilesWorkflowExemptsUpstreamSyncBranch covers both
// directions of the upstream-sync exemption on the guard's existing job
// condition: the sync branch is the only head branch that stops the guard from
// running, and every other branch - including one merely named like it - is
// judged exactly as before. The author exemptions keep working unchanged.
//
// The exemption belongs at the condition because the guard's own three-dot diff
// cannot tell a generated file carried in by an upstream merge from one a human
// typed by hand; TestGuardGeneratedFilesScriptStillFailsHandEditedGeneratedFiles
// proves that below.
func TestGuardGeneratedFilesWorkflowExemptsUpstreamSyncBranch(t *testing.T) {
	condition := loadGuardGeneratedFilesWorkflow(t).checkJob(t).If

	for _, tc := range []struct {
		name    string
		author  string
		headRef string
		wantRun bool
	}{
		{name: "upstream sync branch is exempt", author: "fork-maintainer", headRef: upstreamSyncBranch, wantRun: false},
		{name: "same author on an ordinary branch still runs", author: "fork-maintainer", headRef: "feature/hand-edit-changelog", wantRun: true},
		{name: "branch named like the sync branch still runs", author: "fork-maintainer", headRef: upstreamSyncBranch + "-trial", wantRun: true},
		{name: "sync branch without its prefix still runs", author: "fork-maintainer", headRef: "no-mistakes-upstream-sync", wantRun: true},
		{name: "release-please author exemption unchanged", author: "release-please[bot]", headRef: "release-please--branches--main", wantRun: false},
		{name: "github-actions author exemption unchanged", author: "github-actions[bot]", headRef: "feature/x", wantRun: false},
		{name: "unlisted bot on an ordinary branch still runs", author: "unlisted-automation[bot]", headRef: "feature/x", wantRun: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := evaluateGuardGeneratedFilesCondition(t, condition, tc.author, tc.headRef)
			if got != tc.wantRun {
				t.Fatalf("guard job runs for author %q head ref %q = %t, want %t", tc.author, tc.headRef, got, tc.wantRun)
			}
		})
	}
}

// guardGeneratedFilesRepo is a throwaway git repository shaped like a pull
// request: a base commit carrying the release-please-generated files, then the
// head the guard would judge.
type guardGeneratedFilesRepo struct {
	t       *testing.T
	dir     string
	branch  string
	baseSHA string
	headSHA string
}

func newGuardGeneratedFilesRepo(t *testing.T) *guardGeneratedFilesRepo {
	t.Helper()
	repo := &guardGeneratedFilesRepo{t: t, dir: t.TempDir()}
	repo.git("init", "-q", "--initial-branch=main")
	repo.write("CHANGELOG.md", "# Changelog\n\nGenerated by release-please. Do not edit.\n")
	repo.write(".release-please-manifest.json", "{\n  \".\": \"1.62.0\"\n}\n")
	repo.write("internal/code.go", "package code\n")
	repo.git("add", "-A")
	repo.git("commit", "--no-verify", "-m", "base")
	repo.branch = strings.TrimSpace(repo.git("symbolic-ref", "--short", "HEAD"))
	repo.baseSHA = strings.TrimSpace(repo.git("rev-parse", "HEAD"))
	return repo
}

func (r *guardGeneratedFilesRepo) write(path, content string) {
	r.t.Helper()
	full := filepath.Join(r.dir, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		r.t.Fatalf("create directory for %s: %v", path, err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		r.t.Fatalf("write %s: %v", path, err)
	}
}

// commit edits one file on the current branch, the way a PR head commit does.
func (r *guardGeneratedFilesRepo) commit(path, content, message string) {
	r.t.Helper()
	r.write(path, content)
	r.git("add", "-A")
	r.git("commit", "--no-verify", "-m", message)
	r.headSHA = strings.TrimSpace(r.git("rev-parse", "HEAD"))
}

func (r *guardGeneratedFilesRepo) checkoutNewBranch(name string) {
	r.t.Helper()
	r.git("checkout", "-b", name)
}

func (r *guardGeneratedFilesRepo) checkoutBaseBranch() {
	r.t.Helper()
	r.git("checkout", r.branch)
}

// merge records a two-parent merge of branch into the current branch, which is
// the shape of an upstream-sync head commit.
func (r *guardGeneratedFilesRepo) merge(branch, message string) {
	r.t.Helper()
	r.git("merge", "--no-ff", "--no-verify", "-m", message, branch)
	r.headSHA = strings.TrimSpace(r.git("rev-parse", "HEAD"))
}

func (r *guardGeneratedFilesRepo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", append([]string{
		"-c", "commit.gpgsign=false",
		"-c", "user.name=no-mistakes-test",
		"-c", "user.email=test@invalid",
	}, args...)...)
	cmd.Dir = r.dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=no-mistakes-test", "GIT_AUTHOR_EMAIL=test@invalid",
		"GIT_COMMITTER_NAME=no-mistakes-test", "GIT_COMMITTER_EMAIL=test@invalid",
		"GIT_TERMINAL_PROMPT=0",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// runGuardGeneratedFilesScript executes the guard workflow's real script step
// against a throwaway repository, so "an ordinary pull request still fails" is
// the guard's own verdict rather than a string match. The step is `run:` shell
// and this guard only ever runs on ubuntu-latest, so a POSIX shell is the exact
// environment it judges in.
func runGuardGeneratedFilesScript(t *testing.T, repo *guardGeneratedFilesRepo) (conclusion, output string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("guard script step is a POSIX shell step that only ever runs on ubuntu-latest")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash unavailable to execute the guard script step")
	}
	step := guardGeneratedFilesScriptStep(t)
	// This harness supplies the two facts the script reads. If the workflow ever
	// binds them elsewhere, these assertions fail instead of letting the harness
	// judge a diff the real step would never have seen.
	for name, want := range map[string]string{
		"BASE_SHA": "github.event.pull_request.base.sha",
		"HEAD_SHA": "github.event.pull_request.head.sha",
	} {
		if expr := step.Env[name]; !strings.Contains(expr, want) {
			t.Fatalf("script step env %s = %q, want it bound to %s", name, expr, want)
		}
	}
	if repo.headSHA == "" {
		t.Fatal("fixture repository has no head commit to judge")
	}

	cmd := exec.Command(bash, "-c", step.Run)
	cmd.Dir = repo.dir
	cmd.Env = append(filterEnv(os.Environ(), "BASE_SHA", "HEAD_SHA"),
		"BASE_SHA="+repo.baseSHA, "HEAD_SHA="+repo.headSHA)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err = cmd.Run()
	if err == nil {
		return "success", buf.String()
	}
	if _, ok := err.(*exec.ExitError); !ok {
		t.Fatalf("execute guard script step: %v\n%s", err, buf.String())
	}
	return "failure", buf.String()
}

// TestGuardGeneratedFilesScriptStillFailsHandEditedGeneratedFiles executes the
// guard's own verdict on three pull-request shapes, so the upstream-sync
// exemption above is proven not to have weakened the script for anyone else:
//
//   - a normal pull request that edits CHANGELOG.md by hand fails with the same
//     message it fails with today;
//   - a normal pull request that leaves both generated files alone passes;
//   - a two-parent upstream merge whose only generated-file change came from the
//     merged-in upstream commit also fails, which is exactly why the sync
//     exemption has to live on the job condition and cannot be derived from the
//     diff.
func TestGuardGeneratedFilesScriptStillFailsHandEditedGeneratedFiles(t *testing.T) {
	t.Run("hand-edited generated files fail", func(t *testing.T) {
		repo := newGuardGeneratedFilesRepo(t)
		repo.commit("CHANGELOG.md", "# Changelog\n\nHand-written entry by a contributor.\n", "docs: notes")

		conclusion, output := runGuardGeneratedFilesScript(t, repo)
		if conclusion != "failure" {
			t.Fatalf("conclusion = %q, want failure for a hand-edited CHANGELOG.md\n%s", conclusion, output)
		}
		for _, want := range []string{
			"This PR modifies release-please-generated files: CHANGELOG.md",
			"auto-generated by",
			"Do not hand-edit them",
		} {
			if !strings.Contains(output, want) {
				t.Errorf("output no longer reports %q:\n%s", want, output)
			}
		}
	})

	t.Run("manifest-only hand edit fails", func(t *testing.T) {
		repo := newGuardGeneratedFilesRepo(t)
		repo.commit(".release-please-manifest.json", "{\n  \".\": \"9.9.9\"\n}\n", "chore: bump manifest")

		conclusion, output := runGuardGeneratedFilesScript(t, repo)
		if conclusion != "failure" {
			t.Fatalf("conclusion = %q, want failure for a hand-edited manifest\n%s", conclusion, output)
		}
		if !strings.Contains(output, "release-please-generated files: .release-please-manifest.json") {
			t.Errorf("output does not name the manifest:\n%s", output)
		}
	})

	t.Run("pull request that leaves generated files alone passes", func(t *testing.T) {
		repo := newGuardGeneratedFilesRepo(t)
		repo.commit("internal/code.go", "package code\n\n// fork-only change\n", "fix: a real change")

		conclusion, output := runGuardGeneratedFilesScript(t, repo)
		if conclusion != "success" {
			t.Fatalf("conclusion = %q, want success when neither generated file is touched\n%s", conclusion, output)
		}
		if !strings.Contains(output, "No release-please-generated files modified") {
			t.Errorf("output does not report the clean verdict:\n%s", output)
		}
	})

	t.Run("upstream merge carrying generated files is not distinguishable from a hand edit", func(t *testing.T) {
		repo := newGuardGeneratedFilesRepo(t)
		repo.checkoutNewBranch("upstream")
		repo.commit("CHANGELOG.md", "# Changelog\n\n1.63.0 upstream release notes.\n", "chore(main): release 1.63.0")
		repo.checkoutBaseBranch()
		repo.commit("internal/code.go", "package code\n\n// fork-only feature\n", "feat: fork-only feature")
		repo.merge("upstream", "Merge upstream main into fork main")

		conclusion, output := runGuardGeneratedFilesScript(t, repo)
		if conclusion != "failure" {
			t.Fatalf("conclusion = %q, want failure: the three-dot diff lists upstream's CHANGELOG.md, "+
				"so the guard cannot grant this exemption itself\n%s", conclusion, output)
		}
		if !strings.Contains(output, "This PR modifies release-please-generated files: CHANGELOG.md") {
			t.Errorf("output does not name the merge-carried CHANGELOG.md:\n%s", output)
		}
	})
}

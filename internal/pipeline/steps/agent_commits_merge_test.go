package steps

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// advanceMain adds a commit to the default branch, which the hermetic test
// origin (the repository itself) then serves as the remote main tip, and
// returns to the detached head the agent was working on.
func advanceMain(t *testing.T, dir, file, content string) string {
	t.Helper()
	working := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "checkout", "main")
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", file)
	gitCmd(t, dir, "commit", "-m", "main: "+file)
	tip := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "checkout", "--detach", working)
	return tip
}

func commitParents(t *testing.T, dir, rev string) []string {
	t.Helper()
	return strings.Fields(gitCmd(t, dir, "rev-list", "--no-walk", "--parents", rev))[1:]
}

func requireMergeRefused(t *testing.T, err error, wantReason, dir, wantHead, recorded string, runHead string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "not a linear chain") || !strings.Contains(err.Error(), wantReason) {
		t.Fatalf("err = %v, want a refusal of the non-linear chain naming %q", err, wantReason)
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != wantHead {
		t.Fatalf("HEAD = %s, want the agent's commits %s left untouched", got, wantHead)
	}
	if runHead != recorded {
		t.Fatalf("run head = %s, want unchanged %s", runHead, recorded)
	}
}

func TestCommitAgentFixes_DefaultBranchMergeIsAcceptedAndFollowsCommitSettings(t *testing.T) {
	t.Parallel()
	sctx, dir, headSHA := agentCommitContext(t, attributionTrailers[1:])
	mainTip := advanceMain(t, dir, "upstream.txt", "landed on main\n")
	agentCommit(t, dir, "agent.txt", "fix: before the merge")
	gitCmd(t, dir, "merge", "--no-ff", "-m", "Merge branch 'main'\n\n"+agentAttribution, "main")
	wantTree := gitCmd(t, dir, "rev-parse", "HEAD^{tree}")
	wantAuthor := gitCmd(t, dir, "log", "-1", "--format=%an <%ae> %ad", "--date=raw")

	if _, err := commitAgentFixesWithResult(sctx, types.StepTest, "merge main", "fallback", &agent.Result{Provider: "codex", Model: "gpt-5.5"}); err != nil {
		t.Fatal(err)
	}
	head := gitCmd(t, dir, "rev-parse", "HEAD")
	if sctx.Run.HeadSHA != head {
		t.Fatalf("run head = %s, want the conformed merge %s recorded", sctx.Run.HeadSHA, head)
	}
	got := commitParents(t, dir, "HEAD")
	if len(got) != 2 || got[1] != mainTip {
		t.Fatalf("merge parents = %v, want the default branch tip %s kept as the second parent", got, mainTip)
	}
	if first := commitParents(t, dir, got[0]); len(first) != 1 || first[0] != headSHA {
		t.Fatalf("first parent's parents = %v, want the recorded head %s", first, headSHA)
	}
	want := "no-mistakes(test): merge main\n\nAssisted-by: no-mistakes:codex:gpt-5.5"
	if msg := fullMessage(t, dir, "HEAD"); msg != want {
		t.Fatalf("merge message = %q, want %q", msg, want)
	}
	if msg := fullMessage(t, dir, "HEAD^1"); msg != want {
		t.Fatalf("pre-merge commit message = %q, want %q", msg, want)
	}
	if tree := gitCmd(t, dir, "rev-parse", "HEAD^{tree}"); tree != wantTree {
		t.Fatalf("tree changed: %s, want %s", tree, wantTree)
	}
	if author := gitCmd(t, dir, "log", "-1", "--format=%an <%ae> %ad", "--date=raw"); author != wantAuthor {
		t.Fatalf("author = %q, want %q", author, wantAuthor)
	}
}

func TestCommitAgentFixes_DefaultBranchMergeWithConflictResolutionIsAccepted(t *testing.T) {
	t.Parallel()
	sctx, dir, headSHA := agentCommitContext(t, nil)
	mainTip := advanceMain(t, dir, "feature.txt", "main's version\n")
	// Conflicts on feature.txt, which both sides added.
	if out, err := exec.Command("git", "-C", dir, "merge", "-m", "merge", "main").CombinedOutput(); err == nil {
		t.Fatalf("test setup: merge succeeded, want a conflict: %s", out)
	}
	if !mergeInProgress(sctx.Ctx, dir) {
		t.Fatal("test setup: expected a conflicted merge")
	}
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature code\nmain's version\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "feature.txt")
	gitCmd(t, dir, "commit", "--no-edit")
	// A commit after the merge is still part of the fix round's chain.
	agentCommit(t, dir, "after.txt", "fix: after the merge")

	if _, err := commitAgentFixesWithResult(sctx, types.StepReview, "resolve", "fallback", nil); err != nil {
		t.Fatal(err)
	}
	if got := commitParents(t, dir, "HEAD~1"); len(got) != 2 || got[0] != headSHA || got[1] != mainTip {
		t.Fatalf("merge parents = %v, want [%s %s]", got, headSHA, mainTip)
	}
	if got := gitCmd(t, dir, "log", "--first-parent", "--format=%s", headSHA+"..HEAD"); got != "no-mistakes(review): resolve\nno-mistakes(review): resolve" {
		t.Fatalf("subjects = %q, want both chain commits conformed", got)
	}
	if sctx.Run.HeadSHA != gitCmd(t, dir, "rev-parse", "HEAD") {
		t.Fatal("run head not advanced to the conformed chain")
	}
}

func TestCommitAgentFixes_MergeOfAnEarlierDefaultBranchCommitIsAccepted(t *testing.T) {
	t.Parallel()
	sctx, dir, _ := agentCommitContext(t, nil)
	merged := advanceMain(t, dir, "one.txt", "one\n")
	gitCmd(t, dir, "merge", "--no-ff", "-m", "merge", "main")
	// main moves again between the agent's fetch and the pipeline's check.
	advanceMain(t, dir, "two.txt", "two\n")

	if _, err := commitAgentFixesWithResult(sctx, types.StepTest, "merge main", "fallback", nil); err != nil {
		t.Fatal(err)
	}
	if got := commitParents(t, dir, "HEAD"); len(got) != 2 || got[1] != merged {
		t.Fatalf("merge parents = %v, want second parent %s", got, merged)
	}
}

func TestCommitAgentFixes_DefaultBranchMergeThatAlsoChangesUnrelatedPathsIsRefused(t *testing.T) {
	t.Parallel()
	sctx, dir, headSHA := agentCommitContext(t, nil)
	advanceMain(t, dir, "upstream.txt", "landed on main\n")
	gitCmd(t, dir, "merge", "--no-ff", "--no-commit", "main")
	// An evil merge: base.txt is neither the PR's change nor a conflict.
	if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("smuggled\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "base.txt")
	gitCmd(t, dir, "commit", "-m", "merge")
	merged := gitCmd(t, dir, "rev-parse", "HEAD")

	_, err := commitAgentFixesWithResult(sctx, types.StepTest, "merge main", "fallback", nil)
	requireMergeRefused(t, err, "also changes paths the PR does not: base.txt", dir, merged, headSHA, sctx.Run.HeadSHA)
}

func TestCommitAgentFixes_MergeWithTheDefaultBranchAsFirstParentIsRefused(t *testing.T) {
	t.Parallel()
	sctx, dir, headSHA := agentCommitContext(t, nil)
	mainTip := advanceMain(t, dir, "upstream.txt", "landed on main\n")
	gitCmd(t, dir, "checkout", "--detach", mainTip)
	gitCmd(t, dir, "merge", "--no-ff", "-m", "merge", headSHA)
	merged := gitCmd(t, dir, "rev-parse", "HEAD")

	_, err := commitAgentFixesWithResult(sctx, types.StepTest, "merge main", "fallback", nil)
	requireMergeRefused(t, err, "", dir, merged, headSHA, sctx.Run.HeadSHA)
}

func TestCommitAgentFixes_SecondDefaultBranchMergeIsRefused(t *testing.T) {
	t.Parallel()
	sctx, dir, headSHA := agentCommitContext(t, nil)
	advanceMain(t, dir, "one.txt", "one\n")
	gitCmd(t, dir, "merge", "--no-ff", "-m", "merge one", "main")
	advanceMain(t, dir, "two.txt", "two\n")
	gitCmd(t, dir, "merge", "--no-ff", "-m", "merge two", "main")
	merged := gitCmd(t, dir, "rev-parse", "HEAD")

	_, err := commitAgentFixesWithResult(sctx, types.StepTest, "merge main", "fallback", nil)
	requireMergeRefused(t, err, "more than one merge", dir, merged, headSHA, sctx.Run.HeadSHA)
}

func TestCommitAgentFixes_MergeOfANonDefaultBranchNamesWhyItIsRefused(t *testing.T) {
	t.Parallel()
	sctx, dir, headSHA := agentCommitContext(t, nil)
	agentMergeCommit(t, dir, headSHA)
	merged := gitCmd(t, dir, "rev-parse", "HEAD")

	_, err := commitAgentFixesWithResult(sctx, types.StepReview, "repair", "fallback", nil)
	requireMergeRefused(t, err, "which is not on the remote main branch", dir, merged, headSHA, sctx.Run.HeadSHA)
}

func TestCIStep_CommitRepair_DefaultBranchMergeIsPublishableAndConformed(t *testing.T) {
	t.Parallel()
	sctx, dir, headSHA := agentCommitContext(t, nil)
	sctx.Config.CI.RevalidateRepairs = true
	mainTip := advanceMain(t, dir, "upstream.txt", "landed on main\n")
	gitCmd(t, dir, "merge", "--no-ff", "-m", "Merge branch 'main'", "main")

	if _, err := (&CIStep{}).commitRepair(sctx, "merge main", &agent.Result{Provider: "claude"}); err != nil {
		t.Fatal(err)
	}
	if got, want := fullMessage(t, dir, "HEAD"), "no-mistakes(ci): merge main"; got != want {
		t.Fatalf("commit message = %q, want %q", got, want)
	}
	if got := commitParents(t, dir, "HEAD"); len(got) != 2 || got[0] != headSHA || got[1] != mainTip {
		t.Fatalf("merge parents = %v, want [%s %s]", got, headSHA, mainTip)
	}
}

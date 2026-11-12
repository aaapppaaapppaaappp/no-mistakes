package steps

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// errAgentCommitsNotLinear marks agent commits that are not a chain on the
// recorded head the pipeline can rewrite without restructuring history: single-
// parent commits, plus at most one merge of the default branch that
// verifyDefaultBranchMerge accepts.
var errAgentCommitsNotLinear = errors.New("agent commits are not a linear chain from the recorded head")

// agentChainCommit is one commit on the agent's first-parent chain above the
// recorded head. mergedTip is the second parent of the one default-branch merge
// the chain may carry, and empty for an ordinary single-parent commit.
type agentChainCommit struct {
	sha       string
	mergedTip string
}

// conformAgentCommits applies the repository's commit settings to commits a fix
// agent made itself. When the agent runs `git commit`, the worktree is clean and
// the pipeline would otherwise adopt the agent's subject and trailers as they
// are, bypassing commit.fix_message and commit.trailers.
//
// The rewrite is anchored to the head the run has recorded, the same anchor
// assertPipelineHeadContinuity and ciHeadAwaitsRecording use: every commit above
// sctx.Run.HeadSHA is work the pipeline has not recorded yet, including commits
// an earlier failed agent turn left behind. Only a linear chain on that head is
// rewritten, with one exception: a single merge that brings the default branch
// in (verifyDefaultBranchMerge), which a fix round needs when the base moved
// under the PR or conflicts with it. Trees, authors and parents (both of a
// merge's) are kept, only the message changes, and HEAD moves with a
// compare-and-swap against the head observed. An unexpected shape or a git
// failure returns an error with the agent's commits untouched, so the change is
// never dropped and never published unshaped.
func conformAgentCommits(sctx *pipeline.StepContext, stepName types.StepName, render func() (string, error)) error {
	anchor := strings.TrimSpace(sctx.Run.HeadSHA)
	if anchor == "" {
		return nil
	}
	head, err := stepGitHeadSHA(sctx)
	if err != nil {
		return fmt.Errorf("resolve agent head: %w", err)
	}
	if head == anchor {
		return nil
	}
	// An unfinished rebase makes HEAD a partial result; leave it alone.
	if rebaseInProgress(sctx.Ctx, sctx.WorkDir) {
		return nil
	}
	// The caller's commit would conclude an unfinished merge into a merge commit,
	// the non-linear shape refused below, with the agent's own commits above the
	// recorded head still unshaped.
	if mergeInProgress(sctx.Ctx, sctx.WorkDir) {
		return fmt.Errorf("%s agent left a merge unfinished: %w; refusing to publish agent commits that bypass the commit settings", stepName, errAgentCommitsNotLinear)
	}
	descends, err := headDescendsFrom(sctx, anchor, head)
	if err != nil {
		return fmt.Errorf("inspect %s agent commits: %w", stepName, err)
	}
	if !descends {
		// A rebase rewrote history instead of extending the recorded head. That
		// is not the agent adding commits; continuity checks own that case.
		return nil
	}
	chain, err := agentCommitChain(sctx, anchor, head)
	if err != nil {
		return fmt.Errorf("inspect %s agent commits: %w", stepName, err)
	}
	message, err := render()
	if err != nil {
		return err
	}

	parent, changed := anchor, false
	published, resolvedPublished := -1, false
	for i, commit := range chain {
		sha := commit.sha
		info, err := stepGitRun(sctx, "show", "-s", "--date=raw", "--format=%T%x1f%an%x1f%ae%x1f%ad%x1f%B", sha)
		if err != nil {
			return fmt.Errorf("read %s agent commit %s: %w", stepName, sha, err)
		}
		fields := strings.SplitN(info, "\x1f", 5)
		if len(fields) != 5 {
			return fmt.Errorf("read %s agent commit %s: unexpected git output", stepName, sha)
		}
		if !changed && strings.TrimSpace(fields[4]) == message {
			parent = sha
			continue
		}
		// A CI repair can reach the remote while the run's head still lags (the
		// mirror or database write failed). The retry must finish that publication,
		// not replace what is already published, so those commits stay as they are.
		if !resolvedPublished {
			if published, err = publishedChainIndex(sctx, stepName, chain); err != nil {
				return err
			}
			resolvedPublished = true
		}
		if i <= published {
			parent = sha
			continue
		}
		changed = true
		env := []string{"GIT_AUTHOR_NAME=" + fields[1], "GIT_AUTHOR_EMAIL=" + fields[2], "GIT_AUTHOR_DATE=" + fields[3]}
		args := []string{"commit-tree", fields[0], "-p", parent}
		if commit.mergedTip != "" {
			args = append(args, "-p", commit.mergedTip)
		}
		parent, err = stepGitRunEnv(sctx, env, append(args, "-m", message)...)
		if err != nil {
			return fmt.Errorf("rewrite %s agent commit %s: %w", stepName, sha, err)
		}
	}
	if !changed {
		return nil
	}
	if _, err := stepGitRun(sctx, "update-ref", "-m", "no-mistakes: apply commit settings to agent commits", "HEAD", parent, head); err != nil {
		return fmt.Errorf("move head to rewritten %s agent commits: %w", stepName, err)
	}
	sctx.Log(fmt.Sprintf("applied commit settings to %d agent commit(s)", len(chain)))
	return nil
}

// publishedChainIndex returns the index in chain of the commit the remote
// branch already points at, or -1. Only a CI repair publishes before the run
// records its head, so other steps skip the remote query. A query failure is
// an error: the commits are then left untouched rather than risk rewriting
// published history.
func publishedChainIndex(sctx *pipeline.StepContext, stepName types.StepName, chain []agentChainCommit) (int, error) {
	if stepName != types.StepCI {
		return -1, nil
	}
	// The read gets its own deadline: neither the run context nor the agent
	// timeout bounds it, and a stalled connection would block the repair.
	bounded, cancel := publishedHeadBoundedContext(sctx)
	defer cancel()
	tip, err := lsRemoteSHA(func(args ...string) (string, error) { return stepGitRunTree(bounded, args...) }, resolvePushURL(sctx), normalizedBranchRef(sctx.Run.Branch))
	if err != nil {
		return -1, fmt.Errorf("check which %s agent commits are already published: %w", stepName, err)
	}
	for i, commit := range chain {
		if strings.EqualFold(commit.sha, tip) {
			return i, nil
		}
	}
	return -1, nil
}

// headDescendsFrom reports whether anchor is an ancestor of head. Exit status 1
// is git's answer "no"; any other failure is an error.
func headDescendsFrom(sctx *pipeline.StepContext, anchor, head string) (bool, error) {
	_, err := stepGitRun(sctx, "merge-base", "--is-ancestor", anchor, head)
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false, nil
	}
	return err == nil, err
}

// agentCommitChain lists the first-parent commits in anchor..head oldest first
// and requires them to be a chain that starts at anchor: single-parent commits,
// plus at most one two-parent merge that verifyDefaultBranchMerge accepts. Every
// other shape - a second merge, an octopus, a merge of anything but the default
// branch, or one whose first parent leaves the chain - is refused.
func agentCommitChain(sctx *pipeline.StepContext, anchor, head string) ([]agentChainCommit, error) {
	out, err := stepGitRun(sctx, "rev-list", "--first-parent", "--reverse", "--parents", anchor+".."+head)
	if err != nil {
		return nil, err
	}
	refuse := func(reason string) error {
		if reason != "" {
			reason = " (" + reason + ")"
		}
		return fmt.Errorf("commits %s..%s: %w%s; refusing to publish agent commits that bypass the commit settings", anchor, head, errAgentCommitsNotLinear, reason)
	}
	var chain []agentChainCommit
	parent, merged := anchor, false
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[1] != parent {
			return nil, refuse("")
		}
		commit := agentChainCommit{sha: fields[0]}
		switch len(fields) {
		case 2:
		case 3:
			if merged {
				return nil, refuse("more than one merge")
			}
			if reason, err := verifyDefaultBranchMerge(sctx, fields[0], fields[2]); err != nil {
				return nil, fmt.Errorf("inspect merge %s: %w", fields[0], err)
			} else if reason != "" {
				return nil, refuse(reason)
			}
			commit.mergedTip, merged = fields[2], true
		default:
			return nil, refuse("an octopus merge")
		}
		chain = append(chain, commit)
		parent = fields[0]
	}
	if parent != head {
		return nil, fmt.Errorf("commits %s..%s do not end at the observed head", anchor, head)
	}
	return chain, nil
}

func verifyDefaultBranchMerge(sctx *pipeline.StepContext, merge, second string) (string, error) {
	host, reason := buildHost(sctx, resolvedProvider(sctx))
	if host == nil {
		return "", fmt.Errorf("discover PR base branch to verify the merge: %s", reason)
	}
	branch := strings.TrimPrefix(sctx.Run.Branch, "refs/heads/")
	pr, err := host.FindPR(sctx.Ctx, branch, "")
	if err != nil {
		return "", fmt.Errorf("discover PR base branch to verify the merge: %w", err)
	}
	if owned := runPRURL(sctx); owned != "" && !samePRIdentity(owned, pr) {
		return "", fmt.Errorf("branch PR %q does not match recorded PR %q; cannot verify the merge", describePR(pr), owned)
	}
	base := effectivePRBaseBranch(sctx)
	if pr != nil {
		base = strings.TrimSpace(pr.BaseBranch)
		if base == "" {
			return "", fmt.Errorf("PR base branch is empty; cannot verify the merge")
		}
	}
	tip, resolved := resolveRunDefaultBranchTip(sctx.Ctx, sctx, "", base)
	if !resolved || tip == "" {
		return "", fmt.Errorf("fetch the remote %s branch to verify the merge", base)
	}
	if second != tip {
		contained, err := headDescendsFrom(sctx, second, tip)
		if err != nil {
			return "", err
		}
		if !contained {
			return fmt.Sprintf("merge %s brings in %s, which is not on the remote %s branch (tip %s)", shortSHA(merge), shortSHA(second), base, shortSHA(tip)), nil
		}
	}
	return "", nil
}

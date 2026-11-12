package citest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps/internal/stepstest"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	queuedSuite  = `[{"name":"odoo-suites","status":"QUEUED","bucket":"pending"}]`
	runningSuite = `[{"name":"odoo-suites","state":"IN_PROGRESS","bucket":"pending"}]`
	greenSuite   = `[{"name":"odoo-suites","state":"SUCCESS","bucket":"pass"}]`
)

// timedCIMonitor drives the CI step on a simulated clock. Before each poll
// after the first, next decides the checks the forge reports and how far the
// clock moves; it returns false to stop the monitor.
type timedCIMonitor struct {
	sctx  *pipeline.StepContext
	step  *steps.CIStep
	logs  []string
	polls int
}

func newTimedCIMonitor(t *testing.T, ciTimeout, queuedTimeout time.Duration, first string, next func(poll int) (checks string, advance time.Duration, ok bool)) *timedCIMonitor {
	t.Helper()
	dir, baseSHA, headSHA := stepstest.SetupGitRepo(t)
	m := &timedCIMonitor{polls: 1}
	m.sctx = stepstest.NewTestContext(t, &stepstest.MockAgent{AgentName: "test"}, dir, baseSHA, headSHA, config.Commands{})
	m.sctx.Env = stepstest.FakeCIGH(t, "OPEN", first)
	prURL := "https://github.com/test/repo/pull/42"
	m.sctx.Run.PRURL = &prURL
	m.sctx.Config.CITimeout = ciTimeout
	m.sctx.Config.CIQueuedTimeout = queuedTimeout
	m.sctx.Log = func(s string) { m.logs = append(m.logs, s) }
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m.sctx.Ctx = ctx

	current := time.Date(2026, time.November, 11, 8, 0, 0, 0, time.UTC)
	m.step = (&steps.CIStep{}).
		SetNow(func() time.Time { return current }).
		SetBaseBranchTip(func(context.Context) (string, bool) { return "sha-stable", true }).
		SetWaitForNextPoll(func(ctx context.Context, _ time.Duration) error {
			checks, advance, ok := next(m.polls)
			if !ok {
				cancel()
				return ctx.Err()
			}
			m.polls++
			current = current.Add(advance)
			for i, kv := range m.sctx.Env {
				if strings.HasPrefix(kv, "FAKE_CLI_CHECKS=") {
					m.sctx.Env[i] = "FAKE_CLI_CHECKS=" + checks
				}
			}
			return nil
		})
	return m
}

func (m *timedCIMonitor) logged(substr string) bool {
	for _, line := range m.logs {
		if strings.Contains(line, substr) {
			return true
		}
	}
	return false
}

func TestCIStep_QueuedCheckRearmsTheTimeoutUntilItStartsAndFinishes(t *testing.T) {
	t.Parallel()
	// Queued well past ci_timeout, then running within the re-armed window,
	// then green until the merge watch expires.
	m := newTimedCIMonitor(t, 10*time.Second, 0, queuedSuite, func(poll int) (string, time.Duration, bool) {
		switch {
		case poll < 5:
			return queuedSuite, 12 * time.Second, true
		case poll == 5:
			return runningSuite, 5 * time.Second, true
		case poll == 6:
			return greenSuite, 4 * time.Second, true
		case poll == 7:
			return greenSuite, 12 * time.Second, true
		default:
			return "", 0, false
		}
	})

	outcome, err := m.step.Execute(m.sctx)
	if err != nil {
		t.Fatalf("Execute: %v (logs: %v)", err, m.logs)
	}
	if outcome == nil || outcome.NeedsApproval || outcome.CIReadyNoCI == nil {
		t.Fatalf("outcome = %+v, want checks passed once the queued check ran green", outcome)
	}
	if m.polls != 8 {
		t.Fatalf("polls = %d, want the monitor to outlast the queue and see the checks finish", m.polls)
	}
	if !m.logged("CI timeout re-armed: odoo-suites still queued for a runner") {
		t.Fatalf("missing re-arm log: %v", m.logs)
	}
}

func TestCIStep_QueuedCheckCapNamesTheChecksAndTheWait(t *testing.T) {
	t.Parallel()
	m := newTimedCIMonitor(t, 10*time.Second, 2*time.Minute, queuedSuite, func(int) (string, time.Duration, bool) {
		return queuedSuite, 30 * time.Second, true
	})

	outcome, err := m.step.Execute(m.sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome == nil || !outcome.NeedsApproval {
		t.Fatalf("outcome = %+v, want the queued-check cap to park", outcome)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings.Items) != 1 {
		t.Fatalf("findings = %s, want exactly the queued-check finding", outcome.Findings)
	}
	got := findings.Items[0]
	if got.ID != "ci-checks-queued" || got.ActionOrDefault() != types.ActionAskUser ||
		!strings.Contains(got.Description, "odoo-suites") || !strings.Contains(got.Description, "after waiting 2m0s") {
		t.Fatalf("finding = %+v, want an ask-user finding naming odoo-suites and its 2m0s wait", got)
	}
	if strings.Contains(outcome.Findings, "PR was still open when CI monitoring timed out") {
		t.Fatalf("findings = %s, want the queued-check finding instead of the generic timeout", outcome.Findings)
	}
	if m.polls != 5 {
		t.Fatalf("polls = %d, want the cap reached at the fifth poll (2m after the queue was first seen)", m.polls)
	}
}

func TestCIStep_QueuedCheckCapIsNeverShorterThanCITimeout(t *testing.T) {
	t.Parallel()
	m := newTimedCIMonitor(t, 50*time.Second, 5*time.Second, queuedSuite, func(int) (string, time.Duration, bool) {
		return queuedSuite, 20 * time.Second, true
	})

	outcome, err := m.step.Execute(m.sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome == nil || !strings.Contains(outcome.Findings, "ci-checks-queued") {
		t.Fatalf("outcome = %+v, want the queued-check cap", outcome)
	}
	if m.polls != 4 {
		t.Fatalf("polls = %d, want the cap at ci_timeout (60s >= 50s), not at the shorter 5s cap", m.polls)
	}
}

func TestCIStep_QueuedCheckCapReportsCurrentPollFailures(t *testing.T) {
	t.Parallel()
	const queuedBuildFailure = `[{"name":"odoo-suites","status":"QUEUED","bucket":"pending"},{"name":"build","state":"FAILURE","bucket":"fail"}]`
	const queuedTestFailure = `[{"name":"odoo-suites","status":"QUEUED","bucket":"pending"},{"name":"test","state":"FAILURE","bucket":"fail"}]`
	for _, tc := range []struct {
		name, first, last string
		failures          []string
	}{
		{name: "new failure at the cap", first: queuedSuite, last: queuedBuildFailure, failures: []string{"build"}},
		{name: "previous failure clears at the cap", first: queuedBuildFailure, last: queuedSuite},
		{name: "failure changes at the cap", first: queuedBuildFailure, last: queuedTestFailure, failures: []string{"test"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := newTimedCIMonitor(t, 10*time.Second, 30*time.Second, tc.first, func(poll int) (string, time.Duration, bool) {
				return tc.last, 30 * time.Second, poll == 1
			})
			outcome, err := m.step.Execute(m.sctx)
			if err != nil {
				t.Fatal(err)
			}
			if outcome == nil || !outcome.NeedsApproval || outcome.AutoFixable {
				t.Fatalf("outcome = %+v, want the queue cap to park for a decision", outcome)
			}
			findings, err := types.ParseFindingsJSON(outcome.Findings)
			if err != nil {
				t.Fatal(err)
			}
			if len(findings.Items) != len(tc.failures)+1 {
				t.Fatalf("findings = %+v, want the queue and current failures %v", findings.Items, tc.failures)
			}
			queued := false
			var failures []string
			for _, item := range findings.Items {
				if item.ID == "ci-checks-queued" {
					queued = item.ActionOrDefault() == types.ActionAskUser
				} else if item.Category == types.FindingCategoryCICheck {
					failures = append(failures, item.Check)
				}
			}
			if !queued || strings.Join(failures, ",") != strings.Join(tc.failures, ",") {
				t.Fatalf("findings = %+v, want queue and current failures %v", findings.Items, tc.failures)
			}
		})
	}
}

func TestCIStep_QueuedCheckCapPreservesPendingExternalReview(t *testing.T) {
	t.Parallel()
	for _, withFailure := range []bool{false, true} {
		name, capChecks := "queue and review", queuedSuite
		wantCount := 2
		if withFailure {
			name = "queue, failure and review"
			capChecks = `[{"name":"odoo-suites","status":"QUEUED","bucket":"pending"},{"name":"build","state":"FAILURE","bucket":"fail"}]`
			wantCount = 3
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := newTimedCIMonitor(t, 10*time.Second, 30*time.Second, queuedSuite, func(poll int) (string, time.Duration, bool) {
				return capChecks, 30 * time.Second, poll == 1
			})
			m.sctx.Config.CI.RequiredReviews = []string{"coderabbit"}
			m.sctx.Env = append(m.sctx.Env,
				"FAKE_CLI_PR_HEAD_SHA="+m.sctx.Run.HeadSHA,
				"FAKE_CLI_EXTERNAL_REVIEWS="+externalReviewPayload(m.sctx.Run.HeadSHA, "in_progress", "", ""))
			outcome, err := m.step.Execute(m.sctx)
			if err != nil {
				t.Fatal(err)
			}
			if outcome == nil || !outcome.NeedsApproval || outcome.AutoFixable {
				t.Fatalf("outcome = %+v, want an ask-user timeout", outcome)
			}
			findings, err := types.ParseFindingsJSON(outcome.Findings)
			if err != nil {
				t.Fatal(err)
			}
			queued, review, failure := false, false, false
			for _, item := range findings.Items {
				if item.ActionOrDefault() != types.ActionAskUser {
					t.Fatalf("finding = %+v, want ask-user", item)
				}
				queued = queued || item.ID == "ci-checks-queued"
				review = review || item.Category == types.FindingCategoryCIReviewBot
				failure = failure || item.Category == types.FindingCategoryCICheck && item.Check == "build"
			}
			if len(findings.Items) != wantCount || !queued || !review || failure != withFailure {
				t.Fatalf("findings = %+v, want queue, review, and failure=%t", findings.Items, withFailure)
			}
		})
	}
}

func TestCIStep_GreenChecksAtTheDeadlineEndAsChecksPassed(t *testing.T) {
	t.Parallel()
	m := newTimedCIMonitor(t, 10*time.Second, 0, greenSuite, func(poll int) (string, time.Duration, bool) {
		if poll > 1 {
			return "", 0, false
		}
		return greenSuite, 12 * time.Second, true
	})

	outcome, err := m.step.Execute(m.sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome == nil || outcome.NeedsApproval || outcome.Findings != "" {
		t.Fatalf("outcome = %+v, want a clean pass with no finding", outcome)
	}
	if outcome.CIReadyNoCI == nil || *outcome.CIReadyNoCI {
		t.Fatalf("CIReadyNoCI = %v, want the checks-passed release for registered checks", outcome.CIReadyNoCI)
	}
	if !m.logged("CI timeout reached with every check green") {
		t.Fatalf("missing green-deadline log: %v", m.logs)
	}
}

func TestCIStep_GreenChecksAtTheDeadlineWithAPendingHandoffKeepTheTimeoutPath(t *testing.T) {
	t.Parallel()
	m := newTimedCIMonitor(t, 10*time.Second, 0, greenSuite, func(poll int) (string, time.Duration, bool) {
		if poll > 1 {
			return "", 0, false
		}
		return greenSuite, 12 * time.Second, true
	})
	m.sctx.HandoffPending = func() bool { return true }

	outcome, err := m.step.Execute(m.sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome == nil || !outcome.NeedsApproval || outcome.CIReadyNoCI != nil {
		t.Fatalf("outcome = %+v, want the handoff's timeout park", outcome)
	}
}

func TestCIStep_RunningCheckPastTheDeadlineStillTimesOut(t *testing.T) {
	t.Parallel()
	m := newTimedCIMonitor(t, 10*time.Second, 0, runningSuite, func(poll int) (string, time.Duration, bool) {
		if poll > 1 {
			return "", 0, false
		}
		return runningSuite, 12 * time.Second, true
	})

	outcome, err := m.step.Execute(m.sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome == nil || !outcome.NeedsApproval || !strings.Contains(outcome.Findings, "PR was still open when CI monitoring timed out") {
		t.Fatalf("outcome = %+v, want the idle timeout to park a check that started and never finished", outcome)
	}
}

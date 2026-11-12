package citest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps/internal/stepstest"
	"github.com/kunchenguid/no-mistakes/internal/scm/plugin/fakeplugin"
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

func newTimedCIMonitor(t *testing.T, ciTimeout time.Duration, first string, next func(poll int) (checks string, advance time.Duration, ok bool)) *timedCIMonitor {
	t.Helper()
	dir, baseSHA, headSHA := stepstest.SetupGitRepo(t)
	m := &timedCIMonitor{polls: 1}
	m.sctx = stepstest.NewTestContext(t, &stepstest.MockAgent{AgentName: "test"}, dir, baseSHA, headSHA, config.Commands{})
	m.sctx.Env = stepstest.FakeCIGH(t, "OPEN", first)
	prURL := "https://github.com/test/repo/pull/42"
	m.sctx.Run.PRURL = &prURL
	m.sctx.Config.CITimeout = ciTimeout
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
	m := newTimedCIMonitor(t, 10*time.Second, queuedSuite, func(poll int) (string, time.Duration, bool) {
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

func TestCIStep_QueuedChecksKeepMonitoringBeyondTwoDays(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, checks string
		review       bool
	}{
		{name: "queued", checks: queuedSuite},
		{name: "queued with a failure", checks: `[{"name":"odoo-suites","status":"QUEUED","bucket":"pending"},{"name":"build","state":"FAILURE","bucket":"fail"}]`},
		{name: "queued with an external review", checks: queuedSuite, review: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := newTimedCIMonitor(t, 4*time.Hour, tc.checks, func(poll int) (string, time.Duration, bool) {
				return tc.checks, 8 * time.Hour, poll < 7
			})
			if tc.review {
				m.sctx.Config.CI.RequiredReviews = []string{"coderabbit"}
				m.sctx.Env = append(m.sctx.Env,
					"FAKE_CLI_PR_HEAD_SHA="+m.sctx.Run.HeadSHA,
					"FAKE_CLI_EXTERNAL_REVIEWS="+externalReviewPayload(m.sctx.Run.HeadSHA, "in_progress", "", ""))
			}
			outcome, err := m.step.Execute(m.sctx)
			if !errors.Is(err, context.Canceled) || outcome != nil || m.polls != 7 {
				t.Fatalf("outcome=%+v err=%v polls=%d, want continued monitoring through 48h until cancellation", outcome, err, m.polls)
			}
		})
	}
}

func TestCIStep_GitLabCreatedJobsBehindManualStageKeepTheIdleTimeout(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		state string
		park  bool
	}{
		{state: "created", park: true},
		{state: "pending"},
	} {
		t.Run(tc.state, func(t *testing.T) {
			t.Parallel()
			checks := `[{"id":1,"name":"deploy","status":"manual","stage":"deploy","allow_failure":false},{"id":2,"name":"automatic","status":"` + tc.state + `","stage":"later","allow_failure":false}]`
			m := newTimedCIMonitor(t, 10*time.Second, "[]", func(poll int) (string, time.Duration, bool) {
				return checks, 12 * time.Second, poll == 1
			})
			m.sctx.Env = stepstest.FakeCIGlab(t, "opened", checks, m.sctx.Run.HeadSHA)
			m.sctx.Repo.UpstreamURL = "https://gitlab.com/test/repo.git"
			prURL := "https://gitlab.com/test/repo/-/merge_requests/42"
			m.sctx.Run.PRURL = &prURL
			outcome, err := m.step.Execute(m.sctx)
			if tc.park {
				if err != nil || outcome == nil || !outcome.NeedsApproval || outcome.CIReadyNoCI != nil || m.polls != 2 {
					t.Fatalf("outcome=%+v err=%v polls=%d, want a timeout decision for the manual-stage hold", outcome, err, m.polls)
				}
			} else if !errors.Is(err, context.Canceled) || outcome != nil {
				t.Fatalf("outcome=%+v err=%v, want a reported runner queue to keep waiting", outcome, err)
			}
		})
	}
}

func TestCIStep_GitHubDeploymentWaitingForApprovalKeepsTheIdleTimeout(t *testing.T) {
	t.Parallel()
	checks := `[{"name":"deployment","status":"WAITING","bucket":"pending"}]`
	m := newTimedCIMonitor(t, 10*time.Second, checks, func(poll int) (string, time.Duration, bool) {
		return checks, 12 * time.Second, poll == 1
	})
	outcome, err := m.step.Execute(m.sctx)
	if err != nil || outcome == nil || !outcome.NeedsApproval || outcome.CIReadyNoCI != nil || m.polls != 2 {
		t.Fatalf("outcome=%+v err=%v polls=%d, want a timeout decision for the deployment approval hold", outcome, err, m.polls)
	}
}

func TestCIStep_OtherProvidersKeepThePlainIdleTimeout(t *testing.T) {
	t.Parallel()
	sctx, _ := pluginCIContext(t, fakeplugin.State{Checks: []fakeplugin.Check{{Name: "build", Bucket: "pending", State: "PENDING"}}})
	current := time.Unix(1700000000, 0)
	waits := 0
	step := (&steps.CIStep{}).
		SetNow(func() time.Time { return current }).
		SetBaseBranchTip(func(context.Context) (string, bool) { return "stable-base", true }).
		SetWaitForNextPoll(func(context.Context, time.Duration) error {
			waits++
			if waits > 1 {
				return context.Canceled
			}
			current = current.Add(12 * time.Second)
			return nil
		})
	outcome, err := step.Execute(sctx)
	if err != nil || outcome == nil || !outcome.NeedsApproval || outcome.CIReadyNoCI != nil || waits != 1 {
		t.Fatalf("outcome=%+v err=%v waits=%d, want the ordinary timeout for a non-GitHub/GitLab provider", outcome, err, waits)
	}
}

func TestCIStep_GreenChecksAtTheDeadlineEndAsChecksPassed(t *testing.T) {
	t.Parallel()
	m := newTimedCIMonitor(t, 10*time.Second, greenSuite, func(poll int) (string, time.Duration, bool) {
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
	m := newTimedCIMonitor(t, 10*time.Second, greenSuite, func(poll int) (string, time.Duration, bool) {
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
	m := newTimedCIMonitor(t, 10*time.Second, runningSuite, func(poll int) (string, time.Duration, bool) {
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

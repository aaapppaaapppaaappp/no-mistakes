package agent

import (
	"context"
	"testing"
)

type purposeRecordingAgent struct {
	name        string
	purposes    []string
	resumable   bool
	neutralized bool
}

func (a *purposeRecordingAgent) Name() string { return a.name }

func (a *purposeRecordingAgent) Run(_ context.Context, opts RunOpts) (*Result, error) {
	a.purposes = append(a.purposes, opts.Purpose)
	return &Result{Provider: a.name}, nil
}

func (a *purposeRecordingAgent) Close() error { return nil }

func (a *purposeRecordingAgent) SupportsSessionResume() bool { return a.resumable }

func (a *purposeRecordingAgent) SupportsSessionProvider(provider string) bool {
	return a.resumable && provider == a.name
}

func (a *purposeRecordingAgent) NeutralizesGateInstructions() bool { return a.neutralized }

func TestReviewFixerRouterRoutesOnlyReviewFixPurpose(t *testing.T) {
	defaultAgent := &purposeRecordingAgent{name: "codex"}
	fixer := &purposeRecordingAgent{name: "acp:qwen-local"}
	router := NewReviewFixerRouter(defaultAgent, fixer)

	if _, err := router.Run(context.Background(), RunOpts{Purpose: "review"}); err != nil {
		t.Fatal(err)
	}
	if _, err := router.Run(context.Background(), RunOpts{Purpose: "review-fix"}); err != nil {
		t.Fatal(err)
	}

	if got := len(defaultAgent.purposes); got != 1 || defaultAgent.purposes[0] != "review" {
		t.Fatalf("default invocations = %v, want [review]", defaultAgent.purposes)
	}
	if got := len(fixer.purposes); got != 1 || fixer.purposes[0] != "review-fix" {
		t.Fatalf("fixer invocations = %v, want [review-fix]", fixer.purposes)
	}
}

func TestReviewFixerRouterUsesFixerSessionCapabilities(t *testing.T) {
	defaultAgent := &purposeRecordingAgent{name: "codex", resumable: true}
	fixer := &purposeRecordingAgent{name: "acp:qwen-local"}
	router := NewReviewFixerRouter(defaultAgent, fixer)

	if SupportsSessionResume(router) {
		t.Fatal("router advertised the default agent's session support for review-fix")
	}

	fixer.resumable = true
	if !SupportsSessionResume(router) {
		t.Fatal("router hid the review fixer agent's session support")
	}
	if !SupportsSessionProvider(router, fixer.name) {
		t.Fatalf("router does not support fixer provider %q", fixer.name)
	}
	if SupportsSessionProvider(router, defaultAgent.name) {
		t.Fatalf("router incorrectly supports default provider %q for review-fix sessions", defaultAgent.name)
	}
}

func TestReviewFixerRouterAttributesAttemptsToSelectedAgent(t *testing.T) {
	defaultAgent := &purposeRecordingAgent{name: "codex"}
	fixer := &purposeRecordingAgent{name: "acp:pi-qwen-flash-next-xhigh"}
	router := NewReviewFixerRouter(defaultAgent, fixer)

	var attempts []Attempt
	for _, purpose := range []string{"review", "review-fix"} {
		result, err := router.Run(context.Background(), RunOpts{
			Purpose:   purpose,
			OnAttempt: func(attempt Attempt) { attempts = append(attempts, attempt) },
		})
		if err != nil {
			t.Fatalf("run %s: %v", purpose, err)
		}
		if result == nil {
			t.Fatalf("run %s returned a nil result", purpose)
		}
	}

	if len(attempts) != 2 {
		t.Fatalf("attempts = %d, want 2: %+v", len(attempts), attempts)
	}
	if attempts[0].Agent != "codex" || attempts[0].Result == nil || attempts[0].Result.Provider != "codex" {
		t.Fatalf("review attempt = %+v, want codex attribution", attempts[0])
	}
	if attempts[1].Agent != "acp:pi-qwen-flash-next-xhigh" || attempts[1].Result == nil || attempts[1].Result.Provider != "acp:pi-qwen-flash-next-xhigh" {
		t.Fatalf("review-fix attempt = %+v, want dedicated fixer attribution", attempts[1])
	}
}

func TestReviewFixerRouterRequiresBothAgentsToNeutralizeGateInstructions(t *testing.T) {
	defaultAgent := &purposeRecordingAgent{name: "codex", neutralized: true}
	fixer := &purposeRecordingAgent{name: "acp:pi-qwen-flash-next-xhigh"}
	router := NewReviewFixerRouter(defaultAgent, fixer)

	if NeutralizesGateInstructions(router) {
		t.Fatal("router reported neutralized while its review fixer was unverified")
	}
	fixer.neutralized = true
	if !NeutralizesGateInstructions(router) {
		t.Fatal("router hid that both selected agents neutralize gate instructions")
	}
}

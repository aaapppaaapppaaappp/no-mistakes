package agent

import (
	"context"
	"testing"
)

type purposeRecordingAgent struct {
	name      string
	purposes  []string
	resumable bool
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

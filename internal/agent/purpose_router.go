package agent

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type reviewFixerRouter struct {
	defaultAgent Agent
	fixerAgent   Agent
}

// NewReviewFixerRouter sends review-fix turns to a dedicated agent and every
// other pipeline invocation to the default agent.
func NewReviewFixerRouter(defaultAgent, fixerAgent Agent) Agent {
	if fixerAgent == nil || fixerAgent == defaultAgent {
		return defaultAgent
	}
	return &reviewFixerRouter{defaultAgent: defaultAgent, fixerAgent: fixerAgent}
}

func (a *reviewFixerRouter) Name() string {
	if a.defaultAgent == nil {
		return ""
	}
	return a.defaultAgent.Name()
}

func (a *reviewFixerRouter) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	selected := a.defaultAgent
	if opts.Purpose == "review-fix" {
		selected = a.fixerAgent
	}
	if selected == nil {
		return nil, fmt.Errorf("no agent configured for purpose %q", opts.Purpose)
	}
	startedAt := time.Now()
	result, err := selected.Run(ctx, opts)
	if !ReportsAgentAttempts(selected) {
		emitAgentAttempt(opts, selected.Name(), result, err, startedAt, time.Now())
	}
	if err == nil && result != nil && result.Provider == "" {
		result.Provider = selected.Name()
	}
	return result, err
}

func (a *reviewFixerRouter) Close() error {
	var errs []string
	seen := map[Agent]bool{}
	closeOne := func(current Agent) {
		if current == nil || seen[current] {
			return
		}
		seen[current] = true
		if err := current.Close(); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", current.Name(), err))
		}
	}
	closeOne(a.defaultAgent)
	closeOne(a.fixerAgent)
	if len(errs) > 0 {
		return fmt.Errorf("close review agents: %s", strings.Join(errs, "; "))
	}
	return nil
}

func (a *reviewFixerRouter) SupportsSessionResume() bool {
	return SupportsSessionResume(a.fixerAgent)
}

func (a *reviewFixerRouter) SupportsSessionProvider(provider string) bool {
	return SupportsSessionProvider(a.fixerAgent, provider)
}

func (a *reviewFixerRouter) ReportsAgentAttempts() bool { return true }

func (a *reviewFixerRouter) NeutralizesGateInstructions() bool {
	if !NeutralizesGateInstructions(a.defaultAgent) {
		return false
	}
	return NeutralizesGateInstructions(a.fixerAgent)
}

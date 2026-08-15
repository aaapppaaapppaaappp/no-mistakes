package config

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestResolveAgentRequiresReviewFixerToBeRunnable(t *testing.T) {
	cfg := &Config{
		Agent:            types.AgentCodex,
		ReviewFixerAgent: "acp:qwen-local",
	}
	err := cfg.ResolveAgent(context.Background(), func(bin string) (string, error) {
		if bin == "codex" {
			return "/usr/bin/codex", nil
		}
		return "", &exec.Error{Name: bin, Err: exec.ErrNotFound}
	})
	if err == nil {
		t.Fatal("expected unavailable review fixer to fail resolution")
	}
}

func TestResolveAgentResolvesReviewFixer(t *testing.T) {
	cfg := &Config{
		Agent:            types.AgentCodex,
		ReviewFixerAgent: "acp:qwen-local",
	}
	err := cfg.ResolveAgent(context.Background(), func(bin string) (string, error) {
		switch bin {
		case "codex", "acpx":
			return "/usr/bin/" + bin, nil
		default:
			return "", &exec.Error{Name: bin, Err: exec.ErrNotFound}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.ReviewFixerAgent; got != types.AgentName("acp:qwen-local") {
		t.Fatalf("review fixer agent = %q", got)
	}
}

func TestLoadGlobalReviewFixerAgentAndMerge(t *testing.T) {
	global, err := LoadGlobalFromBytes([]byte(`
agent: codex
review_fixer_agent: codex
review_fixer_command: [unsloth, start, codex, --persist]
`))
	if err != nil {
		t.Fatal(err)
	}

	want := types.AgentCodex
	wantCommand := []string{"unsloth", "start", "codex", "--persist"}
	if global.ReviewFixerAgent != want {
		t.Fatalf("global review fixer agent = %q, want %q", global.ReviewFixerAgent, want)
	}
	if !reflect.DeepEqual(global.ReviewFixerCommand, wantCommand) {
		t.Fatalf("global review fixer command = %v, want %v", global.ReviewFixerCommand, wantCommand)
	}

	merged := Merge(global, &RepoConfig{})
	if merged.ReviewFixerAgent != want {
		t.Fatalf("merged review fixer agent = %q, want %q", merged.ReviewFixerAgent, want)
	}
	if !reflect.DeepEqual(merged.ReviewFixerCommand, wantCommand) {
		t.Fatalf("merged review fixer command = %v, want %v", merged.ReviewFixerCommand, wantCommand)
	}
}

func TestResolveAgentRequiresReviewFixerCommandToBeRunnable(t *testing.T) {
	cfg := &Config{
		Agent:              types.AgentCodex,
		ReviewFixerAgent:   types.AgentCodex,
		ReviewFixerCommand: []string{"unsloth", "start", "codex", "--persist"},
	}
	err := cfg.ResolveAgent(context.Background(), func(bin string) (string, error) {
		if bin == "codex" {
			return "/usr/bin/codex", nil
		}
		return "", &exec.Error{Name: bin, Err: exec.ErrNotFound}
	})
	if err == nil {
		t.Fatal("expected unavailable review fixer command to fail resolution")
	}
}

func TestLoadGlobalRejectsReviewFixerCommandWithoutAgent(t *testing.T) {
	_, err := LoadGlobalFromBytes([]byte(`
agent: codex
review_fixer_command: [unsloth, start, codex, --persist]
`))
	if err == nil {
		t.Fatal("expected review fixer command without agent to fail")
	}
}

func TestRepoConfigCannotOverrideGlobalReviewFixerAgent(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".no-mistakes.yaml"), []byte(`
review_fixer_agent: acp:qwen-local
`), 0o644); err != nil {
		t.Fatal(err)
	}

	repo, err := LoadRepo(dir)
	if err != nil {
		t.Fatal(err)
	}
	global := DefaultGlobalConfig()
	global.ReviewFixerAgent = types.AgentCodex
	merged := Merge(global, repo)
	if got := merged.ReviewFixerAgent; got != types.AgentCodex {
		t.Fatalf("repo-local review_fixer_agent changed global setting to %q", got)
	}
}

//go:build linux

package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
)

func TestReviewFixerACPPiProfilesRouteThroughExactSchemaProcesses(t *testing.T) {
	integrationPath, err := filepath.Abs(filepath.Join("..", "..", "integrations", "pi"))
	if err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(integrationPath, "node_modules", ".bin")
	acpx := filepath.Join(binDir, "acpx")
	piACP := filepath.Join(binDir, "pi-acp")
	pi := filepath.Join(binDir, "pi")
	for _, path := range []string{acpx, piACP, pi} {
		if info, statErr := os.Stat(path); statErr != nil || info.IsDir() {
			if os.Getenv("NM_REQUIRE_PI_ACP_INTEGRATION") == "1" {
				t.Fatalf("pinned executable %s is missing; run npm ci --prefix integrations/pi --ignore-scripts", path)
			}
			t.Skipf("pinned executable %s is required; run npm ci --prefix integrations/pi --ignore-scripts", path)
		}
	}

	fixtureDir := t.TempDir()
	providerPath := filepath.Join(fixtureDir, "fixture-provider.mjs")
	provider := `import { appendFileSync } from "node:fs";
import { createAssistantMessageEventStream } from "@earendil-works/pi-ai";

function model(id, reasoning) {
  return {
    id,
    name: id,
    reasoning,
    input: ["text"],
    cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
    contextWindow: 32000,
    maxTokens: 1024
  };
}

export default function fixtureProvider(pi) {
  const register = (provider, models) => pi.registerProvider(provider, {
    baseUrl: "http://127.0.0.1",
    apiKey: "credential-neutral-fixture",
    api: "openai-completions",
    models,
    streamSimple(selected) {
      appendFileSync(process.env.NM_PI_MODEL_CAPTURE, JSON.stringify({ provider: selected.provider, model: selected.id }) + "\n");
      const stream = createAssistantMessageEventStream();
      queueMicrotask(() => {
        const toolCall = {
          type: "toolCall",
          id: "structured-output-fixture-call",
          name: "structured_output",
          arguments: { route: selected.provider + "/" + selected.id }
        };
        const message = {
          role: "assistant",
          content: [toolCall],
          api: selected.api,
          provider: selected.provider,
          model: selected.id,
          usage: {
            input: 1,
            output: 1,
            cacheRead: 0,
            cacheWrite: 0,
            totalTokens: 2,
            cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 }
          },
          stopReason: "toolUse",
          timestamp: Date.now()
        };
        stream.push({ type: "start", partial: message });
        stream.push({ type: "toolcall_start", contentIndex: 0, partial: message });
        stream.push({ type: "toolcall_end", contentIndex: 0, toolCall, partial: message });
        stream.push({ type: "done", reason: "toolUse", message });
        stream.end();
      });
      return stream;
    }
  });
  register("openai-codex", [model("gpt-5.6-sol", true)]);
  register("flash-next", [model("Qwen/Qwen3.8-Flash-Next-FP8", true)]);
}`
	if err := os.WriteFile(providerPath, []byte(provider), 0o600); err != nil {
		t.Fatal(err)
	}
	fixtureScope := filepath.Join(fixtureDir, "node_modules", "@earendil-works")
	if err := os.MkdirAll(fixtureScope, 0o755); err != nil {
		t.Fatal(err)
	}
	piAIPath := filepath.Join(integrationPath, "node_modules", "@earendil-works", "pi-coding-agent", "node_modules", "@earendil-works", "pi-ai")
	if err := os.Symlink(piAIPath, filepath.Join(fixtureScope, "pi-ai")); err != nil {
		t.Fatal(err)
	}

	trustedWrapper := filepath.Join(integrationPath, "bin", "pi-no-mistakes-acp")
	writePiProfile := func(name, provider, model, thinking string) string {
		t.Helper()
		path := filepath.Join(fixtureDir, name)
		body := fmt.Sprintf(`#!/bin/sh
set -eu
printf '%%s\n' %q %q %q %q %q %q >> "$NM_PI_PROFILE_CAPTURE"
exec %q -e %q --provider %q --model %q --thinking %q --no-session --no-themes --no-context-files --offline "$@"
`, "--provider", provider, "--model", model, "--thinking", thinking, trustedWrapper, providerPath, provider, model, thinking)
		writeExecutableFixture(t, path, body)
		return path
	}
	solWrapper := writePiProfile("pi-sol-high", "openai-codex", "gpt-5.6-sol", "high")
	qwenWrapper := writePiProfile("pi-qwen-flash-next-xhigh", "flash-next", "Qwen/Qwen3.8-Flash-Next-FP8", "xhigh")
	acpxWrapper := filepath.Join(fixtureDir, "acpx")
	writeExecutableFixture(t, acpxWrapper, fmt.Sprintf(`#!/bin/sh
: > "$NM_ACPX_CAPTURE"
for arg do
  printf '%%s\0' "$arg" >> "$NM_ACPX_CAPTURE"
done
exec %q "$@"
`, acpx))

	global, err := config.LoadGlobalFromBytes([]byte(fmt.Sprintf(`
agent: acp:pi-sol-high
review_fixer_agent: acp:pi-qwen-flash-next-xhigh
acpx_path: %q
acp_registry_overrides:
  pi-sol-high: env NO_MISTAKES_PI_STRUCTURED_OUTPUT=1 PI_ACP_PI_COMMAND=%q %q
  pi-qwen-flash-next-xhigh: env NO_MISTAKES_PI_STRUCTURED_OUTPUT=1 PI_ACP_PI_COMMAND=%q %q
agent_config:
  acp:pi-sol-high:
    model: openai-codex/gpt-5.6-sol
  acp:pi-qwen-flash-next-xhigh:
    model: flash-next/Qwen/Qwen3.8-Flash-Next-FP8
`, acpxWrapper, solWrapper, piACP, qwenWrapper, piACP)))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Merge(global, &config.RepoConfig{})
	pipelineAgent, err := newPipelineAgent(context.Background(), cfg, t.TempDir(), exec.LookPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pipelineAgent.Close() })
	if agent.SupportsSessionResume(pipelineAgent) {
		t.Fatal("ACP review fixer unexpectedly advertised resumable sessions")
	}

	workDir := t.TempDir()
	run := func(tag, purpose, wantProvider, wantModel, wantThinking string, session *agent.SessionRef) *agent.Result {
		t.Helper()
		wantRoute := wantProvider + "/" + wantModel
		profileCapture := filepath.Join(fixtureDir, tag+"-profile.txt")
		modelCapture := filepath.Join(fixtureDir, tag+"-model.jsonl")
		acpxCapture := filepath.Join(fixtureDir, tag+"-acpx-args")
		schema := json.RawMessage(fmt.Sprintf(`{"type":"object","properties":{"route":{"type":"string","enum":[%q]}},"required":["route"],"additionalProperties":false}`, wantRoute))
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		result, runErr := pipelineAgent.Run(ctx, agent.RunOpts{
			Prompt:     "Return the credential-neutral fixture result",
			Purpose:    purpose,
			Session:    session,
			CWD:        workDir,
			JSONSchema: schema,
			Env: []string{
				"HOME=" + fixtureDir,
				"PI_CODING_AGENT_DIR=" + filepath.Join(fixtureDir, ".pi", "agent"),
				"NM_PI_PROFILE_CAPTURE=" + profileCapture,
				"NM_PI_MODEL_CAPTURE=" + modelCapture,
				"NM_ACPX_CAPTURE=" + acpxCapture,
			},
		})
		if runErr != nil {
			t.Fatalf("run %s: %v", tag, runErr)
		}
		if string(result.Output) != fmt.Sprintf(`{"route":%q}`, wantRoute) {
			t.Fatalf("%s structured output = %s, want route %q", tag, result.Output, wantRoute)
		}
		assertArgsContainInOrder(t, readNULArgs(t, acpxCapture), "--model", wantRoute, "exec", "--file", "-")
		profileData, readErr := os.ReadFile(profileCapture)
		if readErr != nil {
			t.Fatalf("read %s profile capture: %v", tag, readErr)
		}
		wantProfile := strings.Join([]string{"--provider", wantProvider, "--model", wantModel, "--thinking", wantThinking}, "\n") + "\n"
		if string(profileData) != wantProfile {
			t.Fatalf("%s Pi profile = %q, want %q", tag, profileData, wantProfile)
		}
		modelData, readErr := os.ReadFile(modelCapture)
		if readErr != nil {
			t.Fatalf("read %s model capture: %v", tag, readErr)
		}
		t.Logf("%s Pi profile: %s; selected model: %s", tag, strings.TrimSpace(string(profileData)), strings.TrimSpace(string(modelData)))
		return result
	}

	review := run("review", "review", "openai-codex", "gpt-5.6-sol", "high", nil)
	if review.Provider != "acp:pi-sol-high" {
		t.Fatalf("review provider = %q, want acp:pi-sol-high", review.Provider)
	}
	fix := run("review-fix", "review-fix", "flash-next", "Qwen/Qwen3.8-Flash-Next-FP8", "xhigh", &agent.SessionRef{ID: "must-not-resume", Agent: "acp:pi-qwen-flash-next-xhigh"})
	if fix.Provider != "acp:pi-qwen-flash-next-xhigh" || fix.SessionID != "" || fix.Resumed {
		t.Fatalf("review-fix result = provider %q session %q resumed %t", fix.Provider, fix.SessionID, fix.Resumed)
	}
	evidence := run("test-evidence", "test-evidence", "openai-codex", "gpt-5.6-sol", "high", nil)
	if evidence.Provider != "acp:pi-sol-high" {
		t.Fatalf("non-review provider = %q, want acp:pi-sol-high", evidence.Provider)
	}
}

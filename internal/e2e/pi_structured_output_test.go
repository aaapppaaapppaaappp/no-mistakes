//go:build e2e && linux

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestAcpxPiStructuredOutputTerminatingToolIntegration(t *testing.T) {
	integrationPath, err := filepath.Abs(filepath.Join("..", "..", "integrations", "pi"))
	if err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(integrationPath, "node_modules", ".bin")
	acpx := filepath.Join(binDir, "acpx")
	piACP := filepath.Join(binDir, "pi-acp")
	for _, path := range []string{acpx, filepath.Join(binDir, "pi"), piACP} {
		if info, statErr := os.Stat(path); statErr != nil || info.IsDir() {
			t.Fatalf("pinned executable %s is missing; run npm ci --prefix integrations/pi --ignore-scripts", path)
		}
	}

	dir := t.TempDir()
	providerPath := filepath.Join(dir, "fixture-provider.mjs")
	provider := `import { createAssistantMessageEventStream } from "@earendil-works/pi-ai";
export default function fixtureProvider(pi) {
  pi.registerProvider("no-mistakes-fixture", {
    baseUrl: "http://127.0.0.1",
    apiKey: "credential-neutral-fixture",
    api: "openai-completions",
    models: [{
      id: "structured-output",
      name: "Structured Output Fixture",
      reasoning: false,
      input: ["text"],
      cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
      contextWindow: 32000,
      maxTokens: 1024
    }],
    streamSimple(model) {
      const stream = createAssistantMessageEventStream();
      queueMicrotask(() => {
        const toolCall = {
          type: "toolCall",
          id: "structured-output-fixture-call",
          name: "structured_output",
          arguments: { summary: "through-acp" }
        };
        const message = {
          role: "assistant",
          content: [toolCall],
          api: model.api,
          provider: model.provider,
          model: model.id,
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
}`
	if err := os.WriteFile(providerPath, []byte(provider), 0o600); err != nil {
		t.Fatal(err)
	}
	fixtureScope := filepath.Join(dir, "node_modules", "@earendil-works")
	if err := os.MkdirAll(fixtureScope, 0o755); err != nil {
		t.Fatal(err)
	}
	piAIPath := filepath.Join(integrationPath, "node_modules", "@earendil-works", "pi-coding-agent", "node_modules", "@earendil-works", "pi-ai")
	if err := os.Symlink(piAIPath, filepath.Join(fixtureScope, "pi-ai")); err != nil {
		t.Fatal(err)
	}

	wrapperPath := filepath.Join(integrationPath, "bin", "pi-no-mistakes-acp")
	fixtureWrapperPath := filepath.Join(dir, "pi-fixture")
	fixtureWrapper := fmt.Sprintf(`#!/bin/sh
set -eu
exec %q -e %q --provider no-mistakes-fixture --model structured-output --no-session --no-themes --no-context-files --offline "$@"
`, wrapperPath, providerPath)
	if err := os.WriteFile(fixtureWrapperPath, []byte(fixtureWrapper), 0o700); err != nil {
		t.Fatal(err)
	}
	rawCommand := fmt.Sprintf("env NO_MISTAKES_PI_STRUCTURED_OUTPUT=1 PI_ACP_PI_COMMAND=%q %q", fixtureWrapperPath, piACP)
	a, err := agent.NewWithOptions(types.AgentName("acp:pi"), acpx, nil, agent.Options{
		ACPRegistryOverrides: map[string]string{"pi": rawCommand},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := a.Run(ctx, agent.RunOpts{
		Prompt: "Return the fixture result",
		CWD:    dir,
		Env: []string{
			"HOME=" + dir,
			"PI_CODING_AGENT_DIR=" + filepath.Join(dir, ".pi", "agent"),
		},
		JSONSchema: json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string","enum":["through-acp"]}},"required":["summary"],"additionalProperties":false}`),
	})
	if err != nil {
		t.Fatalf("ACP/Pi structured output integration: %v", err)
	}
	if res.Text != `{"summary":"through-acp"}` {
		t.Fatalf("result text = %q, want terminating tool JSON", res.Text)
	}
}

//go:build linux

package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

func piStructuredOutputIntegrationPath(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", "..", "integrations", "pi"))
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPiStructuredOutputExtensionContract(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required to run the repository-owned Pi extension contract tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "--test", filepath.Join(piStructuredOutputIntegrationPath(t), "test", "structured-output.test.mjs"))
	shellenv.ConfigureShellCommand(cmd)
	if output, err := shellenv.CombinedOutputShellCommand(cmd); err != nil {
		t.Fatalf("Pi extension contract tests: %v\n%s", err, output)
	}
}

func TestAcpxPiStructuredOutputTerminatingToolIntegration(t *testing.T) {
	integrationPath := piStructuredOutputIntegrationPath(t)
	binDir := filepath.Join(integrationPath, "node_modules", ".bin")
	requirePinned := os.Getenv("NM_REQUIRE_PI_ACP_INTEGRATION") == "1"
	acpx := filepath.Join(binDir, "acpx")
	pi := filepath.Join(binDir, "pi")
	piACP := filepath.Join(binDir, "pi-acp")
	for _, path := range []string{acpx, pi, piACP} {
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			continue
		}
		if requirePinned {
			t.Fatalf("pinned executable %s is missing; run npm ci --prefix integrations/pi --ignore-scripts", path)
		}
		t.Skipf("pinned executable %s is required; run npm ci --prefix integrations/pi --ignore-scripts", path)
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
	rawCommand := fmt.Sprintf("env %s=1 PI_ACP_PI_COMMAND=%q %q", acpxStructuredOutputEnvVar, fixtureWrapperPath, piACP)
	schema := json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string","enum":["through-acp"]}},"required":["summary"],"additionalProperties":false}`)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	a := &acpxAgent{bin: acpx, target: "pi", rawCommand: rawCommand}
	res, err := a.Run(ctx, RunOpts{
		Prompt: "Return the fixture result",
		CWD:    dir,
		Env: []string{
			"HOME=" + dir,
			"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
			"PI_CODING_AGENT_DIR=" + filepath.Join(dir, ".pi", "agent"),
		},
		JSONSchema: schema,
	})
	if err != nil {
		t.Fatalf("ACP/Pi structured output integration: %v", err)
	}
	if res.Text != `{"summary":"through-acp"}` {
		t.Fatalf("result text = %q, want terminating tool JSON", res.Text)
	}
}

func TestPiStructuredOutputExtensionLoadsInPiRPC(t *testing.T) {
	integrationPath := piStructuredOutputIntegrationPath(t)
	binDir := filepath.Join(integrationPath, "node_modules", ".bin")
	piPath := filepath.Join(binDir, "pi")
	if info, err := os.Stat(piPath); err != nil || info.IsDir() {
		if os.Getenv("NM_REQUIRE_PI_ACP_INTEGRATION") == "1" {
			t.Fatalf("pinned executable %s is missing; run npm ci --prefix integrations/pi --ignore-scripts", piPath)
		}
		t.Skipf("pinned executable %s is required; run npm ci --prefix integrations/pi --ignore-scripts", piPath)
	}

	dir := t.TempDir()
	agentDir := filepath.Join(dir, ".pi", "agent")
	extensionsDir := filepath.Join(agentDir, "extensions")
	if err := os.MkdirAll(extensionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extensionsDir, "stale-structured-output.mjs"), []byte(`throw new Error("stale profile extension loaded")`), 0o600); err != nil {
		t.Fatal(err)
	}
	schemaPath := filepath.Join(dir, "schema.json")
	schema := []byte(`{"type":"object","properties":{"summary":{"type":"string"}},"required":["summary"]}`)
	if err := os.WriteFile(schemaPath, schema, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(schema)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	wrapperPath := filepath.Join(integrationPath, "bin", "pi-no-mistakes-acp")
	cmd := exec.CommandContext(ctx, wrapperPath,
		"--mode", "rpc",
		"--no-session",
		"--no-themes",
		"--no-context-files",
		"--offline",
	)
	cmd.Dir = dir
	cmd.Env = append(gitSafeEnv(dir),
		"HOME="+dir,
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"PI_CODING_AGENT_DIR="+agentDir,
		"NO_MISTAKES_GATE=1",
		acpxStructuredOutputEnvVar+"=1",
		acpxSchemaEnvVar+"="+schemaPath,
		acpxSchemaDigestEnvVar+"="+hex.EncodeToString(sum[:]),
	)
	cmd.Stdin = strings.NewReader("{\"id\":\"probe\",\"type\":\"get_state\"}\n")
	shellenv.ConfigureShellCommand(cmd)
	output, err := shellenv.CombinedOutputShellCommand(cmd)
	if err != nil {
		t.Fatalf("Pi RPC extension probe: %v\n%s", err, output)
	}
	text := string(output)
	if !strings.Contains(text, `"id":"probe"`) || !strings.Contains(text, `"success":true`) {
		t.Fatalf("Pi RPC extension probe returned no successful response: %s", text)
	}
	if strings.Contains(text, "extension_error") {
		t.Fatalf("Pi rejected the transported schema extension: %s", text)
	}
}

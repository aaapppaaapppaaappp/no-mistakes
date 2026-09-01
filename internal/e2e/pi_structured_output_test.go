//go:build e2e && linux

package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestPiStructuredOutputWrapperReportsMissingPinnedRuntime(t *testing.T) {
	integrationPath, err := filepath.Abs(filepath.Join("..", "..", "integrations", "pi"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	wrapperPath := filepath.Join(binDir, "pi-no-mistakes-acp")
	if err := os.Symlink(filepath.Join(integrationPath, "bin", "pi-no-mistakes-acp"), wrapperPath); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, wrapperPath, "--version")
	shellenv.ConfigureShellCommand(cmd)
	output, err := shellenv.CombinedOutputShellCommand(cmd)
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 127 {
		t.Fatalf("wrapper error = %v, output = %q; want exit 127", err, output)
	}
	want := "no-mistakes structured output dependencies are missing; run `npm ci --prefix integrations/pi --ignore-scripts` from the trusted checkout\n"
	if string(output) != want {
		t.Fatalf("wrapper output = %q, want %q", output, want)
	}
}

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
	t.Logf("ACP/Pi terminating structured output: %s", res.Text)
}

func TestAcpxPiStrictResponsesGateRepairsWithinOneAttempt(t *testing.T) {
	integrationPath, err := filepath.Abs(filepath.Join("..", "..", "integrations", "pi"))
	if err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(integrationPath, "node_modules", ".bin")
	realAcpx := filepath.Join(binDir, "acpx")
	piACP := filepath.Join(binDir, "pi-acp")
	for _, path := range []string{realAcpx, filepath.Join(binDir, "pi"), piACP} {
		if info, statErr := os.Stat(path); statErr != nil || info.IsDir() {
			t.Fatalf("pinned executable %s is missing; run npm ci --prefix integrations/pi --ignore-scripts", path)
		}
	}

	var requests atomic.Int32
	requestBody := make(chan []byte, 3)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestNumber := requests.Add(1)
		body, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			t.Errorf("read provider request: %v", readErr)
		}
		select {
		case requestBody <- body:
		default:
		}
		if r.URL.Path != "/v1/responses" {
			t.Errorf("provider path = %q, want Responses endpoint", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		arguments := `{"summary":"through-responses"}`
		item := map[string]any{
			"type": "function_call", "id": "fc_fixture", "call_id": "call_fixture",
			"name": "structured_output", "arguments": arguments, "status": "completed",
		}
		output := []any{item}
		if requestNumber == 1 {
			// The first provider turn settles without the required call. The Pi
			// extension must inject one fixed repair nudge into this SAME session,
			// not make acpx/no-mistakes start a fresh attempt.
			output = nil
		}
		response := map[string]any{
			"id": "resp_fixture", "object": "response", "status": "completed",
			"output": output, "error": nil, "incomplete_details": nil,
			"usage": map[string]any{
				"input_tokens": 10, "output_tokens": 5, "total_tokens": 15,
				"input_tokens_details":  map[string]any{"cached_tokens": 0},
				"output_tokens_details": map[string]any{"reasoning_tokens": 2},
			},
		}
		writeSSE := func(event string, data any) {
			encoded, marshalErr := json.Marshal(data)
			if marshalErr != nil {
				t.Errorf("marshal SSE: %v", marshalErr)
				return
			}
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, encoded)
		}
		writeSSE("response.created", map[string]any{"type": "response.created", "response": response})
		if requestNumber > 1 {
			writeSSE("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{
				"type": "function_call", "id": "fc_fixture", "call_id": "call_fixture",
				"name": "structured_output", "arguments": "", "status": "in_progress",
			}})
			writeSSE("response.function_call_arguments.done", map[string]any{
				"type": "response.function_call_arguments.done", "output_index": 0,
				"item_id": "fc_fixture", "arguments": arguments,
			})
			writeSSE("response.output_item.done", map[string]any{
				"type": "response.output_item.done", "output_index": 0, "item": item,
			})
		}
		writeSSE("response.completed", map[string]any{"type": "response.completed", "response": response})
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer provider.Close()

	dir := t.TempDir()
	acpxEvents := filepath.Join(dir, "acpx-events.jsonl")
	acpx := filepath.Join(dir, "acpx-capture")
	acpxScript := fmt.Sprintf("#!/bin/bash\nset -o pipefail\n%q \"$@\" | tee %q\n", realAcpx, acpxEvents)
	if err := os.WriteFile(acpx, []byte(acpxScript), 0o700); err != nil {
		t.Fatal(err)
	}
	agentDir := filepath.Join(dir, "source-agent")
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	models := fmt.Sprintf(`{
  "providers": {"no-mistakes-flash-next-responses": {
    "baseUrl": %q, "apiKey": "credential-neutral-fixture", "api": "openai-responses",
    "compat": {"supportsStrictMode": true},
    "models": [{
      "id": "Qwen/Qwen3.8-Flash-Next-FP8", "reasoning": true,
      "thinkingLevelMap": {"off": null, "xhigh": "xhigh"},
      "contextWindow": 262144, "maxTokens": 4096,
      "samplingParams": {"temperature": 1.0, "top_p": 0.95, "seed": 424242}
    }]
  }}
}`, provider.URL+"/v1")
	if err := os.WriteFile(filepath.Join(agentDir, "models.json"), []byte(models), 0o600); err != nil {
		t.Fatal(err)
	}

	piStarts := filepath.Join(dir, "pi-starts")
	countingWrapper := filepath.Join(dir, "pi-gate-counting-wrapper")
	dedicatedWrapper := filepath.Join(integrationPath, "bin", "pi-no-mistakes-flash-next-responses-acp")
	wrapperScript := fmt.Sprintf("#!/bin/sh\nprintf x >> %q\nexec %q \"$@\"\n", piStarts, dedicatedWrapper)
	if err := os.WriteFile(countingWrapper, []byte(wrapperScript), 0o700); err != nil {
		t.Fatal(err)
	}

	rawCommand := fmt.Sprintf(
		"env NO_MISTAKES_PI_STRUCTURED_OUTPUT=1 NO_MISTAKES_ACPX_ATTEMPTS=1 PI_ACP_PI_COMMAND=%q %q",
		countingWrapper, piACP,
	)
	a, err := agent.NewWithOptions(types.AgentName("acp:pi-flash-next-responses-gate"), acpx, nil, agent.Options{
		ACPRegistryOverrides: map[string]string{"pi-flash-next-responses-gate": rawCommand},
		Profile:              agentcfgProfile("no-mistakes-flash-next-responses/Qwen/Qwen3.8-Flash-Next-FP8"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	attempts := 0
	schema := json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string","enum":["through-responses"]}},"required":["summary"],"additionalProperties":false}`)
	res, err := a.Run(ctx, agent.RunOpts{
		Prompt:     "Return the fixture result",
		CWD:        dir,
		Env:        []string{"HOME=" + dir, "PI_CODING_AGENT_DIR=" + agentDir},
		JSONSchema: schema,
		OnChunk:    func(text string) { t.Logf("ACP assistant chunk: %q", text) },
		OnAttempt:  func(agent.Attempt) { attempts++ },
	})
	if err != nil {
		events, _ := os.ReadFile(acpxEvents)
		t.Fatalf("strict Responses process integration: %v (provider requests=%d)\n%s", err, requests.Load(), events)
	}
	if res.Text != `{"summary":"through-responses"}` {
		t.Fatalf("result = %q", res.Text)
	}
	firstBody := <-requestBody
	body := <-requestBody
	if bytes.Equal(firstBody, body) {
		t.Fatal("repair provider turn repeated the original payload instead of continuing the same session with a nudge")
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode provider request: %v\n%s", err, body)
	}
	tools, ok := payload["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %#v, want one", payload["tools"])
	}
	tool := tools[0].(map[string]any)
	if payload["tool_choice"] != "required" || payload["parallel_tool_calls"] != false ||
		payload["model"] != "Qwen/Qwen3.8-Flash-Next-FP8" || tool["name"] != "structured_output" || tool["strict"] != true {
		t.Fatalf("provider request lost strict gate fields: %s", body)
	}
	reasoning := payload["reasoning"].(map[string]any)
	if reasoning["effort"] != "xhigh" || payload["temperature"] != float64(1) ||
		payload["top_p"] != 0.95 || payload["seed"] != float64(424242) {
		t.Fatalf("provider request lost model/effort/sampling pins: %s", body)
	}
	starts, err := os.ReadFile(piStarts)
	if err != nil {
		t.Fatal(err)
	}
	events, err := os.ReadFile(acpxEvents)
	if err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 2 || string(starts) != "x" || attempts != 1 || bytes.Count(events, []byte(`"method":"session/prompt"`)) != 1 {
		t.Fatalf("provider requests=%d Pi processes=%q no-mistakes attempts=%d ACP prompts=%d; want two turns in one process/attempt/session", requests.Load(), starts, attempts, bytes.Count(events, []byte(`"method":"session/prompt"`)))
	}
	sum := sha256.Sum256(schema)
	t.Logf("strict Responses repaired request sha256=%x schema sha256=%x", sha256.Sum256(body), sum)
}

func agentcfgProfile(model string) agentcfg.Profile {
	return agentcfg.Profile{Model: model}
}

func TestPiStructuredOutputExtensionLoadsInPiRPC(t *testing.T) {
	integrationPath, err := filepath.Abs(filepath.Join("..", "..", "integrations", "pi"))
	if err != nil {
		t.Fatal(err)
	}
	piPath := filepath.Join(integrationPath, "node_modules", ".bin", "pi")
	if info, statErr := os.Stat(piPath); statErr != nil || info.IsDir() {
		t.Fatalf("pinned executable %s is missing; run npm ci --prefix integrations/pi --ignore-scripts", piPath)
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
	cmd.Env = append(os.Environ(),
		"HOME="+dir,
		"PI_CODING_AGENT_DIR="+agentDir,
		"NO_MISTAKES_GATE=1",
		"NO_MISTAKES_PI_STRUCTURED_OUTPUT=1",
		"NO_MISTAKES_JSON_SCHEMA_FILE="+schemaPath,
		"NO_MISTAKES_JSON_SCHEMA_SHA256="+hex.EncodeToString(sum[:]),
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

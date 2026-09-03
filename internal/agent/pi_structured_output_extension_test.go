//go:build linux

package agent

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"gopkg.in/yaml.v3"
)

func piStructuredOutputIntegrationPath(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", "..", "integrations", "pi"))
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPiStrictResponsesFixturesPinTheNarrowRoute(t *testing.T) {
	integrationPath := piStructuredOutputIntegrationPath(t)
	modelsBytes, err := os.ReadFile(filepath.Join(integrationPath, "fixtures", "flash-next-responses-models.json"))
	if err != nil {
		t.Fatal(err)
	}
	var models struct {
		Providers map[string]struct {
			API    string `json:"api"`
			Models []struct {
				ID               string         `json:"id"`
				Reasoning        bool           `json:"reasoning"`
				ThinkingLevelMap map[string]any `json:"thinkingLevelMap"`
				SamplingParams   map[string]any `json:"samplingParams"`
				Compat           map[string]any `json:"compat"`
			} `json:"models"`
			Compat map[string]any `json:"compat"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(modelsBytes, &models); err != nil {
		t.Fatal(err)
	}
	provider, ok := models.Providers["no-mistakes-flash-next-responses"]
	if !ok || len(models.Providers) != 1 || provider.API != "openai-responses" || len(provider.Models) != 1 {
		t.Fatalf("fixture provider shape = %+v", models.Providers)
	}
	model := provider.Models[0]
	if model.ID != "Qwen/Qwen3.8-Flash-Next-FP8" || !model.Reasoning ||
		model.ThinkingLevelMap["xhigh"] != "xhigh" || provider.Compat["supportsStrictMode"] != true ||
		model.SamplingParams["temperature"] != float64(1) || model.SamplingParams["top_p"] != 0.95 ||
		model.SamplingParams["seed"] != float64(424242) {
		t.Fatalf("fixture model contract = %+v provider compat=%+v", model, provider.Compat)
	}

	routeBytes, err := os.ReadFile(filepath.Join(integrationPath, "fixtures", "flash-next-responses-route.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var route struct {
		Agent       types.AgentName `yaml:"agent"`
		AgentConfig map[string]struct {
			Model string `yaml:"model"`
		} `yaml:"agent_config"`
		ACPRegistryOverrides map[string]string `yaml:"acp_registry_overrides"`
	}
	if err := yaml.Unmarshal(routeBytes, &route); err != nil {
		t.Fatal(err)
	}
	if route.Agent != types.AgentName("acp:pi-flash-next-responses-gate") || len(route.AgentConfig) != 1 || len(route.ACPRegistryOverrides) != 1 {
		t.Fatalf("route fixture shape = %+v", route)
	}
	profile, ok := route.AgentConfig["acp:pi-flash-next-responses-gate"]
	if !ok || profile.Model != "no-mistakes-flash-next-responses/Qwen/Qwen3.8-Flash-Next-FP8" {
		t.Fatalf("route profile = %+v", route.AgentConfig)
	}
	if got := route.ACPRegistryOverrides["pi-flash-next-responses-gate"]; got != "env NO_MISTAKES_PI_STRUCTURED_OUTPUT=1 NO_MISTAKES_ACPX_ATTEMPTS=1 PI_ACP_PI_COMMAND=/absolute/path/to/no-mistakes/integrations/pi/bin/pi-no-mistakes-flash-next-responses-acp /absolute/path/to/no-mistakes/integrations/pi/node_modules/.bin/pi-acp" {
		t.Fatalf("route override = %q", got)
	}
}

func TestPiStructuredOutputExtensionContract(t *testing.T) {
	integrationPath := piStructuredOutputIntegrationPath(t)
	dependencyPath := filepath.Join(integrationPath, "node_modules", "typebox", "package.json")
	if info, err := os.Stat(dependencyPath); err != nil || info.IsDir() {
		if os.Getenv("NM_REQUIRE_PI_ACP_INTEGRATION") == "1" {
			t.Fatalf("pinned dependency %s is missing; run npm ci --prefix integrations/pi --ignore-scripts", dependencyPath)
		}
		t.Skipf("pinned dependency %s is required; run npm ci --prefix integrations/pi --ignore-scripts", dependencyPath)
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required to run the repository-owned Pi extension contract tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "--test", filepath.Join(integrationPath, "test", "structured-output.test.mjs"))
	shellenv.ConfigureShellCommand(cmd)
	if output, err := shellenv.CombinedOutputShellCommand(cmd); err != nil {
		t.Fatalf("Pi extension contract tests: %v\n%s", err, output)
	}
}

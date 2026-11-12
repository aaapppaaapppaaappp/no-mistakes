package steps

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/forgecontext"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/scm/plugin"
	"github.com/kunchenguid/no-mistakes/internal/scm/plugin/fakeplugin"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestAgentBaseMerge_UsesLiveBaseAcrossProvidersAndFixPaths(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("TEA_CONFIG_DIR", configDir)
	t.Setenv("XDG_CONFIG_HOME", configDir)
	if err := os.WriteFile(filepath.Join(configDir, "hosts.yml"), []byte("hosts:\n  https://gitea.example: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		provider    scm.Provider
		remote, url string
		payload     string
	}{
		{scm.ProviderGitHub, "https://github.com/test/repo", "https://github.com/test/repo/pull/42",
			`[{"number":42,"url":"https://github.com/test/repo/pull/42","baseRefName":"develop"}]`},
		{scm.ProviderGitLab, "https://gitlab.com/test/repo", "https://gitlab.com/test/repo/-/merge_requests/42",
			`[{"iid":42,"web_url":"https://gitlab.com/test/repo/-/merge_requests/42","target_branch":"develop","state":"opened"}]`},
		{scm.ProviderGitea, "https://gitea.example/test/repo", "https://gitea.example/test/repo/pulls/42",
			`[{"number":42,"url":"https://gitea.example/test/repo/pulls/42","state":"open","head":"feature","base":"develop"}]`},
		{scm.ProviderForgejo, "https://forge.example/test/repo", "https://forge.example/test/repo/pulls/42",
			`{"found":true,"pull_request":{"number":42,"url":"https://forge.example/test/repo/pulls/42","api_url":"https://forge.example/api/v1/repos/test/repo/pulls/42","state":"open","title":"Feature","head":"feature","base":"develop","head_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"search_info":{"complete":true,"pages":1,"fetched":1,"total":1}}`},
		{scm.ProviderAzureDevOps, "https://dev.azure.com/test/project/_git/repo", "https://dev.azure.com/test/project/_git/repo/pullrequest/42",
			`[{"pullRequestId":42,"status":"active","targetRefName":"refs/heads/develop","sourceRefName":"refs/heads/feature","repository":{"name":"repo","project":{"name":"project"}}}]`},
		{scm.ProviderBitbucket, "https://bitbucket.org/test/repo", "https://bitbucket.org/test/repo/pull-requests/42",
			`{"values":[{"id":42,"state":"OPEN","destination":{"branch":{"name":"develop"}}}]}`},
	} {
		for _, discovery := range []string{"fresh", "persisted", "sibling"} {
			for _, route := range []string{"shared", "ci", "retained"} {
				t.Run(fmt.Sprintf("%s/%s/%s", tc.provider, discovery, route), func(t *testing.T) {
					sctx, dir, head := agentCommitContext(t, nil)
					gitCmd(t, dir, "checkout", "-b", "develop", "main")
					agentCommit(t, dir, "develop.txt", "only on develop")
					tip := gitCmd(t, dir, "rev-parse", "HEAD")
					gitCmd(t, dir, "checkout", "--detach", head)
					gitCmd(t, dir, "merge", "--no-ff", "-m", "agent merge", "develop")
					wantTree := gitCmd(t, dir, "rev-parse", "HEAD^{tree}")
					sctx.Config.PR.BaseBranch = "main"
					sctx.Config.CI.RevalidateRepairs = true
					sctx.Repo.UpstreamURL = tc.remote
					sctx.ForgeContext = &forgecontext.Context{Provider: tc.provider}
					if discovery != "fresh" {
						sctx.Run.PRURL = strptr(tc.url)
					}
					listedPayload := tc.payload
					if discovery == "sibling" {
						listedPayload = strings.ReplaceAll(strings.ReplaceAll(tc.payload, "42", "99"), "develop", "main")
					}
					if tc.provider == scm.ProviderBitbucket {
						var listed struct {
							Values []json.RawMessage `json:"values"`
						}
						if err := json.Unmarshal([]byte(tc.payload), &listed); err != nil {
							t.Fatal(err)
						}
						server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							if r.Method == http.MethodGet && r.URL.Path == "/2.0/repositories/test/repo/pullrequests/42" {
								_, _ = w.Write(listed.Values[0])
								return
							}
							if r.Method != http.MethodGet || r.URL.Path != "/2.0/repositories/test/repo/pullrequests" ||
								!strings.Contains(r.URL.Query().Get("q"), `source.branch.name="feature"`) ||
								strings.Contains(r.URL.Query().Get("q"), "destination.branch") {
								http.Error(w, "expected branch-only PR discovery", http.StatusBadRequest)
								return
							}
							w.Header().Set("Content-Type", "application/json")
							_, _ = w.Write([]byte(listedPayload))
						}))
						t.Cleanup(server.Close)
						sctx.Env = []string{"NO_MISTAKES_BITBUCKET_EMAIL=test@example.com", "NO_MISTAKES_BITBUCKET_API_TOKEN=test", "NO_MISTAKES_BITBUCKET_API_BASE_URL=" + server.URL}
					} else {
						binDir := fakeCLIBinDir(t)
						linkTestBinary(t, binDir, tc.provider.CLIName())
						sctx.Env = fakeCLIEnv(binDir, map[string]string{
							"FAKE_CLI_MODE": "pr-discovery", "FAKE_CLI_PR_LIST_JSON": listedPayload,
							"FAKE_CLI_PR_DETAIL_JSON": tc.payload, "FAKE_CLI_PR_DETAIL_NUMBER": "42", "FAKE_CLI_PR_BASE": "develop",
							"FORGEJO_BASE_URL": "", "FORGEJO_SSH_DOMAIN": "", "FORGEJO_TOKEN": "test",
							"FAKE_CLI_STATUS_JSON": `{"host":{"url":"https://forge.example","api_url":"https://forge.example/api/v1"},"auth":{"configured":true,"authenticated":true,"source":"FORGEJO_TOKEN"},"capabilities":{"pull_requests":true,"probe":{"source":"swagger","complete":true}}}`,
						})
					}
					var err error
					switch route {
					case "shared":
						_, err = commitAgentFixesWithResult(sctx, types.StepTest, "merge base", "fallback", nil)
					case "ci":
						_, err = (&CIStep{}).commitRepair(sctx, "merge base", nil)
					case "retained":
						_, err = (&CIStep{}).retryProtectedPathRepair(sctx)
					}
					if err != nil {
						t.Fatal(err)
					}
					if parents := commitParents(t, dir, "HEAD"); len(parents) != 2 || parents[0] != head || parents[1] != tip {
						t.Fatalf("parents=%v, want [%s %s]", parents, head, tip)
					}
					if tree := gitCmd(t, dir, "rev-parse", "HEAD^{tree}"); tree != wantTree {
						t.Fatalf("tree=%s, want %s", tree, wantTree)
					}
					if sctx.Run.HeadSHA != gitCmd(t, dir, "rev-parse", "HEAD") {
						t.Fatal("conformed merge was not recorded")
					}
				})
			}
		}
	}
}

func TestAgentBaseMerge_UnreadablePersistedGitLabBaseLeavesCommitsUntouched(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, base, readError, want string
	}{
		{name: "missing base", want: "base branch is empty"},
		{name: "unreadable detail", base: "develop", readError: "provider unavailable", want: "provider unavailable"},
	} {
		for _, route := range []string{"shared", "ci", "retained"} {
			t.Run(tc.name+"/"+route, func(t *testing.T) {
				t.Parallel()
				sctx, dir, head := agentCommitContext(t, nil)
				advanceMain(t, dir, "upstream.txt", "landed on main\n")
				gitCmd(t, dir, "merge", "--no-ff", "-m", "agent merge", "main")
				merged := gitCmd(t, dir, "rev-parse", "HEAD")
				sctx.Config.CI.RevalidateRepairs = true
				sctx.Repo.UpstreamURL = "https://gitlab.com/test/repo"
				sctx.ForgeContext = &forgecontext.Context{Provider: scm.ProviderGitLab}
				sctx.Run.PRURL = strptr("https://gitlab.com/test/repo/-/merge_requests/42")
				binDir := fakeCLIBinDir(t)
				linkTestBinary(t, binDir, "glab")
				sctx.Env = fakeCLIEnv(binDir, map[string]string{
					"FAKE_CLI_MODE": "pr-discovery", "FAKE_CLI_PR_DETAIL_ERR": tc.readError,
					"FAKE_CLI_PR_DETAIL_NUMBER": "42",
					"FAKE_CLI_PR_LIST_JSON":     `[{"iid":99,"web_url":"https://gitlab.com/test/repo/-/merge_requests/99","target_branch":"main","state":"opened"}]`,
					"FAKE_CLI_PR_DETAIL_JSON":   `{"iid":42,"web_url":"https://gitlab.com/test/repo/-/merge_requests/42","state":"opened","target_branch":"` + tc.base + `"}`,
				})
				var err error
				switch route {
				case "shared":
					_, err = commitAgentFixesWithResult(sctx, types.StepTest, "merge base", "fallback", nil)
				case "ci":
					_, err = (&CIStep{}).commitRepair(sctx, "merge base", nil)
				case "retained":
					_, err = (&CIStep{}).retryProtectedPathRepair(sctx)
				}
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("err=%v, want %s", err, tc.want)
				}
				if gitCmd(t, dir, "rev-parse", "HEAD") != merged || sctx.Run.HeadSHA != head {
					t.Fatal("unverified PR base changed the merge or recorded head")
				}
			})
		}
	}
}

func TestAgentBaseMerge_ProviderPluginHandshakePrecedesDiscovery(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		state fakeplugin.State
		valid bool
	}{
		{name: "unsupported protocol", state: fakeplugin.State{ProtocolVersion: 99}},
		{name: "unauthenticated", state: fakeplugin.State{Unauthenticated: true}},
		{name: "invalid body limit", state: fakeplugin.State{MaxPRBodyChars: -1}},
		{name: "valid", valid: true},
	} {
		for _, route := range []string{"shared", "ci", "retained"} {
			t.Run(tc.name+"/"+route, func(t *testing.T) {
				t.Parallel()
				sctx, dir, head := agentCommitContext(t, nil)
				tip := advanceMain(t, dir, "upstream.txt", "landed on main\n")
				gitCmd(t, dir, "merge", "--no-ff", "-m", "agent merge", "main")
				merged := gitCmd(t, dir, "rev-parse", "HEAD")
				wantTree := gitCmd(t, dir, "rev-parse", "HEAD^{tree}")
				sctx.Config.CI.RevalidateRepairs = true
				state := tc.state
				state.PRs = []fakeplugin.PR{{Number: "42", URL: "https://git.example.com/team/repo/pulls/42", HeadBranch: "feature", BaseBranch: "main", HeadSHA: head, State: "open", Title: "Feature"}}
				_, logPath := fakeProviderPlugin(t, sctx, state)
				var err error
				switch route {
				case "shared":
					_, err = commitAgentFixesWithResult(sctx, types.StepTest, "merge base", "fallback", nil)
				case "ci":
					_, err = (&CIStep{}).commitRepair(sctx, "merge base", nil)
				case "retained":
					_, err = (&CIStep{}).retryProtectedPathRepair(sctx)
				}
				ops := pluginOperations(t, logPath)
				if len(ops) == 0 || ops[0] != "status" {
					t.Fatalf("plugin operations=%v, want the handshake first", ops)
				}
				if !tc.valid {
					if err == nil || len(ops) != 1 {
						t.Fatalf("err=%v operations=%v, want refusal before PR discovery", err, ops)
					}
					if !tc.state.Unauthenticated && !errors.Is(err, plugin.ErrProtocol) {
						t.Fatalf("err=%v, want a propagated protocol error", err)
					}
					if gitCmd(t, dir, "rev-parse", "HEAD") != merged || sctx.Run.HeadSHA != head {
						t.Fatal("failed handshake changed the merge or recorded head")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if len(ops) != 2 || ops[1] != "pr find" {
					t.Fatalf("plugin operations=%v, want handshake then discovery", ops)
				}
				if parents := commitParents(t, dir, "HEAD"); len(parents) != 2 || parents[0] != head || parents[1] != tip {
					t.Fatalf("parents=%v, want [%s %s]", parents, head, tip)
				}
				if gitCmd(t, dir, "rev-parse", "HEAD^{tree}") != wantTree || sctx.Run.HeadSHA != gitCmd(t, dir, "rev-parse", "HEAD") {
					t.Fatal("successful handshake did not preserve and record the merge")
				}
			})
		}
	}
}

func TestAgentBaseMerge_UnverifiedPRLeavesAgentCommitsUntouched(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, payload, readError, want string
		fresh                          bool
		state                          string
	}{
		{"closed PR with sibling", `[{"number":43,"url":"https://github.com/test/repo/pull/43","baseRefName":"main"}]`, "", "does not match recorded PR", false, "CLOSED"},
		{"missing recorded PR", `[]`, "", "does not match recorded PR", false, "CLOSED"},
		{"missing base", `[{"number":42,"url":"https://github.com/test/repo/pull/42"}]`, "", "base branch is empty", false, "OPEN"},
		{"unreadable discovery", `[]`, "provider unavailable", "provider unavailable", false, "OPEN"},
		{"fresh discovery error", `[]`, "provider unavailable", "provider unavailable", true, "OPEN"},
		{"fresh missing base", `[{"number":42,"url":"https://github.com/test/repo/pull/42"}]`, "", "base branch is empty", true, "OPEN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sctx, dir, head := agentCommitContext(t, nil)
			advanceMain(t, dir, "upstream.txt", "landed on main\n")
			gitCmd(t, dir, "merge", "--no-ff", "-m", "agent merge", "main")
			merged := gitCmd(t, dir, "rev-parse", "HEAD")
			if !tc.fresh {
				sctx.Run.PRURL = strptr("https://github.com/test/repo/pull/42")
			}
			binDir := fakeCLIBinDir(t)
			linkTestBinary(t, binDir, "gh")
			sctx.Env = fakeCLIEnv(binDir, map[string]string{"FAKE_CLI_MODE": "pr-discovery", "FAKE_CLI_PR_LIST_JSON": tc.payload, "FAKE_CLI_PR_LIST_ERR": tc.readError, "FAKE_CLI_PR_STATE": tc.state})
			_, err := commitAgentFixesWithResult(sctx, types.StepReview, "merge base", "fallback", nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v, want %s", err, tc.want)
			}
			if gitCmd(t, dir, "rev-parse", "HEAD") != merged || sctx.Run.HeadSHA != head {
				t.Fatal("refused merge changed the agent's commits or recorded head")
			}
		})
	}
}

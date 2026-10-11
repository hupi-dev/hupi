package mcpserver

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"testing"

	"hupi/internal/identity"
)

func TestListTools_WritesOnByDefaultAndKillSwitch(t *testing.T) {
	names := func(cfg Config) (string, map[string]bool, string) {
		cs := connect(t, quietServer(&fakeBackend{}, nil, cfg).Handler(), "")
		res, err := cs.ListTools(context.Background(), nil)
		if err != nil {
			t.Fatalf("ListTools: %v", err)
		}
		var out []string
		readOnly := map[string]bool{}
		for _, tool := range res.Tools {
			out = append(out, tool.Name)
			readOnly[tool.Name] = tool.Annotations != nil && tool.Annotations.ReadOnlyHint
			if tool.Name == "remember" || tool.Name == "log_exchange" {
				if a := tool.Annotations; a == nil || a.DestructiveHint == nil || *a.DestructiveHint {
					t.Errorf("%s: want destructiveHint=false", tool.Name)
				}
			}
		}
		sort.Strings(out)
		return strings.Join(out, ","), readOnly, cs.InitializeResult().Instructions
	}

	got, ro, instr := names(Config{})
	if want := "get_entity,list_entities,log_exchange,recent_memory,remember,search_memory"; got != want {
		t.Errorf("tools = %s, want %s", got, want)
	}
	if ro["remember"] || ro["log_exchange"] || !ro["search_memory"] {
		t.Errorf("readOnly hints = %v", ro)
	}
	if !strings.Contains(instr, "Only call remember or log_exchange when the user asks") {
		t.Errorf("instructions missing write guidance: %q", instr)
	}

	got, _, instr = names(Config{DisableWrites: true})
	if want := "get_entity,list_entities,recent_memory,search_memory"; got != want {
		t.Errorf("with writes disabled: tools = %s, want %s", got, want)
	}
	if strings.Contains(instr, "remember") {
		t.Errorf("read-only instructions mention write tools: %q", instr)
	}
}

func TestConfigFromEnv_WritesKillSwitch(t *testing.T) {
	if ConfigFromEnv().DisableWrites {
		t.Errorf("writes disabled with HUPI_MCP_ENABLE_WRITES unset")
	}
	t.Setenv("HUPI_MCP_ENABLE_WRITES", "false")
	if !ConfigFromEnv().DisableWrites {
		t.Errorf("HUPI_MCP_ENABLE_WRITES=false didn't disable writes")
	}
}

func TestRemember_StoresTaggedHighImportanceEpisode(t *testing.T) {
	fb := &fakeBackend{}
	cs := connect(t, quietServer(fb, fakeAuth{"ka": "user:alice"}, Config{}).Handler(), "ka")

	var out WriteOutput
	res := call(t, cs, "remember", map[string]any{"fact": "  The billing service deploys from release/* branches only. "}, &out)
	if res.IsError {
		t.Fatalf("remember failed: %s", resultText(res))
	}
	if len(fb.captured) != 1 {
		t.Fatalf("captured %d episodes, want 1", len(fb.captured))
	}
	c := fb.captured[0]
	ep := c.ep
	if c.scope != (identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:alice"}) || ep.ActorUserID != "user:alice" {
		t.Errorf("scope %+v actor %q", c.scope, ep.ActorUserID)
	}
	if ep.Type != "interaction" || ep.Importance != rememberImportance || ep.ProviderVendor != "mcp" || ep.ProviderModel != "remember" {
		t.Errorf("episode = %+v", ep)
	}
	if ep.InputText != "Please remember: The billing service deploys from release/* branches only." || ep.OutputText != "" {
		t.Errorf("texts = %q / %q", ep.InputText, ep.OutputText)
	}
	if strings.Join(ep.ContextTags, ",") != "mcp,remember" {
		t.Errorf("tags = %v", ep.ContextTags)
	}
	if out.ID != ep.ID || out.Workspace != "private" || out.Note == "" {
		t.Errorf("out = %+v", out)
	}
}

func TestLogExchange_TagsAndHeuristicImportance(t *testing.T) {
	fb := &fakeBackend{}
	cs := connect(t, quietServer(fb, nil, Config{}).Handler(), "")
	var out WriteOutput
	res := call(t, cs, "log_exchange", map[string]any{
		"user_text":      "Should we pin Postgres to 17?",
		"assistant_text": "Yes, we decided to pin 17 going forward.",
		"tags":           []string{"Wombat", "mcp", "wombat", " infra "},
	}, &out)
	if res.IsError {
		t.Fatalf("log_exchange failed: %s", resultText(res))
	}
	ep := fb.captured[0].ep
	if got := strings.Join(ep.ContextTags, ","); got != "mcp,exchange,wombat,infra" {
		t.Errorf("tags = %s", got)
	}
	if ep.ProviderModel != "log_exchange" || ep.Importance == rememberImportance || ep.Importance <= 0 {
		t.Errorf("episode = %+v", ep)
	}
	if ep.InputText != "Should we pin Postgres to 17?" || ep.OutputText != "Yes, we decided to pin 17 going forward." {
		t.Errorf("texts = %q / %q", ep.InputText, ep.OutputText)
	}
}

func TestWriteTools_RejectSecretsAndBadInput(t *testing.T) {
	fb := &fakeBackend{}
	cs := connect(t, quietServer(fb, nil, Config{}).Handler(), "")
	ok := "fine text"
	cases := []struct {
		tool string
		args map[string]any
		want string
	}{
		{"remember", map[string]any{"fact": "my key is hupi_sk_abcdefghijklmnopqrstuv"}, "HUPI API key"},
		{"remember", map[string]any{"fact": "OPENAI_API_KEY=sk-proj-FAKEFAKEFAKEFAKEFAKEFAKE"}, "OpenAI/Anthropic API key"},
		{"remember", map[string]any{"fact": "aws AKIAIOSFODNN7EXAMPLE"}, "AWS access key id"},
		{"remember", map[string]any{"fact": "ghp_" + strings.Repeat("a", 36)}, "GitHub token"},
		{"remember", map[string]any{"fact": "xoxb-1234567890-abcdef"}, "Slack token"},
		{"log_exchange", map[string]any{"user_text": ok, "assistant_text": "-----BEGIN OPENSSH PRIVATE KEY-----\nabc"}, "private key"},
		{"log_exchange", map[string]any{"user_text": "token eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U", "assistant_text": ok}, "JSON Web Token"},
		{"remember", map[string]any{"fact": " "}, "fact is required"},
		{"remember", map[string]any{"fact": strings.Repeat("x", maxFactChars+1)}, "too long"},
		{"log_exchange", map[string]any{"user_text": ok, "assistant_text": ""}, "assistant_text is required"},
		{"log_exchange", map[string]any{"user_text": ok, "assistant_text": ok, "tags": []string{"has space"}}, "invalid tag"},
		{"log_exchange", map[string]any{"user_text": ok, "assistant_text": ok, "tags": []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}}, "at most 8 tags"},
	}
	for _, tc := range cases {
		res := call(t, cs, tc.tool, tc.args, nil)
		text := resultText(res)
		if !res.IsError || !strings.Contains(text, tc.want) {
			t.Errorf("%s %v: IsError=%v text=%q, want %q", tc.tool, tc.args, res.IsError, text, tc.want)
		}
		if strings.Contains(text, "FAKEFAKE") || strings.Contains(text, "hupi_sk_abc") {
			t.Errorf("error echoes the secret: %q", text)
		}
	}
	if len(fb.captured) != 0 {
		t.Errorf("rejected writes were stored: %+v", fb.captured)
	}
}

func TestFindSecret_NoFalsePositivesOnOrdinaryText(t *testing.T) {
	for _, s := range []string{
		"use the sk-learn style API for the classifier",
		"ask-me-anything sessions are on Fridays",
		"the AKIA prefix marks AWS long-term keys",
		"Postgres 17 migration owned by Priya Raman",
		"see https://github.com/hupi-dev/hupi/pull/144",
		"eyJ is how a base64 JSON object starts",
	} {
		if kind := findSecret(s); kind != "" {
			t.Errorf("findSecret(%q) = %q, want none", s, kind)
		}
	}
}

func TestWriteTools_TeamWorkspaceKeepsIndividualActor(t *testing.T) {
	fb := &fakeBackend{}
	s := quietServer(fb, nil, Config{})
	team := identity.Scope{Kind: identity.ScopeKindShared, Owner: "team:core"}
	h := s.HandlerFor(func(r *http.Request) (identity.Scope, identity.Scope, int, error) {
		return identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:alice"}, team, 0, nil
	})
	var out WriteOutput
	call(t, connect(t, h, ""), "remember", map[string]any{"fact": "Standup moved to 9:30."}, &out)
	if c := fb.captured[0]; c.scope != team || c.ep.ActorUserID != "user:alice" || out.Workspace != "team:core" {
		t.Errorf("scope %+v actor %q workspace %q", c.scope, c.ep.ActorUserID, out.Workspace)
	}
}

func TestWriteTools_BackendErrorIsGeneric(t *testing.T) {
	fb := &fakeBackend{err: errors.New("pq: permission denied for table episodes")}
	res := call(t, connect(t, quietServer(fb, nil, Config{}).Handler(), ""), "remember", map[string]any{"fact": "x"}, nil)
	if text := resultText(res); !res.IsError || strings.Contains(text, "permission denied") {
		t.Errorf("IsError=%v text=%q", res.IsError, text)
	}
}

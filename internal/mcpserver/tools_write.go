package mcpserver

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"hupi/internal/gateway"
)

const (
	maxFactChars     = 2000
	maxExchangeChars = 32000
	maxTags          = 8
	maxTagChars      = 40

	// rememberImportance is above consolidation.EpisodeEmbedImportanceThreshold
	// (0.6), so an explicit "remember this" is always embedded at the next
	// consolidation run, not just keyword-searchable.
	rememberImportance = 0.8

	// mcpVendor is the provider_vendor recorded on MCP-written episodes;
	// provider_model holds the tool name. Together with the "mcp" context
	// tag this marks where the episode came from.
	mcpVendor = "mcp"

	consolidationNote = "Searchable now by keyword; summarized and fact-checked at the next nightly consolidation."
)

var tagPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.:-]*$`)

func (s *Server) registerWriteTools() {
	notDestructive := false
	closed := false
	writeAnnotations := func(title string) *mcp.ToolAnnotations {
		return &mcp.ToolAnnotations{Title: title, DestructiveHint: &notDestructive, OpenWorldHint: &closed}
	}

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:  "remember",
		Title: "Remember",
		Description: "Save one durable fact to the user's long-term HUPI memory, shared with all their AI tools. " +
			"Only call this when the user asks you to remember something, or states a lasting preference, decision or fact about themselves or their work that they clearly want kept. " +
			"Write the fact as one self-contained sentence in the user's terms (e.g. 'The billing service deploys from the release/* branches only'). " +
			"Never store secrets, credentials, tokens or passwords, and don't store transient task state.",
		Annotations: writeAnnotations("Remember"),
	}, instrument("remember", s.remember))

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:  "log_exchange",
		Title: "Log exchange",
		Description: "Record one user/assistant exchange from this session in the user's HUPI memory, so other AI tools can recall it later. " +
			"Only call this when the user asks you to record or save the conversation (or a part of it). Pass the user's message and your reply as they were, not a summary. " +
			"Never include secrets, credentials or tokens.",
		Annotations: writeAnnotations("Log exchange"),
	}, instrument("log_exchange", s.logExchange))
}

// normalizeTags lowercases, validates and de-duplicates caller tags, and
// drops any that collide with the reserved provenance tags.
func normalizeTags(tags []string, reserved ...string) ([]string, error) {
	if len(tags) > maxTags {
		return nil, fmt.Errorf("at most %d tags", maxTags)
	}
	seen := map[string]bool{}
	for _, r := range reserved {
		seen[r] = true
	}
	var out []string
	for _, t := range tags {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" || seen[t] {
			continue
		}
		if len(t) > maxTagChars || !tagPattern.MatchString(t) {
			return nil, fmt.Errorf("invalid tag %q: use lowercase letters, digits and _ . : - (max %d characters)", t, maxTagChars)
		}
		seen[t] = true
		out = append(out, t)
	}
	return out, nil
}

// capture writes one MCP-originated interaction episode.
func (s *Server) capture(ctx context.Context, rs requestScope, tool, input, output string, importance float64, tags []string) (string, error) {
	now := s.now()
	ep := gateway.NewInteractionEpisode(gateway.NewEpisodeID(now), now, mcpVendor, tool, input, output)
	if importance > 0 {
		ep.Importance = importance
	}
	ep.ContextTags = tags
	ep.ActorUserID = rs.actingUser.Owner
	if err := s.backend.Capture(ctx, rs.workspace, ep); err != nil {
		return "", s.backendError(tool, err)
	}
	return ep.ID, nil
}

func (s *Server) rejectSecret(tool, kind string) error {
	s.log.Info("mcp: write rejected: looks like a credential", "tool", tool, "kind", kind)
	return fmt.Errorf("not saved: the text looks like it contains a credential (%s). Remove it and try again; HUPI memory must never hold secrets", kind)
}

// --- remember --------------------------------------------------------------

type RememberInput struct {
	Fact string `json:"fact" jsonschema:"one self-contained sentence stating the fact to keep"`
}

type WriteOutput struct {
	Workspace string `json:"workspace"`
	ID        string `json:"id" jsonschema:"id of the stored episode"`
	Note      string `json:"note"`
}

func (s *Server) remember(ctx context.Context, _ *mcp.CallToolRequest, in RememberInput) (*mcp.CallToolResult, WriteOutput, error) {
	rs, err := scopeFrom(ctx)
	if err != nil {
		return nil, WriteOutput{}, err
	}
	fact, err := requireText("fact", in.Fact, maxFactChars)
	if err != nil {
		return nil, WriteOutput{}, err
	}
	if kind := findSecret(fact); kind != "" {
		return nil, WriteOutput{}, s.rejectSecret("remember", kind)
	}
	// Stored as the user's own statement: consolidation grounds facts in
	// what the user said, and the user asked for this to be kept.
	id, err := s.capture(ctx, rs, "remember", "Please remember: "+fact, "", rememberImportance, []string{"mcp", "remember"})
	if err != nil {
		return nil, WriteOutput{}, err
	}
	return nil, WriteOutput{Workspace: workspaceLabel(rs.workspace), ID: id, Note: consolidationNote}, nil
}

// --- log_exchange ----------------------------------------------------------

type LogExchangeInput struct {
	UserText      string   `json:"user_text" jsonschema:"the user's message, verbatim"`
	AssistantText string   `json:"assistant_text" jsonschema:"your reply, verbatim"`
	Tags          []string `json:"tags,omitempty" jsonschema:"optional lowercase labels, e.g. the project name (max 8)"`
}

func (s *Server) logExchange(ctx context.Context, _ *mcp.CallToolRequest, in LogExchangeInput) (*mcp.CallToolResult, WriteOutput, error) {
	rs, err := scopeFrom(ctx)
	if err != nil {
		return nil, WriteOutput{}, err
	}
	user, err := requireText("user_text", in.UserText, maxExchangeChars)
	if err != nil {
		return nil, WriteOutput{}, err
	}
	assistant, err := requireText("assistant_text", in.AssistantText, maxExchangeChars)
	if err != nil {
		return nil, WriteOutput{}, err
	}
	tags, err := normalizeTags(in.Tags, "mcp", "exchange")
	if err != nil {
		return nil, WriteOutput{}, err
	}
	if kind := findSecret(user, assistant); kind != "" {
		return nil, WriteOutput{}, s.rejectSecret("log_exchange", kind)
	}
	id, err := s.capture(ctx, rs, "log_exchange", user, assistant, 0, append([]string{"mcp", "exchange"}, tags...))
	if err != nil {
		return nil, WriteOutput{}, err
	}
	return nil, WriteOutput{Workspace: workspaceLabel(rs.workspace), ID: id, Note: consolidationNote}, nil
}

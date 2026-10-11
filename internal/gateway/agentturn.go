package gateway

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sync"
	"time"

	"hupi/internal/identity"
	"hupi/internal/provider"
)

// An agent turn is one user question answered over several requests: the
// model asks for tools, the client runs them and sends the results back,
// and so on until a final text answer. HUPI never runs tools itself; this
// file holds what the gateway needs to remember such a turn correctly:
//
//   - retrieval (and the aggregation pass) run once per turn, not once per
//     request, via a short-lived per-turn cache, so every step sees the
//     same injected memory;
//   - the turn is captured once, with the user's question and the final
//     answer, tagged "agent" plus "tool:<name>" for the tools it used.
//     Tool arguments and results are never stored, and a turn that ends
//     without a final answer (cancelled, failed) stores nothing.
//
// Plain chat (no tools offered, no tool messages) is untouched.

const (
	maxToolsPerRequest = 128
	maxToolTags        = 16
	agentTurnCacheSize = 256
	agentTurnCacheTTL  = 15 * time.Minute
)

var toolNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// lastUserIndex is the index of the last role "user" message, or -1.
func lastUserIndex(msgs []provider.Message) int {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == provider.RoleUser {
			return i
		}
	}
	return -1
}

// isContinuation reports whether this request continues an agent turn:
// after the last user message there's an assistant tool call or a tool
// result.
func isContinuation(msgs []provider.Message) bool {
	for _, m := range msgs[lastUserIndex(msgs)+1:] {
		if m.Role == provider.RoleTool || (m.Role == provider.RoleAssistant && len(m.ToolCalls) > 0) {
			return true
		}
	}
	return false
}

// usesTools reports whether a request takes part in tool calling at all.
func usesTools(req chatCompletionRequest, msgs []provider.Message) bool {
	return len(req.Tools) > 0 || isContinuation(msgs)
}

// agentTags is the context_tags for a captured agent turn: "agent" plus
// one "tool:<name>" per distinct tool called since the last user message,
// in first-use order. Nil if the turn called no tools.
func agentTags(msgs []provider.Message) []string {
	var tags []string
	seen := map[string]bool{}
	for _, m := range msgs[lastUserIndex(msgs)+1:] {
		for _, tc := range m.ToolCalls {
			name := tc.Function.Name
			if name == "" || seen[name] || len(tags) >= maxToolTags {
				continue
			}
			seen[name] = true
			tags = append(tags, "tool:"+name)
		}
	}
	if len(tags) == 0 {
		return nil
	}
	return append([]string{"agent"}, tags...)
}

// validateToolRequest checks the tool-calling parts of a request.
func validateToolRequest(req chatCompletionRequest) error {
	if len(req.Tools) > maxToolsPerRequest {
		return fmt.Errorf("at most %d tools per request", maxToolsPerRequest)
	}
	for _, t := range req.Tools {
		if t.Type != "" && t.Type != "function" {
			return fmt.Errorf("tool type %q is not supported (only \"function\")", t.Type)
		}
		if !toolNamePattern.MatchString(t.Function.Name) {
			return fmt.Errorf("invalid tool name %q: must match %s", t.Function.Name, toolNamePattern)
		}
		if len(t.Function.Parameters) > 0 && !json.Valid(t.Function.Parameters) {
			return fmt.Errorf("tool %q: parameters is not valid JSON", t.Function.Name)
		}
	}
	for i, m := range req.Messages {
		if m.Role == string(provider.RoleTool) && m.ToolCallID == "" {
			return fmt.Errorf("messages[%d]: a tool message needs tool_call_id", i)
		}
		for _, tc := range m.ToolCalls {
			if m.Role != string(provider.RoleAssistant) {
				return fmt.Errorf("messages[%d]: only assistant messages can carry tool_calls", i)
			}
			if tc.ID == "" || tc.Function.Name == "" {
				return fmt.Errorf("messages[%d]: every tool call needs an id and a function name", i)
			}
		}
	}
	return nil
}

// agentTurnState is what the first request of an agent turn computed and
// later requests of the same turn reuse.
type agentTurnState struct {
	result             RetrievalResult
	aggregationHint    string
	mergedUserContent  string // last user message after attachments were merged
	attachmentWarnings []string
	expires            time.Time
}

// agentTurnKey identifies a turn: who's asking, in which workspace, of
// which provider, and the conversation up to and including the user's
// question (as the client sent it, before any merging), plus the
// attachments, so an edited question is a different turn.
func agentTurnKey(actingUser, workspace identity.Scope, target string, msgs []chatMessage, attachments []chatAttachment) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s:%s|%s:%s|%s|", actingUser.Kind, actingUser.Owner, workspace.Kind, workspace.Owner, target)
	last := -1
	for i, m := range msgs {
		if m.Role == string(provider.RoleUser) {
			last = i
		}
	}
	enc := json.NewEncoder(h)
	for _, m := range msgs[:last+1] {
		_ = enc.Encode([2]string{m.Role, m.Content})
	}
	for _, a := range attachments {
		_ = enc.Encode([3]string{a.Type, a.Filename, a.Data})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// agentTurnCache is a small in-process LRU with a TTL. Losing an entry
// (eviction, another replica, a restart) only costs a recomputation.
type agentTurnCache struct {
	mu    sync.Mutex
	order *list.List // front = most recently used; values are cacheEntry
	byKey map[string]*list.Element
}

type cacheEntry struct {
	key   string
	state agentTurnState
}

func newAgentTurnCache() *agentTurnCache {
	return &agentTurnCache{order: list.New(), byKey: map[string]*list.Element{}}
}

func (c *agentTurnCache) get(key string, now time.Time) (agentTurnState, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.byKey[key]
	if !ok {
		return agentTurnState{}, false
	}
	e := el.Value.(cacheEntry)
	if now.After(e.state.expires) {
		c.order.Remove(el)
		delete(c.byKey, key)
		return agentTurnState{}, false
	}
	c.order.MoveToFront(el)
	return e.state, true
}

func (c *agentTurnCache) put(key string, s agentTurnState, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s.expires = now.Add(agentTurnCacheTTL)
	if el, ok := c.byKey[key]; ok {
		el.Value = cacheEntry{key, s}
		c.order.MoveToFront(el)
		return
	}
	c.byKey[key] = c.order.PushFront(cacheEntry{key, s})
	for c.order.Len() > agentTurnCacheSize {
		oldest := c.order.Back()
		c.order.Remove(oldest)
		delete(c.byKey, oldest.Value.(cacheEntry).key)
	}
}

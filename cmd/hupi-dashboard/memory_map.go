// The "memory map" feature: a graph view of a scope's conversations,
// entities, relationships, and topics, filterable by date range. Glue
// layer only — composes queries.go's plaintext queries and
// content_analysis.go's decrypt-gated ones into one response shape each,
// rather than overloading either file, keeping their existing
// plaintext-vs-decrypt split intact as the source-of-truth distinction.
//
// Split into two endpoints by cost profile, same precedent as
// /api/entity-relationships (cheap, always-on) vs. /api/content-themes
// (decrypt-gated, opt-in):
//
//   - GET /api/memory-map: entities, conversations (undecrypted),
//     relationship edges, mention edges. No decryption, same cost class
//     as every other panel in queries.go.
//   - GET /api/memory-map/topics: per-conversation excerpt + extracted
//     topics, global topic nodes, tagged_with edges. Gated behind
//     HUPI_ENABLE_DASHBOARD_CONTENT_ANALYSIS (content_analysis.go's own
//     flag — see that file's header comment for why this reuses it
//     rather than adding a new one).
//
// The frontend fetches both in parallel and merges them client-side, so
// the cheap graph renders immediately and topics progressively enhance
// it instead of blocking the whole view on a decrypt.
package main

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Defaults are reasoned, not measured — same honest status as this
// package's other UI-facing defaults (forgottenButImportant's,
// content_analysis.go's own consts).
const (
	defaultMemoryMapDays                    = 30
	defaultMemoryMapConversations           = 500
	defaultMemoryMapRelationships           = 500
	defaultMemoryMapTopicsConversations     = 150
	defaultMemoryMapCharsPerConversation    = 2_000
	defaultMemoryMapTopicsMaxTotalChars     = 200_000
	defaultMemoryMapExcerptChars            = 280
	defaultMemoryMapTopTermsPerConversation = 8
	defaultMemoryMapTopTermsGlobal          = 30
	defaultMemoryMapMinGlobalTopicCount     = 2
)

// MemoryMapNode is one node in either memory-map response — an entity,
// a conversation, or a topic. Type is a discriminant; exactly one of
// Entity/Conversation/Topic is set, matching it.
type MemoryMapNode struct {
	ID           string                        `json:"id"`
	Type         string                        `json:"type"` // "entity" | "conversation" | "topic"
	Label        string                        `json:"label"`
	Entity       *MemoryMapEntityPayload       `json:"entity,omitempty"`
	Conversation *MemoryMapConversationPayload `json:"conversation,omitempty"`
	Topic        *MemoryMapTopicPayload        `json:"topic,omitempty"`
}

type MemoryMapEntityPayload struct {
	EntityID string `json:"entity_id"`
	Kind     string `json:"kind"`
}

type MemoryMapConversationPayload struct {
	EpisodeID  string    `json:"episode_id"`
	TS         time.Time `json:"ts"`
	Importance *float64  `json:"importance,omitempty"`
}

type MemoryMapTopicPayload struct {
	Term  string `json:"term"`
	Count int    `json:"count"`
}

// MemoryMapEdge is one edge — a relationship (entity<->entity), a
// mention (conversation->entity), or a tagged_with (conversation->topic,
// topics endpoint only).
type MemoryMapEdge struct {
	ID           string                        `json:"id"`
	Source       string                        `json:"source"`
	Target       string                        `json:"target"`
	Type         string                        `json:"type"` // "relationship" | "mentions" | "tagged_with"
	Relationship *MemoryMapRelationshipPayload `json:"relationship,omitempty"`
}

type MemoryMapRelationshipPayload struct {
	Predicate  string  `json:"predicate"`
	ValidFrom  *string `json:"valid_from,omitempty"`
	ValidUntil *string `json:"valid_until,omitempty"`
}

// MemoryMapRange echoes the resolved date window back to the caller —
// to is the exclusive upper bound used for filtering (queryDateRange's
// own contract), not necessarily the literal ?to= the caller passed.
type MemoryMapRange struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type MemoryMapGraph struct {
	Range          MemoryMapRange  `json:"range"`
	Nodes          []MemoryMapNode `json:"nodes"`
	Edges          []MemoryMapEdge `json:"edges"`
	TotalNodeCount int             `json:"total_node_count"`
	Truncated      bool            `json:"truncated"`
}

// handleMemoryMap is the cheap, always-on graph endpoint: entities,
// conversations, relationship edges, and mention edges, all from
// plaintext columns.
func (s *server) handleMemoryMap(w http.ResponseWriter, r *http.Request) {
	scope := scopeFromContext(r.Context())
	from, to := queryDateRange(r, defaultMemoryMapDays)
	maxConversations := queryInt(r, "max_conversations", defaultMemoryMapConversations)
	maxRelationships := queryInt(r, "max_relationships", defaultMemoryMapRelationships)

	conversations, err := conversationsInRange(r.Context(), s.db, scope, from, to, maxConversations)
	if err != nil {
		internalError(w, err)
		return
	}
	relationships, err := entityRelationshipGraphInRange(r.Context(), s.db, scope, from, to, maxRelationships)
	if err != nil {
		internalError(w, err)
		return
	}
	episodeIDs := make([]string, len(conversations))
	for i, c := range conversations {
		episodeIDs[i] = c.EpisodeID
	}
	mentions, err := conversationEntityMentions(r.Context(), s.db, scope, episodeIDs)
	if err != nil {
		internalError(w, err)
		return
	}

	graph := assembleMemoryMapGraph(conversations, relationships, mentions)
	graph.Range = MemoryMapRange{From: from.Format("2006-01-02"), To: to.Format("2006-01-02")}
	graph.Truncated = len(conversations) >= maxConversations || len(relationships) >= maxRelationships
	writeJSON(w, http.StatusOK, graph)
}

// assembleMemoryMapGraph merges conversationsInRange/
// entityRelationshipGraphInRange/conversationEntityMentions' separate
// query results into one node/edge list, deduplicating entity nodes
// referenced by more than one relationship or mention.
func assembleMemoryMapGraph(conversations []ConversationNode, relationships []EntityRelationshipWithNames, mentions []ConversationEntityMention) MemoryMapGraph {
	var nodes []MemoryMapNode
	var edges []MemoryMapEdge
	entitiesSeen := map[string]bool{}

	addEntityNode := func(id, name, kind string) {
		nodeID := "entity:" + id
		if entitiesSeen[nodeID] {
			return
		}
		entitiesSeen[nodeID] = true
		nodes = append(nodes, MemoryMapNode{
			ID: nodeID, Type: "entity", Label: name,
			Entity: &MemoryMapEntityPayload{EntityID: id, Kind: kind},
		})
	}

	for _, c := range conversations {
		nodes = append(nodes, MemoryMapNode{
			ID:    "conversation:" + c.EpisodeID,
			Type:  "conversation",
			Label: c.TS.Format("2006-01-02 15:04"),
			Conversation: &MemoryMapConversationPayload{
				EpisodeID: c.EpisodeID, TS: c.TS, Importance: c.Importance,
			},
		})
	}

	for i, rel := range relationships {
		addEntityNode(rel.SubjectID, rel.SubjectName, rel.SubjectKind)
		addEntityNode(rel.ObjectID, rel.ObjectName, rel.ObjectKind)
		edges = append(edges, MemoryMapEdge{
			ID:     fmt.Sprintf("relationship:%d", i),
			Source: "entity:" + rel.SubjectID,
			Target: "entity:" + rel.ObjectID,
			Type:   "relationship",
			Relationship: &MemoryMapRelationshipPayload{
				Predicate: rel.Predicate, ValidFrom: rel.ValidFrom, ValidUntil: rel.ValidUntil,
			},
		})
	}

	for _, m := range mentions {
		addEntityNode(m.EntityID, m.EntityName, m.EntityKind)
		edges = append(edges, MemoryMapEdge{
			ID:     "mentions:" + m.EpisodeID + ":" + m.EntityID,
			Source: "conversation:" + m.EpisodeID,
			Target: "entity:" + m.EntityID,
			Type:   "mentions",
		})
	}

	return MemoryMapGraph{Nodes: nodes, Edges: edges, TotalNodeCount: len(nodes)}
}

// MemoryMapConversationTopicsEntry is one conversation's decrypted
// excerpt plus its own extracted topic terms.
type MemoryMapConversationTopicsEntry struct {
	EpisodeID string          `json:"episode_id"`
	Excerpt   string          `json:"excerpt"`
	Topics    []TermFrequency `json:"topics"`
}

// MemoryMapTopicsResponse is the decrypt-gated overlay's response.
// {"enabled":false} (every other field omitted) when the feature is
// off — same convention handleContentThemes already uses, so the
// frontend can treat "not enabled" as a normal, renderable state
// instead of an error to catch.
type MemoryMapTopicsResponse struct {
	Enabled       bool                               `json:"enabled"`
	Truncated     bool                               `json:"truncated,omitempty"`
	Conversations []MemoryMapConversationTopicsEntry `json:"conversations,omitempty"`
	Topics        []MemoryMapNode                    `json:"topics,omitempty"`
	Edges         []MemoryMapEdge                    `json:"edges,omitempty"`
}

// handleMemoryMapTopics is the decrypt-gated overlay: per-conversation
// excerpt + extracted topics, global topic nodes, and tagged_with
// edges. Takes the exact same conversationsInRange query the graph
// endpoint uses, so topic/excerpt data always lines up with a
// conversation node the graph endpoint (fetched with the same range/
// caps) actually returned.
func (s *server) handleMemoryMapTopics(w http.ResponseWriter, r *http.Request) {
	if !contentAnalysisEnabled() {
		writeJSON(w, http.StatusOK, MemoryMapTopicsResponse{Enabled: false})
		return
	}
	scope := scopeFromContext(r.Context())
	from, to := queryDateRange(r, defaultMemoryMapDays)
	maxConversations := queryInt(r, "max_conversations", defaultMemoryMapTopicsConversations)

	conversations, err := conversationsInRange(r.Context(), s.db, scope, from, to, maxConversations)
	if err != nil {
		internalError(w, err)
		return
	}
	episodeIDs := make([]string, len(conversations))
	for i, c := range conversations {
		episodeIDs[i] = c.EpisodeID
	}
	contents, err := decryptConversations(r.Context(), s.db, s.keys, scope, episodeIDs,
		defaultMemoryMapCharsPerConversation, defaultMemoryMapTopicsMaxTotalChars)
	if err != nil {
		internalError(w, err)
		return
	}

	var perConv []ConversationTopics
	var convEntries []MemoryMapConversationTopicsEntry
	var mentionEdges []MemoryMapEdge
	for _, c := range contents {
		topics := localKeywordThemes(c.Text, defaultMemoryMapTopTermsPerConversation)
		perConv = append(perConv, ConversationTopics{EpisodeID: c.EpisodeID, Topics: topics})

		excerpt := c.Text
		if len(excerpt) > defaultMemoryMapExcerptChars {
			excerpt = excerpt[:defaultMemoryMapExcerptChars]
		}
		convEntries = append(convEntries, MemoryMapConversationTopicsEntry{
			EpisodeID: c.EpisodeID, Excerpt: excerpt, Topics: topics,
		})
		for _, t := range topics {
			mentionEdges = append(mentionEdges, MemoryMapEdge{
				ID:     "tagged_with:" + c.EpisodeID + ":" + t.Term,
				Source: "conversation:" + c.EpisodeID,
				Target: "topic:" + t.Term,
				Type:   "tagged_with",
			})
		}
	}

	globalTopics := aggregateConversationTopics(perConv, defaultMemoryMapTopTermsGlobal)
	keepTopic := make(map[string]bool, len(globalTopics))
	var topicNodes []MemoryMapNode
	for _, t := range globalTopics {
		// A term that only ever occurred once across the whole requested
		// range is exactly the shape of noise this surfaced in practice
		// (a one-off word tying with genuine recurring signal on short
		// conversations) — require it to show up at least twice
		// scope-wide before it earns its own graph node. Per-conversation
		// chips (convEntries above) deliberately keep every term; this
		// threshold only thins the global topic-node layer.
		if t.Count < defaultMemoryMapMinGlobalTopicCount {
			continue
		}
		keepTopic[t.Term] = true
		topicNodes = append(topicNodes, MemoryMapNode{
			ID: "topic:" + t.Term, Type: "topic", Label: t.Term,
			Topic: &MemoryMapTopicPayload{Term: t.Term, Count: t.Count},
		})
	}
	// Only keep edges to a topic that survived the global topN cut —
	// otherwise the response could reference a topic node it never
	// actually included.
	var edges []MemoryMapEdge
	for _, e := range mentionEdges {
		if keepTopic[strings.TrimPrefix(e.Target, "topic:")] {
			edges = append(edges, e)
		}
	}

	writeJSON(w, http.StatusOK, MemoryMapTopicsResponse{
		Enabled:       true,
		Truncated:     len(conversations) >= maxConversations,
		Conversations: convEntries,
		Topics:        topicNodes,
		Edges:         edges,
	})
}

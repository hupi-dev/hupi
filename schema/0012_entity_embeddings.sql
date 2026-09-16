-- Entities have never been reachable by vector search — only by
-- stage1EntityMatches' plain substring check against known entity
-- names/id-slugs (internal/store/retrieve.go). That's deliberately cheap
-- (no embedding call, so a query with zero signal can skip retrieval
-- entirely without ever hitting the embedding provider), but it means a
-- paraphrase that doesn't literally contain the entity's name misses
-- every entity outright, even ones that are exactly the answer — found
-- via live testing: "what programming language do I prefer?" found
-- nothing, while "what's my favorite programming language?" only worked
-- because that phrase is the literal entity name.
--
-- This adds an embedding column so entities can also be found by
-- similarity, same as summaries/episodes — see
-- internal/store/retrieve.go's vectorSearchEntities and
-- internal/consolidation/store.go's embedEntities. It's additive, not a
-- replacement: stage1EntityMatches' substring check still runs first and
-- costs nothing when it already finds the entity; vector search only
-- adds a second chance using the query embedding stage 2 already
-- computed for summaries/episodes, not a new network call of its own.
alter table entities add column embedding vector(1536);

create index entities_embedding_idx on entities using hnsw (embedding vector_cosine_ops)
    where embedding is not null;

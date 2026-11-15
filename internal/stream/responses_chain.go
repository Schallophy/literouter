package stream

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"
)

type chainEntry struct {
	key       string
	id        string
	expiresAt int64
}

// ResponseChainStore maps conversation-prefix hashes to platform response ids.
// It lives only in memory; eviction or restart simply falls back to full input.
type ResponseChainStore struct {
	mu      sync.Mutex
	entries map[string]*list.Element
	order   *list.List
}

// NewResponseChainStore creates an empty store.
func NewResponseChainStore() *ResponseChainStore {
	return &ResponseChainStore{entries: map[string]*list.Element{}, order: list.New()}
}

// Size returns the number of live entries.
func (s *ResponseChainStore) Size() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// Lookup returns a live response id and refreshes LRU position.
func (s *ResponseChainStore) Lookup(key string, now int64) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	element, ok := s.entries[key]
	if !ok {
		return ""
	}
	entry := element.Value.(*chainEntry)
	if entry.expiresAt <= now {
		delete(s.entries, key)
		s.order.Remove(element)
		return ""
	}
	s.order.MoveToBack(element)
	return entry.id
}

// Remember stores an id for a prefix, resetting its TTL and LRU position.
func (s *ResponseChainStore) Remember(key, id string, ttlMs int64, maxEntries int, now int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if element, ok := s.entries[key]; ok {
		s.order.Remove(element)
		delete(s.entries, key)
	}
	ttl := ttlMs
	if ttl < 1000 {
		ttl = 1000
	}
	element := s.order.PushBack(&chainEntry{key: key, id: id, expiresAt: now + ttl})
	s.entries[key] = element
	s.pruneLocked(now, maxEntries)
}

// Delete removes a prefix.
func (s *ResponseChainStore) Delete(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if element, ok := s.entries[key]; ok {
		s.order.Remove(element)
		delete(s.entries, key)
	}
}

// Clear drops every entry.
func (s *ResponseChainStore) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = map[string]*list.Element{}
	s.order.Init()
}

// Prune drops expired entries then enforces the size cap.
func (s *ResponseChainStore) Prune(now int64, maxEntries int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now, maxEntries)
}

func (s *ResponseChainStore) pruneLocked(now int64, maxEntries int) {
	for element := s.order.Front(); element != nil; {
		next := element.Next()
		entry := element.Value.(*chainEntry)
		if entry.expiresAt <= now {
			delete(s.entries, entry.key)
			s.order.Remove(element)
		}
		element = next
	}
	for s.order.Len() > maxEntries {
		front := s.order.Front()
		if front == nil {
			break
		}
		entry := front.Value.(*chainEntry)
		delete(s.entries, entry.key)
		s.order.Remove(front)
	}
}

// CanonicalJSON serializes values with sorted object keys at every depth.
func CanonicalJSON(value any) string {
	switch typed := value.(type) {
	case nil:
		return "null"
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		var out strings.Builder
		out.WriteByte('{')
		for index, key := range keys {
			if index > 0 {
				out.WriteByte(',')
			}
			encodedKey, _ := json.Marshal(key)
			out.Write(encodedKey)
			out.WriteByte(':')
			out.WriteString(CanonicalJSON(typed[key]))
		}
		out.WriteByte('}')
		return out.String()
	case []any:
		var out strings.Builder
		out.WriteByte('[')
		for index, item := range typed {
			if index > 0 {
				out.WriteByte(',')
			}
			out.WriteString(CanonicalJSON(item))
		}
		out.WriteByte(']')
		return out.String()
	default:
		encoded, err := json.Marshal(value)
		if err != nil {
			return "null"
		}
		return string(encoded)
	}
}

// HashJSON hashes the canonical JSON form.
func HashJSON(value any) string {
	sum := sha256.Sum256([]byte(CanonicalJSON(value)))
	return hex.EncodeToString(sum[:])
}

// ChainScope binds a response id to caller, provider, model and tool set.
func ChainScope(callerID, providerID, modelName string, tools any) string {
	return callerID + "|" + providerID + "|" + modelName + "|" + HashJSON(tools)
}

// ResponsesInputItems normalizes input into a non-empty item array.
func ResponsesInputItems(input any) []any {
	if text, ok := input.(string); ok {
		return []any{map[string]any{"role": "user", "content": text}}
	}
	if items, ok := input.([]any); ok && len(items) > 0 {
		return items
	}
	return nil
}

// ItemsPrefixHashes returns a rolling hash for each prefix of items.
func ItemsPrefixHashes(scope string, items []any) []string {
	out := make([]string, 0, len(items))
	previous := HashJSON(map[string]any{"scope": scope})
	for _, item := range items {
		sum := sha256.Sum256([]byte(previous + "\n" + CanonicalJSON(item)))
		previous = hex.EncodeToString(sum[:])
		out = append(out, scope+"\x00"+previous)
	}
	return out
}

func isStoredOutput(item any) bool {
	record, ok := item.(map[string]any)
	if !ok {
		return false
	}
	eventType, _ := record["type"].(string)
	if eventType == "function_call" || eventType == "reasoning" || eventType == "item_reference" {
		return true
	}
	return record["role"] == "assistant"
}

// ChainDelta keeps only genuinely new user/tool-result items after a matched
// prefix, dropping outputs the platform already stored. Nil means "cannot chain".
func ChainDelta(items []any, matchedLength int) []any {
	if matchedLength < 0 {
		matchedLength = 0
	}
	if matchedLength > len(items) {
		matchedLength = len(items)
	}
	rest := make([]any, 0, len(items)-matchedLength)
	for _, item := range items[matchedLength:] {
		if !isStoredOutput(item) {
			rest = append(rest, item)
		}
	}
	if len(rest) == 0 {
		return nil
	}
	return rest
}

// CapturedBody sniffs a response body for its response id without altering bytes.
type CapturedBody struct {
	source   bodySource
	pending  string
	settled  bool
	sawSSE   bool
	onID     func(string)
	eof      bool
}

type bodySource interface {
	Read(p []byte) (int, error)
	Close() error
}

// CaptureResponseID wraps a body and reports the first response id it sees.
// It works for SSE (response.created) and plain JSON bodies.
func CaptureResponseID(body bodySource, onID func(string)) bodySource {
	if body == nil {
		return nil
	}
	return &CapturedBody{source: body, onID: onID}
}

// Read passes bytes through while inspecting copied data.
func (c *CapturedBody) Read(p []byte) (int, error) {
	n, err := c.source.Read(p)
	if n > 0 && !c.settled && c.onID != nil {
		c.pending += string(p[:n])
		c.inspectSSE()
		if !c.settled && len(c.pending) > 65536 {
			c.settled = true
		}
	}
	if err != nil && !c.eof {
		c.eof = true
		if !c.settled && !c.sawSSE && strings.TrimSpace(c.pending) != "" {
			var payload map[string]any
			if jsonErr := json.Unmarshal([]byte(c.pending), &payload); jsonErr == nil {
				if id, ok := payload["id"].(string); ok && id != "" {
					c.settled = true
					c.onID(id)
				}
			}
		}
	}
	return n, err
}

// Close closes the underlying body.
func (c *CapturedBody) Close() error { return c.source.Close() }

func (c *CapturedBody) inspectSSE() {
	for {
		index := strings.IndexByte(c.pending, '\n')
		if index < 0 {
			return
		}
		line := strings.TrimSuffix(c.pending[:index], "\r")
		c.pending = c.pending[index+1:]
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		c.sawSSE = true
		payload := strings.TrimSpace(line[5:])
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			continue
		}
		var id string
		if response, ok := event["response"].(map[string]any); ok {
			if value, ok := response["id"].(string); ok {
				id = value
			}
		}
		if id == "" && event["type"] == "response.created" {
			if value, ok := event["id"].(string); ok {
				id = value
			}
		}
		if id != "" {
			c.settled = true
			c.onID(id)
			return
		}
	}
}

// ChainTTLMillis converts configured minutes to a millisecond TTL.
func ChainTTLMillis(minutes int) int64 { return int64(minutes) * int64(time.Minute/time.Millisecond) }

package web

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// pooledConversation tracks a warm ChatHub conversation that can be reused by
// subsequent turns of the same conversation (same API key, model and opening
// user message).
type pooledConversation struct {
	AccountID      string    `json:"accountId"`
	ConversationID string    `json:"conversationId"`
	SessionID      string    `json:"sessionId"`
	Turns          int       `json:"turns"`
	LastUsed       time.Time `json:"lastUsed"`
}

// conversationPool keeps a small LRU of warm conversations. Entries rotate
// after maxTurns turns or maxIdle without use so Copilot-side context does not
// grow unbounded and stale sessions are discarded.
type conversationPool struct {
	mu       sync.Mutex
	path     string
	maxTurns int
	maxIdle  time.Duration
	maxSize  int
	byKey    map[string]*pooledConversation
}

// openConversationPool loads any previously persisted warm conversations so
// multi-turn reuse survives service restarts. The file is written through the
// data volume (M365_CONV_POOL_CACHE), mirroring the session store.
func openConversationPool() *conversationPool {
	path := os.Getenv("M365_CONV_POOL_CACHE")
	if path == "" {
		path = filepath.Join(os.TempDir(), "m365-native-conv-pool.json")
	}
	p := &conversationPool{
		path:     path,
		maxTurns: 48,
		maxIdle:  30 * time.Minute,
		maxSize:  512,
		byKey:    map[string]*pooledConversation{},
	}
	if b, err := os.ReadFile(path); err == nil {
		var saved map[string]*pooledConversation
		if json.Unmarshal(b, &saved) == nil {
			now := time.Now()
			for k, v := range saved {
				if v == nil || v.ConversationID == "" {
					continue
				}
				if v.Turns >= p.maxTurns || now.Sub(v.LastUsed) > p.maxIdle {
					continue
				}
				p.byKey[k] = v
			}
		}
	}
	return p
}

// conversationAnchor hashes the first user message of a conversation. Every
// subsequent turn of the same conversation carries the same opening message, so
// this reliably identifies "the same conversation" without any client support.
// Different users/conversations produce different anchors, which keeps sessions
// isolated even when several clients share one API key.
func conversationAnchor(messages []oaiMsg) string {
	for _, m := range messages {
		if m.Role != "user" {
			continue
		}
		var s string
		switch v := m.Content.(type) {
		case string:
			s = v
		case []any:
			var b strings.Builder
			for _, part := range v {
				if mm, ok := part.(map[string]any); ok {
					if t, _ := mm["type"].(string); t == "text" || t == "input_text" {
						if s2, _ := mm["text"].(string); s2 != "" {
							b.WriteString(s2)
						}
					}
				}
			}
			s = b.String()
		}
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		h := sha256.Sum256([]byte(s))
		return hex.EncodeToString(h[:10])
	}
	return ""
}

func conversationPoolKey(apiKeyID, model, anchor string) string {
	h := sha256.Sum256([]byte(apiKeyID + "|" + model + "|" + anchor))
	return hex.EncodeToString(h[:16])
}

func (p *conversationPool) get(key string) (pooledConversation, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	v, ok := p.byKey[key]
	if !ok {
		return pooledConversation{}, false
	}
	if v.Turns >= p.maxTurns || now.Sub(v.LastUsed) > p.maxIdle {
		delete(p.byKey, key)
		p.saveLocked()
		return pooledConversation{}, false
	}
	return *v, true
}

// record stores the conversation used for a completed request, incrementing the
// turn counter when the same upstream conversation is continued.
func (p *conversationPool) record(key string, v pooledConversation) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	if cur, ok := p.byKey[key]; ok && cur.ConversationID == v.ConversationID {
		v.Turns = cur.Turns + 1
	} else {
		v.Turns = 1
	}
	v.LastUsed = now
	p.byKey[key] = &v
	if len(p.byKey) > p.maxSize {
		var oldestKey string
		var oldest time.Time
		for k, c := range p.byKey {
			if oldestKey == "" || c.LastUsed.Before(oldest) {
				oldestKey, oldest = k, c.LastUsed
			}
		}
		delete(p.byKey, oldestKey)
	}
	p.saveLocked()
}

func (p *conversationPool) saveLocked() {
	if p.path == "" {
		return
	}
	b, err := json.MarshalIndent(p.byKey, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(p.path), 0o700)
	_ = os.WriteFile(p.path, b, 0o600)
}

// deleteByAccount purges pooled conversations pinned to a deleted account so
// round-robin failover is not disabled for those conversations.
func (p *conversationPool) deleteByAccount(accountID string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	removed := 0
	for k, v := range p.byKey {
		if v.AccountID == accountID {
			delete(p.byKey, k)
			removed++
		}
	}
	if removed > 0 {
		p.saveLocked()
	}
	return removed
}

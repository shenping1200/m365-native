package auth

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"m365-native/internal/proxy"
)

type AccountToken struct {
	ID           string    `json:"id"`
	Email        string    `json:"email"`
	DisplayName  string    `json:"displayName,omitempty"`
	Status       string    `json:"status"`
	AccessToken  string    `json:"accessToken"`
	RefreshToken string    `json:"refreshToken,omitempty"`
	ExpiresAt    time.Time `json:"expiresAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
	OID          string    `json:"oid,omitempty"`
	TID          string    `json:"tid,omitempty"`
	ClientID     string    `json:"clientId,omitempty"`
	Proxy        string    `json:"proxy,omitempty"`
}

type Cache struct {
	Accounts []AccountToken `json:"accounts"`
}

type Store struct {
	mu   sync.Mutex
	path string
	data Cache
	// refreshBackoff remembers failed refresh attempts so a broken account is
	// not hammered by the OAuth token endpoint on every request.
	refreshBackoff map[string]time.Time
}

func CachePath() string {
	if p := os.Getenv("M365_TOKEN_CACHE"); p != "" {
		return p
	}
	if p := os.Getenv("M365_TOKEN_FILE"); p != "" {
		return p
	}
	h, err := os.UserHomeDir()
	if err != nil || h == "" {
		return filepath.Join(".", ".config", "m365-native", "accounts.json")
	}
	return filepath.Join(h, ".config", "m365-native", "accounts.json")
}

func OpenStore(path string) (*Store, error) {
	if path == "" {
		path = CachePath()
	}
	s := &Store{path: path, data: Cache{Accounts: []AccountToken{}}, refreshBackoff: map[string]time.Time{}}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &s.data); err != nil {
		return nil, err
	}
	// Normalize oid/tid for older cache entries.
	for i := range s.data.Accounts {
		a := &s.data.Accounts[i]
		if a.OID == "" {
			a.OID = a.ID
		}
		if a.ID == "" {
			a.ID = a.OID
		}
	}
	return s, nil
}

func (s *Store) Path() string {
	return s.path
}

func (s *Store) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		// /tmp has no nested dir needs usually; ignore if parent is root-ish
		if filepath.Dir(s.path) != "/" && filepath.Dir(s.path) != "." {
			// still try write below
		}
	}
	b, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, b, 0o600)
}

func (s *Store) List() []AccountToken {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]AccountToken, len(s.data.Accounts))
	copy(out, s.data.Accounts)
	return out
}

func (s *Store) Upsert(tok TokenSet) (AccountToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := tok.HomeOID
	if id == "" {
		id = tok.Email
	}
	if id == "" {
		id = "account-" + time.Now().Format("150405")
	}
	acc := AccountToken{
		ID:           id,
		Email:        tok.Email,
		DisplayName:  tok.DisplayName,
		Status:       "online",
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresAt:    tok.ExpiresAt,
		UpdatedAt:    time.Now(),
		OID:          firstNonEmpty(tok.HomeOID, id),
		TID:          tok.TenantID,
		ClientID:     ClientID(),
	}
	found := false
	for i, existing := range s.data.Accounts {
		if existing.ID == acc.ID || (acc.Email != "" && existing.Email == acc.Email) {
			if acc.RefreshToken == "" {
				acc.RefreshToken = existing.RefreshToken
			}
			if acc.TID == "" {
				acc.TID = existing.TID
			}
			if acc.OID == "" {
				acc.OID = existing.OID
			}
			// OAuth 登录流程不带 proxy, 保留用户在 Web 界面手动设置的代理
			if acc.Proxy == "" {
				acc.Proxy = existing.Proxy
			}
			s.data.Accounts[i] = acc
			found = true
			break
		}
	}
	if !found {
		s.data.Accounts = append(s.data.Accounts, acc)
	}
	return acc, s.saveLocked()
}

func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.data.Accounts[:0]
	for _, a := range s.data.Accounts {
		if a.ID != id {
			next = append(next, a)
		}
	}
	s.data.Accounts = next
	return s.saveLocked()
}

func (s *Store) SetProxy(id, proxy string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.Accounts {
		if s.data.Accounts[i].ID == id {
			s.data.Accounts[i].Proxy = proxy
			return s.saveLocked()
		}
	}
	return os.ErrNotExist
}

func (s *Store) Get(id string) (AccountToken, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.data.Accounts {
		if a.ID == id || a.OID == id || a.Email == id {
			return a, true
		}
	}
	return AccountToken{}, false
}

func (s *Store) First() (AccountToken, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.data.Accounts) == 0 {
		return AccountToken{}, false
	}
	return s.data.Accounts[0], true
}

func (s *Store) EnsureValid(id string) (AccountToken, error) {
	acc, ok := s.Get(id)
	if !ok {
		return AccountToken{}, os.ErrNotExist
	}
	if time.Now().Before(acc.ExpiresAt.Add(-30 * time.Second)) {
		return acc, nil
	}
	if acc.RefreshToken == "" {
		acc.Status = "expired"
		s.mu.Lock()
		for i, a := range s.data.Accounts {
			if a.ID == acc.ID {
				s.data.Accounts[i] = acc
				_ = s.saveLocked()
				break
			}
		}
		s.mu.Unlock()
		return acc, fmtExpired()
	}
	s.mu.Lock()
	backoffUntil := s.refreshBackoff[acc.ID]
	s.mu.Unlock()
	if !backoffUntil.IsZero() && time.Now().Before(backoffUntil) {
		return acc, fmtExpired()
	}
	client, err := proxy.HTTPClientFor(acc.Proxy)
	if err != nil {
		return acc, err
	}
	tok, err := Refresh(acc.RefreshToken, client)
	if err != nil {
		s.mu.Lock()
		s.refreshBackoff[acc.ID] = time.Now().Add(2 * time.Minute)
		s.mu.Unlock()
		acc.Status = "expired"
		s.mu.Lock()
		for i, a := range s.data.Accounts {
			if a.ID == acc.ID {
				s.data.Accounts[i] = acc
				_ = s.saveLocked()
				break
			}
		}
		s.mu.Unlock()
		return acc, err
	}
	s.mu.Lock()
	delete(s.refreshBackoff, acc.ID)
	s.mu.Unlock()
	if tok.Email == "" {
		tok.Email = acc.Email
	}
	if tok.DisplayName == "" {
		tok.DisplayName = acc.DisplayName
	}
	if tok.HomeOID == "" {
		tok.HomeOID = firstNonEmpty(acc.OID, acc.ID)
	}
	if tok.TenantID == "" {
		tok.TenantID = acc.TID
	}
	return s.Upsert(tok)
}

// RefreshExpiring refreshes every cached account whose access token expires
// within minTTL (or is already expired), respecting the per-account refresh
// backoff. It returns how many accounts were refreshed and how many failed.
func (s *Store) RefreshExpiring(minTTL time.Duration) (refreshed int, failed int) {
	for _, acc := range s.List() {
		if acc.RefreshToken == "" {
			continue
		}
		if time.Now().Before(acc.ExpiresAt.Add(-minTTL)) {
			continue
		}
		if _, err := s.EnsureValid(acc.ID); err != nil {
			failed++
			continue
		}
		refreshed++
	}
	return refreshed, failed
}

func fmtExpired() error {
	return errors.New("token_expired: refresh token missing or expired")
}

package web

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type apiKeyRecord struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	Hash       string     `json:"hash"`
	Raw        string     `json:"raw,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
	Revoked    bool       `json:"revoked"`
}
type apiKeyStore struct {
	mu      sync.Mutex
	Path    string
	Keys    []apiKeyRecord `json:"keys"`
	persist *persistStore
}

func newAPIKeyStore(path string) *apiKeyStore {
	s := &apiKeyStore{Path: path}
	s.persist = &persistStore{flush: s.flush}
	return s
}

func openAPIKeys() *apiKeyStore {
	p := strings.TrimSpace(os.Getenv("M365_API_KEYS"))
	if p == "" {
		h, _ := os.UserHomeDir()
		p = filepath.Join(h, ".config", "m365-copilot2api", "api-keys.json")
	}
	s := newAPIKeyStore(p)
	b, e := os.ReadFile(p)
	if e == nil && json.Unmarshal(b, s) == nil {
		migrated := false
		for i := range s.Keys {
			if s.Keys[i].Raw != "" {
				if s.Keys[i].Hash == "" {
					s.Keys[i].Hash = keyHash(s.Keys[i].Raw)
				}
				s.Keys[i].Raw = ""
				migrated = true
			}
		}
		if migrated {
			_ = s.flush()
		}
	}
	return s
}
func (s *apiKeyStore) flush() error {
	s.mu.Lock()
	b, err := json.MarshalIndent(s, "", "  ")
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0700); err != nil {
		return err
	}
	return writeFileAtomic(s.Path, b, 0600)
}
func keyHash(k string) string { h := sha256.Sum256([]byte(k)); return hex.EncodeToString(h[:]) }
func (s *apiKeyStore) create(name string) (apiKeyRecord, string, error) {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		return apiKeyRecord{}, "", e
	}
	raw := "m365_" + hex.EncodeToString(b)
	r := apiKeyRecord{ID: hex.EncodeToString(b[:8]), Name: name, Prefix: raw[:12], Hash: keyHash(raw), CreatedAt: time.Now()}
	s.mu.Lock()
	s.Keys = append(s.Keys, r)
	s.mu.Unlock()
		if err := s.persist.flushNowBlocking(); err != nil {
			s.mu.Lock()
			s.Keys = s.Keys[:len(s.Keys)-1]
			s.mu.Unlock()
			return apiKeyRecord{}, "", err
		}
		r.Hash = ""
		r.Raw = ""
		return r, raw, nil
	}

// rotate revokes the existing key and creates a replacement atomically from
// the caller's perspective. The replacement secret is returned only once.
func (s *apiKeyStore) rotate(id string) (apiKeyRecord, string, error) {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		return apiKeyRecord{}, "", e
	}
	raw := "m365_" + hex.EncodeToString(b)
	replacement := apiKeyRecord{
		ID:        hex.EncodeToString(b[:8]),
		Prefix:    raw[:12],
		Hash:      keyHash(raw),
		CreatedAt: time.Now(),
	}

	s.mu.Lock()
	oldIndex := -1
	for i := range s.Keys {
		if s.Keys[i].ID == id {
			oldIndex = i
			replacement.Name = s.Keys[i].Name
			break
		}
	}
	if oldIndex < 0 {
		s.mu.Unlock()
		return apiKeyRecord{}, "", nil
	}
	oldRevoked := s.Keys[oldIndex].Revoked
	s.Keys[oldIndex].Revoked = true
	s.Keys = append(s.Keys, replacement)
	s.mu.Unlock()

	if err := s.persist.flushNowBlocking(); err != nil {
		s.mu.Lock()
		for i := range s.Keys {
			if s.Keys[i].ID == replacement.ID {
				s.Keys = append(s.Keys[:i], s.Keys[i+1:]...)
				break
			}
		}
		for i := range s.Keys {
			if s.Keys[i].ID == id {
				s.Keys[i].Revoked = oldRevoked
				break
			}
		}
		s.mu.Unlock()
		return apiKeyRecord{}, "", err
	}

	replacement.Hash = ""
	replacement.Raw = ""
	return replacement, raw, nil
}

func (s *apiKeyStore) list() []apiKeyRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]apiKeyRecord, len(s.Keys))
	copy(out, s.Keys)
	for i := range out {
		out[i].Hash = ""
		out[i].Raw = ""
	}
	return out
}
func (s *apiKeyStore) revoke(id string) (bool, error) {
	s.mu.Lock()
	for i := range s.Keys {
		if s.Keys[i].ID == id && !s.Keys[i].Revoked {
			s.Keys[i].Revoked = true
			s.mu.Unlock()
			if err := s.persist.flushNowBlocking(); err != nil {
				s.mu.Lock()
				s.Keys[i].Revoked = false
				s.mu.Unlock()
				return false, err
			}
			return true, nil
		}
	}
	s.mu.Unlock()
	return false, nil
}

// delete physically removes a key record, rolling back on persistence failure.
func (s *apiKeyStore) delete(id string) (bool, error) {
	s.mu.Lock()
	for i := range s.Keys {
		if s.Keys[i].ID != id {
			continue
		}
		removed := s.Keys[i]
		s.Keys = append(s.Keys[:i], s.Keys[i+1:]...)
		s.mu.Unlock()
		if err := s.persist.flushNowBlocking(); err != nil {
			s.mu.Lock()
			s.Keys = append(s.Keys[:i], append([]apiKeyRecord{removed}, s.Keys[i:]...)...)
			s.mu.Unlock()
			return false, err
		}
		return true, nil
	}
	s.mu.Unlock()
	return false, nil
}

func (s *apiKeyStore) update(id, name string, revoked *bool) (bool, error) {
	s.mu.Lock()
	found := false
	for i := range s.Keys {
		if s.Keys[i].ID != id {
			continue
		}
		if name != "" {
			s.Keys[i].Name = name
		}
		if revoked != nil {
			s.Keys[i].Revoked = *revoked
		}
		found = true
		break
	}
	s.mu.Unlock()
	if !found {
		return false, nil
	}
	if err := s.persist.flushNowBlocking(); err != nil {
		return false, err
	}
	return true, nil
}
func (s *apiKeyStore) authenticate(raw string) (string, bool) {
	s.mu.Lock()
	h := keyHash(raw)
	id := ""
	for i := range s.Keys {
		if s.Keys[i].Hash == h && !s.Keys[i].Revoked {
			now := time.Now()
			s.Keys[i].LastUsedAt = &now
			id = s.Keys[i].ID
			break
		}
	}
	s.mu.Unlock()
	if id != "" {
		s.persist.markDirty()
	}
	return id, id != ""
}

func (s *apiKeyStore) valid(raw string) bool {
	_, ok := s.authenticate(raw)
	return ok
}

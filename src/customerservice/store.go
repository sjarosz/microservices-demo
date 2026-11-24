package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Store keeps one JSON document per customer. Update is a read-modify-write under a lock held by this process,
// which is enough for the single replica this demo runs.
type Store interface {
	Name() string
	Get(sub string) (*Customer, error)
	Update(sub string, fn func(*Customer) (*Customer, error)) error
	Delete(sub string) error
}

func newStore(redisAddr string) Store {
	if redisAddr == "" {
		return &memStore{m: map[string][]byte{}}
	}
	return &redisStore{c: redis.NewClient(&redis.Options{Addr: redisAddr})}
}

type memStore struct {
	mu sync.Mutex
	m  map[string][]byte
}

func (s *memStore) Name() string { return "memory" }

func (s *memStore) Get(sub string) (*Customer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return decode(s.m[sub])
}

func (s *memStore) Update(sub string, fn func(*Customer) (*Customer, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := decode(s.m[sub])
	if err != nil {
		return err
	}
	if c, err = fn(c); err != nil {
		return err
	}
	s.m[sub], err = json.Marshal(c)
	return err
}

func (s *memStore) Delete(sub string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, sub)
	return nil
}

type redisStore struct {
	mu sync.Mutex
	c  *redis.Client
}

func (s *redisStore) Name() string { return "redis" }

func key(sub string) string { return "customer:" + sub }

func ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 3*time.Second)
}

func (s *redisStore) Get(sub string) (*Customer, error) {
	c, cancel := ctx()
	defer cancel()
	raw, err := s.c.Get(c, key(sub)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decode(raw)
}

func (s *redisStore) Update(sub string, fn func(*Customer) (*Customer, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, err := s.Get(sub)
	if err != nil {
		return err
	}
	if cur, err = fn(cur); err != nil {
		return err
	}
	raw, err := json.Marshal(cur)
	if err != nil {
		return err
	}
	c, cancel := ctx()
	defer cancel()
	return s.c.Set(c, key(sub), raw, 0).Err()
}

func (s *redisStore) Delete(sub string) error {
	c, cancel := ctx()
	defer cancel()
	return s.c.Del(c, key(sub)).Err()
}

func decode(raw []byte) (*Customer, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	c := &Customer{}
	return c, json.Unmarshal(raw, c)
}

// vault encrypts card numbers before they are stored (AES-256-GCM, key derived from VAULT_KEY).
type vault struct{ aead cipher.AEAD }

func newVault(secret string) (*vault, error) {
	if secret == "" {
		return nil, errors.New("must be set")
	}
	k := sha256.Sum256([]byte(secret))
	block, err := aes.NewCipher(k[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	return &vault{aead}, err
}

func (v *vault) seal(plain string) (string, error) {
	nonce := make([]byte, v.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.RawStdEncoding.EncodeToString(v.aead.Seal(nonce, nonce, []byte(plain), nil)), nil
}

func (v *vault) open(sealed string) (string, error) {
	raw, err := base64.RawStdEncoding.DecodeString(sealed)
	if err != nil || len(raw) < v.aead.NonceSize() {
		return "", errors.New("sealed value is malformed")
	}
	plain, err := v.aead.Open(nil, raw[:v.aead.NonceSize()], raw[v.aead.NonceSize():], nil)
	return string(plain), err
}

func randomID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

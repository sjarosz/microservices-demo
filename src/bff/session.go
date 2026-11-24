package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Session is everything the BFF keeps for one browser. It lives on the server; the cookie is only its id.
type Session struct {
	ID string `json:"-"`

	// Set once the shopper has signed in.
	Sub       string `json:"sub,omitempty"`
	Name      string `json:"name,omitempty"`
	GivenName string `json:"given_name,omitempty"`
	Email     string `json:"email,omitempty"`
	IDToken   string `json:"id_token,omitempty"` // kept for sign-out at the provider (id_token_hint)

	Pending     *pendingAuth           `json:"pending,omitempty"`      // a sign-in in flight
	Viewed      []string               `json:"viewed,omitempty"`       // products seen while anonymous
	GuestOrders []orderEvent           `json:"guest_orders,omitempty"` // guest purchases whose points can still be claimed
	Results     map[string]orderResult `json:"results,omitempty"`      // what the confirmation page shows per order
	Notice      string                 `json:"notice,omitempty"`       // shown once on the next page
}

type pendingAuth struct {
	State    string `json:"state"`
	Nonce    string `json:"nonce"`
	Verifier string `json:"verifier"`
	ReturnTo string `json:"return_to"`
}

type orderEvent struct {
	OrderID  string   `json:"order_id"`
	TotalUSD float64  `json:"total_usd"`
	Items    []string `json:"items"`
}

type orderResult struct {
	SignedIn        bool   `json:"signed_in"`
	PointsEarned    int    `json:"points_earned"`
	Balance         int    `json:"balance"`
	Tier            string `json:"tier,omitempty"`
	ClaimablePoints int    `json:"claimable_points"`
	CardSaved       bool   `json:"card_saved"`
}

func (s *Session) signedIn() bool { return s != nil && s.Sub != "" }

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

type SessionStore interface {
	Name() string
	Get(id string) (*Session, error)
	Put(s *Session, ttl time.Duration) error
	Delete(id string) error
}

func newSessionStore(redisAddr string) SessionStore {
	if redisAddr == "" {
		return &memSessions{m: map[string]memEntry{}}
	}
	return &redisSessions{c: redis.NewClient(&redis.Options{Addr: redisAddr})}
}

type memEntry struct {
	raw     []byte
	expires time.Time
}

type memSessions struct {
	mu sync.Mutex
	m  map[string]memEntry
}

func (s *memSessions) Name() string { return "memory" }

func (s *memSessions) Get(id string) (*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[id]
	if !ok || time.Now().After(e.expires) {
		delete(s.m, id)
		return nil, nil
	}
	return decodeSession(id, e.raw)
}

func (s *memSessions) Put(sess *Session, ttl time.Duration) error {
	raw, err := json.Marshal(sess)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[sess.ID] = memEntry{raw, time.Now().Add(ttl)}
	return nil
}

func (s *memSessions) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, id)
	return nil
}

type redisSessions struct{ c *redis.Client }

func (s *redisSessions) Name() string { return "redis" }

func sessionKey(id string) string { return "bff:session:" + id }

func redisCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 3*time.Second)
}

func (s *redisSessions) Get(id string) (*Session, error) {
	ctx, cancel := redisCtx()
	defer cancel()
	raw, err := s.c.Get(ctx, sessionKey(id)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeSession(id, raw)
}

func (s *redisSessions) Put(sess *Session, ttl time.Duration) error {
	raw, err := json.Marshal(sess)
	if err != nil {
		return err
	}
	ctx, cancel := redisCtx()
	defer cancel()
	return s.c.Set(ctx, sessionKey(sess.ID), raw, ttl).Err()
}

func (s *redisSessions) Delete(id string) error {
	ctx, cancel := redisCtx()
	defer cancel()
	return s.c.Del(ctx, sessionKey(id)).Err()
}

func decodeSession(id string, raw []byte) (*Session, error) {
	s := &Session{}
	if err := json.Unmarshal(raw, s); err != nil {
		return nil, err
	}
	s.ID = id
	return s, nil
}

// --- the cookie ---------------------------------------------------------------------------------------------------

func (b *BFF) secure() bool { return b.cfg.publicURL.Scheme == "https" }

// cookieName uses the __Host- prefix over HTTPS, so a sibling subdomain cannot plant or overwrite the cookie.
func (b *BFF) cookieName() string {
	if b.secure() {
		return "__Host-bff"
	}
	return "bff"
}

func (b *BFF) sessionCookie(id string, maxAge int) *http.Cookie {
	return expiring(&http.Cookie{Name: b.cookieName(), Value: id, Path: "/", MaxAge: maxAge, HttpOnly: true,
		Secure: b.secure(), SameSite: http.SameSiteLaxMode})
}

// expiring gives a cookie that is being deleted (MaxAge < 0) a past Expires as well. Max-Age=0 alone is enough for
// browsers, but PingGateway drops it when it rewrites Set-Cookie headers, and the deletion would turn into an empty
// session cookie.
func expiring(c *http.Cookie) *http.Cookie {
	if c.MaxAge < 0 {
		c.Expires = time.Unix(1, 0)
	}
	return c
}

// session returns the browser's session, or nil when it has none (or the store cannot be reached).
func (b *BFF) session(r *http.Request) *Session {
	if o, ok := r.Context().Value(sessionCtxKey{}).(sessionOverride); ok {
		return o.s
	}
	c, err := r.Cookie(b.cookieName())
	if err != nil || c.Value == "" {
		return nil
	}
	s, err := b.sessions.Get(c.Value)
	if err != nil {
		return nil
	}
	return s
}

// save stores the session and (re)issues its cookie on h, which is either the response writer's header map or the
// header of a proxied response. A nil session is created.
func (b *BFF) save(h http.Header, s *Session) (*Session, error) {
	if s == nil {
		s = &Session{}
	}
	if s.ID == "" {
		s.ID = randomToken()
	}
	if err := b.sessions.Put(s, b.cfg.sessionTTL); err != nil {
		return s, err
	}
	h.Add("Set-Cookie", b.sessionCookie(s.ID, int(b.cfg.sessionTTL.Seconds())).String())
	return s, nil
}

func (b *BFF) end(h http.Header, s *Session) {
	if s != nil {
		b.sessions.Delete(s.ID)
	}
	h.Add("Set-Cookie", b.sessionCookie("", -1).String())
}

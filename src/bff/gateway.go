package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strings"
)

// Gateway mode: a PingGateway sidecar in the same pod is the OpenID Connect client. It owns the sign-in redirect,
// the callback, the tokens and sign-out at the provider, and tells the BFF who is shopping in one signed header
// (a JWT, HS256) on every request it forwards over localhost. The BFF then holds no client secret and no token; it
// keeps what is about the shop: the anonymous session, rewards, saved cards, personalization.
//
// Turned on by GATEWAY_IDENTITY_KEY (base64). See kustomize/components/identity-pinggateway.

const gatewayHeader = "X-Gateway-Identity"

type gatewayIdentity struct {
	Sub       string `json:"sub"`
	Name      string `json:"name"`
	GivenName string `json:"given_name"`
	Family    string `json:"family_name"`
	Username  string `json:"preferred_username"`
	Email     string `json:"email"`
}

// verifyGatewayJWT checks the sidecar's signature and returns who it vouches for.
func verifyGatewayJWT(token string, key []byte) (*gatewayIdentity, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("not a signed JWT")
	}
	head, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, err
	}
	var h struct {
		Alg string `json:"alg"`
	}
	if json.Unmarshal(head, &h) != nil || h.Alg != "HS256" {
		return nil, errors.New("unexpected algorithm")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(parts[0] + "." + parts[1]))
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return nil, errors.New("bad signature")
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	id := &gatewayIdentity{}
	if err := json.Unmarshal(body, id); err != nil {
		return nil, err
	}
	if id.Sub == "" {
		return nil, errors.New("no subject")
	}
	return id, nil
}

type sessionCtxKey struct{}

// sessionOverride lets the gateway middleware hand the handlers a session other than the one the request's cookie
// names: the fresh one made at sign-in, or none once the gateway's session is gone.
type sessionOverride struct{ s *Session }

// withGateway lines the BFF's own session up with what the gateway says on each request:
//   - the gateway vouches for someone this session is not yet signed in as: a sign-in just finished at the sidecar.
//     The anonymous session becomes a signed-in one under a new id, exactly as after the BFF's own callback.
//   - the gateway vouches for nobody but this session is signed in: the gateway's session ended, so ours does too.
func (b *BFF) withGateway(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := r.Header.Get(gatewayHeader)
		r.Header.Del(gatewayHeader) // goes no further than the BFF
		if strings.HasPrefix(r.URL.Path, "/static/") || r.URL.Path == "/bff/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		var id *gatewayIdentity
		if raw != "" {
			var err error
			if id, err = verifyGatewayJWT(raw, b.cfg.gatewayKey); err != nil {
				log.Printf("gateway identity refused: %v", err)
			}
		}
		s := b.session(r)
		switch {
		case id != nil && (s == nil || s.Sub != id.Sub):
			anon := s
			if anon == nil || anon.signedIn() { // a different member's session is not carried over
				anon = &Session{}
			}
			if s != nil {
				b.sessions.Delete(s.ID)
			}
			name := firstNonEmpty(id.Name, strings.TrimSpace(id.GivenName+" "+id.Family), id.Username, id.Email, "Member")
			fresh := &Session{Sub: id.Sub, Name: name, GivenName: firstNonEmpty(id.GivenName, strings.Fields(name)[0]),
				Email: id.Email}
			b.welcome(r.Context(), w, fresh, anon)
			if _, err := b.save(w.Header(), fresh); err != nil {
				log.Printf("session store: %v", err)
			}
			s = fresh
		case id == nil && s.signedIn():
			b.end(w.Header(), s)
			s = nil
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), sessionCtxKey{}, sessionOverride{s})))
	})
}

// gatewayLogin sends the shopper to the sidecar's sign-in endpoint. The sidecar comes back to return_to, where
// withGateway finds the new identity. signup=1 asks the sidecar to add the provider's sign-up parameters.
func (b *BFF) gatewayLogin(w http.ResponseWriter, r *http.Request) {
	q := url.Values{"registration": {b.cfg.clientID}, "goto": {localPath(r.URL.Query().Get("return_to"))}}
	if r.URL.Path == "/auth/register" {
		q.Set("signup", "1")
	}
	http.Redirect(w, r, "/auth/login?"+q.Encode(), http.StatusFound)
}

// gatewayLogout ends the shop's side, then lets the sidecar end its own session and the provider's.
func (b *BFF) gatewayLogout(w http.ResponseWriter, r *http.Request) {
	if s := b.session(r); s != nil { // withGateway may already have ended it
		b.end(w.Header(), s)
	}
	for _, name := range []string{"shop_session-id", "shop_currency"} {
		http.SetCookie(w, expiring(&http.Cookie{Name: name, Path: "/", MaxAge: -1}))
	}
	http.Redirect(w, r, "/auth/logout?goto=%2F", http.StatusSeeOther)
}

// gatewayFailed is where the sidecar sends a shopper whose sign-in did not complete.
func (b *BFF) gatewayFailed(w http.ResponseWriter, r *http.Request) {
	s := b.session(r)
	if s == nil {
		s = &Session{}
	}
	s.Notice = "Sign-in did not complete. You can keep shopping and try again."
	if _, err := b.save(w.Header(), s); err != nil {
		log.Printf("session store: %v", err)
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

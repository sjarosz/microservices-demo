package main

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// oidcClient talks to whichever OpenID Connect provider OIDC_ISSUER names. Everything it needs comes from the
// provider's discovery document, which is read on first use so the shop starts even while the provider is down.
type oidcClient struct {
	cfg config

	mu         sync.Mutex
	oauth      *oauth2.Config
	provider   *oidc.Provider
	verifier   *oidc.IDTokenVerifier
	endSession string
}

func (o *oidcClient) ready() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.provider != nil {
		return nil
	}
	p, err := oidc.NewProvider(context.Background(), o.cfg.issuer)
	if err != nil {
		return fmt.Errorf("OIDC discovery at %s: %w", o.cfg.issuer, err)
	}
	var meta struct {
		EndSession string `json:"end_session_endpoint"`
	}
	_ = p.Claims(&meta)
	o.provider, o.endSession = p, meta.EndSession
	o.verifier = p.Verifier(&oidc.Config{ClientID: o.cfg.clientID})
	o.oauth = &oauth2.Config{ClientID: o.cfg.clientID, ClientSecret: o.cfg.clientSecret, Endpoint: p.Endpoint(),
		RedirectURL: o.cfg.publicURL.String() + "/auth/callback", Scopes: o.cfg.scopes}
	return nil
}

// localPath keeps redirects after sign-in on this site.
func localPath(v string) string {
	if !strings.HasPrefix(v, "/") || strings.HasPrefix(v, "//") || strings.ContainsAny(v, "\\\r\n") {
		return "/"
	}
	return v
}

// login starts the authorization code flow with PKCE. /auth/register is the same flow with the parameters that ask
// the provider for its sign-up page (OIDC_REGISTER_PARAMS, by default the standard prompt=create).
func (b *BFF) login(w http.ResponseWriter, r *http.Request) {
	if b.oidc == nil {
		http.Error(w, "sign-in is not configured", http.StatusNotFound)
		return
	}
	if err := b.oidc.ready(); err != nil {
		log.Print(err)
		http.Error(w, "sign-in is unavailable right now, please try again", http.StatusServiceUnavailable)
		return
	}
	s := b.session(r)
	if s == nil {
		s = &Session{}
	}
	s.Pending = &pendingAuth{State: randomToken(), Nonce: randomToken(), Verifier: oauth2.GenerateVerifier(),
		ReturnTo: localPath(r.URL.Query().Get("return_to"))}
	if _, err := b.save(w.Header(), s); err != nil {
		log.Printf("session store: %v", err)
		http.Error(w, "sign-in is unavailable right now, please try again", http.StatusServiceUnavailable)
		return
	}
	opts := []oauth2.AuthCodeOption{oidc.Nonce(s.Pending.Nonce), oauth2.S256ChallengeOption(s.Pending.Verifier)}
	if r.URL.Path == "/auth/register" {
		for k, v := range b.cfg.registerParam {
			opts = append(opts, oauth2.SetAuthURLParam(k, v[0]))
		}
	}
	http.Redirect(w, r, b.oidc.oauth.AuthCodeURL(s.Pending.State, opts...), http.StatusFound)
}

func (b *BFF) callback(w http.ResponseWriter, r *http.Request) {
	s := b.session(r)
	if b.oidc == nil || s == nil || s.Pending == nil {
		http.Redirect(w, r, "/", http.StatusFound) // a stale or replayed callback: nothing to finish
		return
	}
	pending := s.Pending
	s.Pending = nil
	q := r.URL.Query()
	fail := func(why string) {
		log.Printf("sign-in failed: %s", why)
		s.Notice = "Sign-in did not complete. You can keep shopping and try again."
		b.save(w.Header(), s)
		http.Redirect(w, r, pending.ReturnTo, http.StatusFound)
	}
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(pending.State)) != 1 {
		fail("state mismatch")
		return
	}
	if e := q.Get("error"); e != "" { // e.g. the shopper cancelled at the provider
		fail(e + " " + q.Get("error_description"))
		return
	}
	if err := b.oidc.ready(); err != nil {
		fail(err.Error())
		return
	}
	tok, err := b.oidc.oauth.Exchange(r.Context(), q.Get("code"), oauth2.VerifierOption(pending.Verifier))
	if err != nil {
		fail("code exchange: " + err.Error())
		return
	}
	raw, _ := tok.Extra("id_token").(string)
	idt, err := b.oidc.verifier.Verify(r.Context(), raw)
	if err != nil {
		fail("id_token: " + err.Error())
		return
	}
	if idt.Nonce != pending.Nonce {
		fail("nonce mismatch")
		return
	}
	var c struct {
		Name      string `json:"name"`
		GivenName string `json:"given_name"`
		Family    string `json:"family_name"`
		Username  string `json:"preferred_username"`
		Email     string `json:"email"`
	}
	_ = idt.Claims(&c)
	if c.Email == "" || (c.Name == "" && c.GivenName == "") { // some providers keep these for the userinfo endpoint
		if ui, err := b.oidc.provider.UserInfo(r.Context(), oauth2.StaticTokenSource(tok)); err == nil {
			_ = ui.Claims(&c)
		}
	}
	name := firstNonEmpty(c.Name, strings.TrimSpace(c.GivenName+" "+c.Family), c.Username, c.Email, "Member")
	given := firstNonEmpty(c.GivenName, strings.Fields(name)[0])

	// A fresh session id for the signed-in session, so an id seen before sign-in is worth nothing afterwards.
	b.sessions.Delete(s.ID)
	fresh := &Session{Sub: idt.Subject, Name: name, GivenName: given, Email: c.Email, IDToken: raw}
	b.welcome(r.Context(), w, fresh, s)
	if _, err := b.save(w.Header(), fresh); err != nil {
		log.Printf("session store: %v", err)
	}
	http.Redirect(w, r, pending.ReturnTo, http.StatusFound)
}

// welcome moves what the anonymous session gathered into the customer's record: products viewed, and the points of
// guest orders placed in this browser. It also applies the preferred currency.
func (b *BFF) welcome(ctx context.Context, w http.ResponseWriter, fresh, anon *Session) {
	cust, err := b.customer.SignedIn(ctx, fresh.Sub, fresh.Email, fresh.Name)
	if err != nil {
		log.Printf("customerservice: %v", err) // signed in all the same; the account features degrade
		return
	}
	var notes []string
	if cust.Created {
		notes = append(notes, fmt.Sprintf("Welcome to Boutique Rewards, %s! %d bonus points are in your account.",
			fresh.GivenName, cust.Points))
	}
	claimed := 0
	for _, o := range anon.GuestOrders {
		before := cust.Points
		if c2, err := b.customer.AddOrder(ctx, fresh.Sub, o, true); err == nil {
			claimed += c2.Points - before
			cust = c2
		}
	}
	if claimed > 0 {
		notes = append(notes, fmt.Sprintf("%d points from your guest order were added.", claimed))
	}
	if len(anon.Viewed) > 0 {
		oldestFirst := make([]string, 0, len(anon.Viewed))
		for i := len(anon.Viewed) - 1; i >= 0; i-- {
			oldestFirst = append(oldestFirst, anon.Viewed[i])
		}
		_ = b.customer.AddViews(ctx, fresh.Sub, oldestFirst)
	}
	if cur := cust.Preferences.Currency; cur != "" {
		http.SetCookie(w, &http.Cookie{Name: "shop_currency", Value: cur, Path: "/", MaxAge: 60 * 60 * 48})
	}
	fresh.Notice = strings.Join(notes, " ")
	fresh.Results = anon.Results
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// logout ends the BFF session, drops the shop's own cookies (so the next person at this browser starts with an empty
// cart) and, when the provider supports it, ends the session there too (RP-initiated logout).
func (b *BFF) logout(w http.ResponseWriter, r *http.Request) {
	if !b.sameOrigin(r) {
		http.Error(w, "cross-site request refused", http.StatusForbidden)
		return
	}
	s := b.session(r)
	b.end(w.Header(), s)
	for _, name := range []string{"shop_session-id", "shop_currency"} {
		http.SetCookie(w, &http.Cookie{Name: name, Path: "/", MaxAge: -1})
	}
	home := b.cfg.publicURL.String() + "/"
	if b.oidc != nil && s.signedIn() && b.oidc.ready() == nil && b.oidc.endSession != "" {
		if u, err := url.Parse(b.oidc.endSession); err == nil {
			q := u.Query()
			q.Set("id_token_hint", s.IDToken)
			q.Set("client_id", b.cfg.clientID)
			q.Set("post_logout_redirect_uri", home)
			u.RawQuery = q.Encode()
			http.Redirect(w, r, u.String(), http.StatusSeeOther)
			return
		}
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// sameOrigin refuses state-changing requests that another site made the browser send.
func (b *BFF) sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		origin = r.Header.Get("Referer")
	}
	u, err := url.Parse(origin)
	return err == nil && u.Host == b.cfg.publicURL.Host
}

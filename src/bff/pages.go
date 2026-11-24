package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const contextHeader = "X-Bff-Context"

// pageContext is all the frontend learns about the shopper. It travels in one signed request header; the frontend
// renders from it and needs no session, token or customer lookup of its own.
type pageContext struct {
	AuthEnabled    bool            `json:"auth_enabled"`
	SignedIn       bool            `json:"signed_in"`
	CheckoutMode   string          `json:"checkout_mode,omitempty"` // "guest" once the shopper chose a one-time purchase
	Notice         string          `json:"notice,omitempty"`
	User           *ctxUser        `json:"user,omitempty"`
	Rewards        *ctxRewards     `json:"rewards,omitempty"`
	PaymentMethods []paymentMethod `json:"payment_methods,omitempty"`
	Address        *address        `json:"address,omitempty"`
	Preferences    *preferences    `json:"preferences,omitempty"`
	Strips         []ctxStrip      `json:"strips,omitempty"`       // personalised product rows for the home page
	GuestPoints    int             `json:"guest_points,omitempty"` // points a guest would get by creating an account
	Orders         []customerOrder `json:"orders,omitempty"`       // account page only
	Ledger         []ledgerEntry   `json:"ledger,omitempty"`       // account page only
}

type ctxUser struct {
	Name      string `json:"name"`
	GivenName string `json:"given_name"`
	Email     string `json:"email"`
}

type ctxRewards struct {
	Tier           string  `json:"tier"`
	Points         int     `json:"points"`
	LifetimePoints int     `json:"lifetime_points"`
	EarnRate       float64 `json:"earn_rate"`
	NextTier       string  `json:"next_tier,omitempty"`
	PointsToNext   int     `json:"points_to_next,omitempty"`
}

type ctxStrip struct {
	Title      string   `json:"title"`
	ProductIDs []string `json:"product_ids"`
}

func (b *BFF) sign(c pageContext) string {
	raw, _ := json.Marshal(c)
	body := base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, b.cfg.contextKey)
	mac.Write([]byte(body))
	return body + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// buildContext assembles the context for one page request. full adds the history shown on the account page.
func (b *BFF) buildContext(r *http.Request, s *Session, full bool) pageContext {
	c := pageContext{AuthEnabled: b.oidc != nil || b.cfg.gatewayKey != nil}
	if r.URL.Path == "/cart" && r.URL.Query().Get("checkout") == "guest" {
		c.CheckoutMode = "guest"
	}
	if s == nil {
		return c
	}
	c.Notice = s.Notice
	if !s.signedIn() {
		if len(s.Viewed) > 0 {
			c.Strips = []ctxStrip{{Title: "Recently viewed", ProductIDs: head(s.Viewed, 3)}}
		}
		for _, o := range s.GuestOrders {
			c.GuestPoints += int(math.Floor(o.TotalUSD))
		}
		return c
	}
	c.SignedIn = true
	c.User = &ctxUser{Name: s.Name, GivenName: s.GivenName, Email: s.Email}
	cust, err := b.customer.Get(r.Context(), s.Sub)
	if errors.Is(err, errNoCustomer) { // e.g. the customer store was reset while the session lived on
		cust, err = b.customer.SignedIn(r.Context(), s.Sub, s.Email, s.Name)
	}
	if err != nil {
		log.Printf("customerservice: %v", err)
		return c
	}
	c.Rewards = &ctxRewards{Tier: cust.Tier.Name, Points: cust.Points, LifetimePoints: cust.LifetimePoints,
		EarnRate: cust.Tier.EarnRate, NextTier: cust.Tier.NextName, PointsToNext: cust.Tier.PointsToNext}
	c.PaymentMethods, c.Address, c.Preferences = cust.PaymentMethods, cust.Address, &cust.Preferences
	if len(cust.Viewed) > 0 {
		c.Strips = append(c.Strips, ctxStrip{Title: "Pick up where you left off, " + s.GivenName,
			ProductIDs: head(cust.Viewed, 3)})
	}
	if again := bought(cust.Orders); len(again) > 0 {
		c.Strips = append(c.Strips, ctxStrip{Title: "Buy it again", ProductIDs: head(again, 3)})
	}
	if full {
		c.Orders, c.Ledger = cust.Orders, cust.Ledger
		if len(c.Orders) > 10 {
			c.Orders = c.Orders[:10]
		}
		if len(c.Ledger) > 10 {
			c.Ledger = c.Ledger[:10]
		}
	}
	return c
}

func head(list []string, n int) []string {
	if len(list) > n {
		return list[:n]
	}
	return list
}

func bought(orders []customerOrder) []string {
	var out []string
	seen := map[string]bool{}
	for _, o := range orders {
		for _, id := range o.Items {
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	return out
}

type ctxKey struct{}

// reqState carries what afterFrontend needs to know about the request it is answering.
type reqState struct {
	session  *Session
	checkout *checkoutIntent
}

type checkoutIntent struct {
	newCard      *card
	rememberCard bool
	address      *address
}

// forward sends the request to the frontend with a freshly signed context (and never the browser's own copy of that
// header, which is dropped here).
func (b *BFF) forward(w http.ResponseWriter, r *http.Request, s *Session, c pageContext, st *reqState) {
	r.Header.Del(contextHeader)
	r.Header.Set(contextHeader, b.sign(c))
	if st == nil {
		st = &reqState{}
	}
	st.session = s
	b.proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, st)))
}

// passThrough serves assets: no session, no context.
func (b *BFF) passThrough(w http.ResponseWriter, r *http.Request) {
	r.Header.Del(contextHeader)
	b.proxy.ServeHTTP(w, r)
}

// page is every request the BFF has no special handling for.
func (b *BFF) page(w http.ResponseWriter, r *http.Request) {
	s := b.session(r)
	c := b.buildContext(r, s, false)
	if s != nil && s.Notice != "" && r.Method == http.MethodGet { // a notice is shown once
		s.Notice = ""
		b.save(w.Header(), s)
	}
	b.forward(w, r, s, c, nil)
}

func (b *BFF) account(w http.ResponseWriter, r *http.Request) {
	s := b.session(r)
	if !s.signedIn() {
		http.Redirect(w, r, "/auth/login?return_to=/account", http.StatusFound)
		return
	}
	c := b.buildContext(r, s, true)
	if s.Notice != "" {
		s.Notice = ""
		b.save(w.Header(), s)
	}
	b.forward(w, r, s, c, nil)
}

// signedIn wraps the account actions: a session, and a request that came from our own pages.
func (b *BFF) signedIn(h func(http.ResponseWriter, *http.Request, *Session) string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s := b.session(r)
		if !s.signedIn() {
			http.Redirect(w, r, "/auth/login?return_to=/account", http.StatusSeeOther)
			return
		}
		if !b.sameOrigin(r) {
			http.Error(w, "cross-site request refused", http.StatusForbidden)
			return
		}
		if to := h(w, r, s); to != "" {
			http.Redirect(w, r, to, http.StatusSeeOther)
		}
	}
}

func (b *BFF) savePreferences(w http.ResponseWriter, r *http.Request, s *Session) string {
	prefs := map[string]any{"marketing": r.FormValue("marketing") == "on"}
	if cur := r.FormValue("currency"); cur != "" {
		prefs["currency"] = cur
		http.SetCookie(w, &http.Cookie{Name: "shop_currency", Value: cur, Path: "/", MaxAge: 60 * 60 * 48})
	}
	s.Notice = "Your preferences were saved."
	if err := b.customer.SavePreferences(r.Context(), s.Sub, prefs); err != nil {
		log.Printf("customerservice: %v", err)
		s.Notice = "Your preferences could not be saved. Please try again."
	}
	b.save(w.Header(), s)
	return "/account"
}

func (b *BFF) deletePayment(w http.ResponseWriter, r *http.Request, s *Session) string {
	s.Notice = "The card was removed."
	if err := b.customer.DeleteCard(r.Context(), s.Sub, r.PathValue("id")); err != nil {
		log.Printf("customerservice: %v", err)
		s.Notice = "The card could not be removed. Please try again."
	}
	b.save(w.Header(), s)
	return "/account"
}

// deleteData erases what the shop holds about the customer and signs them out. Their account at the identity
// provider is the provider's to manage.
func (b *BFF) deleteData(w http.ResponseWriter, r *http.Request, s *Session) string {
	if err := b.customer.Forget(r.Context(), s.Sub); err != nil {
		log.Printf("customerservice: %v", err)
		s.Notice = "Your data could not be deleted. Please try again."
		b.save(w.Header(), s)
		return "/account"
	}
	b.logout(w, r)
	return ""
}

// setCurrency lets the frontend handle the change and remembers it as the customer's preferred currency.
func (b *BFF) setCurrency(w http.ResponseWriter, r *http.Request) {
	s := b.session(r)
	form := b.readForm(w, r)
	if form == nil {
		return
	}
	if cur := form.Get("currency_code"); s.signedIn() && cur != "" {
		if err := b.customer.SavePreferences(r.Context(), s.Sub, map[string]any{"currency": cur}); err != nil {
			log.Printf("customerservice: %v", err)
		}
	}
	b.forward(w, r, s, b.buildContext(r, s, false), nil)
}

// readForm parses a posted form and puts the body back so it can be forwarded.
func (b *BFF) readForm(w http.ResponseWriter, r *http.Request) url.Values {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return nil
	}
	setForm(r, r.PostForm)
	return r.PostForm
}

func setForm(r *http.Request, form url.Values) {
	enc := form.Encode()
	r.Body = io.NopCloser(strings.NewReader(enc))
	r.ContentLength = int64(len(enc))
	r.Header.Set("Content-Length", strconv.Itoa(len(enc)))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
}

// checkout is where identity meets the purchase. The frontend's checkout form and handler stay as they are; the BFF
// fills in a saved card when one was chosen, and afterwards records the order for the customer (or keeps a guest
// order claimable in the session).
func (b *BFF) checkout(w http.ResponseWriter, r *http.Request) {
	s := b.session(r)
	form := b.readForm(w, r)
	if form == nil {
		return
	}
	st := &reqState{checkout: &checkoutIntent{}}
	if s.signedIn() {
		if id := form.Get("saved_payment_id"); id != "" && id != "new" {
			k, err := b.customer.Card(r.Context(), s.Sub, id)
			if err != nil {
				log.Printf("customerservice: %v", err)
				s.Notice = "That saved card could not be used. Please enter a card."
				b.save(w.Header(), s)
				http.Redirect(w, r, "/cart", http.StatusSeeOther)
				return
			}
			form.Set("credit_card_number", k.Number)
			form.Set("credit_card_expiration_month", strconv.Itoa(k.ExpMonth))
			form.Set("credit_card_expiration_year", strconv.Itoa(k.ExpYear))
			// A card on file is charged without its security code, which is never stored. The stock checkout form
			// still requires one, so it gets a placeholder.
			form.Set("credit_card_cvv", "999")
		} else {
			month, _ := strconv.Atoi(form.Get("credit_card_expiration_month"))
			year, _ := strconv.Atoi(form.Get("credit_card_expiration_year"))
			st.checkout.newCard = &card{Number: form.Get("credit_card_number"), ExpMonth: month, ExpYear: year}
			st.checkout.rememberCard = form.Get("remember_payment") == "on"
		}
		if form.Get("remember_address") == "on" {
			st.checkout.address = &address{Street: form.Get("street_address"), City: form.Get("city"),
				State: form.Get("state"), Zip: form.Get("zip_code"), Country: form.Get("country")}
		}
	}
	for _, k := range []string{"saved_payment_id", "remember_payment", "remember_address"} {
		form.Del(k) // the BFF's own fields stop here
	}
	setForm(r, form)
	b.forward(w, r, s, b.buildContext(r, s, false), st)
}

// afterFrontend looks at what the frontend answered. Two things matter to identity: an order that went through
// (the frontend reports it in X-Order-* headers, which are consumed here) and a product page that was viewed.
func (b *BFF) afterFrontend(res *http.Response) error {
	r := res.Request
	st, _ := r.Context().Value(ctxKey{}).(*reqState)
	orderID := res.Header.Get("X-Order-Id")
	total, _ := strconv.ParseFloat(res.Header.Get("X-Order-Total-Usd"), 64)
	var items []string
	if v := res.Header.Get("X-Order-Items"); v != "" {
		items = strings.Split(v, ",")
	}
	for _, h := range []string{"X-Order-Id", "X-Order-Total-Usd", "X-Order-Items"} {
		res.Header.Del(h)
	}
	if st == nil || res.StatusCode != http.StatusOK {
		return nil
	}
	s := st.session
	switch {
	case st.checkout != nil && orderID != "":
		s = b.recordOrder(r.Context(), s, st.checkout, orderEvent{OrderID: orderID, TotalUSD: total, Items: items})
		if _, err := b.save(res.Header, s); err != nil {
			log.Printf("session store: %v", err)
		}
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/product/"):
		id := strings.TrimPrefix(r.URL.Path, "/product/")
		if s.signedIn() {
			go func() {
				if err := b.customer.AddViews(context.Background(), s.Sub, []string{id}); err != nil {
					log.Printf("customerservice: %v", err)
				}
			}()
			return nil
		}
		if s == nil {
			s = &Session{}
		}
		s.Viewed = append([]string{id}, without(s.Viewed, id)...)
		s.Viewed = head(s.Viewed, 8)
		if _, err := b.save(res.Header, s); err != nil {
			log.Printf("session store: %v", err)
		}
	}
	return nil
}

func without(list []string, id string) []string {
	out := make([]string, 0, len(list))
	for _, x := range list {
		if x != id {
			out = append(out, x)
		}
	}
	return out
}

func (b *BFF) recordOrder(ctx context.Context, s *Session, in *checkoutIntent, o orderEvent) *Session {
	if s == nil {
		s = &Session{}
	}
	if s.Results == nil {
		s.Results = map[string]orderResult{}
	}
	if !s.signedIn() {
		// A one-time purchase: nothing about the buyer is stored. The order stays claimable in this browser's
		// session, so creating an account right after still earns its points.
		s.GuestOrders = append(s.GuestOrders, o)
		s.Results[o.OrderID] = orderResult{ClaimablePoints: int(math.Floor(o.TotalUSD))}
		return s
	}
	res := orderResult{SignedIn: true}
	before, err := b.customer.Get(ctx, s.Sub)
	if err == nil {
		var after *customer
		if after, err = b.customer.AddOrder(ctx, s.Sub, o, false); err == nil {
			res.PointsEarned, res.Balance, res.Tier = after.Points-before.Points, after.Points, after.Tier.Name
		}
	}
	if err != nil {
		log.Printf("customerservice: %v", err)
	}
	if in.rememberCard && in.newCard != nil {
		if err := b.customer.SaveCard(ctx, s.Sub, *in.newCard); err != nil {
			log.Printf("customerservice: %v", err)
		} else {
			res.CardSaved = true
		}
	}
	if in.address != nil {
		if err := b.customer.SaveAddress(ctx, s.Sub, *in.address); err != nil {
			log.Printf("customerservice: %v", err)
		}
	}
	s.Results[o.OrderID] = res
	return s
}

// orderResult answers the confirmation page's question: what did this order earn?
func (b *BFF) orderResult(w http.ResponseWriter, r *http.Request) {
	s := b.session(r)
	if s != nil {
		if res, ok := s.Results[r.PathValue("id")]; ok {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			json.NewEncoder(w).Encode(res)
			return
		}
	}
	http.Error(w, "not found", http.StatusNotFound)
}

// customerservice keeps what the shop knows about a signed-in customer: reward points, saved payment methods,
// a shipping address, preferences and recently viewed products. It is keyed by the identity provider's subject
// and holds no credentials: who the caller is has already been settled by the BFF, the only client of this API.
package main

import (
	"encoding/json"
	"errors"
	"log"
	"math"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	welcomeBonus = 100
	maxViewed    = 8
	maxLedger    = 50
)

type PaymentMethod struct {
	ID       string    `json:"id"`
	Brand    string    `json:"brand"`
	Last4    string    `json:"last4"`
	ExpMonth int       `json:"exp_month"`
	ExpYear  int       `json:"exp_year"`
	AddedAt  time.Time `json:"added_at"`
	Sealed   string    `json:"sealed,omitempty"` // the card number, encrypted; never leaves in a customer view
}

type Address struct {
	Street  string `json:"street"`
	City    string `json:"city"`
	State   string `json:"state"`
	Zip     string `json:"zip"`
	Country string `json:"country"`
}

type Preferences struct {
	Currency  string `json:"currency"`
	Marketing bool   `json:"marketing"`
}

type LedgerEntry struct {
	At          time.Time `json:"at"`
	Description string    `json:"description"`
	Points      int       `json:"points"`
}

type Order struct {
	ID       string    `json:"id"`
	At       time.Time `json:"at"`
	TotalUSD float64   `json:"total_usd"`
	Points   int       `json:"points"`
	Items    []string  `json:"items"`
	Guest    bool      `json:"guest"` // placed as a guest and claimed after sign-in
}

type Customer struct {
	Sub            string          `json:"sub"`
	Email          string          `json:"email"`
	Name           string          `json:"name"`
	CreatedAt      time.Time       `json:"created_at"`
	Points         int             `json:"points"`
	LifetimePoints int             `json:"lifetime_points"`
	Ledger         []LedgerEntry   `json:"ledger"`
	Orders         []Order         `json:"orders"`
	PaymentMethods []PaymentMethod `json:"payment_methods"`
	Address        *Address        `json:"address,omitempty"`
	Preferences    Preferences     `json:"preferences"`
	Viewed         []string        `json:"viewed"`
}

// Tier is derived from lifetime points, never stored.
type Tier struct {
	Name         string  `json:"name"`
	EarnRate     float64 `json:"earn_rate"` // points per US dollar
	NextName     string  `json:"next_name,omitempty"`
	PointsToNext int     `json:"points_to_next,omitempty"`
}

var tiers = []struct {
	name string
	from int
	rate float64
}{{"Bronze", 0, 1}, {"Silver", 500, 1.25}, {"Gold", 2000, 1.5}}

func tierFor(lifetime int) Tier {
	t := Tier{}
	for i, x := range tiers {
		if lifetime >= x.from {
			t = Tier{Name: x.name, EarnRate: x.rate}
			if i+1 < len(tiers) {
				t.NextName, t.PointsToNext = tiers[i+1].name, tiers[i+1].from-lifetime
			}
		}
	}
	return t
}

type customerView struct {
	*Customer
	Tier    Tier `json:"tier"`
	Created bool `json:"created,omitempty"`
}

func view(c *Customer, created bool) customerView {
	out := *c
	out.PaymentMethods = make([]PaymentMethod, len(c.PaymentMethods))
	for i, p := range c.PaymentMethods {
		p.Sealed = ""
		out.PaymentMethods[i] = p
	}
	return customerView{Customer: &out, Tier: tierFor(c.LifetimePoints), Created: created}
}

func (c *Customer) award(points int, why string) {
	if points == 0 {
		return
	}
	c.Points += points
	c.LifetimePoints += points
	c.Ledger = append([]LedgerEntry{{At: time.Now().UTC(), Description: why, Points: points}}, c.Ledger...)
	if len(c.Ledger) > maxLedger {
		c.Ledger = c.Ledger[:maxLedger]
	}
}

type server struct {
	store Store
	vault *vault
	token string
}

var errNotFound = errors.New("not found")

func main() {
	v, err := newVault(os.Getenv("VAULT_KEY"))
	if err != nil {
		log.Fatalf("VAULT_KEY: %v", err)
	}
	s := &server{store: newStore(os.Getenv("REDIS_ADDR")), vault: v, token: os.Getenv("INTERNAL_API_TOKEN")}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /customers/{sub}", s.guard(s.get))
	mux.HandleFunc("DELETE /customers/{sub}", s.guard(s.forget))
	mux.HandleFunc("PUT /customers/{sub}/profile", s.guard(s.putProfile))
	mux.HandleFunc("PUT /customers/{sub}/address", s.guard(s.putAddress))
	mux.HandleFunc("PUT /customers/{sub}/preferences", s.guard(s.putPreferences))
	mux.HandleFunc("POST /customers/{sub}/views", s.guard(s.addView))
	mux.HandleFunc("POST /customers/{sub}/orders", s.guard(s.addOrder))
	mux.HandleFunc("POST /customers/{sub}/payment-methods", s.guard(s.addPayment))
	mux.HandleFunc("DELETE /customers/{sub}/payment-methods/{id}", s.guard(s.deletePayment))
	mux.HandleFunc("GET /customers/{sub}/payment-methods/{id}/card", s.guard(s.revealPayment))
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("customerservice listening on :%s (store=%s)", port, s.store.Name())
	log.Fatal(http.ListenAndServe(":"+port, mux))
}

// guard admits only the BFF (a shared internal token; in-cluster traffic only, the Service is ClusterIP).
func (s *server) guard(h func(http.ResponseWriter, *http.Request) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.token != "" && r.Header.Get("Authorization") != "Bearer "+s.token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		out, err := h(w, r)
		switch {
		case errors.Is(err, errNotFound):
			http.Error(w, "not found", http.StatusNotFound)
		case err != nil:
			var bad badRequest
			if errors.As(err, &bad) {
				http.Error(w, string(bad), http.StatusBadRequest)
				return
			}
			log.Printf("%s %s: %v", r.Method, r.URL.Path, err)
			http.Error(w, "internal error", http.StatusInternalServerError)
		default:
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(out)
		}
	}
}

type badRequest string

func (b badRequest) Error() string { return string(b) }

func body[T any](r *http.Request) (T, error) {
	var v T
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<16)).Decode(&v); err != nil {
		return v, badRequest("invalid JSON body")
	}
	return v, nil
}

// change loads the customer, applies fn and stores the result. The customer must exist unless create is set.
func (s *server) change(sub string, create bool, fn func(c *Customer, created bool) error) (customerView, error) {
	var out customerView
	err := s.store.Update(sub, func(c *Customer) (*Customer, error) {
		created := false
		if c == nil {
			if !create {
				return nil, errNotFound
			}
			c, created = &Customer{Sub: sub, CreatedAt: time.Now().UTC()}, true
		}
		if err := fn(c, created); err != nil {
			return nil, err
		}
		out = view(c, created)
		return c, nil
	})
	return out, err
}

func (s *server) get(_ http.ResponseWriter, r *http.Request) (any, error) {
	c, err := s.store.Get(r.PathValue("sub"))
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, errNotFound
	}
	return view(c, false), nil
}

// forget removes everything held about the customer (the account at the identity provider is not ours to delete).
func (s *server) forget(_ http.ResponseWriter, r *http.Request) (any, error) {
	return map[string]bool{"deleted": true}, s.store.Delete(r.PathValue("sub"))
}

// putProfile is called at every sign-in: it creates the customer on first sight (with a welcome bonus) and keeps
// the name and e-mail in step with the identity provider.
func (s *server) putProfile(_ http.ResponseWriter, r *http.Request) (any, error) {
	in, err := body[struct{ Email, Name string }](r)
	if err != nil {
		return nil, err
	}
	return s.change(r.PathValue("sub"), true, func(c *Customer, created bool) error {
		c.Email, c.Name = in.Email, in.Name
		if created {
			c.award(welcomeBonus, "Welcome bonus")
		}
		return nil
	})
}

func (s *server) putAddress(_ http.ResponseWriter, r *http.Request) (any, error) {
	in, err := body[Address](r)
	if err != nil {
		return nil, err
	}
	return s.change(r.PathValue("sub"), false, func(c *Customer, _ bool) error { c.Address = &in; return nil })
}

func (s *server) putPreferences(_ http.ResponseWriter, r *http.Request) (any, error) {
	in, err := body[struct {
		Currency  *string `json:"currency"`
		Marketing *bool   `json:"marketing"`
	}](r)
	if err != nil {
		return nil, err
	}
	return s.change(r.PathValue("sub"), false, func(c *Customer, _ bool) error {
		if in.Currency != nil {
			c.Preferences.Currency = *in.Currency
		}
		if in.Marketing != nil {
			c.Preferences.Marketing = *in.Marketing
		}
		return nil
	})
}

func (s *server) addView(_ http.ResponseWriter, r *http.Request) (any, error) {
	in, err := body[struct {
		ProductIDs []string `json:"product_ids"`
	}](r)
	if err != nil {
		return nil, err
	}
	return s.change(r.PathValue("sub"), false, func(c *Customer, _ bool) error {
		for _, id := range in.ProductIDs { // oldest first, so the last one ends up in front
			c.Viewed = prepend(c.Viewed, id, maxViewed)
		}
		return nil
	})
}

func prepend(list []string, id string, max int) []string {
	out := []string{id}
	for _, x := range list {
		if x != id && len(out) < max {
			out = append(out, x)
		}
	}
	return out
}

// addOrder records an order and awards its points once, however often it is reported.
func (s *server) addOrder(_ http.ResponseWriter, r *http.Request) (any, error) {
	in, err := body[struct {
		OrderID  string   `json:"order_id"`
		TotalUSD float64  `json:"total_usd"`
		Items    []string `json:"items"`
		Guest    bool     `json:"guest"`
	}](r)
	if err != nil {
		return nil, err
	}
	if in.OrderID == "" || in.TotalUSD < 0 {
		return nil, badRequest("order_id and a non-negative total_usd are required")
	}
	return s.change(r.PathValue("sub"), false, func(c *Customer, _ bool) error {
		for _, o := range c.Orders {
			if o.ID == in.OrderID {
				return nil
			}
		}
		points := int(math.Floor(in.TotalUSD * tierFor(c.LifetimePoints).EarnRate))
		why := "Order " + short(in.OrderID)
		if in.Guest {
			why = "Guest order " + short(in.OrderID) + " claimed"
		}
		c.Orders = append([]Order{{ID: in.OrderID, At: time.Now().UTC(), TotalUSD: in.TotalUSD, Points: points,
			Items: in.Items, Guest: in.Guest}}, c.Orders...)
		c.award(points, why)
		return nil
	})
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func (s *server) addPayment(_ http.ResponseWriter, r *http.Request) (any, error) {
	in, err := body[struct {
		Number   string `json:"number"`
		ExpMonth int    `json:"exp_month"`
		ExpYear  int    `json:"exp_year"`
	}](r)
	if err != nil {
		return nil, err
	}
	number := strings.NewReplacer(" ", "", "-", "").Replace(in.Number)
	if len(number) < 12 || in.ExpMonth < 1 || in.ExpMonth > 12 {
		return nil, badRequest("a card number and expiry are required")
	}
	return s.change(r.PathValue("sub"), false, func(c *Customer, _ bool) error {
		last4 := number[len(number)-4:]
		for i, p := range c.PaymentMethods { // the same card again only refreshes its expiry
			if n, err := s.vault.open(p.Sealed); err == nil && n == number {
				c.PaymentMethods[i].ExpMonth, c.PaymentMethods[i].ExpYear = in.ExpMonth, in.ExpYear
				return nil
			}
		}
		sealed, err := s.vault.seal(number)
		if err != nil {
			return err
		}
		c.PaymentMethods = append(c.PaymentMethods, PaymentMethod{ID: randomID(), Brand: brand(number), Last4: last4,
			ExpMonth: in.ExpMonth, ExpYear: in.ExpYear, AddedAt: time.Now().UTC(), Sealed: sealed})
		return nil
	})
}

func brand(number string) string {
	switch {
	case strings.HasPrefix(number, "4"):
		return "Visa"
	case len(number) > 1 && number[0] == '5' && number[1] >= '1' && number[1] <= '5':
		return "Mastercard"
	case strings.HasPrefix(number, "34"), strings.HasPrefix(number, "37"):
		return "Amex"
	}
	return "Card"
}

func (s *server) deletePayment(_ http.ResponseWriter, r *http.Request) (any, error) {
	return s.change(r.PathValue("sub"), false, func(c *Customer, _ bool) error {
		kept := c.PaymentMethods[:0]
		for _, p := range c.PaymentMethods {
			if p.ID != r.PathValue("id") {
				kept = append(kept, p)
			}
		}
		c.PaymentMethods = kept
		return nil
	})
}

// revealPayment hands the BFF what the payment step needs to charge a saved card.
func (s *server) revealPayment(_ http.ResponseWriter, r *http.Request) (any, error) {
	c, err := s.store.Get(r.PathValue("sub"))
	if err != nil {
		return nil, err
	}
	if c != nil {
		for _, p := range c.PaymentMethods {
			if p.ID == r.PathValue("id") {
				number, err := s.vault.open(p.Sealed)
				if err != nil {
					return nil, err
				}
				return map[string]any{"number": number, "exp_month": p.ExpMonth, "exp_year": p.ExpYear}, nil
			}
		}
	}
	return nil, errNotFound
}

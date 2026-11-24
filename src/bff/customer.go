package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// The parts of a customerservice record the BFF uses.
type customer struct {
	Sub            string          `json:"sub"`
	Points         int             `json:"points"`
	LifetimePoints int             `json:"lifetime_points"`
	Ledger         []ledgerEntry   `json:"ledger"`
	Orders         []customerOrder `json:"orders"`
	PaymentMethods []paymentMethod `json:"payment_methods"`
	Address        *address        `json:"address,omitempty"`
	Preferences    preferences     `json:"preferences"`
	Viewed         []string        `json:"viewed"`
	Tier           tier            `json:"tier"`
	Created        bool            `json:"created"`
}

type tier struct {
	Name         string  `json:"name"`
	EarnRate     float64 `json:"earn_rate"`
	NextName     string  `json:"next_name,omitempty"`
	PointsToNext int     `json:"points_to_next,omitempty"`
}

type ledgerEntry struct {
	At          time.Time `json:"at"`
	Description string    `json:"description"`
	Points      int       `json:"points"`
}

type customerOrder struct {
	ID       string    `json:"id"`
	At       time.Time `json:"at"`
	TotalUSD float64   `json:"total_usd"`
	Points   int       `json:"points"`
	Items    []string  `json:"items"`
}

type paymentMethod struct {
	ID       string `json:"id"`
	Brand    string `json:"brand"`
	Last4    string `json:"last4"`
	ExpMonth int    `json:"exp_month"`
	ExpYear  int    `json:"exp_year"`
}

type address struct {
	Street  string `json:"street"`
	City    string `json:"city"`
	State   string `json:"state"`
	Zip     string `json:"zip"`
	Country string `json:"country"`
}

type preferences struct {
	Currency  string `json:"currency"`
	Marketing bool   `json:"marketing"`
}

type card struct {
	Number   string `json:"number"`
	ExpMonth int    `json:"exp_month"`
	ExpYear  int    `json:"exp_year"`
}

type customerClient struct {
	base  string
	token string
	http  http.Client
}

var errNoCustomer = fmt.Errorf("customer not found")

func (c *customerClient) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return errNoCustomer
	}
	if res.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 200))
		return fmt.Errorf("customerservice %s %s: %d %s", method, path, res.StatusCode, msg)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(res.Body).Decode(out)
}

func p(sub string, rest ...string) string {
	out := "/customers/" + url.PathEscape(sub)
	for _, r := range rest {
		out += "/" + url.PathEscape(r)
	}
	return out
}

func (c *customerClient) Get(ctx context.Context, sub string) (*customer, error) {
	out := &customer{}
	return out, c.do(ctx, "GET", p(sub), nil, out)
}

func (c *customerClient) SignedIn(ctx context.Context, sub, email, name string) (*customer, error) {
	out := &customer{}
	return out, c.do(ctx, "PUT", p(sub, "profile"), map[string]string{"email": email, "name": name}, out)
}

func (c *customerClient) AddOrder(ctx context.Context, sub string, o orderEvent, guest bool) (*customer, error) {
	out := &customer{}
	return out, c.do(ctx, "POST", p(sub, "orders"), map[string]any{"order_id": o.OrderID, "total_usd": o.TotalUSD,
		"items": o.Items, "guest": guest}, out)
}

func (c *customerClient) AddViews(ctx context.Context, sub string, ids []string) error {
	return c.do(ctx, "POST", p(sub, "views"), map[string]any{"product_ids": ids}, nil)
}

func (c *customerClient) SaveCard(ctx context.Context, sub string, k card) error {
	return c.do(ctx, "POST", p(sub, "payment-methods"), k, nil)
}

func (c *customerClient) Card(ctx context.Context, sub, id string) (*card, error) {
	out := &card{}
	return out, c.do(ctx, "GET", p(sub, "payment-methods", id, "card"), nil, out)
}

func (c *customerClient) DeleteCard(ctx context.Context, sub, id string) error {
	return c.do(ctx, "DELETE", p(sub, "payment-methods", id), nil, nil)
}

func (c *customerClient) SaveAddress(ctx context.Context, sub string, a address) error {
	return c.do(ctx, "PUT", p(sub, "address"), a, nil)
}

func (c *customerClient) SavePreferences(ctx context.Context, sub string, prefs map[string]any) error {
	return c.do(ctx, "PUT", p(sub, "preferences"), prefs, nil)
}

func (c *customerClient) Forget(ctx context.Context, sub string) error {
	return c.do(ctx, "DELETE", p(sub), nil, nil)
}

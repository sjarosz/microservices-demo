// Copyright 2018 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	pb "github.com/GoogleCloudPlatform/microservices-demo/src/frontend/genproto"
)

// Everything the frontend knows about who is shopping arrives in one request header, X-Bff-Context, written and
// signed by the BFF (src/bff). The frontend never sees a sign-in, a token or the customer store: it only renders
// what the context says. Without BFF_CONTEXT_KEY the header is ignored and the shop is the stock, anonymous demo.

const bffContextHeader = "X-Bff-Context"

type identityContext struct {
	AuthEnabled  bool   `json:"auth_enabled"`
	SignedIn     bool   `json:"signed_in"`
	CheckoutMode string `json:"checkout_mode"`
	Notice       string `json:"notice"`
	User         struct {
		Name      string `json:"name"`
		GivenName string `json:"given_name"`
		Email     string `json:"email"`
	} `json:"user"`
	Rewards struct {
		Tier           string  `json:"tier"`
		Points         int     `json:"points"`
		LifetimePoints int     `json:"lifetime_points"`
		EarnRate       float64 `json:"earn_rate"`
		NextTier       string  `json:"next_tier"`
		PointsToNext   int     `json:"points_to_next"`
	} `json:"rewards"`
	PaymentMethods []struct {
		ID       string `json:"id"`
		Brand    string `json:"brand"`
		Last4    string `json:"last4"`
		ExpMonth int    `json:"exp_month"`
		ExpYear  int    `json:"exp_year"`
	} `json:"payment_methods"`
	Address *struct {
		Street  string `json:"street"`
		City    string `json:"city"`
		State   string `json:"state"`
		Zip     string `json:"zip"`
		Country string `json:"country"`
	} `json:"address"`
	Preferences struct {
		Currency  string `json:"currency"`
		Marketing bool   `json:"marketing"`
	} `json:"preferences"`
	Strips []struct {
		Title      string   `json:"title"`
		ProductIDs []string `json:"product_ids"`
	} `json:"strips"`
	GuestPoints int `json:"guest_points"`
	Orders      []struct {
		ID       string    `json:"id"`
		At       time.Time `json:"at"`
		TotalUSD float64   `json:"total_usd"`
		Points   int       `json:"points"`
	} `json:"orders"`
	Ledger []struct {
		At          time.Time `json:"at"`
		Description string    `json:"description"`
		Points      int       `json:"points"`
	} `json:"ledger"`
}

// Guest is true once an anonymous shopper has chosen a one-time purchase, or when there is no sign-in at all.
func (i *identityContext) Guest() bool { return !i.AuthEnabled || i.CheckoutMode == "guest" }

type ctxKeyIdentity struct{}

var bffContextKey = []byte(os.Getenv("BFF_CONTEXT_KEY"))

// identityFromBFF reads and verifies the context header. A missing or badly signed header means an anonymous
// shopper, never an error.
func identityFromBFF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := &identityContext{}
		if body, sig, ok := strings.Cut(r.Header.Get(bffContextHeader), "."); ok && len(bffContextKey) > 0 {
			mac := hmac.New(sha256.New, bffContextKey)
			mac.Write([]byte(body))
			want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
			if hmac.Equal([]byte(sig), []byte(want)) {
				if raw, err := base64.RawURLEncoding.DecodeString(body); err == nil {
					if json.Unmarshal(raw, id) != nil {
						id = &identityContext{}
					}
				}
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKeyIdentity{}, id)))
	})
}

func identity(r *http.Request) *identityContext {
	if id, ok := r.Context().Value(ctxKeyIdentity{}).(*identityContext); ok {
		return id
	}
	return &identityContext{}
}

type productStrip struct {
	Title    string
	Products []*pb.Product
}

// personalStrips turns the product ids the BFF picked for this shopper into rows for the home page.
func (fe *frontendServer) personalStrips(r *http.Request) []productStrip {
	var out []productStrip
	for _, s := range identity(r).Strips {
		strip := productStrip{Title: s.Title}
		for _, id := range s.ProductIDs {
			if p, err := fe.getProduct(r.Context(), id); err == nil {
				strip.Products = append(strip.Products, p)
			}
		}
		if len(strip.Products) > 0 {
			out = append(out, strip)
		}
	}
	return out
}

// reportOrder tells the BFF, in response headers it consumes, that an order went through and what it was worth,
// so it can credit reward points. The headers never reach the browser.
func (fe *frontendServer) reportOrder(w http.ResponseWriter, r *http.Request, order *pb.OrderResult, total *pb.Money) {
	usd, err := fe.convertCurrency(r.Context(), total, defaultCurrency)
	if err != nil {
		return
	}
	ids := make([]string, 0, len(order.GetItems()))
	for _, it := range order.GetItems() {
		ids = append(ids, it.GetItem().GetProductId())
	}
	w.Header().Set("X-Order-Id", order.GetOrderId())
	w.Header().Set("X-Order-Total-Usd", fmt.Sprintf("%d.%02d", usd.GetUnits(), usd.GetNanos()/10000000))
	w.Header().Set("X-Order-Items", strings.Join(ids, ","))
}

// accountHandler renders the member's account page. The BFF only lets signed-in shoppers reach it and handles
// every change made from it.
func (fe *frontendServer) accountHandler(w http.ResponseWriter, r *http.Request) {
	log := r.Context().Value(ctxKeyLog{}).(logrus.FieldLogger)
	if !identity(r).SignedIn {
		http.Redirect(w, r, baseUrl+"/", http.StatusFound)
		return
	}
	currencies, err := fe.getCurrencies(r.Context())
	if err != nil {
		renderHTTPError(log, r, w, err, http.StatusInternalServerError)
		return
	}
	cart, _ := fe.getCart(r.Context(), sessionID(r))
	if err := templates.ExecuteTemplate(w, "account", injectCommonTemplateData(r, map[string]interface{}{
		"show_currency": false,
		"currencies":    currencies,
		"cart_size":     cartSize(cart),
	})); err != nil {
		log.Println(err)
	}
}

// tierProgress is how far, in percent, a member is towards the next tier.
func tierProgress(lifetime, toNext int) int {
	if lifetime+toNext <= 0 {
		return 0
	}
	return lifetime * 100 / (lifetime + toNext)
}

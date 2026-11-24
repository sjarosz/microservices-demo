// bff is the Backend for Frontend of Online Boutique: the only service that knows about sign-in.
//
// The browser talks to the BFF and holds nothing but an opaque session cookie. The BFF runs the OpenID Connect
// flows against any standards-following provider, keeps the tokens on the server, calls customerservice for what the
// shop knows about the customer, and forwards every other request to the stock frontend together with one signed
// header (X-Bff-Context) that says who is shopping and what to show them. No other service sees a token, a
// cookie or the identity provider.
package main

import (
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"
)

type config struct {
	publicURL     *url.URL // how browsers reach the BFF, e.g. https://shop.example.com
	frontend      *url.URL
	issuer        string
	clientID      string
	clientSecret  string
	scopes        []string
	registerParam url.Values // extra authorization parameters that ask the provider for its sign-up page
	contextKey    []byte
	sessionTTL    time.Duration
}

type BFF struct {
	cfg      config
	sessions SessionStore
	customer *customerClient
	oidc     *oidcClient
	proxy    *httputil.ReverseProxy
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func mustURL(key, def string) *url.URL {
	u, err := url.Parse(env(key, def))
	if err != nil || u.Host == "" {
		log.Fatalf("%s must be an absolute URL", key)
	}
	return u
}

func main() {
	reg, err := url.ParseQuery(env("OIDC_REGISTER_PARAMS", "prompt=create"))
	if err != nil {
		log.Fatalf("OIDC_REGISTER_PARAMS: %v", err)
	}
	cfg := config{
		publicURL:     mustURL("PUBLIC_URL", "http://localhost:8080"),
		frontend:      mustURL("FRONTEND_ADDR", "http://frontend:80"),
		issuer:        os.Getenv("OIDC_ISSUER"),
		clientID:      os.Getenv("OIDC_CLIENT_ID"),
		clientSecret:  os.Getenv("OIDC_CLIENT_SECRET"),
		scopes:        strings.Fields(env("OIDC_SCOPES", "openid profile email")),
		registerParam: reg,
		contextKey:    []byte(os.Getenv("BFF_CONTEXT_KEY")),
		sessionTTL:    8 * time.Hour,
	}
	if len(cfg.contextKey) < 16 {
		log.Fatal("BFF_CONTEXT_KEY must be set (16+ characters): it signs the context the frontend trusts")
	}
	b := &BFF{
		cfg:      cfg,
		sessions: newSessionStore(os.Getenv("REDIS_ADDR")),
		customer: &customerClient{base: env("CUSTOMER_SERVICE_ADDR", "http://customerservice:8080"),
			token: os.Getenv("INTERNAL_API_TOKEN")},
	}
	if cfg.issuer != "" {
		b.oidc = &oidcClient{cfg: cfg}
	} else {
		log.Print("OIDC_ISSUER is not set: sign-in is off and the shop behaves like the stock demo")
	}
	b.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(cfg.frontend)
			pr.Out.Host = pr.In.Host
			pr.SetXForwarded()
		},
		ModifyResponse: b.afterFrontend,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /bff/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /bff/api/order/{id}", b.orderResult)
	mux.HandleFunc("GET /auth/login", b.login)
	mux.HandleFunc("GET /auth/register", b.login)
	mux.HandleFunc("GET /auth/callback", b.callback)
	mux.HandleFunc("POST /auth/logout", b.logout)
	mux.HandleFunc("GET /account", b.account)
	mux.HandleFunc("POST /account/preferences", b.signedIn(b.savePreferences))
	mux.HandleFunc("POST /account/payment-methods/{id}/delete", b.signedIn(b.deletePayment))
	mux.HandleFunc("POST /account/delete", b.signedIn(b.deleteData))
	mux.HandleFunc("POST /cart/checkout", b.checkout)
	mux.HandleFunc("POST /setCurrency", b.setCurrency)
	mux.HandleFunc("/static/", b.passThrough)
	mux.HandleFunc("/", b.page)

	addr := ":" + env("PORT", "8080")
	log.Printf("bff listening on %s, frontend %s, public %s, sessions in %s", addr, cfg.frontend, cfg.publicURL,
		b.sessions.Name())
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

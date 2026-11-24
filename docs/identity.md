# Identity for Online Boutique (BFF)

Branch `identity` gives the shop a sign-in experience without teaching the shop about sign-in. One new service, the
Backend for Frontend (`src/bff`), owns identity. The stock services keep working as they did; the frontend gained a
few template changes and learns who is shopping from a single signed request header.

## What a shopper sees

| Moment | Behaviour |
|---|---|
| Browsing, cart | Anonymous. No sign-in wall, no BFF session until one is needed. A "Recently viewed" row appears from the browser's own session. |
| Checkout | The cart asks: **Sign in**, **Create an account**, or **Check out as a guest**. |
| Guest | A one-time purchase. Nothing about the buyer is stored. The confirmation page offers to create an account and keep the points the order earned. |
| Create an account / sign in | OpenID Connect at the provider (authorization code + PKCE). New members get 100 bonus points; guest orders from the same browser are credited. |
| Member checkout | E-mail and saved address are filled in, saved cards are offered, and "Remember this card" / "Save this address" are opt-in. |
| Rewards | 1 point per US dollar; Silver (500 lifetime points) earns 1.25, Gold (2000) earns 1.5. Balance and tier sit in the header. |
| Personalization | Greeting, "Pick up where you left off" and "Buy it again" rows on the home page, preferred currency applied at sign-in. |
| Account page | Points and history, orders, saved cards (remove), address, preferences, and "Delete my shop data". |

## How it is put together

```
browser ──cookie──> bff ──X-Bff-Context (signed)──> frontend ──gRPC──> the stock services
                     │ ├── OpenID Connect ──> identity provider (any)
                     │ └── REST ──> customerservice ──> identity-redis
                     └── sessions ──> identity-redis
```

- **bff** (`src/bff`, Go). The only public entry. Runs the OIDC flows, keeps tokens in a server-side session
  (the browser holds an opaque `HttpOnly` cookie, `__Host-bff` over HTTPS), and forwards everything else to the
  frontend. At checkout it fills in a saved card and afterwards records the order for the member.
- **customerservice** (`src/customerservice`, Go). Points, tiers, orders, saved cards (card numbers encrypted with
  AES-256-GCM, security codes never stored), address, preferences, viewed products. Keyed by the provider's `sub`.
  Only the BFF may call it (internal token).
- **frontend** (stock, thin edits). `identity.go` verifies and reads the `X-Bff-Context` header; templates render
  from it. `placeOrderHandler` reports a finished order to the BFF in `X-Order-*` response headers, which the BFF
  consumes. Without `BFF_CONTEXT_KEY` the frontend ignores the header and is the stock demo.
- **No other service changed.** None of them sees a token, a cookie or the provider.

The identity provider is configured by discovery only (`OIDC_ISSUER`, `OIDC_CLIENT_ID`, `OIDC_CLIENT_SECRET`).
Sign-up uses the standard `prompt=create` (override with `OIDC_REGISTER_PARAMS`). Sign-out uses RP-initiated logout
when the provider advertises `end_session_endpoint`.

## Deploying

Kustomize components, next to the existing ones:

- `kustomize/components/identity`: bff, customerservice, identity-redis, and the frontend patch. The overlay
  supplies Secret `identity-secrets` (`BFF_CONTEXT_KEY`, `INTERNAL_API_TOKEN`, `VAULT_KEY`, `OIDC_CLIENT_SECRET`),
  the values of ConfigMap `identity-config`, and routes outside traffic to Service `bff`.
- `kustomize/components/identity-dev-idp`: a throwaway Keycloak (realm `boutique`, self-registration on, user
  `demo@example.com`) to use until a real provider is wired in. Needs `IDP_ADMIN_PASSWORD` and
  `DEMO_USER_PASSWORD` in the Secret and `IDP_URL` in the ConfigMap. Expose only `/realms/` and `/resources/`.

To switch providers: register a confidential client with redirect URI `<PUBLIC_URL>/auth/callback` and post-logout
URI `<PUBLIC_URL>/`, set the three `OIDC_*` values, and drop the dev-idp component.

Images are built from `src/bff`, `src/customerservice` and `src/frontend` (`docker buildx build --platform
linux/amd64 ...`).

## Testing

`src/bff/e2e/e2e.py` drives the BFF like a browser through the whole experience (33 checks): anonymous browsing,
guest purchase, sign-up with the points claim, member checkout with a new and a saved card, preferences,
sign-out and sign-in, card removal and data deletion.

```
BASE_URL=https://<shop host> python3 src/bff/e2e/e2e.py
```

## Deliberately left out

- Redeeming points, and member pricing: both need changes in checkoutservice.
- A cart that follows the member across devices: the cart stays keyed by the frontend's own session cookie.
- Token-based calls between services: the BFF vouches for the shopper with a signed header and an internal token
  instead of forwarding access tokens, because a provider-neutral build cannot assume a token format or audience.
- Step-up authentication, passkeys, consent records and account linking: these are provider features.
- More than one replica of customerservice: its read-modify-write is guarded in-process only.

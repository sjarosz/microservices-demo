# Branch: `identity`

Starter notes for this branch. Update them as the work takes shape.

## What this branch is for

A meaningful identity experience for Online Boutique, kept out of the core demo by a Backend for Frontend:
anonymous until checkout, then guest purchase, sign-in or sign-up over OpenID Connect, saved payment details,
personalization and membership rewards. See [docs/identity.md](docs/identity.md).

## Where it comes from

| | |
|---|---|
| Repository | `sjarosz/microservices-demo` (fork of `GoogleCloudPlatform/microservices-demo`) |
| Base branch | `master` |
| Base commit | `c7cb6d5b` (2026-10-27), which contains release `v0.10.7` |
| Created | 2026-11-23 |

## Working on it

- Deployable manifests: `release/kubernetes-manifests.yaml` (prebuilt `v0.10.7` images) or `kustomize/`.
- Service sources: `src/<service>`; build and run locally with `skaffold run` (see `docs/development-guide.md`).
- Keep changes to the stock services small and note each one below, so the branch stays easy to rebase on upstream.

## Changes so far

- 2026-11-23: branch created, this file added.
- 2026-11-23: identity behind a BFF. New `src/bff` and `src/customerservice`; frontend reads a signed context
  header (`identity.go`, an account page, template and CSS additions; `main.go` and `handlers.go` gain a few lines);
  kustomize components `identity` and `identity-dev-idp`; `docs/identity.md`. No other stock service changed.
- 2026-11-24: optional component `identity-pinggateway`: a PingGateway sidecar in the BFF pod is the OIDC client;
  the BFF gains a gateway mode (`src/bff/gateway.go`) and deletes cookies with a past `Expires` as well.

## Open questions

- Which real identity provider replaces the throwaway Keycloak?
- Should points be redeemable (needs checkoutservice changes)?

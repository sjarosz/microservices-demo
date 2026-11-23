# Branch: `identity`

Starter notes for this branch. Update them as the work takes shape.

## What this branch is for

Identity work on Online Boutique: adding sign-in for shoppers and identity-aware calls between the services,
on top of the stock demo. Nothing is changed yet; this file is the only difference from the base.

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

## Open questions

- Which identity provider and which flows (shopper sign-in, service-to-service, agent access)?
- Which services change first (most likely `frontend` and `checkoutservice`)?

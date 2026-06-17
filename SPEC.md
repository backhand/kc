# kc — Kubernetes operations tool

A keyboard-driven, Midnight-Commander-style CLI for **daily Kubernetes operations** —
deliberately *not* a cluster viewer (k9s / Headlamp already do that). `kc` answers "get the
latest mailon out", "restart this", "what's deployed vs what's released?" — it's git/GitHub-aware
and **learns your habits** to eliminate repetitive UI steps.

## Principles

- **Operations, not observation.** Focused on the handful of things you do daily; consciously NOT a full Kubernetes UI.
- **Predict, then confirm.** Never make the user build a selection from zero — open pre-filled with the most-likely choice from history; confirm (Enter) or adjust.
- **Fast.** Startup feels instant: render from cache immediately, refresh in the background (see below).
- **Portable.** A small static binary.
- **Local learning, no telemetry.**

## Stack

- **Go + Bubble Tea** (the Charm stack: `bubbletea` / `lipgloss` / `bubbles`) → a ~15–25 MB static binary; trivial cross-compile (`GOOS`/`GOARCH`, `CGO_ENABLED=0`).
- **Shell-out** to `kubectl` / `gh` / `git`. This reuses their auth for free — including the Dex/OIDC kubeconfigs — and they're standard prerequisites for a k8s/GitHub ops tool. (Deliberately NOT `client-go`: its dependency bulk pushes the binary to ~80 MB, k9s-sized, which defeats the small-binary goal.)
- Rewrite of the archived Bun/OpenTUI prototype in [`../kc-bun`](../kc-bun/) — switched for binary size (Bun `--compile` ≈ 70 MB, almost all embedded JS runtime).

## Startup & data freshness — optimistic caching

Startup must feel instant; **never block first paint on `kubectl`/`gh`**.

- On launch: **load the last-known snapshot from `~/.kc/cache/` and render it immediately** (clearly marked stale), while **firing fresh fetches in the background**.
- When fresh data lands: update the view and rewrite the cache. A subtle indicator conveys freshness (`refreshing…` / `updated 8s ago`); stale rows are never hidden behind a blocking spinner.
- **Stale-while-revalidate**, keyed by cluster (× repo for releases). A periodic tick re-fetches for a live feel.
- **Bubble Tea fit:** `Init` loads the cache → initial model + fetch `Cmd`s; `Update` folds in `…LoadedMsg`s (update model + persist cache); `View` always renders the current model + freshness. The cache read/write + fetchers live in the data layer (pure, testable); the optimistic orchestration lives in the app.

## Views — one zoom stack

`Backspace` zooms out, `Enter` drills in:

```
all-namespaces  ⇄  app group (mailon-*)  ⇄  namespace  ⇄  deployment  ⇄  pods
```

- **Entry point is contextual:** in a git repo → that app's last-selected namespace (resolved by matching the repo's GHCR image to running deployments, then remembered). Not in a repo → all-namespaces.
- **all-namespaces:** node-resource header + namespace rows; common/system namespaces (`kube-*`, cert-manager, dex, buildkit, actions-runner…) sorted to the bottom.
- **namespace:** deployments with version (image tag) + ready + resource usage.
- **deployment → pods:** status, node, restarts, usage.

## Operations (contextual keybindings)

`[d]` deploy · `[r]` restart · `[l]` logs · `[s]` shell. Read-mostly; mutations confirmed; rollout shown after.

## Deploy flow (v1)

`d` opens a modal:
1. **Deployment checkboxes** — the top learned preset pre-checked; other saved permutations as one-key chips.
2. **Version list** — 5 latest GitHub Releases (**incl. pre-releases, marked**), each annotated with build status (Actions) + image availability (GHCR). `…older` pages back.
3. Pick a version → `kubectl set image` on the checked deployments → rollout view.

Release `vX.Y.Z` → image `:vX.Y.Z`. **v1 uses imperative `kubectl set image`** (pluggable; a later mode does Kustomize-bump + commit).

## Learning subsystem

Generic — **deploy is the first consumer**; logs/restart/shell adopt it later with no rework. State `~/.kc/state.json`, keyed by cluster × app, recency-weighted-frequency ranking; deploy presets = distinct deployment-sets ranked most-recent-first. No telemetry. (Distinct from the `~/.kc/cache/` data cache above.)

## Repo → namespace resolution

Match the repo's GHCR image (`ghcr.io/<owner>/<repo>`) to running deployments → their namespace(s); remember it. An app may span multiple namespaces (`mailon`, `mailon-staging`): start at the last-selected, `Backspace` zooms to the `<app>-*` group, then to all-namespaces.

## Home

`tools/kc/` (Go). Archived Bun prototype: `tools/kc-bun/`.

## Build order

1. **Re-spike: Go + Bubble Tea** → confirm a small static binary + trivial Linux cross-compile.
2. **Data layer** — `kubectl`/`gh`/`git` shell-out + the **cache store** + the generic `ActionHistory` learning store. (Ports cleanly from `../kc-bun/src`.)
3. **Views + zoom navigation** — optimistic render from cache, background refresh.
4. **Deploy flow.**

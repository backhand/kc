# kc — Kubernetes operations tool

A keyboard-driven, Midnight-Commander-style CLI for **daily Kubernetes operations** —
deliberately *not* a cluster viewer (k9s / Headlamp already do that well). `kc` answers
"get the latest mailon out", "restart this", "what's deployed vs what's released?" — it's
git/GitHub-aware, and it **learns your habits** to eliminate repetitive UI steps.

## Principles

- **Operations, not observation.** Focused on the handful of things you do daily; consciously NOT a full Kubernetes UI.
- **Predict, then confirm.** Never make the user build a selection from zero. Every flow opens pre-filled with the most-likely choice from history; the user confirms (Enter) or adjusts. In the common case, `kc → d → Enter` deploys.
- **Portable.** Ships as a standalone compiled binary.
- **Local learning, no telemetry.** It adapts to *you* on *your* machine; nothing phones home.

## Stack

- **Bun + OpenTUI** (`@opentui/react` React reconciler) → `bun build --compile` standalone binary.
  - OpenTUI is chosen over Ink specifically because it is built for Bun-compiled binaries (it's what OpenCode ships). Ink + `bun --compile` fights `yoga.wasm` embedding.
- **Shell-out** to `kubectl` / `gh` / `git`. `kc` is a portable *orchestrator* — it expects those on PATH (every machine the team operates from has them). Not hermetic by design; revisit only if zero external deps is ever required.

## Views — one zoom stack

`Backspace` zooms out, `Enter` drills in:

```
all-namespaces  ⇄  app group (mailon-*)  ⇄  namespace  ⇄  deployment  ⇄  pods
```

- **Entry point is contextual:** started inside a git repo → land on that app's last-selected namespace (resolved by matching the repo's GHCR image to running deployments, then remembered). Not in a repo → all-namespaces.
- **all-namespaces:** node-resource header + namespace rows; common/system namespaces (`kube-*`, `cert-manager`, `dex`, `buildkit`, `actions-runner`, …) sorted to the bottom.
- **namespace:** deployments with version (image tag) + ready + resource usage.
- **deployment → pods:** status, node, restarts, usage.

## Operations (contextual keybindings)

`[d]` deploy · `[r]` restart · `[l]` logs · `[s]` shell. Read-mostly; mutations confirmed; rollout shown after.

## Deploy flow (v1)

`d` opens a modal:

1. **Deployment checkboxes** — the top learned preset pre-checked; other saved permutations offered as one-key chips.
2. **Version list** — the 5 latest GitHub Releases (**incl. pre-releases, marked**), each annotated with **build status** (from in-flight Actions) + **image availability** (GHCR): `✓ ready` / `⏳ building (#441)` / `⚠ no image`. `…older` pages back.
3. Pick a version → `kubectl set image` on the checked deployments → rollout view.

- Release tag `vX.Y.Z` → image `:vX.Y.Z` (matches the app repos' `docker/metadata-action` tags).
- **v1 uses imperative `kubectl set image`.** Known trade-off: it diverges from the repo's Kustomize source-of-truth, so a later `kubectl apply -k` reverts it. The deploy *mechanism* is built pluggable; a later version adds a Kustomize-bump-and-commit mode.

## Learning subsystem

Generic by design — **deploy is the first consumer**; logs/shell/restart adopt it later without rework.

- **State:** `~/.kc/state.json`, keyed by **cluster × app**. Records each action as `(action, scope, params, timestamp)`.
- **Ranking:** recency-weighted frequency → top-ranked params become the pre-filled defaults.
- **Deploy presets:** every distinct *set* of deployments deployed together is saved as a permutation; the most recent is pre-checked, others offered as chips.
- **No telemetry** — a local file only.

## Repo → namespace resolution

- Match the repo's GHCR image (`ghcr.io/<org>/<app>`) to running deployments → their namespace(s); remember the choice.
- An app may span **multiple namespaces** (e.g. `mailon`, `mailon-staging`): start at the last-selected one; `Backspace` zooms to the `<app>-*` group, then to all-namespaces.

## Home

`tools/kc/` in `thinkpilot/infrastructure` (break out into its own repo later if warranted).

## Build order

1. **Spike: OpenTUI → portable compiled binary** — prove the hard requirement before building on it.
2. **Data layer** — `kubectl` / `gh` / `git` shell-out + the generic `ActionHistory` learning store.
3. **Views + zoom navigation.**
4. **Deploy flow.**

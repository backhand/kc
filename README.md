# kc

**kc** — *kubernetes commander.* A keyboard-driven, Midnight-Commander-style
terminal UI for daily Kubernetes **operations**: browse the cluster, then
deploy, restart, scale, tail logs, and exec — all from the keyboard, with
learned defaults so the common path is a couple of keystrokes.

It's not another read-only dashboard: kc is built around *doing* things safely.
Mutations (deploy / restart / scale) are confirm-gated; logs and exec hand the
terminal to `kubectl`. See [`SPEC.md`](./SPEC.md) for the design.

## Install

### curl \| sh (quickest — no prerequisites)

```sh
curl -fsSL https://raw.githubusercontent.com/backhand/kc/master/install.sh | sh
```

Grabs the prebuilt static binary for your OS/arch from the latest GitHub
release, verifies it against the release `checksums.txt`, and installs it to
`/usr/local/bin`. Override the destination with `KC_INSTALL_DIR`, or pin a
version with `KC_VERSION` (e.g. `KC_VERSION=v0.1.0`). No Homebrew tap-trust
needed.

### Homebrew (macOS / Linux)

```sh
brew tap backhand/tap
brew trust --cask backhand/tap/kc   # one-time, per machine (see below)
brew install kc
```

Homebrew 6.0+ won't evaluate a third-party tap's (unsandboxed) Ruby until you
explicitly trust it, so the one-time `brew trust` step is required before
`install`. Prefer trusting just the cask as above; to trust the whole tap
instead, run `brew trust backhand/tap`.

### go install

```sh
go install github.com/backhand/kc@latest
```

### Build from source

```sh
git clone https://github.com/backhand/kc && cd kc
make build      # -> ./kc  (static, no cgo)
```

**Runtime requirements:** `kubectl` (required — kc shells out to it for
everything), plus `git` + `gh` for the deploy flow (it lists GitHub releases to
pick a version). The Homebrew cask pulls in `kubernetes-cli` automatically.

## Usage

```sh
kc            # launch the TUI against your current kube-context
kc --version
kc --help
```

kc uses the ambient `KUBECONFIG` / current context (so OIDC/Dex auth via
`kubectl` works for free). Launched inside a git repo whose GHCR image runs on
the cluster, it opens straight at that app's namespace; elsewhere it opens at
all-namespaces. `KC_NO_ALTSCREEN=1 kc` renders a linear, pipeable stream.

### Navigation — a zoom stack

```
all-namespaces  →  app-group  →  namespace  →  deployment  →  pods
```

Each level paints instantly from an on-disk cache and refreshes in the
background, so it's never blocked on the apiserver.

| Key            | Action                                  |
| -------------- | --------------------------------------- |
| `↑`/`k` `↓`/`j`| move the cursor                         |
| `enter` / `→`  | drill in                                |
| `backspace`/`←`/`h` | zoom out                           |
| `/`            | search-everywhere (namespaces/deployments/pods) |
| `q` / `Ctrl+C` | quit                                    |
| `?`            | toggle help                             |

### Operations (on the selected workload)

| Key | Operation | Notes                                                       |
| --- | --------- | ----------------------------------------------------------- |
| `d` | deploy    | pick a release → `kubectl set image`; confirm-gated; learns presets |
| `r` | restart   | `kubectl rollout restart` a set; confirm-gated              |
| `s` | scale     | scale a set to N replicas (0 = pause); confirm-gated        |
| `l` | logs      | stream `kubectl logs -f` for the selected deployment/pod    |
| `e` | exec      | open a shell (`kubectl exec -it … -- sh`)                   |

Deploy, restart, and scale share a **set selection** (checkboxes + learned
preset chips): the set containing the workload you're on is pre-checked, `←`/`→`
cycle presets, number keys toggle them, and `space` toggles individual rows. The
pods list shows each pod's image version so you can watch a rollout flip live.

## Development

```sh
make build        # native static binary -> ./kc
make build-small  # native, trimmed (-ldflags "-s -w")
make linux        # Linux/amd64 static, one command
make test         # headless render+input+exit smoke test (teatest)
```

- **Go** + the Charm stack (`bubbletea`, `lipgloss`, `bubbles`). Go ≥ 1.24.
- No cgo; the binary is a small (~3–5 MB) static executable that cross-compiles
  to Linux in one command.
- Everything Kubernetes/git/GitHub-shaped is a shell-out (`kubectl`/`git`/`gh`)
  behind the `internal/` packages; `internal/tui` is the Bubble Tea app.

Releases are automated with GoReleaser — see [`RELEASING.md`](./RELEASING.md).

## License

[MIT](./LICENSE) © Frederik Hannibal

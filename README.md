# kc

A keyboard-driven, Midnight-Commander-style CLI for daily Kubernetes
operations. See [`SPEC.md`](./SPEC.md) for the design.

> **Status: spike skeleton.** This is the de-risking spike for the Go + Bubble
> Tea stack — it proves a small, static, portable binary with a one-command
> Linux cross-compile. It contains **no kc features yet** (no kubectl/gh/git,
> no views, no cache, no deploy); those land on top of this base per the build
> order in `SPEC.md`. The archived Bun/OpenTUI prototype is in
> [`../kc-bun`](../kc-bun/) (reference only).

## Stack

- **Go** + the Charm TUI stack: [`bubbletea`](https://github.com/charmbracelet/bubbletea),
  [`lipgloss`](https://github.com/charmbracelet/lipgloss),
  [`bubbles`](https://github.com/charmbracelet/bubbles).
- Requires **Go ≥ 1.24** (`go` directive in `go.mod`; the floor is set by the
  test-only `teatest` helper — the shipped binary itself builds on older Go).
- No cgo. The smoke test drives the app headlessly via `teatest`.

## Build

```sh
make build        # native static binary -> ./kc
make build-small  # native, trimmed (-ldflags "-s -w")
make linux        # Linux/amd64 static, ONE command
make dist         # trimmed darwin + linux binaries
make test         # headless render+input+exit smoke test
```

Equivalent raw commands:

```sh
# native static (no cgo)
CGO_ENABLED=0 go build -o kc .

# native static, trimmed
CGO_ENABLED=0 go build -ldflags "-s -w" -o kc .

# Linux/amd64 static cross-compile — single command, no native packages
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o kc-linux-amd64 .
```

## Verified (spike acceptance)

Built with Go 1.26.4 (darwin/arm64 host); pinned: `bubbletea v1.3.10`,
`lipgloss v1.1.0`, `bubbles v1.0.0`.

| Build                         | `file`                                    | Size  |
| ----------------------------- | ----------------------------------------- | ----- |
| native (`CGO_ENABLED=0`)      | Mach-O 64-bit arm64 (only libSystem/libresolv via `otool -L`) | 4.1 MB |
| native `-s -w`                | Mach-O 64-bit arm64                        | 2.8 MB |
| Linux/amd64 (`CGO_ENABLED=0`) | ELF 64-bit x86-64, **statically linked**   | 4.2 MB |
| Linux/amd64 `-s -w`           | ELF 64-bit x86-64, statically linked, stripped | 2.8 MB |

- **Portable:** copying only the binary to `/tmp` and running it under a
  scrubbed env (no Go toolchain on `PATH`) renders correctly; `↑/↓` and `j/k`
  navigate (clamped at both ends); `q` / `Ctrl+C` quit cleanly and the terminal
  is restored (alt-screen entered on start, left on quit, cursor re-shown).
- **Linux cross-compile is one command** — no fetching of native packages.

## Keys

| Key             | Action          |
| --------------- | --------------- |
| `↑` / `k`       | move up         |
| `↓` / `j`       | move down       |
| `q` / `Ctrl+C`  | quit            |

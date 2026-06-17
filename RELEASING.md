# Releasing kc

Releases are automated with [GoReleaser](https://goreleaser.com). Pushing a
semver tag builds the binaries, publishes a GitHub release, and updates the
Homebrew formula.

## One-time setup

The release workflow publishes the formula to a **separate** repo
(`backhand/homebrew-tap`). GitHub Actions' default `GITHUB_TOKEN` only has
access to the repo it runs in, so cross-repo formula pushes need a Personal
Access Token:

1. **Create the tap repo** (once): `backhand/homebrew-tap`, public.
2. **Create a PAT** with write access to `backhand/homebrew-tap`:
   - Fine-grained token (recommended): repository = `backhand/homebrew-tap`,
     Repository permissions → **Contents: Read and write**.
   - Or a classic token with the `repo` scope.
3. **Add it as a secret** on `backhand/kc`: Settings → Secrets and variables →
   Actions → New repository secret, name **`HOMEBREW_TAP_TOKEN`**, value = the PAT.

## Cutting a release

```sh
git tag v0.1.0
git push origin v0.1.0
```

The `release` workflow then:

1. builds static `kc` binaries for darwin/linux × amd64/arm64,
2. creates the GitHub release with the archives + `checksums.txt`,
3. writes `Formula/kc.rb` to `backhand/homebrew-tap`.

Users install with:

```sh
brew tap backhand/tap
brew install kc
# or: brew install backhand/tap/kc
```

## Trying it locally first

```sh
goreleaser check                      # validate this config
goreleaser build --snapshot --clean   # build binaries, skip publishing
```

## Path to a bare `brew install kc`

A bare `brew install kc` (no tap prefix) requires the formula to live in
**homebrew-core**, which has notability requirements (≥75★, in active use) and a
review PR. Once kc qualifies, submit the formula there with
`brew bump-formula-pr` / a homebrew-core PR; until then the tap is the install path.

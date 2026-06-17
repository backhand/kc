# Security Policy

## Reporting a vulnerability

Please report security issues **privately** — don't open a public issue.

- Preferred: open a private
  [GitHub security advisory](https://github.com/backhand/kc/security/advisories/new)
  (the repo's **Security → Advisories → "Report a vulnerability"**).
- Or email **frederik@backhand.dk**.

I'll acknowledge within a few days and keep you updated on a fix. Please allow a
reasonable window to address the issue before any public disclosure.

## Supported versions

kc is pre-1.0: fixes land on the latest release. Please upgrade to the newest
release before reporting.

## Verifying downloads

Every release ships a `checksums.txt`. It is signed with
[cosign](https://github.com/sigstore/cosign) (keyless — Sigstore Fulcio + Rekor),
and the archives also carry [SLSA build provenance](https://slsa.dev). The
`install.sh` script already verifies the checksum; to additionally verify the
signature and provenance:

```sh
# 1. checksum
sha256sum -c checksums.txt --ignore-missing

# 2. cosign signature over the checksums (Sigstore bundle)
cosign verify-blob \
  --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp 'https://github.com/backhand/kc/.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt

# 3. SLSA build provenance (needs the GitHub CLI)
gh attestation verify kc_<version>_<os>_<arch>.tar.gz --repo backhand/kc
```

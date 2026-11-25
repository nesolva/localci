# Contributing

Thanks for wanting to help. Bug reports, fixes, docs and examples are all welcome. For a larger change, open an issue
first so we can agree on the shape before you write it.

## Set up

You need Go (the version in `go.mod`) and Node 20 or newer. Docker, `act` and `mise` are optional; tests that need
Docker skip when the daemon is not running.

```bash
git clone https://github.com/nesolva/localci && cd localci
npm ci                                  # installs the workspaces (packages/*, examples/*)
make build                              # writes bin/localci
npx playwright install chromium         # only for the example suite and the Playwright package's browser tests
```

## Before you open a pull request

```bash
make ci-go                              # gofmt, generated docs up to date, vet, staticcheck, race tests, govulncheck
make ci-playwright                      # builds and tests the Playwright package
make ci-pytest                          # tests the pytest plugin in a virtualenv (Python 3.9 or newer)
```

For a quick loop, `go test ./internal/<package> -run <Name> -count=1` runs one test, and `go vet ./...` plus
`gofmt -l .` cover most of the rest.

If you change a setting, run `make generate` and commit `docs/config.md`; it is written from the schema and CI fails
when it drifts.

## What a good change looks like

- It does one thing, and the pull request says what and why.
- It has tests that would catch a realistic bug and still pass after a refactor. Skip tests that only repeat the
  code or check a mock.
- A change a user can see updates the docs and, when it touches the agent loop, the skill.
- Fixtures, examples and tests use made-up names (`acme/shop`), never real companies, people or paths.

## Releases

Releases are cut by the maintainer; contributors do not publish anything. You can check that a release is the one the
maintainer built.

### Verifying a release

Every release has `checksums.txt`, its signature `checksums.txt.sig`, the public key `cosign.pub` and a provenance file
(`localci_<version>.intoto.jsonl`). Download them next to the archive you installed, then:

```bash
cosign verify-blob --key cosign.pub --insecure-ignore-tlog --bundle checksums.txt.sig checksums.txt
shasum -a 256 -c --ignore-missing checksums.txt
```

The first command proves `checksums.txt` is the maintainer's (releases are not in a transparency log, hence
`--insecure-ignore-tlog`); the second proves your archive is one it lists. To check the provenance of an archive:

```bash
cosign verify-blob-attestation --key cosign.pub --insecure-ignore-tlog --type slsaprovenance1 \
  --bundle localci_<version>.intoto.jsonl localci_<version>_<os>_<arch>.tar.gz
```

`install.sh` and the Homebrew formula do the
checksum step for you.

## Conduct and security

Be decent: see the [code of conduct](CODE_OF_CONDUCT.md). Report vulnerabilities privately, as
[SECURITY.md](SECURITY.md) describes.

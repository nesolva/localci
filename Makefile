VERSION ?= dev

# Pinned so a new release of a checker never fails a pull request by itself.
STATICCHECK ?= honnef.co/go/tools/cmd/staticcheck@2026.2.2
GOVULNCHECK ?= golang.org/x/vuln/cmd/govulncheck@v1.10.0

.PHONY: build test generate check ci ci-go ci-playwright ci-pytest
build:
	go build -ldflags "-X main.version=$(VERSION)" -o bin/localci ./cmd/localci

test:
	go test ./...

generate:
	go generate ./...

check:
	go vet ./... && go test ./...

# What every pull request must pass. The workflow calls the parts as
# separate jobs; run `make ci` to get all of them.
ci: ci-go ci-playwright ci-pytest

ci-go:
	@test -z "$$(gofmt -l .)" || { echo "gofmt: these files need formatting:"; gofmt -l .; exit 1; }
	@scripts/check-config-docs.sh
	go vet ./...
	go run $(STATICCHECK) ./...
	go test -race ./...
	go run $(GOVULNCHECK) ./...

ci-playwright:
	npm ci
	npx playwright install --with-deps chromium
	npm run build -w packages/playwright
	npm test -w packages/playwright

# The pytest plugin's own tests, in a throwaway virtualenv (needs Python 3.9 or newer). CI also runs them on the oldest
# pytest the plugin supports: `make ci-pytest PYTEST_REQ=pytest==7.0.1`.
PYTEST_REQ ?= pytest
ci-pytest:
	python3 -m venv packages/pytest/.venv
	packages/pytest/.venv/bin/pip install -q -e "packages/pytest[test]" "$(PYTEST_REQ)"
	packages/pytest/.venv/bin/python -m pytest -q packages/pytest/tests

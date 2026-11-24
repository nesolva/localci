VERSION ?= dev

# Pinned so a new release of a checker never fails a pull request by itself.
STATICCHECK ?= honnef.co/go/tools/cmd/staticcheck@2026.2.2
GOVULNCHECK ?= golang.org/x/vuln/cmd/govulncheck@v1.10.0

.PHONY: build test generate check ci ci-go ci-playwright
build:
	go build -ldflags "-X main.version=$(VERSION)" -o bin/localci ./cmd/localci

test:
	go test ./...

generate:
	go generate ./...

check:
	go vet ./... && go test ./...

# What every pull request must pass. The workflow calls the two halves as
# separate jobs; run `make ci` to get both.
ci: ci-go ci-playwright

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

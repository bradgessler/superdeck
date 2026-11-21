# Switcharoo is one Go binary. `make` builds it; `make release VERSION=v0.1.0`
# builds the archives that go on a GitHub release.

VERSION ?= dev
LDFLAGS := -s -w -X main.Version=$(VERSION)

switcharoo: *.go go.mod go.sum migrations/* skill/* ui/templates/* ui/static/*
	go build -trimpath -ldflags "$(LDFLAGS)" -o switcharoo .

test:
	go test -race ./...

# macOS only: Switcharoo leans on launchd, the Keychain and macOS's ps.
release:
	rm -rf dist && mkdir -p dist
	for arch in arm64 amd64; do \
		GOOS=darwin GOARCH=$$arch CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/switcharoo . && \
		tar -C dist -czf dist/switcharoo-darwin-$$arch.tar.gz switcharoo && \
		rm dist/switcharoo || exit 1; \
	done
	cd dist && shasum -a 256 *.tar.gz > checksums.txt

.PHONY: test release

VERSION ?= $(shell git describe --tags --always 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test race lint run docker up down clean release

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/agentd .

test:
	go test ./...

race:
	go test -race -count=1 ./...

lint:
	gofmt -l . | (! grep .) && go vet ./...

run: build
	./bin/agentd -config $${AGENT_CONFIG:-agents.yaml}

docker:
	docker build --build-arg VERSION=$(VERSION) -t agentd:$(VERSION) .

# Local multi-tenant stack (runtime + Postgres), see README "Local test".
up:
	docker compose -p agentd -f deploy/docker-compose.yaml up -d --build

down: # secrets are only needed to start; dummies let compose tear down
	AGENT_RUNTIME_TOKEN=x RUNTIME_SECRET_KEY=x docker compose -p agentd -f deploy/docker-compose.yaml down

# Static Linux binaries + checksums in dist/ (used by .github/workflows/release.yml).
RELEASE_TARGETS ?= linux/amd64 linux/arm64
release:
	rm -rf dist && mkdir -p dist
	for t in $(RELEASE_TARGETS); do \
	  os=$${t%/*}; arch=$${t#*/}; d=agentd_$(VERSION)_$${os}_$${arch}; \
	  mkdir -p dist/$$d && \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags="$(LDFLAGS)" -o dist/$$d/agentd . && \
	  cp README.md agents.yaml dist/$$d/ && cp -r examples dist/$$d/ && \
	  tar -C dist -czf dist/$$d.tar.gz $$d && rm -rf dist/$$d || exit 1; \
	done
	cd dist && sha256sum *.tar.gz > SHA256SUMS 2>/dev/null || shasum -a 256 *.tar.gz > SHA256SUMS

clean:
	rm -rf bin dist

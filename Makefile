VERSION ?= $(shell git describe --tags --always 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test race lint run docker up down clean

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

clean:
	rm -rf bin

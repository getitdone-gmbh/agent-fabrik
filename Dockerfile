# syntax=docker/dockerfile:1
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /agentd .

# ~2 MB base with CA certificates and tzdata, runs as non-root.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /agentd /agentd
# Soft heap limit keeps RSS small; raise it for many concurrent runs.
ENV GOMEMLIMIT=48MiB
EXPOSE 8080
ENTRYPOINT ["/agentd"]

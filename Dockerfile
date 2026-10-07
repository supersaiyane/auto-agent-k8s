# Build stage. go1.26.6 carries the standard library fixes govulncheck
# reported for go1.26.1 (ISS-016). Pinned by digest (ISS-010).
FROM golang:1.26.6@sha256:0d1d3a794be25f809dd2cb3160d8c73276c4056a9f8242a138e908ddeee7b6b6 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=${VERSION}" -o /out/auto-agent ./cmd/auto-agent

# Runtime stage: distroless, no shell, runs as uid 65532 (ISS-010).
# /var/log/auto-agent is a mounted volume, never the image filesystem.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/auto-agent /auto-agent
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/auto-agent"]

# Build stage
FROM golang:1.22 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/auto-agent ./cmd/auto-agent

# Runtime stage
FROM alpine:3.19
RUN apk add --no-cache ca-certificates && mkdir -p /var/log/auto-agent
COPY --from=build /out/auto-agent /auto-agent
EXPOSE 8080
ENTRYPOINT ["/auto-agent"]

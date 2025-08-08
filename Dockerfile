# build
FROM golang:1.22 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/auto-agent ./cmd/auto-agent

# run
FROM gcr.io/distroless/base-debian12
USER 65532:65532
COPY --from=build /out/auto-agent /auto-agent
EXPOSE 8080
ENTRYPOINT ["/auto-agent"]

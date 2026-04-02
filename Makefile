IMAGE ?= ghcr.io/yourorg/auto-agent:1.0.0

.PHONY: build test lint vet docker push helm-install clean

build:
	CGO_ENABLED=0 go build -o bin/auto-agent ./cmd/auto-agent

test:
	go test -race -count=1 ./...

test-cover:
	go test -race -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out
	@echo ""
	@echo "HTML report: go tool cover -html=coverage.out"

vet:
	go vet ./...

lint: vet
	@which staticcheck > /dev/null 2>&1 || echo "install staticcheck: go install honnef.co/go/tools/cmd/staticcheck@latest"
	staticcheck ./... || true

tidy:
	go mod tidy

docker:
	docker build -t $(IMAGE) .

push:
	docker push $(IMAGE)

helm-install:
	helm upgrade --install auto-agent charts/auto-agent -n kube-system --create-namespace

helm-template:
	helm template auto-agent charts/auto-agent

clean:
	rm -rf bin/ coverage.out

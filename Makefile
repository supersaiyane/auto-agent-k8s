IMAGE ?= ghcr.io/yourorg/auto-agent:0.2.0

build:
	go build -o bin/auto-agent ./cmd/auto-agent

docker:
	docker build -t $(IMAGE) .

push:
	docker push $(IMAGE)

helm-install:
	helm upgrade --install auto-agent charts/auto-agent -n kube-system --create-namespace

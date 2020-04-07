BINARY=csccm
TAG=latest
IMAGE=tsuru/$(BINARY)
LOCAL_REGISTRY=10.200.10.1:5000
LINTER_ARGS = -j 4 --enable-gc --exclude "vendor" --skip="vendor" --vendor --enable=misspell --enable=gofmt --enable=goimports --enable=unused --deadline=60m --tests
RUN_FLAGS=-v 4

.PHONY: run
run: build
	./$(BINARY) $(RUN_FLAGS)

.PHONY: build
build:
	go build -o $(BINARY) ./cmd/controller

.PHONY: build-docker
build-docker:
	docker build --rm -t $(IMAGE):$(TAG) .

.PHONY: push
push: build-docker
	docker push $(IMAGE):$(TAG)

.PHONY: test
test:
	go test ./... -race -cover

.PHONY: lint
lint:
	curl -sfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b $$(go env GOPATH)/bin
	$$(go env GOPATH)/bin/golangci-lint run -c ./.golangci.yml ./...

.PHONY: minikube
minikube:
	make IMAGE=$(LOCAL_REGISTRY)/$(BINARY) push
	kubectl delete -f deployments/local.yml || true
	cat deployments/local.yml | sed 's~IMAGE~$(LOCAL_REGISTRY)/$(BINARY)~g' | kubectl apply -f -

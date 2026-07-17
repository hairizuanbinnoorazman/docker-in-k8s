DOCKUBE_BUNDLED_REGISTRY ?= true

ifneq ($(DOCKUBE_BUNDLED_REGISTRY),true)
ifneq ($(DOCKUBE_BUNDLED_REGISTRY),false)
$(error DOCKUBE_BUNDLED_REGISTRY must be true or false)
endif
endif

.PHONY: build deploy-dev test test-unit test-e2e fmt vet install install-registry uninstall uninstall-registry

build:
	mkdir -p bin
	go build -o bin/dockube ./cmd/dockube
	go build -o bin/dockube-controller ./cmd/controller

deploy-dev: install
	@tag="dockube-controller:dev-$$(date +%s)"; \
	docker build -f Dockerfile.controller -t "$$tag" .; \
	minikube image load "$$tag"; \
	kubectl set image deployment/dockube-controller -n dockube-system controller="$$tag"
	kubectl rollout status deployment/dockube-controller -n dockube-system --timeout=120s

test: test-unit

test-unit:
	go test ./...

test-e2e: build
	bash test/e2e/lifecycle.sh
	bash test/e2e/compose.sh
	bash test/e2e/build.sh

fmt:
	gofmt -w $$(find . -name '*.go' -type f)

vet:
	go vet ./...

install:
	minikube kubectl -- apply -f deploy/crd.yaml
	minikube kubectl -- apply -f deploy/install.yaml
ifeq ($(DOCKUBE_BUNDLED_REGISTRY),true)
	$(MAKE) install-registry
else
	@echo "Bundled registry disabled; builds must target an external registry"
endif

install-registry:
	minikube kubectl -- apply -f deploy/registry.yaml

uninstall:
	$(MAKE) uninstall-registry
	minikube kubectl -- delete -f deploy/install.yaml --ignore-not-found
	minikube kubectl -- delete -f deploy/crd.yaml --ignore-not-found

uninstall-registry:
	minikube kubectl -- delete -f deploy/registry.yaml --ignore-not-found

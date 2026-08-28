# Image URL to use for docker build/push targets
IMG ?= ghcr.io/alexli8408/tide:latest
CONTROLLER_GEN_VERSION ?= v0.21.0

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

.PHONY: all
all: build

##@ Development

.PHONY: generate
generate: ## Generate DeepCopy methods and CRD manifests with controller-gen.
	go run sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION) \
		object:headerFile="hack/boilerplate.go.txt" paths="./..."
	go run sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION) \
		crd rbac:roleName=tide-manager-role paths="./..." \
		output:crd:artifacts:config=config/crd/bases output:rbac:artifacts:config=config/rbac

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: fmt vet ## Run tests.
	go test ./... -coverprofile cover.out

.PHONY: cover
cover: test ## Open an HTML coverage report.
	go tool cover -html=cover.out -o cover.html

##@ Build

.PHONY: build
build: fmt vet ## Build the manager binary.
	go build -o bin/manager ./cmd

.PHONY: run
run: fmt vet ## Run the controller against the current kubeconfig context.
	go run ./cmd

.PHONY: docker-build
docker-build: ## Build the docker image.
	docker build -t $(IMG) .

##@ Deployment

.PHONY: install
install: ## Install the CRD into the current cluster.
	kubectl apply -f config/crd/bases

.PHONY: uninstall
uninstall: ## Remove the CRD from the current cluster.
	kubectl delete -f config/crd/bases

.PHONY: deploy
deploy: install ## Deploy the controller to the current cluster.
	kubectl apply -k config/default

.PHONY: undeploy
undeploy: ## Remove the controller from the current cluster.
	kubectl delete -k config/default

##@ Help

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)

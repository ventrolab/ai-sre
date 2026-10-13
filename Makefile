# Build, test and deploy Ventrolab AI SRE.
#
#   make test                      run all unit tests
#   make demo                      local kind cluster with AI SRE and broken workloads (deploy/demo)
#   make images                    build and push all images to REGISTRY_PUSH
#   make deploy VALUES=<file>      install or upgrade the chart in the current kube context
#
# REGISTRY_PULL is where the cluster pulls the images from (the same registry unless the
# nodes reach it under another address).

REGISTRY_PUSH ?= localhost:5050/ai-sre
REGISTRY_PULL ?= $(REGISTRY_PUSH)
# Evaluated once: a recursive variable re-ran `date` on every use and could split one build
# across two tags when the expansion crossed a second boundary.
ifndef TAG
TAG := $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)-$(shell date +%Y%m%d%H%M%S)
endif
NAMESPACE ?= ai-sre
RELEASE ?= ai-sre
# Values of the target deployment; required by `make deploy` (no default on purpose: the
# current kube context may be any cluster).
VALUES ?=
# Extra helm flags, e.g. HELM_ARGS='--set llm.provider=vllm --set-json llm.fallbacks=[]'
HELM_ARGS ?=

PY_SERVICES := ingest controller chat-gateway web
VENV := .venv
PIP_ENV := PIP_CONFIG_FILE=/dev/null PIP_INDEX_URL=https://pypi.org/simple

.PHONY: test test-python test-go venv images images-build deploy demo lint-chart host-agent cli

venv:
	test -d $(VENV) || python3 -m venv $(VENV)
	$(PIP_ENV) $(VENV)/bin/pip install -q -e libs/python/aisre-common pytest==8.3.3 pytest-asyncio==0.24.0 \
		$(foreach s,$(PY_SERVICES),-r services/$(s)/requirements.txt)

test: test-python test-go

test-python: venv
	@for s in $(PY_SERVICES); do (cd services/$$s && ../../$(VENV)/bin/python -m pytest -q) || exit 1; done

test-go:
	docker run --rm -v $(CURDIR)/services/action-guard:/src -v $(CURDIR)/playbooks:/playbooks:ro \
		-v ai-sre-gomod:/go/pkg/mod -w /src golang:1.23.4 sh -c 'go vet ./... && go test ./...'
	docker run --rm -v $(CURDIR):/src -v ai-sre-gomod:/go/pkg/mod -w /src golang:1.25.14 \
		sh -c 'go vet ./... && go test ./...'

IMAGES := $(PY_SERVICES) action-guard tool-hub k8s-agent

# Build all images locally as $(REGISTRY_PUSH)/<name>:$(TAG) without pushing.
images-build:
	@for s in $(PY_SERVICES); do \
		docker build -q -f services/$$s/Dockerfile -t $(REGISTRY_PUSH)/$$s:$(TAG) . >/dev/null || exit 1; \
	done
	docker build -q -t $(REGISTRY_PUSH)/action-guard:$(TAG) services/action-guard >/dev/null
	@for c in services/tool-hub agents/k8s-agent; do n=$$(basename $$c); \
		docker build -q -f build/go.Dockerfile --build-arg CMD=$$c --build-arg VERSION=$(TAG) -t $(REGISTRY_PUSH)/$$n:$(TAG) . >/dev/null || exit 1; \
	done
	@echo "built $(IMAGES) as $(REGISTRY_PUSH)/<name>:$(TAG)"

images: images-build
	@for i in $(IMAGES); do docker push -q $(REGISTRY_PUSH)/$$i:$(TAG) >/dev/null || exit 1; done
	@echo $(TAG) > .last-tag
	@echo "pushed tag $(TAG)"

# Static host-agent binary for standalone hosts (dist/host-agent).
host-agent:
	docker run --rm -v $(CURDIR):/src -v ai-sre-gomod:/go/pkg/mod -w /src -e CGO_ENABLED=0 golang:1.25.14 \
		go build -buildvcs=false -trimpath -ldflags="-s -w -X main.version=$(TAG)" -o dist/host-agent ./agents/host-agent

# aisre CLI for this machine (dist/aisre); GOOS/GOARCH override, e.g. make cli GOOS=darwin GOARCH=arm64.
GOOS ?= linux
GOARCH ?= amd64
cli:
	docker run --rm -v $(CURDIR):/src -v ai-sre-gomod:/go/pkg/mod -w /src -e CGO_ENABLED=0 -e GOOS=$(GOOS) -e GOARCH=$(GOARCH) \
		golang:1.25.14 go build -buildvcs=false -trimpath -ldflags="-s -w -X main.version=$(TAG)" -o dist/aisre ./cli/aisre

lint-chart:
	helm dependency build charts/ai-sre >/dev/null
	helm lint charts/ai-sre -f $(or $(VALUES),deploy/demo/values.yaml)

deploy:
	@[ -n "$(VALUES)" ] || { echo "set VALUES=<values file of the target deployment>" >&2; exit 1; }
	@echo "deploying to kube context $$(kubectl config current-context) with $(VALUES)"
	helm dependency build charts/ai-sre >/dev/null
	helm upgrade --install $(RELEASE) charts/ai-sre -n $(NAMESPACE) --create-namespace \
		-f $(VALUES) --set image.registry=$(REGISTRY_PULL) --set image.tag=$$(cat .last-tag) $(HELM_ARGS) --wait --timeout 10m
	@# Holmes reads skills only at startup.
	kubectl -n $(NAMESPACE) rollout restart deploy/$(RELEASE)-holmes
	kubectl -n $(NAMESPACE) rollout status deploy/$(RELEASE)-holmes --timeout 5m

demo:
	$(MAKE) -C deploy/demo demo

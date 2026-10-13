# Ventrolab AI SRE demo on a local kind cluster.
#
#   export ANTHROPIC_API_KEY=...         # or OPENAI_API_KEY with LLM_PROVIDER=openai
#   make demo                            # cluster, monitoring, AI SRE, broken workloads
#   make status                          # incidents and their root causes
#   make clean                           # delete the cluster
#
# Options: LLM_PROVIDER=claude|openai|mock, MODE=readonly|autofix, DEMO_PORT=8088,
# VERSION=<chart version> (default: latest release), DEV=1 (build images and use the chart
# from this checkout instead of the release).

CLUSTER ?= ventrolab-demo
DEMO_PORT ?= 8088
LLM_PROVIDER ?= claude
MODE ?= readonly
VERSION ?=
DEV ?=
KIND ?= kind
CHART ?= oci://ghcr.io/ventrolab/ai-sre/charts/ai-sre
NS := ai-sre
MON := monitoring
CTX := kind-$(CLUSTER)
KUBECTL := kubectl --context $(CTX)
HELM := helm --kube-context $(CTX)
ROOT := $(abspath ../..)
DEV_TAG := dev
DEV_IMAGES := ingest controller chat-gateway web action-guard tool-hub k8s-agent

.PHONY: demo check cluster images llm-secret install monitoring alerting workloads info status smoke clean

demo: check cluster images llm-secret install monitoring alerting workloads info

check:
	@for t in docker $(KIND) helm kubectl; do command -v $$t >/dev/null || { echo "missing: $$t" >&2; exit 1; }; done
	@docker info >/dev/null 2>&1 || { echo "docker is not running" >&2; exit 1; }
	@case "$(LLM_PROVIDER)" in \
	  claude) [ -n "$$ANTHROPIC_API_KEY" ] || { echo "set ANTHROPIC_API_KEY (or LLM_PROVIDER=openai with OPENAI_API_KEY)" >&2; exit 1; } ;; \
	  openai) [ -n "$$OPENAI_API_KEY" ] || { echo "set OPENAI_API_KEY" >&2; exit 1; } ;; \
	  mock) ;; \
	  *) echo "LLM_PROVIDER must be claude, openai or mock" >&2; exit 1 ;; \
	esac

# The console NodePort 30080 is published on localhost:$(DEMO_PORT).
cluster:
	@$(KIND) get clusters 2>/dev/null | grep -qx $(CLUSTER) || \
	printf 'kind: Cluster\napiVersion: kind.x-k8s.io/v1alpha4\nnodes:\n  - role: control-plane\n    extraPortMappings:\n      - {containerPort: 30080, hostPort: $(DEMO_PORT), listenAddress: "127.0.0.1"}\n' | \
	$(KIND) create cluster --name $(CLUSTER) --config -

# DEV=1: build the images of this checkout and load them into the kind node.
images:
ifneq ($(DEV),)
	$(MAKE) -C $(ROOT) images-build REGISTRY_PUSH=ventrolab-demo TAG=$(DEV_TAG)
	@for i in $(DEV_IMAGES); do $(KIND) load docker-image ventrolab-demo/$$i:$(DEV_TAG) --name $(CLUSTER) || exit 1; done
else
	@true
endif

llm-secret:
	@$(KUBECTL) create namespace $(NS) --dry-run=client -o yaml | $(KUBECTL) apply -f - >/dev/null
	@$(KUBECTL) -n $(NS) create secret generic ai-sre-llm-providers \
		--from-literal=ANTHROPIC_API_KEY="$${ANTHROPIC_API_KEY:-unset}" --from-literal=OPENAI_API_KEY="$${OPENAI_API_KEY:-unset}" \
		--dry-run=client -o yaml | $(KUBECTL) apply -f - >/dev/null
	@echo "LLM credentials stored in Secret $(NS)/ai-sre-llm-providers"

ifneq ($(DEV),)
INSTALL_CHART := $(ROOT)/charts/ai-sre
INSTALL_ARGS := --set image.registry=ventrolab-demo --set image.tag=$(DEV_TAG) --set image.pullPolicy=IfNotPresent
else
INSTALL_CHART := $(CHART)
INSTALL_ARGS := $(if $(VERSION),--version $(VERSION))
endif

install:
ifneq ($(DEV),)
	helm dependency build $(ROOT)/charts/ai-sre >/dev/null
endif
	$(HELM) upgrade --install ai-sre $(INSTALL_CHART) $(INSTALL_ARGS) -n $(NS) -f values.yaml \
		--set llm.provider=$(LLM_PROVIDER) --set remediation.mode=$(MODE) \
		--set web.baseUrl=http://localhost:$(DEMO_PORT) --wait --timeout 15m >/dev/null
	@echo "ai-sre installed"

monitoring:
	@helm repo add vm https://victoriametrics.github.io/helm-charts/ >/dev/null 2>&1 || true
	@helm repo add prometheus-community https://prometheus-community.github.io/helm-charts >/dev/null 2>&1 || true
	@helm repo update vm prometheus-community >/dev/null
	$(HELM) upgrade --install vmsingle vm/victoria-metrics-single --version 0.48.0 -n $(MON) --create-namespace \
		-f monitoring/vmsingle.yaml --wait >/dev/null
	$(HELM) upgrade --install kube-state-metrics prometheus-community/kube-state-metrics --version 8.6.0 -n $(MON) \
		-f monitoring/ksm.yaml --wait >/dev/null
	@echo "monitoring installed"

# Alertmanager authenticates to the ingest webhook with the token of the release.
alerting:
	@values=$$(mktemp) && trap 'rm -f $$values' EXIT && \
	token=$$($(KUBECTL) -n $(NS) get secret ai-sre-secrets -o jsonpath='{.data.INGEST_TOKEN}' | base64 -d) && \
	sed "s/__INGEST_TOKEN__/$$token/" monitoring/vmalert.yaml.tmpl > $$values && \
	$(HELM) upgrade --install vmalert vm/victoria-metrics-alert --version 0.50.0 -n $(MON) -f $$values --wait >/dev/null && \
	echo "alerting installed"

workloads:
	$(KUBECTL) apply -f workloads.yaml

info:
	@pw=$$($(KUBECTL) -n $(NS) get secret ai-sre-web -o jsonpath='{.data.WEB_ADMIN_PASSWORD}' | base64 -d); \
	echo; echo "Ventrolab AI SRE demo is running."; \
	echo "  Console:  http://localhost:$(DEMO_PORT)   user admin, password $$pw"; \
	echo "  Alerts fire within 2-5 minutes; investigations take about a minute each."; \
	echo "  Follow:   make status    or    kubectl --context $(CTX) -n $(NS) logs -f deploy/ai-sre-chat-gateway"; \
	echo "  CLI:      create an API token (scope status:read) in the console, then"; \
	echo "            aisre login --url http://localhost:$(DEMO_PORT)"; \
	echo "  Remove:   make clean"

status:
	@$(KUBECTL) -n $(NS) exec ai-sre-postgres-0 -- psql -U aisre -d aisre -P pager=off \
		-c "select left(id::text, 8) as id, status, title, round(confidence::numeric, 2) as conf, left(root_cause, 90) as root_cause from incidents order by created_at"

# CI check: waits until SMOKE_INCIDENTS incidents are diagnosed (alerts need 2-5 minutes).
SMOKE_INCIDENTS ?= 2
SMOKE_TIMEOUT ?= 900
smoke:
	@end=$$(( $$(date +%s) + $(SMOKE_TIMEOUT) )); \
	while [ $$(date +%s) -lt $$end ]; do \
	  n=$$($(KUBECTL) -n $(NS) exec ai-sre-postgres-0 -- psql -U aisre -d aisre -tA \
	      -c "select count(*) from incidents where status = 'diagnosed'" 2>/dev/null || echo 0); \
	  echo "diagnosed incidents: $$n"; \
	  [ "$${n:-0}" -ge $(SMOKE_INCIDENTS) ] && { $(MAKE) -s status; exit 0; }; \
	  sleep 20; \
	done; \
	echo "fewer than $(SMOKE_INCIDENTS) diagnosed incidents after $(SMOKE_TIMEOUT)s" >&2; $(MAKE) -s status; exit 1

clean:
	$(KIND) delete cluster --name $(CLUSTER)

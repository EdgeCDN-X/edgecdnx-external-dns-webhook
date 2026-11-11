# End-to-end test harness for the EdgeCDN-X ExternalDNS webhook.
# Copy .env.example to .env first; see ./README.md.

.PHONY: all create build deploy ingress status verify cleanup

all: ## create clusters, build image, deploy, create ingresses
	./e2e.sh all

create: ## create the two kind clusters (fra, ams)
	./e2e.sh create

build: ## build the webhook image and load it into both clusters
	./e2e.sh build

deploy: ## deploy ExternalDNS + webhook sidecar into both clusters
	./e2e.sh deploy

ingress: ## create/update the e2e Ingress with the shared hostname
	./e2e.sh ingress

status: ## show pods, ingresses, and recent logs
	./e2e.sh status

verify: ## list resulting Locations/profiles/DNS endpoints from the API
	./e2e.sh verify

cleanup: ## delete both kind clusters
	./e2e.sh cleanup

.PHONY: check build vet fmt test cover examples tidy walkthrough \
        docker-up docker-obs docker-down docker-reset demo demo-requests

# --- Local stack for examples/api (see examples/infra/local) ----------------

COMPOSE := docker compose -f examples/infra/local/docker-compose.yml

POSTGRES_USER     ?= kit
POSTGRES_PASSWORD ?= secret
POSTGRES_DB       ?= kitdemo
POSTGRES_PORT     ?= 5433

# sslmode=disable is stated rather than left to pgx's negotiation, so a local
# run cannot stall on a TLS handshake the container was never set up for.
DATABASE_URL  ?= postgres://$(POSTGRES_USER):$(POSTGRES_PASSWORD)@localhost:$(POSTGRES_PORT)/$(POSTGRES_DB)?sslmode=disable
OTLP_ENDPOINT ?= localhost:4317
KIT_LOG_DIR   ?= /tmp/kit-logs

# 8081, not the service's own :8080 default, for the same reason Postgres is on
# 5433: freelance-tax-copilot's API holds 8080. Changing this means changing the
# scrape target in examples/infra/local/prometheus.yml to match.
API_PORT ?= 8081
API_ADDR ?= http://localhost:$(API_PORT)

# docker-compose.yml carries its own ${VAR:-default} fallbacks, and make does not
# export variables it defined itself -- only ones from the environment or the
# command line. Without these exports the two sets of defaults are independent
# and drift apart silently the first time one of them is edited.
export POSTGRES_USER POSTGRES_PASSWORD POSTGRES_DB POSTGRES_PORT KIT_LOG_DIR

# examples/ is a nested module, so `go test ./...` from here silently skips it.
# Use `make check` rather than remembering that.
check: fmt build vet test examples

build:
	go build ./...

vet:
	go vet ./...
	go -C examples vet ./...

# Fails if anything is unformatted, rather than reformatting silently.
fmt:
	@out="$$(gofmt -l . )"; \
	if [ -n "$$out" ]; then echo "unformatted:"; echo "$$out"; exit 1; fi

test:
	go test ./... -count=1

cover:
	go test ./... -count=1 -cover

# The examples module: vet and test compile it, and every Example function
# asserts its own output. `go build ./...` cannot be used here — it would try
# to write a binary named "api" over the api/ directory.
examples:
	go -C examples vet ./...
	go -C examples test ./... -count=1

# Run the end-to-end walkthrough and print what a client actually sees.
walkthrough:
	KIT_EXAMPLE_LOGS=1 go -C examples test ./api -run Example -v

tidy:
	go mod tidy
	go -C examples mod tidy

# Database only. Enough to run the example; nothing to look at yet.
docker-up: $(KIT_LOG_DIR)
	$(COMPOSE) up -d

# Adds Grafana/Tempo/Loki, Prometheus and Alloy. Grafana is on :3001.
docker-obs: $(KIT_LOG_DIR)
	$(COMPOSE) --profile obs up -d

# Must exist before compose runs, not after. Docker creates a missing bind-mount
# source itself, as root, and then `make demo` cannot write into it.
$(KIT_LOG_DIR):
	@mkdir -p $(KIT_LOG_DIR)

docker-down:
	$(COMPOSE) --profile obs down

# Drops the volumes too. Needed after editing schema.sql, because Postgres runs
# its initdb scripts only against an empty data directory.
docker-reset:
	$(COMPOSE) --profile obs down -v

# Runs the example service on the host against the stack above.
#
# Only stdout is teed: obs.NewLogger writes JSON logs there, while stderr is
# reserved for the process failing to start. Keeping them apart means Alloy
# never has to parse a non-JSON line.
demo: $(KIT_LOG_DIR)
	@test -w "$(KIT_LOG_DIR)" || { \
		echo "$(KIT_LOG_DIR) is not writable."; \
		echo "Docker most likely created it as root before make could. Remove it and retry:"; \
		echo "    sudo rmdir $(KIT_LOG_DIR) && make docker-obs"; \
		exit 1; }
	@echo "logs -> $(KIT_LOG_DIR)/api.log   traces -> $(OTLP_ENDPOINT)   metrics -> $(API_ADDR)/metrics"
	DATABASE_URL="$(DATABASE_URL)" OTLP_ENDPOINT="$(OTLP_ENDPOINT)" ADDR=":$(API_PORT)" \
		go -C examples run ./api | tee $(KIT_LOG_DIR)/api.log

# Drives the same requests Example_endToEnd asserts against the real service, so
# Grafana has something in it. Run it in a second terminal while `make demo` is up.
demo-requests:
	@scripts/demo-requests.sh $(API_ADDR)

#!/usr/bin/env bash
# Drives the example service through the same sequence Example_endToEnd asserts
# in memory, but against the real thing: real Postgres, real spans exported over
# OTLP, real metrics on /metrics.
#
# The point is not the output here -- the in-memory example already pins that
# byte for byte -- it is that afterwards Grafana has a trace, a log line and a
# counter you can click through.
set -euo pipefail

API="${1:-http://localhost:8080}"
NAME="bolt-$$" # unique per run, so the conflict below is the only conflict

req() {
	local method="$1" path="$2" body="${3:-}"
	printf '\n\033[1m%s %s\033[0m\n' "$method" "$path"
	if [ -n "$body" ]; then
		curl -sS -X "$method" "$API$path" \
			-H 'Content-Type: application/json' -d "$body" \
			-w '  -> %{http_code} %{content_type}\n'
	else
		curl -sS -X "$method" "$API$path" -w '  -> %{http_code} %{content_type}\n'
	fi
}

# A path parameter that is not an identifier at all: 422, not 404.
req GET /api/v1/widgets/banana

# Well-formed id, no such row. The service maps its store's sentinel.
req GET /api/v1/widgets/11111111-1111-1111-1111-111111111111

# Two field failures at once, named as the client sent them.
req POST /api/v1/widgets '{"name":"","quantity":-4}'

# Strict decoding: a misspelled field is rejected, not ignored.
req POST /api/v1/widgets "{\"name\":\"$NAME\",\"quantity\":1,\"qty\":2}"

# The happy path. Capture the id so the reads below are real.
printf '\n\033[1mPOST /api/v1/widgets\033[0m\n'
CREATED=$(curl -sS -X POST "$API/api/v1/widgets" \
	-H 'Content-Type: application/json' \
	-d "{\"name\":\"$NAME\",\"quantity\":12}")
echo "  $CREATED"
ID=$(printf '%s' "$CREATED" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')

# The same name again: a conflict carrying an extension member.
req POST /api/v1/widgets "{\"name\":\"$NAME\",\"quantity\":3}"

if [ -n "$ID" ]; then
	req GET "/api/v1/widgets/$ID"
	req DELETE "/api/v1/widgets/$ID"
	req GET "/api/v1/widgets/$ID"
fi

# A panic, to show the recoverer producing the same problem+json shape and the
# span being marked Error with an exception event -- the one case that should
# actually redden a dashboard.
req GET /api/v1/boom

cat <<EOF

Now look at it:
  Grafana    http://localhost:3001
    Traces   Explore -> Tempo -> Search -> service.name = kit-example-api
             a POST span with widgetService.create and INSERT widgets beneath it
    Logs     Explore -> Loki -> {service_name="kit-example-api"}
             expand a line, click the TraceID field to jump to its trace
    Metrics  Explore -> Prometheus -> widgets_created_total
                                      http_server_request_duration_seconds_count
  Raw        $API/metrics
EOF

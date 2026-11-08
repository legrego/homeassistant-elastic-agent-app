#!/usr/bin/env bash
# Smoke test for the Elastic Agent app image. Needs Docker, curl, jq and python3.
#
# 1. Builds a journal file with a newer systemd than the image's journalctl, the
#    way Home Assistant OS does.
# 2. Fleet mode: runs the app entrypoint against a fake Fleet URL and checks
#    that it finds the journal and handles enrollment state.
# 3. Standalone mode: runs the app against a local Elasticsearch with security
#    on and a fake Supervisor, given only the elastic superuser's password.
#    Checks that the app installs the Core ingest pipeline, creates an API key
#    that can only write the app's data streams, and that the journal arrives
#    parsed and labelled with the Home Assistant host.
# 4. Restarts without the password and checks that the saved key is reused and
#    nothing is sent twice.
# 5. Checks unparsed delivery when a saved key can't recreate a missing pipeline.
# 6. Runs with only an API key, no pipeline and some log sources turned off.
#
# Usage: tests/smoke/smoke.sh   (set IMAGE to skip the build)
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "${HERE}/../.." && pwd)"
IMAGE="${IMAGE:-}"
ES_IMAGE="${ES_IMAGE:-docker.elastic.co/elasticsearch/elasticsearch:9.5.6}"
SYSTEMD_IMAGE="${SYSTEMD_IMAGE:-fedora:41}"
PYTHON_IMAGE="${PYTHON_IMAGE:-python:3.13-alpine}"
NAME="haos-smoke-$$"
DATA="${NAME}-data" # Docker volume for the app's /data
WORKDIR="$(mktemp -d)"
ES_PORT="${ES_PORT:-19200}"
ES="http://localhost:${ES_PORT}"
ES_PASSWORD="smoke-$(python3 -c 'import secrets; print(secrets.token_hex(8))')"
SUPERVISOR_TOKEN="smoke-supervisor-token"
HA_HOST_NAME="haos-smoke"
MACHINE_ID="$(python3 -c 'import uuid; print(uuid.uuid4().hex)')"
failures=0

log() { printf '\n==> %s\n' "$*"; }
pass() { printf '  ok   %s\n' "$*"; }
fail() {
    printf '  FAIL %s\n' "$*"
    failures=$((failures + 1))
}
check() { # check DESCRIPTION EXPECTED ACTUAL
    if [[ "$2" == "$3" ]]; then pass "$1"; else fail "$1: expected '$2', got '$3'"; fi
}
expect() { # expect DESCRIPTION COMMAND...
    local description="$1"
    shift
    if "$@"; then pass "${description}"; else fail "${description}"; fi
}
expect_not() { # expect_not DESCRIPTION COMMAND...
    local description="$1"
    shift
    if "$@"; then fail "${description}"; else pass "${description}"; fi
}

# The app's /data is a Docker volume, as on Home Assistant OS. That keeps the
# root-owned state off the host, and the agent's control socket works on
# Docker Desktop too, where bind mounts don't support it.
in_data() { docker run --rm -i -v "${DATA}:/data" --entrypoint bash "${IMAGE}" -c "$1"; }
set_options() { in_data 'cat >/data/options.json'; }
update_options() { # update_options JQ_FILTER
    in_data "jq '$1' /data/options.json >/tmp/o && cat /tmp/o >/data/options.json"
}

# run_to_exit LOG [DOCKER_RUN_ARGS...]: runs the app until it exits, for at most
# 60 seconds, and returns its exit code. Used for starts that should fail.
run_to_exit() {
    local out="$1"
    shift
    docker run -d --name "${NAME}-app" "$@" -v "${DATA}:/data" "${IMAGE}" >/dev/null
    for _ in $(seq 1 60); do
        [[ "$(docker inspect -f '{{.State.Running}}' "${NAME}-app")" == true ]] || break
        sleep 1
    done
    docker logs "${NAME}-app" >"${out}" 2>&1 || true
    local code
    code="$(docker inspect -f '{{.State.ExitCode}}' "${NAME}-app")"
    docker rm -f "${NAME}-app" >/dev/null
    [[ "$(cat "${out}")" == *"Starting Elastic Agent"* ]] && return 0
    return "${code}"
}

cleanup() {
    docker rm -f "${NAME}-es" "${NAME}-app" "${NAME}-supervisor" >/dev/null 2>&1 || true
    docker network rm "${NAME}" >/dev/null 2>&1 || true
    docker volume rm -f "${DATA}" >/dev/null 2>&1 || true
    rm -rf "${WORKDIR}"
}
trap cleanup EXIT

if [[ -z "${IMAGE}" ]]; then
    log "Building the app image"
    IMAGE="ha-app-elastic-agent:smoke"
    docker build -q -t "${IMAGE}" "${ROOT}/elastic_agent" >/dev/null
fi
docker network create "${NAME}" >/dev/null
docker volume create "${DATA}" >/dev/null

log "Writing a journal with ${SYSTEMD_IMAGE}"
mkdir -p "${WORKDIR}/journal/${MACHINE_ID}"
python3 "${HERE}/make_journal.py" "${WORKDIR}/haos.export" "${MACHINE_ID}"
docker run --rm -v "${WORKDIR}:/work" "${SYSTEMD_IMAGE}" bash -c "
    dnf install -q -y systemd-journal-remote >/dev/null 2>&1
    echo \"  writer: \$(journalctl --version | head -1)\"
    /usr/lib/systemd/systemd-journal-remote --output=/work/journal/${MACHINE_ID}/system.journal /work/haos.export
    chmod -R a+rX /work/journal"
docker run --rm --entrypoint journalctl -v "${WORKDIR}/journal:/var/log/journal:ro" "${IMAGE}" --version | head -1 | sed 's/^/  reader: /'
entries="$(docker run --rm --entrypoint bash -v "${WORKDIR}/journal:/var/log/journal:ro" "${IMAGE}" \
    -c "echo ${MACHINE_ID} > /etc/machine-id && journalctl --no-pager -o cat | wc -l")"
check "image's journalctl reads the newer journal" 11 "${entries}"

# --- Fleet mode -------------------------------------------------------------------

log "Fleet mode: first start"
set_options <<EOF
{"fleet_url": "https://fleet.invalid:8220", "enrollment_token": "token-one", "insecure": false, "tags": ["home-assistant", "smoke"]}
EOF
run_entrypoint() {
    docker run -d --name "${NAME}-app" \
        -v "${DATA}:/data" -v "${WORKDIR}/journal:/var/log/journal:ro" \
        "${IMAGE}" >/dev/null
    sleep "${1:-15}"
    docker logs "${NAME}-app" >"${WORKDIR}/entry.log" 2>&1 || true
    docker exec "${NAME}-app" cat /etc/machine-id >"${WORKDIR}/machine-id" 2>/dev/null || true
    # The environment the agent process was started with.
    docker exec "${NAME}-app" bash -c '
        for p in /proc/[0-9]*; do
            if grep -qa FLEET_URL "$p/environ" 2>/dev/null; then tr "\0" "\n" <"$p/environ"; break; fi
        done' >"${WORKDIR}/env" 2>/dev/null || true
    docker rm -f "${NAME}-app" >/dev/null
}
run_entrypoint 15
expect "logs the mode" grep -q "Running in fleet mode" "${WORKDIR}/entry.log"
expect "entrypoint finds the host journal" grep -q "Found the host journal for machine ID ${MACHINE_ID}" "${WORKDIR}/entry.log"
check "machine ID is the host's" "${MACHINE_ID}" "$(cat "${WORKDIR}/machine-id")"
check "FLEET_URL" "FLEET_URL=https://fleet.invalid:8220" "$(grep '^FLEET_URL=' "${WORKDIR}/env")"
check "ELASTIC_AGENT_TAGS" "ELASTIC_AGENT_TAGS=home-assistant,smoke" "$(grep '^ELASTIC_AGENT_TAGS=' "${WORKDIR}/env")"
check "STATE_PATH" "STATE_PATH=/data/state" "$(grep '^STATE_PATH=' "${WORKDIR}/env")"
expect "agent tries to enroll with the configured URL" grep -q "fleet.invalid" "${WORKDIR}/entry.log"
expect "state lives under /data/state" in_data 'test -d /data/state'
expect_not "the enrollment token isn't logged" grep -q "enrollment_token\|token-one" "${WORKDIR}/entry.log"

log "Fleet mode: token changed"
in_data 'touch /data/state/marker'
update_options '.enrollment_token = "token-two"'
run_entrypoint 5
expect "old state moved to state.previous" in_data 'test -f /data/state.previous/marker'
expect "logs the re-enrollment" grep -q "Enrolling again" "${WORKDIR}/entry.log"

log "Fleet mode: unchanged options preserve state"
in_data 'touch /data/state/unchanged-marker'
run_entrypoint 5
expect "unchanged options keep existing state" in_data 'test -f /data/state/unchanged-marker'

log "Fleet mode: certificate verification changed"
for insecure in true false; do
    in_data "touch /data/state/tls-${insecure}-marker"
    update_options ".insecure = ${insecure}"
    run_entrypoint 5
    expect "insecure=${insecure} replaces the previous enrollment state" \
        in_data "test -f /data/state.previous/tls-${insecure}-marker"
    check "FLEET_INSECURE" "FLEET_INSECURE=${insecure}" "$(grep '^FLEET_INSECURE=' "${WORKDIR}/env")"
done

log "Both modes configured"
in_data 'cp /data/options.json /data/options.fleet.json'
update_options '.elasticsearch_url = "http://elasticsearch:9200"'
if run_to_exit "${WORKDIR}/both.log"; then
    fail "the app starts with both elasticsearch_url and fleet_url"
else
    expect "refuses to start with both modes" grep -q "not both" "${WORKDIR}/both.log"
fi
in_data 'mv /data/options.fleet.json /data/options.json'

# --- Standalone mode -------------------------------------------------------------

log "Starting Elasticsearch with security on"
docker run -d --name "${NAME}-es" --network "${NAME}" --network-alias elasticsearch \
    -p "127.0.0.1:${ES_PORT}:9200" \
    -e discovery.type=single-node -e ELASTIC_PASSWORD="${ES_PASSWORD}" \
    -e xpack.security.http.ssl.enabled=false -e xpack.security.transport.ssl.enabled=false \
    -e ES_JAVA_OPTS="-Xms512m -Xmx512m" "${ES_IMAGE}" >/dev/null
es() { curl -fsS -u "elastic:${ES_PASSWORD}" -H 'Content-Type: application/json' "$@"; }
for _ in $(seq 1 90); do
    es "${ES}/_cluster/health?wait_for_status=yellow&timeout=1s" >/dev/null 2>&1 && break
    sleep 2
done
es "${ES}" >/dev/null || {
    fail "Elasticsearch didn't start"
    exit 1
}

# Answers /host/info like the Supervisor, for callers with the right token.
docker run -d --name "${NAME}-supervisor" --network "${NAME}" --network-alias supervisor \
    "${PYTHON_IMAGE}" python3 -c "
import http.server, json
class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        ok = self.path == '/host/info' and self.headers.get('Authorization') == 'Bearer ${SUPERVISOR_TOKEN}'
        self.send_response(200 if ok else 401)
        self.send_header('Content-Type', 'application/json')
        self.end_headers()
        self.wfile.write(json.dumps({'result': 'ok', 'data': {'hostname': '${HA_HOST_NAME}'}} if ok else {}).encode())
http.server.HTTPServer(('', 80), Handler).serve_forever()" >/dev/null

count() {
    es -XPOST "${ES}/logs-$1-default/_refresh" >/dev/null 2>&1 || true
    es "${ES}/logs-$1-default/_count" 2>/dev/null | jq -r '.count // 0' || echo 0
}
# wait_for CORE SYSTEM: waits until at least that many documents arrived.
wait_for() {
    for _ in $(seq 1 60); do
        [[ "$(count haos.core)" -ge "$1" && "$(count haos.system)" -ge "$2" ]] && break
        sleep 2
    done
    sleep 5 # let any extra documents arrive before counting
}
start_app() {
    docker run -d --name "${NAME}-app" --network "${NAME}" \
        -e SUPERVISOR_TOKEN="${SUPERVISOR_TOKEN}" \
        -v "${DATA}:/data" -v "${WORKDIR}/journal:/var/log/journal:ro" \
        "${IMAGE}" >/dev/null
}
stop_app() {
    docker logs "${NAME}-app" >"${WORKDIR}/app.log" 2>&1 || true
    docker rm -f "${NAME}-app" >/dev/null
}
search() { es "${ES}/logs-haos.$1-default/_search?size=20&sort=@timestamp"; }

log "Standalone mode: superuser password only"
in_data 'touch /data/state/fleet-marker'
set_options <<EOF
{"elasticsearch_url": "http://elasticsearch:9200", "username": "elastic", "password": "${ES_PASSWORD}", "insecure": false, "tags": ["smoke"], "read_existing_logs": true}
EOF
start_app
wait_for 4 3
stop_app
expect "logs the mode" grep -q "Running in standalone mode" "${WORKDIR}/app.log"
expect "Fleet state moved aside" in_data 'test -f /data/state.previous/fleet-marker'
expect "reads the host name from the Supervisor" grep -q "Reporting host name ${HA_HOST_NAME}" "${WORKDIR}/app.log"
expect "installs the ingest pipeline" grep -q "Installed version 1 of the logs-haos.core@custom ingest pipeline" "${WORKDIR}/app.log"
check "pipeline is marked as the app's" "homeassistant-elastic-agent-app" \
    "$(es "${ES}/_ingest/pipeline/logs-haos.core@custom" | jq -r '.[]._meta.managed_by')"
expect "creates an API key" grep -q "Created API key" "${WORKDIR}/app.log"
check "saved key is private" "600" "$(in_data 'stat -c %a /data/elasticsearch-api-key.json')"
expect_not "the password isn't logged" grep -q "${ES_PASSWORD}" "${WORKDIR}/app.log"
check "Core events (8 lines joined into 4)" 4 "$(count haos.core)"
check "Supervisor, app and host events" 3 "$(count haos.system)"
check "system sources" "NetworkManager,addon_core_ssh,hassio_supervisor" \
    "$(search system | jq -r '[.hits.hits[]._source | .container.name // .syslog.identifier // .log.syslog.appname // "?"] | sort | join(",")')"
search core >"${WORKDIR}/core.json"
docs() { jq -r "[.hits.hits[]._source | $1] | join(\"|\")" "${WORKDIR}/core.json"; }
check "host.name is the Home Assistant host" "${HA_HOST_NAME}" "$(docs '.host.name' | tr '|' '\n' | sort -u)"
check "host.id is the host machine ID" "${MACHINE_ID}" "$(docs '.host.id' | tr '|' '\n' | sort -u)"
check "tags" "smoke" "$(docs '.tags | join(",")' | tr '|' '\n' | sort -u)"

log "Standalone mode: Core logs are parsed on ingest"
check "log.level" "info|warning|error|info" "$(docs '.log.level // "-"')"
check "log.logger" "homeassistant.bootstrap|homeassistant.components.http.ban|custom_components.elasticsearch.es_publish|homeassistant.components.recorder.core" "$(docs '.log.logger // "-"')"
check "homeassistant.integration" "-|http|elasticsearch|recorder" "$(docs '.homeassistant.integration // "-"')"
check "process.thread.name" "MainThread|MainThread|MainThread|SyncWorker_3" "$(docs '.process.thread.name // "-"')"
check "error.type" "-|-|ConnectionError|-" "$(docs '.error.type // "-"')"
check "error.message" "-|-|cluster unavailable|-" "$(docs '.error.message // "-"')"
check "message without ANSI codes or traceback" "Error publishing documents" "$(jq -r '.hits.hits[2]._source.message' "${WORKDIR}/core.json")"
check "stack trace lines" 4 "$(jq -r '.hits.hits[2]._source.error.stack_trace' "${WORKDIR}/core.json" | wc -l | tr -d ' ')"
check "no parse failures" "" "$(docs '.tags // [] | map(select(. != "smoke")) | join(",")' | tr -d '|')"

log "Standalone mode: the created API key can only write the app's data"
key="$(in_data 'jq -r "\"\(.id):\(.api_key)\" | @base64" /data/elasticsearch-api-key.json')"
as_key() { curl -s -o /dev/null -w '%{http_code}' -H "Authorization: ApiKey ${key}" -H 'Content-Type: application/json' "$@"; }
doc='{"@timestamp": "2026-11-07T00:00:00Z", "message": "smoke"}'
check "can write logs-haos.*" 201 "$(as_key -XPOST "${ES}/logs-haos.keycheck-default/_doc" -d "${doc}")"
check "can't write other data streams" 403 "$(as_key -XPOST "${ES}/logs-other-default/_doc" -d "${doc}")"
check "can't read log data" 403 "$(as_key "${ES}/logs-haos.core-default/_search")"
check "can check ingest pipelines" 200 "$(as_key "${ES}/_ingest/pipeline/logs-haos.core@custom")"
check "can't modify ingest pipelines" 403 "$(as_key -XPUT "${ES}/_ingest/pipeline/smoke-forbidden" -d '{"processors": []}')"
es -XDELETE "${ES}/_data_stream/logs-haos.keycheck-default" >/dev/null

log "Standalone mode: restart with the password cleared"
update_options '.password = ""'
start_app
sleep 40
expect "keeps parsing with only the saved key" docker exec "${NAME}-app" \
    jq -e '.inputs[] | select(.id == "haos-core") | .streams[0].pipeline == "logs-haos.core@custom"' \
    /run/ha-elastic-agent/elastic-agent.yml
stop_app
expect "ignores the username without a password" grep -q "No password is set" "${WORKDIR}/app.log"
expect "reuses the saved API key" grep -q "Sending data with the API key the app created earlier" "${WORKDIR}/app.log"
check "Core events aren't sent again" 4 "$(count haos.core)"
check "system events aren't sent again" 3 "$(count haos.system)"

log "Standalone mode: saved key and a missing pipeline"
es -XDELETE "${ES}/_ingest/pipeline/logs-haos.core@custom" >/dev/null
es -XDELETE "${ES}/_data_stream/logs-haos.*" >/dev/null
in_data 'rm -rf /data/state'
start_app
wait_for 4 3
stop_app
expect "warns that the saved key can't reinstall the pipeline" grep -q "Couldn't install" "${WORKDIR}/app.log"
expect "announces unparsed delivery" grep -q "Core logs will be sent unparsed" "${WORKDIR}/app.log"
check "Core events survive a missing pipeline" 4 "$(count haos.core)"
check "system events survive a missing pipeline" 3 "$(count haos.system)"

log "Standalone mode: wrong password"
update_options ".username = \"elastic\" | .password = \"wrong\""
if run_to_exit "${WORKDIR}/wrong.log" --network "${NAME}"; then
    fail "the app starts with a wrong password"
else
    expect "says the credentials were rejected" grep -q "rejected the credentials" "${WORKDIR}/wrong.log"
fi

log "Standalone mode: API key only, no pipeline, apps and host turned off"
es -XDELETE "${ES}/_data_stream/logs-haos.*" >/dev/null
in_data 'rm -rf /data/state /data/state.previous /data/elasticsearch-api-key.json'
user_key="$(es -XPOST "${ES}/_security/api_key" -d '{
    "name": "smoke-writer",
    "role_descriptors": {"writer": {"cluster": ["monitor"],
        "indices": [{"names": ["logs-haos.*-*"], "privileges": ["auto_configure", "create_doc"]}]}}
}' | jq -r '.encoded')"
set_options <<EOF
{"elasticsearch_url": "http://elasticsearch:9200/", "api_key": "${user_key}", "insecure": false, "collect_apps": false, "collect_host": false, "read_existing_logs": true}
EOF
start_app
wait_for 4 1
stop_app
expect "uses the configured API key" grep -q "Sending data with the configured API key" "${WORKDIR}/app.log"
expect "warns that the key can't manage pipelines" grep -q "can't read ingest pipelines" "${WORKDIR}/app.log"
expect "doesn't save a key of its own" in_data '! test -e /data/elasticsearch-api-key.json'
expect_not "the API key isn't logged" grep -q "${user_key}" "${WORKDIR}/app.log"
check "Core events" 4 "$(count haos.core)"
check "Core events arrive unparsed" true \
    "$(search core | jq '[.hits.hits[]._source | .log.logger == null and (.message | contains("2026-11-07"))] | length == 4 and all')"
check "only the Supervisor's system events" "hassio_supervisor" \
    "$(search system | jq -r '[.hits.hits[]._source | .container.name // "host"] | unique | join(",")')"

echo
if [[ "${failures}" -gt 0 ]]; then
    echo "${failures} check(s) failed. Last app log:"
    tail -40 "${WORKDIR}/app.log" 2>/dev/null || true
    exit 1
fi
echo "All checks passed."

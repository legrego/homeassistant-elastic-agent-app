#!/usr/bin/env bash
# Smoke test for the Elastic Agent app image. Needs Docker, curl, jq and python3.
#
# 1. Builds a journal file with a newer systemd than the image's journalctl, the
#    way Home Assistant OS does.
# 2. Runs the app entrypoint against a fake Fleet URL and checks that it finds
#    the journal and handles enrollment state.
# 3. Runs the image as a standalone agent with the suggested journald inputs,
#    sends the journal to a local Elasticsearch, and checks the documents.
# 4. Runs the Core ingest pipeline on those documents and checks the fields.
#
# Usage: tests/smoke/smoke.sh   (set IMAGE to skip the build)
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "${HERE}/../.." && pwd)"
IMAGE="${IMAGE:-}"
ES_IMAGE="${ES_IMAGE:-docker.elastic.co/elasticsearch/elasticsearch:9.5.6}"
SYSTEMD_IMAGE="${SYSTEMD_IMAGE:-fedora:41}"
NAME="haos-smoke-$$"
WORKDIR="$(mktemp -d)"
ES_PORT="${ES_PORT:-19200}"
ES="http://localhost:${ES_PORT}"
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

# The app runs as root, as on Home Assistant OS, so the state it writes under
# data/ is owned by root. On Linux the user running this script can't change or
# delete those files, so do that from a container.
in_data() { docker run --rm -v "${WORKDIR}/data:/data" --entrypoint bash "${IMAGE}" -c "$1"; }

cleanup() {
    docker rm -f "${NAME}-es" "${NAME}-agent" "${NAME}-entry" >/dev/null 2>&1 || true
    docker network rm "${NAME}" >/dev/null 2>&1 || true
    if [[ -n "${IMAGE}" ]]; then
        docker run --rm -v "${WORKDIR}:/work" --entrypoint rm "${IMAGE}" -rf /work/data >/dev/null 2>&1 || true
    fi
    rm -rf "${WORKDIR}"
}
trap cleanup EXIT

if [[ -z "${IMAGE}" ]]; then
    log "Building the app image"
    IMAGE="ha-app-elastic-agent:smoke"
    docker build -q -t "${IMAGE}" "${ROOT}/elastic_agent" >/dev/null
fi

log "Writing a journal with ${SYSTEMD_IMAGE}"
mkdir -p "${WORKDIR}/journal/${MACHINE_ID}" "${WORKDIR}/data"
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

log "Entrypoint: first start"
cat >"${WORKDIR}/data/options.json" <<EOF
{"fleet_url": "https://fleet.invalid:8220", "enrollment_token": "token-one", "insecure": false, "tags": ["home-assistant", "smoke"]}
EOF
run_entrypoint() {
    docker run -d --name "${NAME}-entry" \
        -v "${WORKDIR}/data:/data" -v "${WORKDIR}/journal:/var/log/journal:ro" \
        "${IMAGE}" >/dev/null
    sleep "${1:-15}"
    docker logs "${NAME}-entry" >"${WORKDIR}/entry.log" 2>&1 || true
    docker exec "${NAME}-entry" cat /etc/machine-id >"${WORKDIR}/machine-id" 2>/dev/null || true
    # The environment the agent process was started with.
    docker exec "${NAME}-entry" bash -c '
        for p in /proc/[0-9]*; do
            if grep -qa FLEET_URL "$p/environ" 2>/dev/null; then tr "\0" "\n" <"$p/environ"; break; fi
        done' >"${WORKDIR}/env" 2>/dev/null || true
    docker rm -f "${NAME}-entry" >/dev/null
}
run_entrypoint 15
expect "entrypoint finds the host journal" grep -q "Found the host journal for machine ID ${MACHINE_ID}" "${WORKDIR}/entry.log"
check "machine ID is the host's" "${MACHINE_ID}" "$(cat "${WORKDIR}/machine-id")"
check "FLEET_URL" "FLEET_URL=https://fleet.invalid:8220" "$(grep '^FLEET_URL=' "${WORKDIR}/env")"
check "ELASTIC_AGENT_TAGS" "ELASTIC_AGENT_TAGS=home-assistant,smoke" "$(grep '^ELASTIC_AGENT_TAGS=' "${WORKDIR}/env")"
check "STATE_PATH" "STATE_PATH=/data/state" "$(grep '^STATE_PATH=' "${WORKDIR}/env")"
expect "agent tries to enroll with the configured URL" grep -q "fleet.invalid" "${WORKDIR}/entry.log"
expect "state lives under /data/state" test -d "${WORKDIR}/data/state"
if grep -q "enrollment_token\|token-one" "${WORKDIR}/entry.log"; then
    fail "the enrollment token shows up in the log"
else
    pass "the enrollment token isn't logged"
fi

log "Entrypoint: token changed"
in_data 'touch /data/state/marker'
jq '.enrollment_token = "token-two"' "${WORKDIR}/data/options.json" >"${WORKDIR}/o.json"
mv "${WORKDIR}/o.json" "${WORKDIR}/data/options.json"
run_entrypoint 5
expect "old state moved to state.previous" in_data 'test -f /data/state.previous/marker'
expect "logs the re-enrollment" grep -q "Enrolling again" "${WORKDIR}/entry.log"

log "Entrypoint: unchanged options preserve state"
in_data 'touch /data/state/unchanged-marker'
run_entrypoint 5
expect "unchanged options keep existing state" in_data 'test -f /data/state/unchanged-marker'

log "Entrypoint: certificate verification changed"
for insecure in true false; do
    in_data "touch /data/state/tls-${insecure}-marker"
    jq --argjson insecure "${insecure}" '.insecure = $insecure' \
        "${WORKDIR}/data/options.json" >"${WORKDIR}/o.json"
    mv "${WORKDIR}/o.json" "${WORKDIR}/data/options.json"
    run_entrypoint 5
    expect "insecure=${insecure} replaces the previous enrollment state" \
        in_data "test -f /data/state.previous/tls-${insecure}-marker"
    check "FLEET_INSECURE" "FLEET_INSECURE=${insecure}" "$(grep '^FLEET_INSECURE=' "${WORKDIR}/env")"
done

log "Starting Elasticsearch"
docker network create "${NAME}" >/dev/null
docker run -d --name "${NAME}-es" --network "${NAME}" --network-alias elasticsearch \
    -p "127.0.0.1:${ES_PORT}:9200" \
    -e discovery.type=single-node -e xpack.security.enabled=false \
    -e ES_JAVA_OPTS="-Xms512m -Xmx512m" "${ES_IMAGE}" >/dev/null
for _ in $(seq 1 90); do
    curl -fs "${ES}/_cluster/health?wait_for_status=yellow&timeout=1s" >/dev/null 2>&1 && break
    sleep 2
done
curl -fs "${ES}" >/dev/null || {
    fail "Elasticsearch didn't start"
    exit 1
}

log "Standalone agent reading the journal"
docker run -d --name "${NAME}-agent" --network "${NAME}" \
    -v "${WORKDIR}/journal:/var/log/journal:ro" \
    -v "${HERE}/elastic-agent.yml:/smoke/elastic-agent.yml:ro" \
    --entrypoint bash "${IMAGE}" -c "
        echo ${MACHINE_ID} > /etc/machine-id
        cp /smoke/elastic-agent.yml /usr/share/elastic-agent/elastic-agent.yml
        exec /usr/local/bin/docker-entrypoint" >/dev/null
count() {
    curl -fs -XPOST "${ES}/logs-$1-default/_refresh" >/dev/null 2>&1 || true
    curl -fs "${ES}/logs-$1-default/_count" 2>/dev/null | jq -r '.count // 0' || echo 0
}
for _ in $(seq 1 90); do
    [[ "$(count haos.core)" -ge 4 && "$(count haos.system)" -ge 3 ]] && break
    sleep 2
done
sleep 5 # let any extra documents arrive before counting
check "Core events (8 lines joined into 4)" 4 "$(count haos.core)"
check "Supervisor, app and host events" 3 "$(count haos.system)"
system_names="$(curl -fs "${ES}/logs-haos.system-default/_search?size=10" |
    jq -r '[.hits.hits[]._source | .container.name // .syslog.identifier // .log.syslog.appname // "?"] | sort | join(",")')"
check "system sources" "NetworkManager,addon_core_ssh,hassio_supervisor" "${system_names}"
check "host.id is the host machine ID" "${MACHINE_ID}" \
    "$(curl -fs "${ES}/logs-haos.core-default/_search?size=1" | jq -r '.hits.hits[0]._source.host.id')"

log "Core ingest pipeline"
if ! curl -sS -XPUT "${ES}/_ingest/pipeline/logs-haos.core@custom" -H 'Content-Type: application/json' \
    --data-binary "@${ROOT}/pipelines/logs-haos.core@custom.json" | jq -e '.acknowledged' >/dev/null; then
    fail "Elasticsearch rejected the pipeline"
    exit 1
fi
curl -fs "${ES}/logs-haos.core-default/_search?size=10&sort=@timestamp" |
    jq '{docs: [.hits.hits[] | {_source}]}' >"${WORKDIR}/docs.json"
curl -fs -XPOST "${ES}/_ingest/pipeline/logs-haos.core@custom/_simulate" -H 'Content-Type: application/json' \
    --data-binary "@${WORKDIR}/docs.json" >"${WORKDIR}/simulated.json"
docs() { jq -r "[.docs[].doc._source | $1] | join(\"|\")" "${WORKDIR}/simulated.json"; }
check "log.level" "info|warning|error|info" "$(docs '.log.level // "-"')"
check "log.logger" "homeassistant.bootstrap|homeassistant.components.http.ban|custom_components.elasticsearch.es_publish|homeassistant.components.recorder.core" "$(docs '.log.logger // "-"')"
check "homeassistant.integration" "-|http|elasticsearch|recorder" "$(docs '.homeassistant.integration // "-"')"
check "process.thread.name" "MainThread|MainThread|MainThread|SyncWorker_3" "$(docs '.process.thread.name // "-"')"
check "error.type" "-|-|ConnectionError|-" "$(docs '.error.type // "-"')"
check "error.message" "-|-|cluster unavailable|-" "$(docs '.error.message // "-"')"
check "message without ANSI codes or traceback" "Error publishing documents" "$(jq -r '.docs[2].doc._source.message' "${WORKDIR}/simulated.json")"
check "stack trace lines" 4 "$(jq -r '.docs[2].doc._source.error.stack_trace' "${WORKDIR}/simulated.json" | wc -l | tr -d ' ')"
check "no parse failures" "" "$(docs '.tags // [] | join(",")' | tr -d '|')"

echo
if [[ "${failures}" -gt 0 ]]; then
    echo "${failures} check(s) failed. Agent log:"
    docker logs "${NAME}-agent" 2>&1 | tail -40
    exit 1
fi
echo "All checks passed."

#!/usr/bin/env bash
# ==============================================================================
# Home Assistant Add-on: Vector
# Generator tests - run INSIDE the built add-on image:
#
#   docker run --rm -v "$PWD/vector/tests:/tests:ro" \
#       --entrypoint /bin/bash hassio-addon-vector:test /tests/run.sh
#
# These exist because `vector validate` cannot see the two things that matter
# most here: whether a credential reached the log or the config file, and how
# the generated VRL behaves per event. Both have been regressions before.
# ==============================================================================
set -uo pipefail

TESTS_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=/dev/null
source /usr/lib/vector-common.sh   # VECTOR_CONFIG, VECTOR_OPTIONS_FILE
export PATH="/usr/local/bin:${PATH}"

LOG=/tmp/case.log
VRL_OUT=/tmp/vrl.out
failures=0
current=""
rc=0

fail() { printf 'FAIL  %s: %s\n' "${current}" "$1"; failures=$((failures + 1)); }
ok() { printf 'ok    %s: %s\n' "${current}" "$1"; }
check() { local d="$1"; shift; if "$@" > /dev/null 2>&1; then ok "${d}"; else fail "${d}"; fi; }
check_not() { local d="$1"; shift; if "$@" > /dev/null 2>&1; then fail "${d}"; else ok "${d}"; fi; }

# Each fixture is an overlay on base.json, so adding an add-on option means
# touching one file rather than nine.
# Credentials are stripped from the environment so that a regression where
# load_credentials stops exporting cannot be masked by the harness.
run_case() {
    current="$1"
    rm -f "${VECTOR_CONFIG}" "${VECTOR_VRL}"
    jq -s '.[0] * .[1]' "${TESTS_DIR}/fixtures/base.json" \
        "${TESTS_DIR}/fixtures/$1.json" > "${VECTOR_OPTIONS_FILE}"
    env -u VICTORIALOGS_USER -u VICTORIALOGS_PASSWORD \
        /usr/bin/generate-config.sh > "${LOG}" 2>&1
    rc=$?
}

mkdir -p /run/s6/container_environment /run/log/journal /data /share/vector

run_case minimal
check     "exits 0"                          test "${rc}" -eq 0
check     "config is mode 600"               test "$(stat -c %a "${VECTOR_CONFIG}")" = 600
check_not "no auth block without a username" grep -q "strategy: basic" "${VECTOR_CONFIG}"

run_case auth-quotes
check     "exits 0"                     test "${rc}" -eq 0
check_not "password not on disk"        grep -Fq SENTINELPW "${VECTOR_CONFIG}"
check_not "password not in the log"     grep -Fq SENTINELPW "${LOG}"
check     "auth references the env var" \
    grep -Fq "password: '\${VICTORIALOGS_PASSWORD}'" "${VECTOR_CONFIG}"

check "config points at the VRL file" grep -Fq "file: ${VECTOR_VRL}" "${VECTOR_CONFIG}"
# Keep this one for the VRL section below; later cases overwrite it
cp "${VECTOR_VRL}" /tmp/enrich.vrl

run_case endpoint-creds
check     "exits 0"                    test "${rc}" -eq 0
check_not "endpoint secret not logged" grep -Fq SENTINELURL9z "${LOG}"

# A password may contain @, so the mask has to find the LAST @ before the path
run_case endpoint-at-in-password
check     "exits 0"                            test "${rc}" -eq 0
check_not "no tail of an @-bearing password"   grep -Fq SENTINELAT9z "${LOG}"

run_case empty-stream-fields
check     "exits 0"                   test "${rc}" -eq 0
check_not "no empty _stream_fields"   grep -q _stream_fields "${VECTOR_CONFIG}"

run_case template-units
check "exits 0"             test "${rc}" -eq 0
check "@-prefixed unit is quoted" grep -q -- '- "@reboot.service"' "${VECTOR_CONFIG}"

# jq's `//` treats false as empty, so a boolean option set to false is easy to
# read back as true - pin both directions
run_case no-source
check "exits non-zero"            test "${rc}" -ne 0
check "explains the only source"  grep -q "only log source" "${LOG}"

run_case no-redaction
check     "exits 0"                test "${rc}" -eq 0
check_not "no redaction block"     grep -q "REDACTED" "${VECTOR_CONFIG}"

run_case password-trailing-newline
check "exits non-zero"          test "${rc}" -ne 0
check "explains the line break" grep -q "line breaks" "${LOG}"

run_case custom-missing
check "exits non-zero"        test "${rc}" -ne 0
check "names the missing path" grep -q does-not-exist "${LOG}"

# A custom config is arbitrary YAML from a share every add-on can write, and
# Vector interpolates env vars into it - the credentials must not be reachable
printf 'sources:\n  s:\n    type: demo_logs\n    format: syslog\nsinks:\n  o:\n    type: blackhole\n    inputs: [s]\n' \
    > /share/vector/custom.yaml
current="custom-present"
jq -s '.[0] * .[1]' "${TESTS_DIR}/fixtures/base.json" \
    "${TESTS_DIR}/fixtures/custom-present.json" > "${VECTOR_OPTIONS_FILE}"
env -u VICTORIALOGS_USER -u VICTORIALOGS_PASSWORD bash -c '
    source /usr/lib/vector-common.sh
    vector_addon::load_credentials
    printf "%s%s" "${VICTORIALOGS_USER}" "${VICTORIALOGS_PASSWORD}"' > "${LOG}"
check_not "credentials not exported for a custom config" grep -Fq SENTINELPW "${LOG}"

# The generated VRL, over the events that used to abort it
current="vrl"
check "the VRL program is non-empty" test -s /tmp/enrich.vrl

: > "${VRL_OUT}"
vrl_fail=0
while IFS= read -r event; do
    [[ -n "${event}" ]] || continue
    printf '%s\n' "${event}" > /tmp/one.json
    if ! vector vrl --print-object --program /tmp/enrich.vrl \
        --input /tmp/one.json >> "${VRL_OUT}" 2>&1; then
        printf '      event failed: %s\n' "${event}"
        vrl_fail=1
    fi
done < "${TESTS_DIR}/events.ndjson"
grep -o '"message": "[^"]*"' "${VRL_OUT}" > /tmp/messages.txt

check     "every event runs clean" test "${vrl_fail}" -eq 0
check     "host set on every event" \
    test "$(grep -c '"host"' "${VRL_OUT}")" -eq "$(grep -c . "${TESTS_DIR}/events.ndjson")"
check_not "message secret redacted"  grep -Fq hunter2sentinel /tmp/messages.txt
check     "level comes from the message" grep -q '"level": "warn"' "${VRL_OUT}"
check     "message-less event gets a placeholder" \
    grep -Fq '"message": "(no message)"' "${VRL_OUT}"
# _CMDLINE still ships as its own field, as every journald field does; what must
# not happen is it being folded into .message, past the redaction written for it
check_not "journal metadata not folded into message" \
    grep -Fq restic-sentinel /tmp/messages.txt

printf '\n%s failing assertion(s)\n' "${failures}"
[[ ${failures} -eq 0 ]]

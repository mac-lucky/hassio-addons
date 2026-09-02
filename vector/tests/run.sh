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
check     "auth references the secret backend" \
    grep -Fq "password: 'SECRET[victorialogs.password]'" "${VECTOR_CONFIG}"
check     "the secret backend is declared" \
    grep -Fq -- "- ${VECTOR_SECRETS_HELPER}" "${VECTOR_CONFIG}"

# `vector validate` does not resolve secrets, so the generator's own validation
# step cannot see a substitution that breaks the config. Only a real load can,
# and that is how 1.6.0 shipped broken. Config errors are fatal at startup, so
# reaching the timeout is the pass.
timeout 10 vector --config-yaml "${VECTOR_CONFIG}" > /tmp/realrun.log 2>&1
check_not "the generated config survives a real load, not just validate" \
    grep -q "Configuration error" /tmp/realrun.log

check "config points at the VRL file" grep -Fq "file: ${VECTOR_VRL}" "${VECTOR_CONFIG}"
# Keep this one for the VRL section below; later cases overwrite it
cp "${VECTOR_VRL}" /tmp/enrich.vrl

# The credentials as they actually leave the process.
#
# This is the assertion the suite was missing. 1.6.0 and 1.6.1 passed every
# check above and still authenticated with the literal text
# "${VICTORIALOGS_PASSWORD}", because Vector does not interpolate the
# environment into a --config-yaml config. Nothing short of a real request shows
# that, so both ends here are Vector itself - no dependency the image lacks.
#
# The auth and secret blocks are lifted out of the config the generator just
# wrote rather than restated, so a change in how they are emitted is exercised
# instead of bypassed. Still running on the auth-quotes fixture, whose username
# and password carry a quote, a backslash, a dollar and braces.
current="auth-wire"
probe=/tmp/wire
rm -rf "${probe}"; mkdir -p "${probe}"

awk '/^    auth:/{f=1;print;next} f&&/^    [a-z]/{f=0} f' "${VECTOR_CONFIG}" > "${probe}/auth.yaml"
awk '/^secret:/{f=1} f' "${VECTOR_CONFIG}" > "${probe}/secret.yaml"

cat > "${probe}/recv.yaml" <<EOF
data_dir: ${probe}
sources:
  inbound:
    type: http_server
    address: 127.0.0.1:18099
    strict_path: false
    headers:
      - Authorization
    decoding:
      codec: bytes
sinks:
  captured:
    type: file
    inputs:
      - inbound
    path: ${probe}/captured.log
    encoding:
      codec: json
EOF

{
    cat <<EOF
data_dir: ${probe}
sources:
  gen:
    type: demo_logs
    format: syslog
    interval: 0.2
sinks:
  victorialogs:
    type: elasticsearch
    inputs:
      - gen
    endpoints:
      - "http://127.0.0.1:18099"
    api_version: v8
    healthcheck:
      enabled: false
EOF
    cat "${probe}/auth.yaml"
    cat "${probe}/secret.yaml"
} > "${probe}/send.yaml"

check "auth block was lifted"   test -s "${probe}/auth.yaml"
check "secret block was lifted" test -s "${probe}/secret.yaml"

vector --config-yaml "${probe}/recv.yaml" --quiet > "${probe}/recv.log" 2>&1 &
recv_pid=$!
for _ in $(seq 1 100); do
    (exec 3<>/dev/tcp/127.0.0.1/18099) 2>/dev/null && break
    sleep 0.2
done
vector --config-yaml "${probe}/send.yaml" --quiet > "${probe}/send.log" 2>&1 &
send_pid=$!
for _ in $(seq 1 150); do
    [[ -s "${probe}/captured.log" ]] && break
    sleep 0.2
done
kill "${send_pid}" "${recv_pid}" 2> /dev/null
wait "${send_pid}" "${recv_pid}" 2> /dev/null

# Found by shape, not by field name, so a rename in Vector's http_server source
# fails loudly here instead of silently matching nothing
wire_header=$(grep -o '"Basic [^"]*"' "${probe}/captured.log" 2>/dev/null | head -1 | tr -d '"')
wire_creds=$(printf '%s' "${wire_header#Basic }" | base64 -d 2>/dev/null)
expected_creds="$(jq -r '.victorialogs_username' "${VECTOR_OPTIONS_FILE}")"
expected_creds+=":$(jq -r '.victorialogs_password' "${VECTOR_OPTIONS_FILE}")"

check "a request reached the receiver" test -s "${probe}/captured.log"
check "the request carries basic auth" test -n "${wire_header}"
check "the credentials resolve on the wire" test "${wire_creds}" = "${expected_creds}"

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

# The two VRL placeholders are substituted one after the other, so a hostname
# carrying the other token would be rewritten again by the second pass and
# silently take the instance value
run_case hostname-placeholder
check "exits non-zero"        test "${rc}" -ne 0
check "names the placeholder" grep -q "__INSTANCE__" "${LOG}"

# Backslash starts an escape in the double-quoted YAML scalar the endpoint
# lands in; a trailing one swallows the closing quote
run_case endpoint-backslash
check "exits non-zero"          test "${rc}" -ne 0
check "rejected as invalid"     grep -q "invalid characters" "${LOG}"

# An explicit empty instance used to fail with "contains invalid characters"
run_case empty-instance
check "exits non-zero"       test "${rc}" -ne 0
check "says it is empty"     grep -q "must not be empty" "${LOG}"

# [""] must fail like ["a", ""] does, not read as "unset": the old guard
# captured jq's output with $(), which strips the newline a lone empty entry
# produces
run_case journal-empty-unit
check "exits non-zero"          test "${rc}" -ne 0
check "rejects the empty unit"  grep -q "Invalid journal unit name" "${LOG}"

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

# The full custom-config branch, end to end: a valid file is accepted, and no
# option validation runs in this mode (the fixture below carries no endpoint
# problem, but the branch exits before any of those checks)
printf 'sources:\n  s:\n    type: demo_logs\n    format: syslog\nsinks:\n  o:\n    type: blackhole\n    inputs: [s]\n' \
    > /share/vector/custom.yaml
run_case custom-present
check "a valid custom config is accepted" test "${rc}" -eq 0
check "and announced" grep -q "Custom configuration validation passed" "${LOG}"

# A failing custom config must be reported without printing anything from it:
# the validator quotes literal values back (an invalid enum echoes the value,
# an unquoted numeric password comes back as invalid type: integer), and the
# masker only knows URL-shaped credentials
printf 'sources:\n  s:\n    type: demo_logs\n    format: syslog\nsinks:\n  o:\n    type: elasticsearch\n    inputs: [s]\n    endpoints: ["http://127.0.0.1:1"]\n    compression: CUSTOMSENTINEL9z\n' \
    > /share/vector/custom.yaml
run_case custom-present
check     "a failing custom config is fatal"     test "${rc}" -ne 0
check     "and says so"                          grep -q "Custom configuration validation failed" "${LOG}"
check_not "without echoing the config's values"  grep -Fq CUSTOMSENTINEL9z "${LOG}"

# A custom config is arbitrary YAML from a share every add-on can write, and it
# can declare the same secret backend - the credentials must not be reachable
printf 'sources:\n  s:\n    type: demo_logs\n    format: syslog\nsinks:\n  o:\n    type: blackhole\n    inputs: [s]\n' \
    > /share/vector/custom.yaml
current="custom-present"
jq -s '.[0] * .[1]' "${TESTS_DIR}/fixtures/base.json" \
    "${TESTS_DIR}/fixtures/custom-present.json" > "${VECTOR_OPTIONS_FILE}"
printf '{"version":"1.0","secrets":["user","password"]}' \
    | "${VECTOR_SECRETS_HELPER}" > "${LOG}" 2>&1
check_not "backend serves nothing for a custom config" grep -Fq SENTINELPW "${LOG}"
check     "backend errors instead of returning empty" grep -Fq 'no such secret' "${LOG}"
check_not "no username either"                        grep -Fq "us'er" "${LOG}"

# The backend answers exactly what Vector asks for, with the values intact
current="secrets-backend"
jq -s '.[0] * .[1]' "${TESTS_DIR}/fixtures/base.json" \
    "${TESTS_DIR}/fixtures/auth-quotes.json" > "${VECTOR_OPTIONS_FILE}"
printf '{"version":"1.0","secrets":["user","password"]}' \
    | "${VECTOR_SECRETS_HELPER}" > "${LOG}" 2>&1
raw_password=$(jq -r '.victorialogs_password' "${VECTOR_OPTIONS_FILE}")
raw_username=$(jq -r '.victorialogs_username' "${VECTOR_OPTIONS_FILE}")
# Escaped for the single-quoted scalar Vector substitutes them into, so what
# comes out here is not the plain value. The auth-wire case above is what pins
# the pair of escape and scalar down to the right plaintext on the wire.
check "password comes back escaped for YAML" \
    test "$(jq -r '.password.value' "${LOG}")" = "${raw_password//\'/\'\'}"
check "username comes back escaped for YAML" \
    test "$(jq -r '.user.value' "${LOG}")" = "${raw_username//\'/\'\'}"
check "the fixture actually exercises the escape" \
    grep -Fq "'" <<< "${raw_password}${raw_username}"
check "an unknown name is an error, not an empty value" \
    test "$(printf '{"version":"1.0","secrets":["nope"]}' \
        | "${VECTOR_SECRETS_HELPER}" | jq -r '.nope.value')" = "null"

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

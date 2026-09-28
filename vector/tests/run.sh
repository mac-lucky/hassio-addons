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
check     "the multiline reduce is wired in"  grep -q "type: reduce" "${VECTOR_CONFIG}"
check     "the size cap reads both branches" \
    grep -q -- "- split_multiline._unmatched" "${VECTOR_CONFIG}"
check     "the sink reads the size cap"     grep -q -- "- cap_size" "${VECTOR_CONFIG}"
check     "Core and Supervisor are joined by default" \
    grep -Fq "(?:homeassistant|hassio_supervisor)" "${VECTOR_VRL}"
check_not "no container filter unless asked" grep -q "excluded container" "${VECTOR_VRL}"
check     "the sink acknowledges deliveries" \
    grep -Pzq '\n    acknowledgements:\n      enabled: true\n' "${VECTOR_CONFIG}"

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
check_not "no redaction block"     grep -q "REDACTED" "${VECTOR_VRL}"

run_case password-trailing-newline
check "exits non-zero"          test "${rc}" -ne 0
check "explains the line break" grep -q "line breaks" "${LOG}"

run_case custom-missing
check "exits non-zero"        test "${rc}" -ne 0
check "names the missing path" grep -q does-not-exist "${LOG}"

# The full custom-config branch, end to end: a valid file is accepted, and no
# option validation runs in this mode (the fixture below carries no endpoint
# problem, but the branch exits before any of those checks). Shared with the
# backend-lockout case below so the two cannot drift apart.
valid_custom_config='sources:\n  s:\n    type: demo_logs\n    format: syslog\nsinks:\n  o:\n    type: blackhole\n    inputs: [s]\n'
printf '%b' "${valid_custom_config}" > /share/vector/custom.yaml
run_case custom-present
check "a valid custom config is accepted" test "${rc}" -eq 0
check "and announced" grep -q "Custom configuration validation passed" "${LOG}"

# A sink that is down while the add-on starts is an outage, not a broken
# config: validation must not run healthchecks, or the add-on would stop for good
printf 'sources:\n  s:\n    type: demo_logs\n    format: syslog\nsinks:\n  o:\n    type: elasticsearch\n    inputs: [s]\n    endpoints: ["http://127.0.0.1:9"]\n' \
    > /share/vector/custom.yaml
run_case custom-present
check "a custom config whose sink is down is still accepted" test "${rc}" -eq 0

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
printf '%b' "${valid_custom_config}" > /share/vector/custom.yaml
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

# The generated VRL, one event at a time. Every line of events.ndjson is a
# journald-shaped event plus the _t_* keys that say what must come out of it:
#   _t_name       what the case is, for the report
#   _t_level      expected .level
#   _t_msg        expected .message, exactly
#   _t_unit       expected .unit
#   _t_container  expected .container_name
#   _t_host       expected .host
#   _t_absent     text that must not survive anywhere in .message
#   _t_fields     {"field": value} pairs the output must carry
#   _t_gone       fields the output must not carry
#   _t_drop       true when the program must drop the event (abort with the
#                 "vector-addon-drop:" sentinel) instead of passing it on
#
# `vector vrl` exits 0 even when the program fails at runtime and only says so
# on stderr, so stdout and stderr are kept apart and ANY stderr is a failure. It
# matters: remap forwards the original, unredacted event when the program errors.
run_vrl_events() {
    local program="$1" events="$2" event name out
    # --print-object writes VRL values, not JSON (a timestamp comes out as
    # t'...', a tab as a raw tab), so a copy of the program ends in
    # encode_json(.) and what comes out is a JSON string holding the event
    { cat "${program}"; printf '\nencode_json(.)\n'; } \
        > /tmp/program-json.vrl
    while IFS= read -r event; do
        [[ -n "${event}" ]] || continue
        name=$(jq -r '._t_name // "unnamed"' <<< "${event}")
        current="vrl: ${name}"
        printf '%s\n' "${event}" > /tmp/one.json
        VECTOR_LOG=error vector vrl --program /tmp/program-json.vrl \
            --input /tmp/one.json > /tmp/one.out 2> /tmp/one.err
        if [[ "$(jq -r "._t_drop // false" <<< "${event}")" == "true" ]]; then
            if [[ ! -s /tmp/one.out ]] && grep -q "^vector-addon-drop:" /tmp/one.err; then
                ok "dropped"
            else
                fail "expected a drop, got: $(head -c 300 /tmp/one.out /tmp/one.err)"
            fi
            continue
        fi
        if [[ -s /tmp/one.err ]]; then
            fail "the program errored: $(head -c 300 /tmp/one.err)"
            continue
        fi
        out=$(jq -c 'fromjson' /tmp/one.out 2> /dev/null)
        if ! jq -e 'type == "object"' <<< "${out}" > /dev/null 2>&1; then
            fail "no event came out"
            continue
        fi
        # One line per mismatch; an empty result is a pass
        jq -r '
            def expect($key; $field):
                if has($key) and (.[$field] != .[$key])
                then "\($field): got \(.[$field] | tojson), want \(.[$key] | tojson)"
                else empty end;
            expect("_t_level"; "level"),
            expect("_t_msg"; "message"),
            expect("_t_unit"; "unit"),
            expect("_t_container"; "container_name"),
            expect("_t_host"; "host"),
            ((._t_gone // [])[] as $g | if has($g) then "\($g) should be gone" else empty end),
            (((._t_fields // {}) | to_entries[]) as $f
             | if .[$f.key] != $f.value
               then "\($f.key): got \(.[$f.key] | tojson), want \($f.value | tojson)"
               else empty end),
            (if has("_t_absent")
                 and (._t_absent as $a | (.message // "") | tostring | contains($a))
             then "message still carries \(._t_absent | tojson)" else empty end)
        ' <<< "${out}" > /tmp/one.mismatch
        if [[ -s /tmp/one.mismatch ]]; then
            while IFS= read -r line; do fail "${line}"; done < /tmp/one.mismatch
        else
            ok "as expected"
        fi
    done < "${events}"
}

current="vrl"
check "the VRL program is non-empty" test -s /tmp/enrich.vrl
run_vrl_events /tmp/enrich.vrl "${TESTS_DIR}/events.ndjson"

# Without the hostname option the host journald reports is kept, rather than
# this container's own name
run_case no-hostname
check "exits 0" test "${rc}" -eq 0
check "says where the host comes from" grep -q "taken from the journal" "${LOG}"
printf '%s\n' '{"_t_name":"the journald host is kept","_t_host":"journal-host","host":"journal-host","message":"x","PRIORITY":"6"}' \
    > /tmp/host-events.ndjson
run_vrl_events "${VECTOR_VRL}" /tmp/host-events.ndjson

# An extra label named like a field the add-on sets would overwrite it on every
# event; "message" would replace every log line
run_case extra-label-reserved
check "exits non-zero" test "${rc}" -ne 0
check "names the key"  grep -q "Extra label 'message'" "${LOG}"

run_case extra-labels
check "exits 0" test "${rc}" -eq 0
printf '%s\n' '{"_t_name":"extra labels land, quotes intact","_t_fields":{"env":"prod \"q\" \\ x","site":"home"},"message":"x","PRIORITY":"6"}' \
    > /tmp/label-events.ndjson
run_vrl_events "${VECTOR_VRL}" /tmp/label-events.ndjson

# The container options
run_case container-invalid
check "an invalid container name is refused" test "${rc}" -ne 0
check "and named" grep -q "Invalid container name in exclude_containers" "${LOG}"

run_case multiline-off
check     "exits 0" test "${rc}" -eq 0
check_not "an empty multiline_containers marks nothing" grep -q "%multiline" "${VECTOR_VRL}"

# include_containers narrows the container records only; host units, the
# kernel and audit still come through
run_case include
check "exits 0" test "${rc}" -eq 0
cat > /tmp/include-events.ndjson <<'EVENTS'
{"_t_name":"an included container passes","_t_container":"homeassistant","message":"x","PRIORITY":"6","_SYSTEMD_UNIT":"docker.service","CONTAINER_NAME":"homeassistant"}
{"_t_name":"an included add-on passes by its slug","_t_container":"app_d5369777_music_assistant","message":"x","PRIORITY":"6","_SYSTEMD_UNIT":"docker.service","CONTAINER_NAME":"app_d5369777_music_assistant"}
{"_t_name":"another container is dropped","_t_drop":true,"message":"x","PRIORITY":"6","_SYSTEMD_UNIT":"docker.service","CONTAINER_NAME":"app_45df7312_zigbee2mqtt"}
{"_t_name":"a slug is not a prefix match","_t_drop":true,"message":"x","PRIORITY":"6","_SYSTEMD_UNIT":"docker.service","CONTAINER_NAME":"app_4ab554b2_homeassistant-time-machine"}
{"_t_name":"a host unit is not a container and passes","_t_unit":"NetworkManager","message":"x","PRIORITY":"6","_SYSTEMD_UNIT":"NetworkManager.service"}
{"_t_name":"the kernel passes","_t_unit":"kernel","message":"x","PRIORITY":"6","_TRANSPORT":"kernel","SYSLOG_IDENTIFIER":"kernel"}
{"_t_name":"exclude wins over include","_t_drop":true,"message":"x","PRIORITY":"6","_SYSTEMD_UNIT":"docker.service","CONTAINER_NAME":"app_a0d7b954_appdaemon"}
EVENTS
run_vrl_events "${VECTOR_VRL}" /tmp/include-events.ndjson

# The whole generated pipeline, end to end. `vector vrl` runs one event at a
# time and cannot see a route or a reduce, so the real config is run instead,
# with only its journald source swapped for stdin and its sink for stdout: the
# partial-line join, the enrichment, the multiline route and reduce and the size
# cap are all the generator's own. A stdin source ends at EOF and Vector then
# flushes every open reduce group, so the run finishes on its own; the timeout
# only guards against a hang.
current="pipeline"
pl=/tmp/pipeline
rm -rf "${pl}"; mkdir -p "${pl}"
run_case pipeline
check "exits 0" test "${rc}" -eq 0
current="pipeline"

sink_inputs=$(awk '/^sinks:/{s=1} s&&/^    inputs:/{f=1;next} f&&/^      - /{print;next} f{exit}' "${VECTOR_CONFIG}")
check "the sink inputs were found" test -n "${sink_inputs}"
awk -v sink_inputs="${sink_inputs}" '
    /^sources:/ { print "sources:\n  journald:\n    type: stdin\n    decoding:\n      codec: json"; skip = 1; next }
    /^transforms:/ { skip = 0 }
    /^sinks:/ { print "sinks:\n  out:\n    type: console\n    inputs:\n" sink_inputs "\n    encoding:\n      codec: json"; skip = 1; next }
    /^secret:/ { skip = 0 }
    !skip
' "${VECTOR_CONFIG}" > "${pl}/cfg.yaml"

# Core and Music Assistant log at the same time, so their lines interleave;
# group_by keeps each traceback with its own container. Core's opener is a
# WARNING whose frames are PRIORITY=3, so the joined entry must keep warn. The
# second Core opener is longer than 16 KiB and arrives in two pieces, and its
# frames must still join it rather than the record before.
{
    cat <<'EVENTS'
{"message":"\u001b[33m2026-01-11 09:04:15.123 WARNING (MainThread) [homeassistant.core] ha-boom\u001b[0m","PRIORITY":"3","_SYSTEMD_UNIT":"docker.service","CONTAINER_NAME":"homeassistant"}
{"message":"2026-01-11 09:04:15.123 ERROR (MainThread) [music_assistant] ma-boom","PRIORITY":"3","_SYSTEMD_UNIT":"docker.service","CONTAINER_NAME":"app_d5369777_music_assistant"}
{"message":"Traceback (most recent call last):","PRIORITY":"3","_SYSTEMD_UNIT":"docker.service","CONTAINER_NAME":"homeassistant"}
{"message":"Traceback (most recent call last):","PRIORITY":"3","_SYSTEMD_UNIT":"docker.service","CONTAINER_NAME":"app_d5369777_music_assistant"}
{"message":"  File \"/x.py\", line 1, in <module>","PRIORITY":"3","_SYSTEMD_UNIT":"docker.service","CONTAINER_NAME":"homeassistant"}
{"message":"  File \"/ma.py\", line 2, in f","PRIORITY":"3","_SYSTEMD_UNIT":"docker.service","CONTAINER_NAME":"app_d5369777_music_assistant"}
{"message":"ValueError: bad","PRIORITY":"3","_SYSTEMD_UNIT":"docker.service","CONTAINER_NAME":"homeassistant"}
{"message":"\u001b[32m2026-01-11 09:04:16.000 INFO (MainThread) [homeassistant.core] ha-next\u001b[0m","PRIORITY":"3","_SYSTEMD_UNIT":"docker.service","CONTAINER_NAME":"homeassistant"}
{"message":"2026-01-11 09:04:16.000 INFO (MainThread) [music_assistant] ma-next","PRIORITY":"3","_SYSTEMD_UNIT":"docker.service","CONTAINER_NAME":"app_d5369777_music_assistant"}
{"message":"2026-01-11 09:04:17.000 ERROR (MainThread) [homeassistant.core] long-","PRIORITY":"3","_SYSTEMD_UNIT":"docker.service","CONTAINER_NAME":"homeassistant","CONTAINER_PARTIAL_ID":"p2","CONTAINER_PARTIAL_ORDINAL":"1","CONTAINER_PARTIAL_MESSAGE":"true","CONTAINER_PARTIAL_LAST":"false"}
{"message":"opener","PRIORITY":"3","_SYSTEMD_UNIT":"docker.service","CONTAINER_NAME":"homeassistant","CONTAINER_PARTIAL_ID":"p2","CONTAINER_PARTIAL_ORDINAL":"2","CONTAINER_PARTIAL_LAST":"true"}
{"message":"Traceback (most recent call last):","PRIORITY":"3","_SYSTEMD_UNIT":"docker.service","CONTAINER_NAME":"homeassistant"}
{"message":"KeyError: long","PRIORITY":"3","_SYSTEMD_UNIT":"docker.service","CONTAINER_NAME":"homeassistant"}
{"message":"2026-01-11 09:04:18.000 INFO (MainThread) [homeassistant.core] ha-last","PRIORITY":"3","_SYSTEMD_UNIT":"docker.service","CONTAINER_NAME":"homeassistant"}
{"message":"2026-01-11 09:04:15.123456 ERROR AppDaemon: ad-boom","PRIORITY":"3","_SYSTEMD_UNIT":"docker.service","CONTAINER_NAME":"app_a0d7b954_appdaemon"}
{"message":"Traceback (most recent call last):","PRIORITY":"3","_SYSTEMD_UNIT":"docker.service","CONTAINER_NAME":"app_a0d7b954_appdaemon"}
{"message":"","PRIORITY":"3","_SYSTEMD_UNIT":"docker.service","CONTAINER_NAME":"app_a0d7b954_appdaemon"}
{"message":"[ALARM SpecifiedDeviceNotFound] the device is not working","PRIORITY":"6","_SYSTEMD_UNIT":"docker.service","CONTAINER_NAME":"app_c8a990ad_wmbusmeters-ha-addon"}
{"message":"[2026-09-27 18:37:26] info: \tz2m: payload ","PRIORITY":"6","_SYSTEMD_UNIT":"docker.service","CONTAINER_NAME":"app_45df7312_zigbee2mqtt","CONTAINER_PARTIAL_ID":"p1","CONTAINER_PARTIAL_ORDINAL":"1","CONTAINER_PARTIAL_MESSAGE":"true","CONTAINER_PARTIAL_LAST":"false"}
{"message":"{\"a\":1} passw","PRIORITY":"6","_SYSTEMD_UNIT":"docker.service","CONTAINER_NAME":"app_45df7312_zigbee2mqtt","CONTAINER_PARTIAL_ID":"p1","CONTAINER_PARTIAL_ORDINAL":"2","CONTAINER_PARTIAL_MESSAGE":"true","CONTAINER_PARTIAL_LAST":"false"}
{"message":"ord=hunter5sentinel end","PRIORITY":"6","_SYSTEMD_UNIT":"docker.service","CONTAINER_NAME":"app_45df7312_zigbee2mqtt","CONTAINER_PARTIAL_ID":"p1","CONTAINER_PARTIAL_ORDINAL":"3","CONTAINER_PARTIAL_LAST":"true"}
EVENTS
    # One line far past the size cap
    jq -nc '{message: ("y" * 250000), PRIORITY: "6", _SYSTEMD_UNIT: "docker.service", CONTAINER_NAME: "app_00000000_big"}'
} > "${pl}/in.ndjson"

timeout 60 vector --config-yaml "${pl}/cfg.yaml" < "${pl}/in.ndjson" > "${pl}/out.ndjson" 2> "${pl}/run.log"
check "the pipeline ran to the end of its input" grep -q "All sources have finished" "${pl}/run.log"

# Asserted by content, never by order: the sink fans in from two branches
out_count() { jq -s "[.[] | select($1)] | length" "${pl}/out.ndjson"; }
check "every expected event came out, and nothing else" test "$(out_count 'true')" -eq 10
check "Core's traceback rejoined its opening line" \
    test "$(out_count '.container_name == "homeassistant" and .message == "2026-01-11 09:04:15.123 WARNING (MainThread) [homeassistant.core] ha-boom\nTraceback (most recent call last):\n  File \"/x.py\", line 1, in <module>\nValueError: bad"')" -eq 1
check "and kept that line's level, not the frames' PRIORITY" \
    test "$(out_count '(.message | contains("ha-boom")) and .level == "warn"')" -eq 1
check "Music Assistant's frames stayed with Music Assistant" \
    test "$(out_count '.container_name == "app_d5369777_music_assistant" and .message == "2026-01-11 09:04:15.123 ERROR (MainThread) [music_assistant] ma-boom\nTraceback (most recent call last):\n  File \"/ma.py\", line 2, in f"')" -eq 1
check "no traceback crossed between containers" \
    test "$(out_count '(.container_name == "homeassistant" and (.message | contains("ma.py"))) or (.container_name != "homeassistant" and (.message | contains("x.py")))')" -eq 0
check "Core's next line stands alone" \
    test "$(out_count '.message == "2026-01-11 09:04:16.000 INFO (MainThread) [homeassistant.core] ha-next"')" -eq 1
check "a long opener in pieces still collects its own traceback" \
    test "$(out_count '.message == "2026-01-11 09:04:17.000 ERROR (MainThread) [homeassistant.core] long-opener\nTraceback (most recent call last):\nKeyError: long" and .level == "error"')" -eq 1
check "and the line after it stands alone" \
    test "$(out_count '.message == "2026-01-11 09:04:18.000 INFO (MainThread) [homeassistant.core] ha-last"')" -eq 1
check "an unlisted add-on's lines are not joined" \
    test "$(out_count '.container_name == "app_a0d7b954_appdaemon"')" -eq 2
check "an excluded container sends nothing" \
    test "$(out_count '.container_name | test("wmbusmeters")')" -eq 0
check "a long line's pieces are one event again" \
    test "$(out_count '.message == "[2026-09-27 18:37:26] info: \tz2m: payload {\"a\":1} password: [REDACTED] end"')" -eq 1
check "with the partial-line fields gone" \
    test "$(out_count '[has("CONTAINER_PARTIAL_ID", "CONTAINER_PARTIAL_LAST", "CONTAINER_PARTIAL_MESSAGE", "CONTAINER_PARTIAL_ORDINAL", "timestamp_end")] | any')" -eq 0
check_not "a secret split across pieces is still redacted" grep -Fq hunter5sentinel "${pl}/out.ndjson"
check_not "no colour codes reach the sink" grep -Fq '\u001b' "${pl}/out.ndjson"
check "an oversized line is cut to the cap" \
    test "$(out_count '.container_name == "app_00000000_big" and (.message | length) < 50100 and (.message | endswith("[truncated by the add-on]"))')" -eq 1

# A configuration error stops the add-on rather than restarting it forever: run
# exits with the fatal code, and finish records it as the container's exit code
# and halts. /run belongs to the container, so halt can be stubbed.
current="halt"
mkdir -p /run/s6/basedir/bin /run/s6-linux-init-container-results
printf '#!/bin/sh\ntouch /tmp/halted\n' > /run/s6/basedir/bin/halt
chmod +x /run/s6/basedir/bin/halt
rm -f /tmp/halted /run/s6-linux-init-container-results/exitcode
jq -s '.[0] * .[1]' "${TESTS_DIR}/fixtures/base.json" \
    "${TESTS_DIR}/fixtures/no-source.json" > "${VECTOR_OPTIONS_FILE}"
/etc/s6-overlay/s6-rc.d/vector/run > "${LOG}" 2>&1
rc=$?
check "run exits with the fatal code on a bad config" test "${rc}" -eq "${VECTOR_ADDON_EXIT_FATAL}"
/etc/s6-overlay/s6-rc.d/vector/finish "${rc}" 0 >> "${LOG}" 2>&1
check "finish records the exit code" \
    test "$(cat /run/s6-linux-init-container-results/exitcode 2> /dev/null)" = "${VECTOR_ADDON_EXIT_FATAL}"
check "and halts the container" test -e /tmp/halted
rm -f /tmp/halted
/etc/s6-overlay/s6-rc.d/vector/finish 256 15 >> "${LOG}" 2>&1
check_not "a normal stop does not halt" test -e /tmp/halted

printf '\n%s failing assertion(s)\n' "${failures}"
[[ ${failures} -eq 0 ]]

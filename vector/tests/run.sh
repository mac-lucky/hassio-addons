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
check     "the sink reads both branches" \
    grep -q -- "- split_multiline._unmatched" "${VECTOR_CONFIG}"

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
            ((._t_fields // {}) | to_entries[]) as $f
            | (if .[$f.key] != $f.value
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

# A Python traceback reaches journald as one stderr line per frame, all stamped
# PRIORITY=3. `vector vrl` runs one event at a time and cannot see a reduce, so
# this drives the real transform. The block is lifted out of the config the
# generator just wrote, the same way the auth block is above, so a change in how
# it is emitted is exercised instead of restated here.
current="multiline"
ml=/tmp/multiline
rm -rf "${ml}"; mkdir -p "${ml}"

# Regenerate first: the cases above leave VECTOR_CONFIG on the custom-config
# branch, which never writes a transforms section.
run_case minimal
awk '/^  join_multiline:/{f=1;print;next} f&&!/^    /{f=0} f' "${VECTOR_CONFIG}" \
    | sed 's/- split_multiline.homeassistant/- tag/' > "${ml}/reduce.yaml"
current="multiline"
check "the reduce block was found in the config" test -s "${ml}/reduce.yaml"

cat > "${ml}/in.log" <<'INLOG'
2026-01-11 09:04:15 ERROR (MainThread) [homeassistant.core] boom
Traceback (most recent call last):
  File "/x.py", line 1, in <module>
    raise ValueError("bad")
2026-01-11 09:04:16 INFO (MainThread) [homeassistant.core] next
INLOG

{
    cat <<EOF
data_dir: ${ml}
sources:
  infile:
    type: file
    include:
      - ${ml}/in.log
    read_from: beginning
transforms:
  tag:
    type: remap
    inputs:
      - infile
    source: '.container_name = "homeassistant"'
EOF
    cat "${ml}/reduce.yaml"
    cat <<EOF
sinks:
  out:
    type: file
    inputs:
      - join_multiline
    path: ${ml}/out.log
    encoding:
      codec: json
EOF
} > "${ml}/cfg.yaml"

timeout 10 vector --config-yaml "${ml}/cfg.yaml" > "${ml}/run.log" 2>&1

check "two log lines started two events, the frames between them did not" \
    test "$(wc -l < "${ml}/out.log" 2> /dev/null || echo 0)" -eq 2
check "the traceback rejoined the line that opened it" \
    grep -q 'boom.*Traceback.*raise ValueError' "${ml}/out.log"


printf '\n%s failing assertion(s)\n' "${failures}"
[[ ${failures} -eq 0 ]]

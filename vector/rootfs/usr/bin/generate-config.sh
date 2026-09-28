#!/command/with-contenv bashio
# shellcheck shell=bash
# ==============================================================================
# Home Assistant Add-on: Vector
# Generates the Vector configuration from addon options
# ==============================================================================

set -e

declare victorialogs_endpoint
declare hostname
declare instance
declare collect_journal
declare redact_sensitive
declare stream_fields
declare custom_config_path
declare auth_username


# shellcheck source=/dev/null
source /usr/lib/vector-common.sh

# Only the username is read. It decides whether the auth block is emitted at
# all; the password is never loaded into a shell variable here, so there is no
# copy of it in this process to escape, log or leak. vector-secrets.sh hands
# both straight to Vector at config load.
auth_username=$(vector_addon::auth_username)

victorialogs_endpoint=$(jq -r '.victorialogs_endpoint // ""' "${VECTOR_OPTIONS_FILE}")
hostname=$(jq -r '.hostname // ""' "${VECTOR_OPTIONS_FILE}")
instance=$(jq -r '.instance // "homeassistant"' "${VECTOR_OPTIONS_FILE}")
# Not `// true`: jq's alternative operator treats false as empty, so an option
# the user explicitly set to false would read back as true
collect_journal=$(jq -r 'if .collect_journal == null then true else .collect_journal end' "${VECTOR_OPTIONS_FILE}")
redact_sensitive=$(jq -r 'if .redact_sensitive == null then true else .redact_sensitive end' "${VECTOR_OPTIONS_FILE}")
stream_fields=$(jq -r '.stream_fields // [] | join(",")' "${VECTOR_OPTIONS_FILE}")
custom_config_path=$(vector_addon::custom_config_path)

readonly IDENTIFIER_RE='^[a-zA-Z_][a-zA-Z0-9_]*$'
readonly UNIT_NAME_RE='^[a-zA-Z0-9._@-]+$'
readonly CONTAINER_NAME_RE='^[a-zA-Z0-9_-]+$'

# Function to sanitize strings for safe use in sed and YAML
sanitize_for_sed() {
    # Escape sed special characters: \ & / and newlines
    printf '%s' "$1" | sed -e 's/[\\&/]/\\&/g' -e ':a;N;$!ba;s/\n/\\n/g'
}

# List form of validate_safe_string: every element a jq filter yields must match
# the pattern, or the add-on refuses to start. The filters are literals from
# this file, never user input.
validate_list() {
    local filter="$1"
    local pattern="$2"
    local label="$3"
    local item
    while IFS= read -r item; do
        if [[ ! "${item}" =~ ${pattern} ]]; then
            bashio::log.fatal "Invalid ${label}: ${item}"
            bashio::exit.nok
        fi
    done < <(jq -r "${filter}" "${VECTOR_OPTIONS_FILE}")
}

# Function to validate input contains only safe characters
validate_safe_string() {
    local value="$1"
    local name="$2"
    # Checked separately so an empty value gets an honest message instead of
    # "contains invalid characters"
    if [[ -z "${value}" ]]; then
        bashio::log.fatal "${name} must not be empty"
        bashio::exit.nok
    fi
    # Allow alphanumeric, dots, hyphens, underscores, and spaces
    if [[ ! "${value}" =~ ^[a-zA-Z0-9._\ -]+$ ]]; then
        bashio::log.fatal "${name} contains invalid characters. Only alphanumeric, dots, hyphens, underscores allowed."
        bashio::exit.nok
    fi
    # The VRL placeholders are substituted one after the other over the same
    # file, so a value carrying a placeholder token would be matched again by
    # the later pass and silently end up as the other option's value
    if [[ "${value}" == *__HOSTNAME__* ]] || [[ "${value}" == *__INSTANCE__* ]]; then
        bashio::log.fatal "${name} must not contain __HOSTNAME__ or __INSTANCE__"
        bashio::exit.nok
    fi
}

# Function to mask userinfo anywhere in a stream of text. Matches up to the LAST
# @ before the path, because a password may itself contain an @ - stopping at the
# first one would print the tail of it. Cannot cross / so a query string is safe.
mask_credentials_stream() {
    sed -E 's|(https?://)[^/[:space:]]*@|\1***:***@|g'
}

# Function to print the config with any credentials removed. The generated file
# only ever references the password, but the endpoint may carry user:pass@ and a
# user-supplied custom config can contain a literal password.
dump_config_redacted() {
    mask_credentials_stream < "${VECTOR_CONFIG}" \
        | sed -E 's/^([[:space:]]*(user|password|auth_token|token):[[:space:]]*).*$/\1[REDACTED]/'
}

# Function to run vector validate with its output masked - Vector quotes the
# endpoint back in its error messages, credentials included
run_vector_validate() {
    # --skip-healthchecks: a sink that is down at this moment is an outage, not
    # a broken config. Vector only logs a failed healthcheck when it runs, and
    # a validate that failed on one would stop the add-on for good.
    vector validate --skip-healthchecks --config-yaml "${VECTOR_CONFIG}" 2>&1 | mask_credentials_stream
    return "${PIPESTATUS[0]}"
}

# Function to validate one journal unit list and append it to the journald
# source. Takes the options.json key and the Vector key to write it as.
emit_journal_units() {
    local option="$1"
    local yaml_key="$2"
    # Validate before the any-content guard: [""] must be rejected the same
    # way ["a", ""] is. The old guard captured jq's output with $(), which
    # strips trailing newlines and collapsed a lone empty entry to "unset".
    validate_list ".${option} // [] | .[]" "${UNIT_NAME_RE}" 'journal unit name'
    [[ "$(jq -r --arg o "${option}" '.[$o] // [] | length' "${VECTOR_OPTIONS_FILE}")" -gt 0 ]] || return 0

    echo "    ${yaml_key}:" >> "${VECTOR_CONFIG}"
    # @json quotes the value: a unit starting with @ is a reserved YAML indicator
    jq -r --arg o "${option}" '.[$o][] | "      - " + (. | @json)' \
        "${VECTOR_OPTIONS_FILE}" >> "${VECTOR_CONFIG}"
}

# Function to validate URL doesn't contain YAML-breaking characters
validate_url_for_yaml() {
    local url="$1"
    # Note: $ and {} are rejected because Vector interpolates them textually
    # before parsing, not because YAML would mind them
    if [[ "${url}" =~ [\"\'\`\$\{\}] ]]; then
        bashio::log.fatal "VictoriaLogs endpoint contains invalid characters"
        bashio::exit.nok
    fi
    # Backslash starts an escape sequence in the double-quoted YAML scalar the
    # endpoint lands in: a trailing one swallows the closing quote, and a \x22
    # style escape decodes to a character the class above is meant to ban.
    # Checked with == not =~ because bash strips backslashes from a pattern
    # written inline in [[ ]] before the regex engine sees them (a pattern
    # held in a variable would survive, but the glob is plainer still).
    if [[ "${url}" == *\\* ]]; then
        bashio::log.fatal "VictoriaLogs endpoint contains invalid characters"
        bashio::exit.nok
    fi
    # Must start with http:// or https://
    if [[ ! "${url}" =~ ^https?:// ]]; then
        bashio::log.fatal "VictoriaLogs endpoint must start with http:// or https://"
        bashio::exit.nok
    fi
}

# Check for custom config with path validation (TOCTOU-safe). This runs before
# any option validation: none of the option-driven settings apply in this mode,
# so a user running purely on a custom config does not need a throwaway
# endpoint just to satisfy the checks below.
if [[ -n "${custom_config_path}" ]]; then
    # Falling back to the generated config would silently ignore what the user asked for
    if [[ ! -f "${custom_config_path}" ]]; then
        bashio::log.fatal "Custom config not found: ${custom_config_path}"
        bashio::log.fatal "Fix custom_config_path or clear it to use the generated configuration"
        bashio::exit.nok
    fi
    # Resolve the ACTUAL path (not -m which doesn't require existence)
    # This prevents symlink attacks between check and use
    real_path=$(realpath "${custom_config_path}" 2>/dev/null || echo "")
    if [[ -z "${real_path}" ]]; then
        bashio::log.fatal "Invalid custom config path!"
        bashio::exit.nok
    fi
    # Only allow paths under /config (this add-on's own config dir, the
    # addon_config map) or /share. /addon_configs is a host-side name that
    # never exists inside an add-on container; the same folder is mounted
    # here at /config.
    if [[ ! "${real_path}" =~ ^/(config|share)/ ]]; then
        bashio::log.fatal "Custom config must be in /config (the add-on's config folder, /addon_configs on the host) or /share!"
        bashio::exit.nok
    fi
    # Use the resolved real_path for the copy to prevent TOCTOU
    bashio::log.info "Using custom configuration from: ${real_path}"
    mkdir -p "$(dirname "${VECTOR_CONFIG}")"
    # Vector's default data_dir for a custom config that sets none - it does
    # not exist in the image and validation fails on that alone. /data/vector
    # is the persistent choice a custom config should prefer.
    mkdir -p /var/lib/vector /data/vector
    install -m 600 "${real_path}" "${VECTOR_CONFIG}"
    # Validate custom config before accepting it. The output is discarded, not
    # printed: the validator quotes offending values back verbatim (an invalid
    # enum or a wrongly-typed scalar echoes the literal value), this file is
    # arbitrary user YAML that can hold a credential anywhere, and
    # mask_credentials_stream only knows URL-shaped ones.
    if ! run_vector_validate > /dev/null; then
        bashio::log.fatal "Custom configuration validation failed!"
        bashio::log.fatal "Run 'vector validate' against your file to see the details"
        bashio::exit.nok
    fi
    bashio::log.info "Custom configuration validation passed"
    exit 0
fi

# Validate required configuration
if [[ -z "${victorialogs_endpoint}" ]]; then
    bashio::log.fatal "VictoriaLogs endpoint is required!"
    bashio::exit.nok
fi

# Validate endpoint URL for YAML safety
validate_url_for_yaml "${victorialogs_endpoint}"

# Check the raw JSON, not the shell variables: command substitution has already
# stripped any trailing newline by then, so a password ending in one would
# silently reach Vector truncated. CR and NEL are YAML line breaks too, and any
# of them would end the quoted scalar these values are substituted into.
# NEL has to be written \x{85}: a raw U+0085 in the class breaks it in Oniguruma
if ! jq -e '((.victorialogs_username // "") + (.victorialogs_password // "")
             + (.victorialogs_endpoint // "")) | test("[\\n\\r\\x{85}]") | not' \
        "${VECTOR_OPTIONS_FILE}" > /dev/null; then
    bashio::log.fatal "Endpoint, username and password must not contain line breaks"
    bashio::exit.nok
fi

# Read straight from the options rather than through auth_username, so that "no
# username" and "custom config in use" stay distinguishable here
if jq -e '(.victorialogs_username // "") == "" and (.victorialogs_password // "") != ""' \
        "${VECTOR_OPTIONS_FILE}" > /dev/null; then
    bashio::log.warning "A VictoriaLogs password is set but the username is empty; basic auth will not be used"
elif jq -e '(.victorialogs_username // "") != "" and (.victorialogs_password // "") == ""' \
        "${VECTOR_OPTIONS_FILE}" > /dev/null; then
    bashio::log.warning "A VictoriaLogs username is set but the password is empty; the server will likely reject it"
fi

validate_list '.stream_fields // [] | .[]' "${IDENTIFIER_RE}" 'stream field (must be a valid identifier)'

# The three container lists. An entry matches a container named exactly that or
# ending in _<entry>, so "music_assistant" finds app_d5369777_music_assistant
# and keeps finding it across the Supervisor's addon_ -> app_ rename, which an
# exact journald match would not. Each list becomes one regex literal spliced
# into the VRL, which is safe only because every entry is validated first: the
# character class keeps out quotes, backslashes and every regex metacharacter
# except the literal - and _.
validate_list '.include_containers // [] | .[]' "${CONTAINER_NAME_RE}" 'container name in include_containers'
validate_list '.exclude_containers // [] | .[]' "${CONTAINER_NAME_RE}" 'container name in exclude_containers'
# null means "not set", so the default applies; an explicit [] turns joining off
readonly MULTILINE_FILTER='if .multiline_containers == null then ["homeassistant", "hassio_supervisor"] else .multiline_containers end'
validate_list "${MULTILINE_FILTER} | .[]" "${CONTAINER_NAME_RE}" 'container name in multiline_containers'

container_regex() {
    jq -r "($1) // [] | if length == 0 then \"\" else \"(?:^|_)(?:\" + join(\"|\") + \")\$\" end" \
        "${VECTOR_OPTIONS_FILE}"
}
include_containers_re=$(container_regex '.include_containers')
exclude_containers_re=$(container_regex '.exclude_containers')
multiline_containers_re=$(container_regex "${MULTILINE_FILTER}")

# Validate hostname and instance to prevent injection. An empty hostname is
# fine: the VRL then keeps the host journald reports for each entry. Falling
# back to $(hostname) here would stamp every event with this container's own
# name, a Supervisor-assigned slug like b55247da-vector.
if [[ -n "${hostname}" ]]; then
    validate_safe_string "${hostname}" "hostname"
fi
validate_safe_string "${instance}" "instance"

# Keys a label must not take: they would overwrite a field the add-on sets on
# every event (an extra "message" label would replace every log line), or one
# VictoriaLogs reserves for itself.
readonly RESERVED_LABEL_RE='^(message|timestamp|level|host|instance|unit|container_name|source_type|_msg|_time|_stream|_stream_id)$'
while IFS= read -r label_key; do
    if [[ "${label_key}" =~ ${RESERVED_LABEL_RE} ]]; then
        bashio::log.fatal "Extra label '${label_key}' would overwrite a field the add-on sets itself"
        bashio::exit.nok
    fi
done < <(jq -r '.extra_labels // {} | keys | .[]' "${VECTOR_OPTIONS_FILE}")

# journald is the only source, so disabling it leaves nothing to collect
if [[ "${collect_journal}" != "true" ]]; then
    bashio::log.fatal "collect_journal is disabled and it is the only log source!"
    bashio::exit.nok
fi

masked_endpoint=$(printf '%s\n' "${victorialogs_endpoint}" | mask_credentials_stream)

bashio::log.info "Generating Vector configuration..."
bashio::log.info "VictoriaLogs endpoint: ${masked_endpoint}"
bashio::log.info "Hostname: ${hostname:-(taken from the journal)}"
bashio::log.info "Instance: ${instance}"
bashio::log.info "Redact sensitive: ${redact_sensitive}"
# The container lists, since a typo in include_containers silently drops
# every container but the ones it does match. Validated, so safe to print.
for list in include_containers exclude_containers; do
    if [[ "$(jq -r --arg l "${list}" '.[$l] // [] | length' "${VECTOR_OPTIONS_FILE}")" -gt 0 ]]; then
        bashio::log.info "${list}: $(jq -r --arg l "${list}" '.[$l] | join(", ")' "${VECTOR_OPTIONS_FILE}")"
    fi
done
bashio::log.info "multiline_containers: $(jq -r "${MULTILINE_FILTER} | join(\", \") | if . == \"\" then \"(none)\" else . end" "${VECTOR_OPTIONS_FILE}")"

# Create required directories and clear any existing config. The file is created
# empty and locked down first, because the endpoint written into it further down
# can carry credentials and > preserves the mode of an existing file.
mkdir -p "$(dirname "${VECTOR_CONFIG}")"
# Vector state (journald cursor, buffers) lives in the add-on's private /data,
# never on /share: /share is writable by every share-mapped add-on, which could
# forge the cursor to suppress log shipping or fill the buffer directory.
mkdir -p /data/vector
rm -f "${VECTOR_CONFIG}" "${VECTOR_VRL}"
install -m 600 /dev/null "${VECTOR_CONFIG}"
install -m 600 /dev/null "${VECTOR_VRL}"

# Start generating the configuration
cat > "${VECTOR_CONFIG}" << 'VECTORCONFIG'
# Vector Configuration - Auto-generated by Home Assistant Add-on
# Do not edit directly; modify addon options instead

data_dir: /data/vector

# API for healthcheck and monitoring (localhost only for security)
api:
  enabled: true
  address: 127.0.0.1:8686

VECTORCONFIG

# Try both common journal locations - HA OS may use either
# Vector's journalctl will use --directory flag
journal_dir="/var/log/journal"
if [[ ! -d "${journal_dir}" ]] || [[ -z "$(ls -A "${journal_dir}" 2>/dev/null)" ]]; then
    journal_dir="/run/log/journal"
fi
# Vector would fail on a missing directory too, but with an error that names
# neither the cause nor the fix
if [[ ! -d "${journal_dir}" ]]; then
    bashio::log.fatal "No journal found in /var/log/journal or /run/log/journal; the host journal is not mapped into the add-on"
    bashio::exit.nok
fi
bashio::log.info "Using journal directory: ${journal_dir}"

cat >> "${VECTOR_CONFIG}" << JOURNALDSOURCE
sources:
  journald:
    type: journald
    current_boot_only: true
    journal_directory: ${journal_dir}
JOURNALDSOURCE

emit_journal_units journal_include_units include_units
emit_journal_units journal_exclude_units exclude_units

# Add transforms section - the enrichment program lives in its own file. This
# heredoc is unquoted so the VRL path lands; nothing below may contain a $.
cat >> "${VECTOR_CONFIG}" << TRANSFORMS_HEADER

transforms:
  # Docker's journald driver cuts a line longer than 16 KiB into pieces, marks
  # every piece with the same CONTAINER_PARTIAL_ID and the last one with
  # CONTAINER_PARTIAL_LAST=true. They are glued back together here, before the
  # enrichment, so redaction sees the whole line: a secret can straddle a cut.
  #
  # Every event goes through this one reduce, not just the pieces: a line that
  # is not a piece ends its group at once and passes straight on. Routing only
  # the pieces through here put them a step behind the lines around them, so a
  # long line that opens a traceback arrived after its own frames and
  # join_multiline glued the frames onto the record before it.
  #
  # Six pieces (96 KiB) stay under cap_size below, so a longer line goes out as
  # more than one event instead of being cut.
  join_partial:
    type: reduce
    inputs:
      - journald
    group_by:
      - CONTAINER_PARTIAL_ID
    ends_when: '!exists(.CONTAINER_PARTIAL_ID) || .CONTAINER_PARTIAL_LAST == "true"'
    merge_strategies:
      message: concat_raw
      timestamp: discard
    expire_after_ms: 2000
    max_events: 6

  enrich_logs:
    type: remap
    inputs:
      - join_partial
    file: ${VECTOR_VRL}

  # Docker's journald driver stamps every stderr line PRIORITY=3, and a Python
  # traceback is one stderr line per frame. Each frame therefore arrived as its
  # own error event: one real Home Assistant error became fifteen, the stack was
  # scattered across fifteen rows, and the error count read about fifteen times
  # what it was. Rejoin them onto the line that opened the traceback, which is
  # the one carrying the real level.
  #
  # Only the containers in multiline_containers are routed through the reduce;
  # enrich_logs marks them with %multiline. Everything else goes straight to
  # the sink, so nothing is buffered that does not need to be, and a stall here
  # cannot hold up another add-on's logs.
  split_multiline:
    type: route
    inputs:
      - enrich_logs
    route:
      multiline: '%multiline == true'

  join_multiline:
    type: reduce
    inputs:
      - split_multiline.multiline
    group_by:
      - container_name
    # A new group starts at a line that opens a log record: a timestamp, a
    # bracketed time (bashio), a bare level word (esphome) or an s6-rc line.
    # Anything else is a continuation and merges into the open group. The colour
    # codes are already gone by now. A block scalar, not a quoted one: the VRL
    # regex literal is r'...' and a single-quoted YAML scalar would end at its
    # first quote.
    starts_when: >-
      match(to_string(.message) ?? "", r'^(?:\[?\d{4}-\d{2}-\d{2}(?:T|\s+)\d{2}:\d{2}:\d{2}|\[\d{2}:\d{2}:\d{2}|s6-rc:|(?:TRACE|DEBUG|INFO|NOTICE|WARNING|WARN|ERROR|CRITICAL|FATAL)\b)')
    merge_strategies:
      message: concat_newline
      timestamp: discard
    # A group that goes quiet is flushed 2s after its last line. The other two
    # bound a group that keeps being fed: end_every_period_ms caps it in time
    # when the gaps between lines stay under that 2s, and max_events caps it in
    # size, so a startup banner or a library looping on stderr cannot build one
    # unbounded event on its way to the sink.
    expire_after_ms: 2000
    end_every_period_ms: 10000
    max_events: 200

  # VictoriaLogs skips an entry over 256 KiB (-insert.maxLineSizeBytes) and
  # still answers 200, so the sink would count it delivered and it would be
  # gone for good. Cut the message well short of that instead. length counts
  # bytes and truncate counts characters, so the cut is to 50000 characters:
  # at most 200 KB even if every one of them takes four bytes.
  cap_size:
    type: remap
    inputs:
      - split_multiline._unmatched
      - join_multiline
    source: |-
      msg = to_string(.message) ?? ""
      if length(msg) > 200000 {
        .message = truncate(msg, 50000) + " [truncated by the add-on]"
      }
TRANSFORMS_HEADER

# Write the VRL program. Quoted heredocs so its $, \d and \s survive verbatim;
# the two runtime values are substituted by sed afterwards, and the parts built
# from options are appended with jq/printf from validated values only.
cat > "${VECTOR_VRL}" << 'TRANSFORMS_VRL'
# No `!` anywhere in this program. A runtime error makes remap forward the
# ORIGINAL event - unlabelled, unredacted, colour codes and all - so every
# fallible call is guarded or coalesced instead. `abort` is the only exit and
# it drops the event (remap's drop_on_abort defaults to true); every abort
# message starts "vector-addon-drop:", which is what the tests look for.

# Standard labels. host_override is empty unless the hostname option is set
# (sed fills it in below): journald already fills .host from _HOSTNAME, the
# real host name.
host_override = "__HOSTNAME__"
if host_override != "" {
  .host = host_override
} else if !exists(.host) {
  .host = get_hostname() ?? "unknown"
}
.instance = "__INSTANCE__"
if !exists(.source_type) { .source_type = "unknown" }

# The systemd unit, without its .service suffix
if is_string(._SYSTEMD_UNIT) {
  .unit = replace(string(._SYSTEMD_UNIT) ?? "", r'\.service$', "")
  .container_name = .unit
}
# Core, Supervisor, the plugins and every add-on log through docker.service;
# the container name is what tells them apart
is_container = exists(.CONTAINER_NAME)
if is_container { .container_name = del(.CONTAINER_NAME) }
# Kernel and audit records carry no unit. Named after their syslog identifier
# or their transport they get a stream of their own, not the bare host one.
if !exists(.unit) {
  ident = string(.SYSLOG_IDENTIFIER) ?? string(._TRANSPORT) ?? ""
  if ident != "" {
    .unit = ident
    if !exists(.container_name) { .container_name = ident }
  }
}
container = string(.container_name) ?? ""
TRANSFORMS_VRL

# Replace placeholders with actual values (using sanitized strings). Done before
# anything built from options is appended, so no option value can carry a
# placeholder into this pass.
sed -i -e "s/__HOSTNAME__/$(sanitize_for_sed "${hostname}")/g" \
       -e "s/__INSTANCE__/$(sanitize_for_sed "${instance}")/g" "${VECTOR_VRL}"

# Container filters and the multiline marker, from the validated regexes built
# above. Container records only: host units, the kernel and audit are left to
# the journal unit filters.
{
    echo ""
    echo "# include_containers, exclude_containers and multiline_containers"
    if [[ -n "${exclude_containers_re}" ]]; then
        printf 'if is_container && match(container, r\x27%s\x27) { abort "vector-addon-drop: excluded container" }\n' \
            "${exclude_containers_re}"
    fi
    if [[ -n "${include_containers_re}" ]]; then
        printf 'if is_container && !match(container, r\x27%s\x27) { abort "vector-addon-drop: container not included" }\n' \
            "${include_containers_re}"
    fi
    if [[ -n "${multiline_containers_re}" ]]; then
        printf 'if is_container && match(container, r\x27%s\x27) { %%multiline = true }\n' \
            "${multiline_containers_re}"
    fi
} >> "${VECTOR_VRL}"

cat >> "${VECTOR_VRL}" << 'MESSAGE_VRL'

# A missing or non-string message is replaced rather than left to break the
# program. The placeholder is not encode_json(.): the journal fields ship
# separately anyway, and folding them into .message would push things like
# _CMDLINE (which carries credentials) through a redaction pass that is only
# written for message-shaped text.
if !exists(.message) {
  if exists(.MESSAGE) { .message = del(.MESSAGE) } else { .message = "(no message)" }
}
# Not redundant with the coalesce below: to_string on an array yields the
# fallback and would throw the message away, encode_json keeps it
if !is_string(.message) { .message = encode_json(.message) }
msg = to_string(.message) ?? ""

# What join_partial leaves on a rejoined long line
del(.CONTAINER_PARTIAL_MESSAGE)
del(.CONTAINER_PARTIAL_ID)
del(.CONTAINER_PARTIAL_ORDINAL)
del(.CONTAINER_PARTIAL_LAST)
del(.timestamp_end)

# Colour codes. CSI sequences only: strip_ansi_escape_codes deletes tabs as
# well, and zigbee2mqtt and Java stack frames rely on them. esphome prints some
# of its colour codes as the literal text \033[...m, so those go too.
msg = replace(msg, r'\x1b\[[0-9;?]*[ -/]*[@-~]', "")
msg = replace(msg, r'\\033\[[0-9;]*m', "")
# Trailing whitespace, mostly the \r that sshd in the SSH add-on ends lines with
msg = replace(msg, r'\s+$', "")

# A blank line carries nothing, and VictoriaLogs stores it as "missing _msg field"
if strip_whitespace(msg) == "" { abort "vector-addon-drop: blank message" }

# The level. Docker's journald driver stamps every stderr line PRIORITY=3,
# whatever the line says, so the level word in the line itself is trusted
# first and PRIORITY is only the fallback. The forms are tried in order, and a
# word that is not a known level falls through to the next one. Critical and
# above fold into error.
level_words = {
  "trace": "debug", "trc": "debug", "debug": "debug", "dbg": "debug",
  "info": "info", "inf": "info", "notice": "notice",
  "warn": "warn", "warning": "warn", "wrn": "warn",
  "error": "error", "err": "error", "critical": "error", "crit": "error",
  "fatal": "error", "ftl": "error", "panic": "error", "emerg": "error",
  "emergency": "error", "alert": "error"
}
# A timestamp, then the level:
#   2026-01-11 09:04:15.123 WARNING (MainThread) ...  Core, Supervisor, Python add-ons
#   2026-09-27T05:30:50.123Z  INFO vector::...        Vector, Go and Rust programs
#   2026-09-26T19:26:31Z WRN Serve tunnel error       cloudflared
#   [2026-09-27 18:37:26] error:  z2m: ...            zigbee2mqtt
m = parse_regex(msg, r'^\[?\d{4}-\d{2}-\d{2}(?:T|\s+)\d{2}:\d{2}:\d{2}(?:[.,]\d+)?(?:Z|[+-]\d{2}:?\d{2})?\]?:?\s+(?P<level>[A-Za-z]+)\b') ?? {}
lvl = string(get(level_words, [downcase(string(m.level) ?? "")]) ?? null) ?? ""
# bashio: [05:30:50] INFO: ...
if lvl == "" {
  m = parse_regex(msg, r'^\[\d{2}:\d{2}:\d{2}\]\s+(?P<level>[A-Za-z]+):') ?? {}
  lvl = string(get(level_words, [downcase(string(m.level) ?? "")]) ?? null) ?? ""
}
# s6-overlay: s6-rc: info: service ...
if lvl == "" {
  m = parse_regex(msg, r'^s6-rc:\s+(?P<level>[A-Za-z]+):') ?? {}
  lvl = string(get(level_words, [downcase(string(m.level) ?? "")]) ?? null) ?? ""
}
# esphome and others: the line opens with a bare level word, INFO Reading ...
if lvl == "" {
  m = parse_regex(msg, r'^(?P<level>TRACE|DEBUG|INFO|NOTICE|WARNING|WARN|ERROR|CRITICAL|FATAL)\b') ?? {}
  lvl = string(get(level_words, [downcase(string(m.level) ?? "")]) ?? null) ?? ""
}
# logfmt, Go's slog among others: time=... level=INFO msg=...
if lvl == "" {
  m = parse_regex(msg, r'(?:^|\s)level=(?P<level>[A-Za-z]+)') ?? {}
  lvl = string(get(level_words, [downcase(string(m.level) ?? "")]) ?? null) ?? ""
}
# AirConnect (the AirCast and AirSonos add-ons) prints no level at all and
# writes everything to stderr: [08:10:05.030] AddCastDevice:673 [0x...]: ...
# Its default output is routine device traffic, so it is not left to PRIORITY,
# except for lines whose wording says something went wrong: its errors look
# exactly like the rest.
if lvl == "" && match(msg, r'^\[\d{2}:\d{2}:\d{2}\.\d{3}\]\s+[A-Za-z_]\w*:\d+(?:\s|$)') && !match(msg, r'(?i)\b(?:cannot|unable|error|failed|too many|unsupported|unknown)\b') {
  lvl = "info"
}
if lvl == "" && exists(.PRIORITY) {
  p = to_int(.PRIORITY) ?? 6
  lvl = if p <= 3 { "error" } else if p == 4 { "warn" } else if p == 5 { "notice" } else if p == 6 { "info" } else { "debug" }
}
# Audit records carry no PRIORITY at all; an AppArmor denial deserves a look
if lvl == "" {
  lvl = if contains(msg, "apparmor=\"DENIED\"") { "warn" } else { "info" }
}
.level = lvl

# sshd in the SSH add-on writes its routine connection traffic to stderr.
# Downgraded for ssh containers only: a "Connection reset by" line from
# anything else is a real failure.
if .level == "error" && contains(container, "ssh") && match(msg, r'^(Connection from|Connection closed by|Connection reset by|Close session|Starting session|Received disconnect|Disconnected from|Accepted publickey|Accepted key|Postponed publickey|User child is on pid|Server listening on)') {
  .level = "info"
}

# Add timestamp if missing
if !exists(.timestamp) { .timestamp = now() }
MESSAGE_VRL

# Add sensitive data redaction if enabled
if [[ "${redact_sensitive}" == "true" ]]; then
    bashio::log.info "Adding sensitive data redaction..."
    # No backreferences, so there is no $1 for anything to expand
    cat >> "${VECTOR_VRL}" << 'REDACT_VRL'

# Redact sensitive data. Works on msg, which becomes .message below.
#
# A key only counts when a separator follows it (:, =, := or =>), so a
# sentence that merely mentions a password or a secret is left alone. The key
# and the value may sit in quotes, including the escaped quotes of JSON logged
# inside JSON. \x27 is a single quote, which a raw string cannot hold.
msg = replace(msg, r'(?i)Authorization\\?["\x27]?\s*[:=]\s*\\?["\x27]?\s*Bearer\s+[A-Za-z0-9\-._~+/]+={0,2}', "Authorization: Bearer [REDACTED]")
msg = replace(msg, r'(?i)Authorization\\?["\x27]?\s*[:=]\s*\\?["\x27]?\s*Basic\s+[A-Za-z0-9+/]+={0,2}', "Authorization: Basic [REDACTED]")
msg = replace(msg, r'(?i)X-API-Key\\?["\x27]?\s*[:=]\s*\\?["\x27]?[A-Za-z0-9\-._~+/]+', "X-API-Key: [REDACTED]")
msg = replace(msg, r'(?i)X-Auth-Token\\?["\x27]?\s*[:=]\s*\\?["\x27]?[A-Za-z0-9\-._~+/]+', "X-Auth-Token: [REDACTED]")
msg = replace(msg, r'(?i)api[_-]?key\\?["\x27]?\s*(?::=|=>|[:=])\s*\\?["\x27]?[A-Za-z0-9\-._]{16,}', "api_key: [REDACTED]")
msg = replace(msg, r'(?i)token\\?["\x27]?\s*(?::=|=>|[:=])\s*\\?["\x27]?[A-Za-z0-9\-._]{16,}', "token: [REDACTED]")
msg = replace(msg, r'(?i)password\\?["\x27]?\s*(?::=|=>|[:=])\s*\\?["\x27]?[^\s"\x27]+', "password: [REDACTED]")
msg = replace(msg, r'(?i)secret\\?["\x27]?\s*(?::=|=>|[:=])\s*\\?["\x27]?[A-Za-z0-9\-._]{8,}', "secret: [REDACTED]")
# Command-line flags, which take the value after a space: mysql --password x
msg = replace(msg, r'(?i)--password\s+[^\s-]\S*', "--password [REDACTED]")
msg = replace(msg, r'(?i)--token\s+[^\s-]\S*', "--token [REDACTED]")
msg = replace(msg, r'(?i)--secret\s+[^\s-]\S*', "--secret [REDACTED]")
msg = replace(msg, r'(?i)--api-?key\s+[^\s-]\S*', "--api-key [REDACTED]")
# Credentials in a URL: user:password@, :password@ (Redis) or token@ alike.
# Up to the LAST @ before the path, because a password may contain one.
msg = replace(msg, r'://[^/?#\s]*@', "://[REDACTED]@")
# A bare JWT, which is what a Home Assistant long-lived access token is
msg = replace(msg, r'eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}', "[REDACTED_JWT]")

# A process command line ships as its own field and can carry a credential
# (restic --password-command, a URL with user:pass@), and none of the rules
# above are written for it. SYSLOG_RAW is a second, unredacted copy of the
# message that journald keeps when it rewrites one. Neither says anything a
# log reader needs, so both go.
del(._CMDLINE)
del(.SYSLOG_RAW)
REDACT_VRL
fi

# Unconditional: everything above worked on msg, redaction or not
cat >> "${VECTOR_VRL}" << 'FINISH_VRL'

.message = msg
FINISH_VRL

# Add extra labels if specified (with validation to prevent VRL injection)
extra_labels_count=$(jq -r '.extra_labels // {} | keys | length' "${VECTOR_OPTIONS_FILE}")
if [[ "${extra_labels_count}" -gt 0 ]]; then
    bashio::log.info "Adding extra labels..."
    validate_list '.extra_labels // {} | keys | .[]' "${IDENTIFIER_RE}" 'extra label key (must be a valid identifier)'
    # Values are escaped by jq's @json, preventing injection
    echo "" >> "${VECTOR_VRL}"
    echo "# Extra custom labels" >> "${VECTOR_VRL}"
    jq -r '.extra_labels // {} | to_entries | .[] | "." + .key + " = " + (.value | @json)' "${VECTOR_OPTIONS_FILE}" >> "${VECTOR_VRL}"
fi

# Add sinks section
cat >> "${VECTOR_CONFIG}" << SINKS

sinks:
  victorialogs:
    type: elasticsearch
    inputs:
      - cap_size
    endpoints:
      - "${victorialogs_endpoint}"
    api_version: v8
    compression: gzip
    healthcheck:
      enabled: false
    # The journald cursor only moves past an entry once VictoriaLogs has
    # accepted it, so a crash, an update or an OOM kill during an outage
    # replays what was in flight instead of losing it. The price is a
    # possible duplicate after such a restart. A batch the server rejects is
    # still not retried: journald moves on at the next delivered one.
    acknowledgements:
      enabled: true
SINKS

# Add basic auth if username is provided.
# SECRET[<backend>.<name>] is Vector's own indirection: it runs the backend
# declared at the bottom of this file and substitutes what comes back, so the
# credentials never land here. Bash must expand the backend name, hence the
# unquoted delimiter; SECRET[...] survives it because it holds no $.
#
# Three things are load-bearing and none is obvious:
# - the scalars must stay SINGLE-quoted. Vector substitutes the secret in as
#   text and parses afterwards, so an unescaped quote in a password ends the
#   scalar and breaks the config. vector-secrets.sh doubles ' to match.
# - do not swap this for an environment reference. Vector does not interpolate
#   ${VAR} in a --config-yaml config and the sink then authenticates with the
#   literal text. That shipped in 1.6.0 and was rejected by every server.
# - `vector validate` does not resolve secrets, so it cannot catch a mistake
#   here. Only a real run can; tests/run.sh does one.
if [[ -n "${auth_username}" ]]; then
    bashio::log.info "Adding basic auth for VictoriaLogs..."
    cat >> "${VECTOR_CONFIG}" << AUTHCONFIG
    auth:
      strategy: basic
      user: 'SECRET[${VECTOR_SECRETS_BACKEND}.user]'
      password: 'SECRET[${VECTOR_SECRETS_BACKEND}.password]'
AUTHCONFIG
fi

# Add query section
cat >> "${VECTOR_CONFIG}" << 'QUERY'
    query:
      _msg_field: message
      _time_field: timestamp
QUERY

# An empty value parses as null and the sink rejects it. @json does the quoting,
# the same way the extra_labels values above are emitted.
if [[ -n "${stream_fields}" ]]; then
    jq -r '.stream_fields | join(",") | "      _stream_fields: " + @json' \
        "${VECTOR_OPTIONS_FILE}" >> "${VECTOR_CONFIG}"
else
    bashio::log.warning "No stream fields configured; logs will land in a single stream"
fi

# The backend that resolves the SECRET[] references in the sink. It has to come
# last: it is a top-level key, and everything above is still inside the sink
# block until the indentation drops back to column 0.
if [[ -n "${auth_username}" ]]; then
    cat >> "${VECTOR_CONFIG}" << SECRETS

secret:
  ${VECTOR_SECRETS_BACKEND}:
    type: exec
    command:
      - ${VECTOR_SECRETS_HELPER}
SECRETS
fi

bashio::log.info "Vector configuration generated successfully"
bashio::log.info "Configuration saved to ${VECTOR_CONFIG}"

# Validate the configuration
if run_vector_validate; then
    bashio::log.info "Configuration validation passed"
else
    bashio::log.error "Configuration validation failed!"
    bashio::log.error "Generated configuration (credentials redacted):"
    dump_config_redacted
    bashio::exit.nok
fi

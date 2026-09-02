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
    vector validate --config-yaml "${VECTOR_CONFIG}" 2>&1 | mask_credentials_stream
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

# Use hostname from system if not specified
if [[ -z "${hostname}" ]]; then
    hostname=$(hostname)
fi

# Validate hostname and instance to prevent injection
validate_safe_string "${hostname}" "hostname"
validate_safe_string "${instance}" "instance"

# journald is the only source, so disabling it leaves nothing to collect
if [[ "${collect_journal}" != "true" ]]; then
    bashio::log.fatal "collect_journal is disabled and it is the only log source!"
    bashio::exit.nok
fi

masked_endpoint=$(printf '%s\n' "${victorialogs_endpoint}" | mask_credentials_stream)

bashio::log.info "Generating Vector configuration..."
bashio::log.info "VictoriaLogs endpoint: ${masked_endpoint}"
bashio::log.info "Hostname: ${hostname}"
bashio::log.info "Instance: ${instance}"
bashio::log.info "Redact sensitive: ${redact_sensitive}"

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

# Add transforms section - the program itself lives in its own file
cat >> "${VECTOR_CONFIG}" << TRANSFORMS_HEADER

transforms:
  enrich_logs:
    type: remap
    inputs:
      - journald
    file: ${VECTOR_VRL}
TRANSFORMS_HEADER

# Write the VRL program - quoted heredoc so its $, \d and \s survive verbatim,
# with the two runtime values substituted by sed afterwards
cat > "${VECTOR_VRL}" << 'TRANSFORMS_VRL'
# Add standard labels (HOSTNAME and INSTANCE replaced by sed below)
.host = "__HOSTNAME__"
.instance = "__INSTANCE__"
if !exists(.source_type) { .source_type = "unknown" }

# For journald logs - extract unit name (strip .service suffix)
if exists(._SYSTEMD_UNIT) {
  .unit = replace(string!(._SYSTEMD_UNIT), r'\.service', "")
  .container_name = .unit
}

# Extract container name from journald if available
if exists(.CONTAINER_NAME) { .container_name = del(.CONTAINER_NAME) }

# A missing or non-string message aborts the whole program, which would
# skip both the enrichment above and the redaction below.
# The last resort is a placeholder rather than encode_json(.): the journal
# fields are shipped separately anyway, and folding them into .message
# would push things like _CMDLINE (which carries credentials) through a
# redaction pass that is only written for message-shaped text.
if !exists(.message) {
  if exists(.MESSAGE) { .message = del(.MESSAGE) } else { .message = "(no message)" }
}
# Not redundant with the coalesce below: to_string on an array yields the
# fallback and would throw the message away, encode_json keeps it
if !is_string(.message) { .message = encode_json(.message) }
msg = to_string(.message) ?? ""

# Extract level from message content first (more accurate for HA logs).
# Home Assistant logs format: "2026-01-11 09:04:15 WARNING (MainThread)..."
# with an optional ANSI colour prefix.
level_match = parse_regex(msg, r'^(?:\x1b\[[\d;]*m)?\d{4}-\d{2}-\d{2}\s+\d{2}:\d{2}:\d{2}[\.,]?\d*\s+(?P<level>DEBUG|INFO|WARNING|ERROR|CRITICAL|FATAL)') ?? {}
if is_string(level_match.level) {
  lvl = downcase(string!(level_match.level))
  if lvl == "warning" { lvl = "warn" }
  if lvl == "critical" { lvl = "error" }
  if lvl == "fatal" { lvl = "error" }
  .level = lvl
} else if exists(.PRIORITY) {
  # Fall back to syslog priority if no level found in message
  p = to_int(.PRIORITY) ?? 6
  .level = if p == 0 { "emergency" } else if p == 1 { "alert" } else if p == 2 { "critical" } else if p == 3 { "error" } else if p == 4 { "warn" } else if p == 5 { "notice" } else if p == 6 { "info" } else { "debug" }
}

# Docker's journald driver stamps every stderr line PRIORITY=3, so a daemon
# that writes routine traffic to stderr (sshd in the SSH add-on) arrives as an
# error. Downgrade the connection chatter; a real sshd failure does not start
# like this.
if .level == "error" && match(msg, r'^(Connection from|Connection closed by|Connection reset by|Close session|Starting session|Received disconnect|Disconnected from|Accepted publickey|Server listening on)') {
  .level = "info"
}

# Add timestamp if missing
if !exists(.timestamp) { .timestamp = now() }
TRANSFORMS_VRL

# Replace placeholders with actual values (using sanitized strings)
sed -i -e "s/__HOSTNAME__/$(sanitize_for_sed "${hostname}")/g" \
       -e "s/__INSTANCE__/$(sanitize_for_sed "${instance}")/g" "${VECTOR_VRL}"

# Add sensitive data redaction if enabled
if [[ "${redact_sensitive}" == "true" ]]; then
    bashio::log.info "Adding sensitive data redaction..."
    # Redact sensitive data - simplified approach without backreferences to avoid $1 env var issues
    cat >> "${VECTOR_VRL}" << 'REDACT_VRL'

# Redact sensitive data (API keys, tokens, authorization headers).
# Works on msg, so anything added below sees the redacted text too.
msg = replace(msg, r'(?i)Authorization:\s*Bearer\s+[A-Za-z0-9\-._~+/]+={0,2}', "Authorization: Bearer [REDACTED]")
msg = replace(msg, r'(?i)Authorization:\s*Basic\s+[A-Za-z0-9+/]+={0,2}', "Authorization: Basic [REDACTED]")
msg = replace(msg, r'(?i)X-API-Key:\s*[A-Za-z0-9\-._~+/]+', "X-API-Key: [REDACTED]")
msg = replace(msg, r'(?i)X-Auth-Token:\s*[A-Za-z0-9\-._~+/]+', "X-Auth-Token: [REDACTED]")
msg = replace(msg, r'(?i)api[_-]?key["\s:=]+[A-Za-z0-9\-._]{16,}', "api_key: [REDACTED]")
msg = replace(msg, r'(?i)token["\s:=]+[A-Za-z0-9\-._]{16,}', "token: [REDACTED]")
msg = replace(msg, r'(?i)password["\s:=]+[^\s"]+', "password: [REDACTED]")
msg = replace(msg, r'(?i)secret["\s:=]+[A-Za-z0-9\-._]{8,}', "secret: [REDACTED]")
.message = msg
REDACT_VRL
fi

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
      - enrich_logs
    endpoints:
      - "${victorialogs_endpoint}"
    api_version: v8
    compression: gzip
    healthcheck:
      enabled: false
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

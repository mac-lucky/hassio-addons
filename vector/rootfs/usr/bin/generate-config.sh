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

# Read configuration directly from options.json
CONFIG_FILE="/data/options.json"

victorialogs_endpoint=$(jq -r '.victorialogs_endpoint // ""' "${CONFIG_FILE}")
hostname=$(jq -r '.hostname // ""' "${CONFIG_FILE}")
instance=$(jq -r '.instance // "homeassistant"' "${CONFIG_FILE}")
collect_journal=$(jq -r '.collect_journal // false' "${CONFIG_FILE}")
redact_sensitive=$(jq -r '.redact_sensitive // true' "${CONFIG_FILE}")
stream_fields=$(jq -r '.stream_fields // [] | join(",")' "${CONFIG_FILE}")
custom_config_path=$(jq -r '.custom_config_path // ""' "${CONFIG_FILE}")

# Function to sanitize strings for safe use in sed and YAML
sanitize_for_sed() {
    # Escape sed special characters: \ & / and newlines
    printf '%s' "$1" | sed -e 's/[\\&/]/\\&/g' -e ':a;N;$!ba;s/\n/\\n/g'
}

# Function to validate input contains only safe characters
validate_safe_string() {
    local value="$1"
    local name="$2"
    # Allow alphanumeric, dots, hyphens, underscores, and spaces
    if [[ ! "${value}" =~ ^[a-zA-Z0-9._\ -]+$ ]]; then
        bashio::log.fatal "${name} contains invalid characters. Only alphanumeric, dots, hyphens, underscores allowed."
        exit 1
    fi
}

# Function to mask credentials in URLs for logging
mask_url_credentials() {
    local url="$1"
    # Mask user:pass@ in URLs
    printf '%s\n' "${url}" | sed -E 's|(https?://)([^:]+):([^@]+)@|\1***:***@|g'
}

# Function to validate URL doesn't contain YAML-breaking characters
validate_url_for_yaml() {
    local url="$1"
    # URLs should not contain unescaped quotes, newlines, or YAML special sequences
    if [[ "${url}" =~ [\"\'\`\$\{\}] ]] || [[ "${url}" == *$'\n'* ]]; then
        bashio::log.fatal "VictoriaLogs endpoint contains invalid characters"
        exit 1
    fi
    # Must start with http:// or https://
    if [[ ! "${url}" =~ ^https?:// ]]; then
        bashio::log.fatal "VictoriaLogs endpoint must start with http:// or https://"
        exit 1
    fi
}

# Validate required configuration
if [[ -z "${victorialogs_endpoint}" ]]; then
    bashio::log.fatal "VictoriaLogs endpoint is required!"
    exit 1
fi

# Validate endpoint URL for YAML safety
validate_url_for_yaml "${victorialogs_endpoint}"

# Validate stream_fields contain only safe characters
while IFS= read -r field; do
    if [[ -n "${field}" ]] && [[ ! "${field}" =~ ^[a-zA-Z_][a-zA-Z0-9_]*$ ]]; then
        bashio::log.fatal "Invalid stream field: ${field} (must be valid identifier)"
        exit 1
    fi
done < <(jq -r '.stream_fields // [] | .[]' "${CONFIG_FILE}")

# Use hostname from system if not specified
if [[ -z "${hostname}" ]]; then
    hostname=$(hostname)
fi

# Validate hostname and instance to prevent injection
validate_safe_string "${hostname}" "hostname"
validate_safe_string "${instance}" "instance"

# Check for custom config with path validation (TOCTOU-safe)
if [[ -n "${custom_config_path}" ]]; then
    # First check if file exists
    if [[ -f "${custom_config_path}" ]]; then
        # Resolve the ACTUAL path (not -m which doesn't require existence)
        # This prevents symlink attacks between check and use
        real_path=$(realpath "${custom_config_path}" 2>/dev/null || echo "")
        if [[ -z "${real_path}" ]]; then
            bashio::log.fatal "Invalid custom config path!"
            exit 1
        fi
        # Only allow paths under /addon_configs or /share
        if [[ ! "${real_path}" =~ ^/(addon_configs|share)/ ]]; then
            bashio::log.fatal "Custom config must be in /addon_configs or /share directory!"
            exit 1
        fi
        # Use the resolved real_path for the copy to prevent TOCTOU
        bashio::log.info "Using custom configuration from: ${real_path}"
        mkdir -p /etc/vector
        cp "${real_path}" /etc/vector/vector.yaml
        # Validate custom config before accepting it
        if ! vector validate --config-yaml /etc/vector/vector.yaml; then
            bashio::log.fatal "Custom configuration validation failed!"
            bashio::exit.nok
        fi
        bashio::log.info "Custom configuration validation passed"
        exit 0
    fi
fi

# Mask credentials in endpoint URL for logging
masked_endpoint=$(mask_url_credentials "${victorialogs_endpoint}")

bashio::log.info "Generating Vector configuration..."
bashio::log.info "VictoriaLogs endpoint: ${masked_endpoint}"
bashio::log.info "Hostname: ${hostname}"
bashio::log.info "Instance: ${instance}"
bashio::log.info "Collect journal: ${collect_journal}"
bashio::log.info "Redact sensitive: ${redact_sensitive}"

# Create required directories and clear any existing config
mkdir -p /etc/vector
mkdir -p /share/vector
rm -f /etc/vector/vector.yaml

# Start generating the configuration
cat > /etc/vector/vector.yaml << 'VECTORCONFIG'
# Vector Configuration - Auto-generated by Home Assistant Add-on
# Do not edit directly; modify addon options instead

data_dir: /share/vector

# API for healthcheck and monitoring (localhost only for security)
api:
  enabled: true
  address: 127.0.0.1:8686

VECTORCONFIG

# Add sources section
echo "sources:" >> /etc/vector/vector.yaml

# Track which sources are enabled for transform inputs
declare -a enabled_sources=()

# Add journald source if enabled
if [[ "${collect_journal}" == "true" ]]; then
    bashio::log.info "Enabling journald source..."
    enabled_sources+=("journald")

    # Try both common journal locations - HA OS may use either
    # Vector's journalctl will use --directory flag
    journal_dir="/var/log/journal"
    if [[ ! -d "${journal_dir}" ]] || [[ -z "$(ls -A "${journal_dir}" 2>/dev/null)" ]]; then
        journal_dir="/run/log/journal"
    fi
    bashio::log.info "Using journal directory: ${journal_dir}"

    cat >> /etc/vector/vector.yaml << JOURNALDSOURCE
  journald:
    type: journald
    current_boot_only: false
    journal_directory: ${journal_dir}
JOURNALDSOURCE

    # Add include_units if specified (with validation)
    units_count=$(jq -r '.journal_include_units // [] | length' /data/options.json)
    if [[ "${units_count}" -gt 0 ]]; then
        # Validate unit names contain only safe characters
        while IFS= read -r unit; do
            if [[ ! "${unit}" =~ ^[a-zA-Z0-9._@-]+$ ]]; then
                bashio::log.fatal "Invalid journal unit name: ${unit}"
                bashio::exit.nok
            fi
        done < <(jq -r '.journal_include_units // [] | .[]' /data/options.json)
        echo "    include_units:" >> /etc/vector/vector.yaml
        jq -r '.journal_include_units // [] | .[] | "      - " + .' /data/options.json >> /etc/vector/vector.yaml
    fi

    # Add exclude_units if specified (with validation)
    units_count=$(jq -r '.journal_exclude_units // [] | length' /data/options.json)
    if [[ "${units_count}" -gt 0 ]]; then
        # Validate unit names contain only safe characters
        while IFS= read -r unit; do
            if [[ ! "${unit}" =~ ^[a-zA-Z0-9._@-]+$ ]]; then
                bashio::log.fatal "Invalid journal unit name: ${unit}"
                bashio::exit.nok
            fi
        done < <(jq -r '.journal_exclude_units // [] | .[]' /data/options.json)
        echo "    exclude_units:" >> /etc/vector/vector.yaml
        jq -r '.journal_exclude_units // [] | .[] | "      - " + .' /data/options.json >> /etc/vector/vector.yaml
    fi

    echo "" >> /etc/vector/vector.yaml
fi

# Check if any sources are enabled
if [[ ${#enabled_sources[@]} -eq 0 ]]; then
    bashio::log.fatal "At least one log source must be enabled!"
    bashio::exit.nok
fi

# Build inputs list for transforms
inputs_yaml=""
for source in "${enabled_sources[@]}"; do
    inputs_yaml="${inputs_yaml}      - ${source}\n"
done

# Add transforms section - header
cat >> /etc/vector/vector.yaml << 'TRANSFORMS_HEADER'
transforms:
  enrich_logs:
    type: remap
    inputs:
TRANSFORMS_HEADER

# Add inputs list
printf '%b' "${inputs_yaml}" >> /etc/vector/vector.yaml

# Add VRL source - use quoted heredoc for VRL syntax, substitute variables after
cat >> /etc/vector/vector.yaml << 'TRANSFORMS_VRL'
    source: |
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

      # Map syslog priority to level name
      if exists(.PRIORITY) {
        p = to_int(.PRIORITY) ?? 6
        .level = if p == 0 { "emergency" } else if p == 1 { "alert" } else if p == 2 { "critical" } else if p == 3 { "error" } else if p == 4 { "warning" } else if p == 5 { "notice" } else if p == 6 { "info" } else { "debug" }
      }

      # Ensure message field exists
      if !exists(.message) {
        if exists(.MESSAGE) { .message = del(.MESSAGE) } else { .message = encode_json(.) }
      }

      # Add timestamp if missing
      if !exists(.timestamp) { .timestamp = now() }
TRANSFORMS_VRL

# Replace placeholders with actual values (using sanitized strings)
escaped_hostname=$(sanitize_for_sed "${hostname}")
escaped_instance=$(sanitize_for_sed "${instance}")
sed -i "s/__HOSTNAME__/${escaped_hostname}/g" /etc/vector/vector.yaml
sed -i "s/__INSTANCE__/${escaped_instance}/g" /etc/vector/vector.yaml

# Add sensitive data redaction if enabled
if [[ "${redact_sensitive}" == "true" ]]; then
    bashio::log.info "Adding sensitive data redaction..."
    # Redact sensitive data - simplified approach without backreferences to avoid $1 env var issues
    cat >> /etc/vector/vector.yaml << 'REDACT_VRL'

      # Redact sensitive data (API keys, tokens, authorization headers)
      .message = replace(string!(.message), r'(?i)Authorization:\s*Bearer\s+[A-Za-z0-9\-._~+/]+={0,2}', "Authorization: Bearer [REDACTED]")
      .message = replace(.message, r'(?i)Authorization:\s*Basic\s+[A-Za-z0-9+/]+={0,2}', "Authorization: Basic [REDACTED]")
      .message = replace(.message, r'(?i)X-API-Key:\s*[A-Za-z0-9\-._~+/]+', "X-API-Key: [REDACTED]")
      .message = replace(.message, r'(?i)X-Auth-Token:\s*[A-Za-z0-9\-._~+/]+', "X-Auth-Token: [REDACTED]")
      .message = replace(.message, r'(?i)api[_-]?key["\s:=]+[A-Za-z0-9\-._]{16,}', "api_key: [REDACTED]")
      .message = replace(.message, r'(?i)token["\s:=]+[A-Za-z0-9\-._]{16,}', "token: [REDACTED]")
      .message = replace(.message, r'(?i)password["\s:=]+[^\s"]+', "password: [REDACTED]")
      .message = replace(.message, r'(?i)secret["\s:=]+[A-Za-z0-9\-._]{8,}', "secret: [REDACTED]")
REDACT_VRL
fi

# Add extra labels if specified (with validation to prevent VRL injection)
extra_labels_count=$(jq -r '.extra_labels // {} | keys | length' /data/options.json)
if [[ "${extra_labels_count}" -gt 0 ]]; then
    bashio::log.info "Adding extra labels..."
    # Validate label keys and values contain only safe characters
    while IFS= read -r key; do
        if [[ ! "${key}" =~ ^[a-zA-Z_][a-zA-Z0-9_]*$ ]]; then
            bashio::log.fatal "Invalid extra label key: ${key} (must be valid identifier)"
            bashio::exit.nok
        fi
    done < <(jq -r '.extra_labels // {} | keys | .[]' /data/options.json)
    # Values are escaped by jq's @json, preventing injection
    echo "" >> /etc/vector/vector.yaml
    echo "      # Extra custom labels" >> /etc/vector/vector.yaml
    jq -r '.extra_labels // {} | to_entries | .[] | "      ." + .key + " = " + (.value | @json)' /data/options.json >> /etc/vector/vector.yaml
fi

# Add sinks section
cat >> /etc/vector/vector.yaml << SINKS

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
    query:
      _msg_field: message
      _time_field: timestamp
      _stream_fields: ${stream_fields}
SINKS

bashio::log.info "Vector configuration generated successfully"
bashio::log.info "Configuration saved to /etc/vector/vector.yaml"

# Validate the configuration
if vector validate --config-yaml /etc/vector/vector.yaml; then
    bashio::log.info "Configuration validation passed"
else
    bashio::log.error "Configuration validation failed!"
    bashio::log.error "Generated configuration:"
    cat /etc/vector/vector.yaml
    bashio::exit.nok
fi

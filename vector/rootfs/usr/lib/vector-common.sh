# shellcheck shell=bash
# ==============================================================================
# Home Assistant Add-on: Vector
# Constants and credential loading shared by the s6 run script and the generator
#
# The generated vector.yaml references ${VICTORIALOGS_USER} and
# ${VICTORIALOGS_PASSWORD} instead of holding the values, so the password never
# reaches disk and cannot leak through the config dump on a validation failure.
#
# Vector substitutes environment variables textually into the config before
# parsing it, and the references sit inside single-quoted YAML scalars. A single
# quote is the only character that can terminate such a scalar, so doubling it
# makes any password safe to substitute.
#
# Sourced by both callers; it only sets variables, so each stays in control of
# its own error paths.
# ==============================================================================

# Guarded so that sourcing this file twice in one shell does not fail on the
# readonly reassignment.
if [[ -z "${VECTOR_CONFIG:-}" ]]; then
    readonly VECTOR_CONFIG="/etc/vector/vector.yaml"
    # The enrichment VRL lives in its own file that the remap transform points
    # at, so it can be read and run on its own rather than unpicked out of the
    # YAML. Not written when a custom config is in use.
    # shellcheck disable=SC2034  # read by generate-config.sh and the tests
    readonly VECTOR_VRL="/etc/vector/enrich.vrl"
    readonly VECTOR_OPTIONS_FILE="/data/options.json"
fi

# The configured custom config path, or empty. Shared so that the credential
# gate below and the branch it guards can never disagree about what counts as
# "a custom config is in use".
vector_addon::custom_config_path() {
    jq -r '.custom_config_path // ""' "${VECTOR_OPTIONS_FILE}"
}

vector_addon::load_credentials() {
    local user password

    VICTORIALOGS_USER=""
    VICTORIALOGS_PASSWORD=""
    export VICTORIALOGS_USER VICTORIALOGS_PASSWORD

    # A custom config is arbitrary YAML from /share, which every share-mapped
    # add-on can write, and Vector interpolates env vars into it before parsing.
    # Keep the credentials out of the environment entirely in that case, or such
    # a config could echo them back through a validation error or ship them to
    # an attacker-controlled sink.
    if [[ -n "$(vector_addon::custom_config_path)" ]]; then
        return 0
    fi

    user=$(jq -r '.victorialogs_username // ""' "${VECTOR_OPTIONS_FILE}")
    password=$(jq -r '.victorialogs_password // ""' "${VECTOR_OPTIONS_FILE}")

    VICTORIALOGS_USER="${user//\'/\'\'}"
    VICTORIALOGS_PASSWORD="${password//\'/\'\'}"
    export VICTORIALOGS_USER VICTORIALOGS_PASSWORD
}

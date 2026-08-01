# shellcheck shell=bash
# ==============================================================================
# Home Assistant Add-on: Vector
# Constants and helpers shared by the s6 run script, the generator and the tests
#
# The generated vector.yaml holds SECRET[victorialogs.user] and
# SECRET[victorialogs.password] rather than the credentials themselves. Vector
# resolves those at config load by running vector-secrets.sh and reading the
# values off its stdout, so the password never reaches disk and cannot leak
# through the config dump on a validation failure.
#
# The environment was used for this in 1.6.0 and 1.6.1 and silently did not
# work. Vector does not expand ${VAR} inside a --config-yaml config, so the sink
# authenticated with the literal string "${VICTORIALOGS_PASSWORD}" and the
# server rejected every request. Quoted or unquoted made no difference. Do not
# reintroduce an environment reference here without a test that inspects the
# Authorization header Vector actually sends.
#
# Sourced by all three callers; it only defines things, so each stays in control
# of its own error paths.
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
    # shellcheck disable=SC2034  # read by generate-config.sh and the tests
    readonly VECTOR_SECRETS_HELPER="/usr/bin/vector-secrets.sh"
    # The name the sink refers to: SECRET[<backend>.user]. Kept here so the
    # generated `secret:` block and the generated references cannot drift.
    # shellcheck disable=SC2034  # read by generate-config.sh and the tests
    readonly VECTOR_SECRETS_BACKEND="victorialogs"
fi

# The configured custom config path, or empty. Shared so that the credential
# gate in vector-secrets.sh and the branch it guards can never disagree about
# what counts as "a custom config is in use".
vector_addon::custom_config_path() {
    jq -r '.custom_config_path // ""' "${VECTOR_OPTIONS_FILE}"
}

# The configured username, or empty when basic auth should not be configured.
# Only the username is read here, and only to decide whether to emit the auth
# block: the password is never loaded into a shell variable at all, so there is
# no copy of it to escape, log or leak.
#
# Empty for a custom config too. That is arbitrary YAML from /share, which every
# share-mapped add-on can write, so it must not be able to reach the
# credentials; vector-secrets.sh enforces the same rule at request time.
vector_addon::auth_username() {
    if [[ -n "$(vector_addon::custom_config_path)" ]]; then
        return 0
    fi
    jq -r '.victorialogs_username // ""' "${VECTOR_OPTIONS_FILE}"
}

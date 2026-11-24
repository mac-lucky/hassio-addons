#!/usr/bin/env bash
# ==============================================================================
# Home Assistant Add-on: Vector
# Secret backend for Vector's `exec` secrets management
#
# Vector writes {"version":"1.0","secrets":["user","password"]} to stdin once at
# config load and reads a JSON object back, one entry per requested name. The
# credentials therefore exist only on that pipe: not in vector.yaml, not in the
# redacted config dump the generator prints when validation fails, and not in
# Vector's own validator output.
#
# Vector execs this directly, so it cannot rely on anything the run script set
# up. It reads the options file itself.
#
# The values go out exactly as configured, with no escaping. Since 0.62 Vector
# parses the config first and substitutes what comes back into the string value
# itself, so quotes and other YAML syntax in a password are plain characters.
# Escaping here would put the escape on the wire: up to 0.61 the substitution
# was textual and this doubled ' for the single-quoted scalar.
# ==============================================================================
set -euo pipefail

# shellcheck source=/dev/null
source /usr/lib/vector-common.sh

request=$(cat)

# Every name Vector asked for gets an answer. An unknown name gets an error
# rather than an empty value: Vector reads a null error as "this is the secret"
# and would authenticate with "" instead of refusing to start.
#
# A custom config is arbitrary YAML from /share and can declare this same
# backend, so in that mode nothing is known and every name errors out. The
# generated config never asks in that mode either, because auth_username returns
# empty for it - two independent gates on the same rule.
jq -cn \
    --argjson request "${request}" \
    --slurpfile options "${VECTOR_OPTIONS_FILE}" \
    --arg custom "$(vector_addon::custom_config_path)" \
    '
    ($options[0] // {}) as $o
    | (if $custom != "" then {} else {
          user:     ($o.victorialogs_username // ""),
          password: ($o.victorialogs_password // "")
      } end) as $known
    | reduce ($request.secrets // [])[] as $name ({};
        .[$name] = (if ($known | has($name))
                    then {value: $known[$name], error: null}
                    else {value: null, error: "no such secret: \($name)"}
                    end))
    '

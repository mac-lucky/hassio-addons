// Redaction shared by the three flow-driving layers. Two rules kept apart:
// redactStepSecrets asks whether a FIELD NAME looks like a credential;
// redactValues asks nothing and scrubs every secret:// resolved value.
//
// Both exist because the text is not this add-on's to predict - a step
// rejection comes from an integration's own validator, and internal/httperr
// quotes 400 characters of a Supervisor body verbatim. It reaches the
// activity feed, /data/history.jsonl, the log and state.json.

package regapply

import (
	"regexp"
	"strings"
)

// secretFieldName matches a flow field whose submitted value must never be
// echoed into an error message, a log line or the events feed. Kept narrow
// so an ordinary declared value can never garble a legitimate message.
var secretFieldName = regexp.MustCompile(`(?i)pass(word|wd)?|token|secret|api[_-]?key|access[_-]?key|private[_-]?key|credential`)

// redactStepSecrets replaces every value stepData submitted under a
// secret-looking field name with a marker wherever it appears in text.
//
// Nested one level because a form SECTION submits as a nested mapping
// (subentries.go builds those) and sections do not themselves nest. The
// result reaches the activity feed and the subentries layer's on-disk
// attempts state, so a miss here is not cosmetic.
func redactStepSecrets(text string, stepData map[string]any) string {
	for name, raw := range stepData {
		switch value := raw.(type) {
		case string:
			if value == "" || !secretFieldName.MatchString(name) {
				continue
			}
			text = strings.ReplaceAll(text, value, "***REDACTED***")
		case map[string]any:
			// A section: its own field names decide, not the section's.
			for innerName, innerRaw := range value {
				inner, isString := innerRaw.(string)
				if !isString || inner == "" || !secretFieldName.MatchString(innerName) {
					continue
				}
				text = strings.ReplaceAll(text, inner, "***REDACTED***")
			}
		}
	}
	return text
}

// minRedactableLength is the shortest resolved value redactValues acts on.
// A secrets.yaml key can legitimately hold "true" or "1", and blanking
// every occurrence of a 1-to-3 character string would shred the message;
// a secret that short is not a credential worth protecting.
const minRedactableLength = 4

// redactValues replaces every value in values, wherever it appears in text,
// with the same marker redactStepSecrets uses. values is
// registries.RegOp.Secrets, already sorted and deduplicated by the planner;
// anything shorter than minRedactableLength is skipped.
func redactValues(text string, values []string) string {
	for _, value := range values {
		if len(value) < minRedactableLength {
			continue
		}
		text = strings.ReplaceAll(text, value, "***REDACTED***")
	}
	return text
}

// redactedErr is an error whose TEXT is scrubbed while its identity is not:
// Error() renders the redacted message, Unwrap() hands back the cause, so
// errors.Is and errors.As see through to its sentinel. Collapsing the two
// broke executeFlowOp's ErrIntegrationNotLoaded check for secret-declaring
// ops.
type redactedErr struct {
	msg   string
	cause error
}

func (e *redactedErr) Error() string { return e.msg }
func (e *redactedErr) Unwrap() error { return e.cause }

// redactedError returns err with every value in values scrubbed out of its
// text, or nil when err is nil. The whole error is scrubbed, not just the
// part advanceFlow handles, because the wrapping quotes foreign text.
func redactedError(err error, values []string) error {
	if err == nil || len(values) == 0 {
		return err
	}
	return &redactedErr{msg: redactValues(err.Error(), values), cause: err}
}

package differ

import (
	"path"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/gitsync"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/secretshape"
)

// maskMarker replaces every secret value in a published diff. Fixed-width
// so it leaks nothing about the value it hides, not even its length.
const maskMarker = "*****"

// secretSummary is the whole-file DiffText when no real diff of a file
// holding secret values can be published safely. Not "", which reads as a
// phantom no-op on the page.
const secretSummary = "diff hidden: the file holds secret values"

// secretKeyRe is internal/secretshape's rule, so the keys masked out of a
// diff are by construction the keys a capture refuses to push.
var secretKeyRe = regexp.MustCompile(secretshape.KeyRegex)

// mappingLineRe matches a block-style "key: value", "- key: value"
// included; groups are lead, key, inline value. Tabs are excluded so
// "\tpassword" cannot pass for a key - such a line matches nothing here
// and takes maskSecrets' fail-closed exit instead.
var mappingLineRe = regexp.MustCompile(`^( *(?:- +)*)([^ \t#][^:\t]*?) *:(?: +(.*))?$`)

// seqItemRe matches a plain sequence item ("- value", or a bare "-").
var seqItemRe = regexp.MustCompile(`^( *-)(?: +(.*))?$`)

// flowKeyRe finds "key:" pairs anywhere in a line - how a secret hides in
// flow syntax (see lineMayHideSecret). Group 1 is the key, unquoted.
var flowKeyRe = regexp.MustCompile(`['"]?([A-Za-z0-9_.-]+)['"]? *:`)

// holdsSecrets reports whether either side of a diff carries a literal
// value under a secret-shaped key, which is when its diff is masked before
// publishing. Both sides: the "-" lines quote the live file and the "+"
// lines the repository, and a secret typed into either is still a secret.
func holdsSecrets(p string, sides ...[]byte) bool {
	for _, data := range sides {
		if len(data) > 0 && len(secretshape.Literals(p, data)) > 0 {
			return true
		}
	}
	return false
}

// isYAMLFile reports whether p is a YAML file by extension, the only kind
// maskSecrets can read.
func isYAMLFile(p string) bool {
	ext := strings.ToLower(path.Ext(strings.ReplaceAll(p, `\`, "/")))
	return ext == ".yaml" || ext == ".yml"
}

// maskedDiff is makeDiff for a secret-bearing file: BOTH sides are masked
// before the diff, since DiffText is published verbatim and a unified diff
// quotes context from both. Only YAML reaches maskSecrets - JSON and
// extensionless files fail closed to secretSummary rather than be
// classified by YAML rules.
func maskedDiff(beforeBytes, afterBytes []byte, path string) string {
	if !isYAMLFile(path) {
		return secretSummary
	}
	before, beforeOK := maskSecrets(beforeBytes, path)
	after, afterOK := maskSecrets(afterBytes, path)
	if !beforeOK || !afterOK {
		return secretSummary
	}
	if before == after {
		return secretSummary
	}
	text := makeDiff([]byte(before), []byte(after), path)
	if text == "" {
		return secretSummary
	}
	return text
}

// maskSecrets rewrites YAML with secret values replaced by maskMarker;
// ok=false is the fail-closed exit for any line it cannot prove safe.
// Every value of a secrets file is secret (none should reach a diff, they
// are excluded, but this does not rely on it), and elsewhere every value
// of a KeyRegex key; a secret key's whole block collapses to one marker.
func maskSecrets(data []byte, relPath string) (masked string, ok bool) {
	if looksBinary(data) || !utf8.Valid(data) {
		return "", false
	}

	maskEveryValue := gitsync.IsSecretsFile(relPath)
	lines := splitLinesKeepEnds(string(data))
	var out strings.Builder
	// contBase is the column the last classified line started at; anything
	// deeper and unclassifiable continues a plain scalar of a non-secret
	// key (a secret one's block is consumed below), so it publishes as-is.
	contBase := -1

	for i := 0; i < len(lines); i++ {
		body, ending := splitLineEnding(lines[i])
		// A tab is the fail-closed exit, checked first: YAML accepts one
		// after a key's colon, and tab-free mappingLineRe would miss the
		// line and let the continuation branch publish it verbatim.
		if strings.ContainsRune(body, '\t') {
			return "", false
		}
		trimmed := strings.TrimSpace(body)
		if trimmed == "" || trimmed == "---" || trimmed == "..." {
			out.WriteString(lines[i])
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			// A comment in a secrets file can carry a secret as easily as
			// a value. Elsewhere comments are ordinary config.
			if maskEveryValue {
				out.WriteString(strings.Repeat(" ", leadingSpaces(body)) + "#" + maskMarker + ending)
				continue
			}
			out.WriteString(lines[i])
			continue
		}

		if m := mappingLineRe.FindStringSubmatch(body); m != nil {
			lead, key, value := m[1], m[2], m[3]
			contBase = len(lead)
			if strings.ContainsAny(key, "{[") || keyIsUnreadable(key) {
				// A flow opening where a key was expected, or a key hidden
				// behind an escape: neither can be proved safe.
				return "", false
			}
			if !maskEveryValue && !secretKeyRe.MatchString(unquoteKey(key)) {
				if lineMayHideSecret(body) {
					return "", false
				}
				out.WriteString(lines[i])
				continue
			}
			// A key whose only "value" is a trailing comment has its real
			// value in the block underneath, sequence items included.
			noInlineValue := value == "" || strings.HasPrefix(value, "#")
			out.WriteString(lead + key + ": " + maskMarker + ending)
			i = skipMaskedBlock(lines, i, len(lead), noInlineValue)
			continue
		}

		if m := seqItemRe.FindStringSubmatch(body); m != nil {
			dash := m[1]
			contBase = len(dash) - 1
			if !maskEveryValue {
				if lineMayHideSecret(body) {
					return "", false
				}
				out.WriteString(lines[i])
				continue
			}
			out.WriteString(dash + " " + maskMarker + ending)
			i = skipMaskedBlock(lines, i, len(dash)-1, false)
			continue
		}

		if !maskEveryValue && contBase >= 0 && leadingSpaces(body) > contBase && !lineMayHideSecret(body) {
			out.WriteString(lines[i])
			continue
		}
		return "", false
	}
	return out.String(), true
}

// skipMaskedBlock returns the last line of the block opened at start:
// everything indented past base, plus same-indent sequence items when the
// key had no inline value. A blank line counts only if content follows.
func skipMaskedBlock(lines []string, start, base int, keyHadNoValue bool) int {
	last := start
	for j := start + 1; j < len(lines); j++ {
		body, _ := splitLineEnding(lines[j])
		if strings.TrimSpace(body) == "" {
			continue
		}
		indent := leadingSpaces(body)
		if indent > base {
			last = j
			continue
		}
		if keyHadNoValue && indent == base && strings.HasPrefix(body[indent:], "-") {
			last = j
			continue
		}
		break
	}
	return last
}

// lineMayHideSecret reports whether a line about to be published unmasked
// could hide a secret in flow syntax ("mqtt: {password: hunter2}"), whose
// inner keys this pass never descends into. Reads the whole line, both
// brace directions, and is gated on a flow indicator so Jinja does not
// trip it.
func lineMayHideSecret(line string) bool {
	if !strings.ContainsAny(line, "{}[]") {
		return false
	}
	for _, match := range flowKeyRe.FindAllStringSubmatch(line, -1) {
		if secretKeyRe.MatchString(match[1]) {
			return true
		}
	}
	return false
}

// unquoteKey strips quotes off a mapping key, or a quoted "password"
// would miss the anchored KeyRegex and publish in the clear.
// Escapes are not decoded; keyIsUnreadable fails those closed instead.
func unquoteKey(key string) string {
	if len(key) >= 2 && (key[0] == '"' || key[0] == '\'') && key[len(key)-1] == key[0] {
		return key[1 : len(key)-1]
	}
	return key
}

// keyIsUnreadable reports whether a key carries an escape this pass does
// not decode, so its real name cannot be checked against KeyRegex.
func keyIsUnreadable(key string) bool {
	return strings.Contains(key, `\`)
}

// splitLineEnding splits a line kept by splitLinesKeepEnds into its
// content and its trailing newline (empty for a final line that has none).
func splitLineEnding(line string) (body, ending string) {
	if strings.HasSuffix(line, "\n") {
		return line[:len(line)-1], "\n"
	}
	return line, ""
}

// leadingSpaces counts a line's indentation.
func leadingSpaces(body string) int {
	return len(body) - len(strings.TrimLeft(body, " "))
}

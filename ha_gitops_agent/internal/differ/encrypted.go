package differ

import (
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/sopscrypt"
)

// RepoTransform sops-decrypts one repository file's bytes (rel is
// repo-relative). encrypted=true routes the comparison through
// yamlSemanticallyEqual and maskedDiff; an error makes Compute report the
// path rather than call a repository it cannot read in sync.
type RepoTransform func(rel string, data []byte) (out []byte, encrypted bool, err error)

// maskMarker replaces every secret value in a published diff. Fixed-width
// so it leaks nothing about the value it hides, not even its length.
const maskMarker = "*****"

// encryptedSummary is the whole-file DiffText when no real diff can be
// published safely. Not "", which reads as a phantom no-op on the page.
const encryptedSummary = "encrypted values changed (hidden)"

// noAgeKeyReason is the decryptFailure reason for repository content that
// is sops ciphertext when no decryption is available at all.
const noAgeKeyReason = "repository contains SOPS-encrypted files but no age_key is configured"

// secretKeyRe is sopscrypt's own rule, so the keys masked out of a diff
// are by construction the keys sops was told to encrypt.
var secretKeyRe = regexp.MustCompile(sopscrypt.SecretKeyRegex)

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

// applyRepoTransform runs transform over one file. With no transform,
// encrypted content is an error: passing ciphertext through would let the
// applier write ENC[...] strings into /homeassistant.
func applyRepoTransform(transform RepoTransform, rel string, data []byte) (out []byte, encrypted bool, err error) {
	if transform == nil {
		if sopscrypt.IsEncrypted(data) {
			return nil, false, errors.New(noAgeKeyReason)
		}
		return data, false, nil
	}
	return transform(rel, data)
}

// yamlSemanticallyEqual reports whether the decrypted repo copy and the
// live file say the same thing, ignoring formatting sops rewrites. Shared
// with gitsync's import, so the two layers cannot disagree.
func yamlSemanticallyEqual(a, b []byte) bool {
	return sopscrypt.SemanticallyEqual(a, b)
}

// maskedDiff is makeDiff for an encrypted file: BOTH sides are masked
// before the diff, since DiffText is published verbatim and a unified diff
// quotes context from both. Only YAML reaches maskSecrets - JSON and
// dotenv fail closed rather than be classified by YAML rules.
func maskedDiff(beforeBytes, afterBytes []byte, path string) string {
	if !sopscrypt.IsYAMLFile(path) {
		return encryptedSummary
	}
	before, beforeOK := maskSecrets(beforeBytes, path)
	after, afterOK := maskSecrets(afterBytes, path)
	if !beforeOK || !afterOK {
		return encryptedSummary
	}
	if before == after {
		return encryptedSummary
	}
	text := makeDiff([]byte(before), []byte(after), path)
	if text == "" {
		return encryptedSummary
	}
	return text
}

// maskSecrets rewrites YAML with secret values replaced by maskMarker;
// ok=false is the fail-closed exit for any line it cannot prove safe.
// Secret follows sopscrypt (every value in secrets.yaml, SecretKeyRegex
// elsewhere), and a secret key's whole block collapses to one marker.
func maskSecrets(data []byte, relPath string) (masked string, ok bool) {
	if looksBinary(data) || !utf8.Valid(data) {
		return "", false
	}

	maskEveryValue := sopscrypt.IsSecretsFile(relPath)
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
			// sops encrypts comments in secrets.yaml, so publishing one
			// leaks what the file was encrypted to hide. Elsewhere they are
			// plaintext in the repo too.
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
// would miss the anchored SecretKeyRegex and publish in the clear.
// Escapes are not decoded; keyIsUnreadable fails those closed instead.
func unquoteKey(key string) string {
	if len(key) >= 2 && (key[0] == '"' || key[0] == '\'') && key[len(key)-1] == key[0] {
		return key[1 : len(key)-1]
	}
	return key
}

// keyIsUnreadable reports whether a key carries an escape this pass does
// not decode, so its real name cannot be checked against SecretKeyRegex.
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

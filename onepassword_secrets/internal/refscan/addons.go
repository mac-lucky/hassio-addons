package refscan

import (
	"fmt"
	"sort"
	"strings"
)

// AddonRef is one use of a key in another add-on's options.
type AddonRef struct {
	Key  string
	Slug string
	Name string
	// Option is the option's path, "logins[0].password".
	Option string
	// SecretURL marks the GitOps agent's "secret://key" form, which that
	// add-on resolves itself at startup; "!secret key" is resolved by
	// Supervisor when the add-on starts. Either way a restart picks up a
	// new value, and both read the root secrets.yaml only.
	SecretURL bool
}

// AddonOptionRefs walks one add-on's options (as Supervisor returns them,
// with "!secret" values unresolved) and returns the keys they use. Only
// references are read; no other option value is kept or returned.
func AddonOptionRefs(slug, name string, options map[string]any) []AddonRef {
	var out []AddonRef
	var walk func(v any, at string)
	walk = func(v any, at string) {
		switch t := v.(type) {
		case string:
			if key, ok := strings.CutPrefix(t, "!secret "); ok {
				if key = strings.TrimSpace(key); key != "" {
					out = append(out, AddonRef{Key: key, Slug: slug, Name: name, Option: at})
				}
			} else if key, ok := strings.CutPrefix(t, "secret://"); ok && key != "" {
				out = append(out, AddonRef{Key: key, Slug: slug, Name: name, Option: at, SecretURL: true})
			}
		case map[string]any:
			keys := make([]string, 0, len(t))
			for k := range t {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				next := k
				if at != "" {
					next = at + "." + k
				}
				walk(t[k], next)
			}
		case []any:
			for i, e := range t {
				walk(e, fmt.Sprintf("%s[%d]", at, i))
			}
		}
	}
	walk(options, "")
	return out
}

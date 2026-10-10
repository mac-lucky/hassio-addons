// Package options loads the add-on's options, which Supervisor writes to
// /data/options.json per config.yaml's schema. The schema is the first
// line of validation; Load re-checks every value anyway, because a
// development run or a hand-edited file skips Supervisor.
package options

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"strings"

	"github.com/mac-lucky/hassio-addons/onepassword_secrets/internal/discover"
)

// Supervisor is the base URL for the Supervisor API.
const Supervisor = "http://supervisor"

// Path is where Supervisor writes the options.
const Path = "/data/options.json"

// Reference is one explicit key -> op:// reference.
type Reference struct {
	Key string `json:"key"`
	Ref string `json:"ref"`
}

// Options is a typed view of options.json.
type Options struct {
	// ConnectCredentials is the 1password-credentials.json content of the
	// embedded Connect server, decoded from raw JSON or base64. Empty with
	// ConnectURL set (an external server needs none). Never logged.
	ConnectCredentials []byte
	// CredentialsSet records that credentials were given, so main can
	// wipe ConnectCredentials once the Connect server has them on disk.
	CredentialsSet bool
	// ConnectToken is the Connect access token. Never logged.
	ConnectToken string
	// ConnectURL points at an external Connect server instead of the
	// embedded one.
	ConnectURL string
	// Vaults are the vaults whose fields become keys, by name or ID.
	Vaults []string
	// References map extra keys to op:// references.
	References []Reference
	// SecretsFiles are the config-relative files this add-on renders. The
	// first is always the root secrets.yaml.
	SecretsFiles []string
	// PollIntervalSeconds is how often 1Password is checked for changes.
	PollIntervalSeconds int
	// RestartAddons restarts add-ons whose options use a changed key.
	RestartAddons bool
	// CoreRestart is "auto" (restart Home Assistant, guarded) or "notify"
	// when a changed key needs a restart.
	CoreRestart string
	// DryRun computes and shows every change but writes nothing.
	DryRun bool
	// LogLevel is debug, info, warning or error.
	LogLevel string
}

// Defaults mirrors config.yaml's options block.
func Defaults() Options {
	return Options{
		Vaults:              []string{"homeassistant"},
		SecretsFiles:        []string{"secrets.yaml"},
		PollIntervalSeconds: 60,
		RestartAddons:       true,
		CoreRestart:         "auto",
		LogLevel:            "info",
	}
}

// Limits on what the options may hold.
const (
	MinPollSeconds = 15
	MaxPollSeconds = 3600
	maxListLen     = 64
)

// ErrMissingSupervisorToken is returned when SUPERVISOR_TOKEN is unset.
var ErrMissingSupervisorToken = errors.New("options: SUPERVISOR_TOKEN is not set in the environment")

// SupervisorToken returns the token Supervisor gives every add-on.
func SupervisorToken() (string, error) {
	if t := os.Getenv("SUPERVISOR_TOKEN"); t != "" {
		return t, nil
	}
	return "", ErrMissingSupervisorToken
}

// raw is the wire shape. Pointers tell "absent" from "zero".
type raw struct {
	ConnectCredentials  *string     `json:"connect_credentials"`
	ConnectToken        *string     `json:"connect_token"`
	ConnectURL          *string     `json:"connect_url"`
	Vaults              []string    `json:"vaults"`
	References          []Reference `json:"references"`
	SecretsFiles        []string    `json:"secrets_files"`
	PollIntervalSeconds *int        `json:"poll_interval_seconds"`
	RestartAddons       *bool       `json:"restart_addons"`
	CoreRestart         *string     `json:"core_restart"`
	DryRun              *bool       `json:"dry_run"`
	LogLevel            *string     `json:"log_level"`
}

// Load reads and validates the options file. Errors name the option,
// never its value.
func Load(file string) (Options, error) {
	data, err := os.ReadFile(file) // #nosec G304 -- fixed path, or a test's
	if err != nil {
		return Options{}, fmt.Errorf("options: reading %s: %w", file, err)
	}
	var r raw
	if err := json.Unmarshal(data, &r); err != nil {
		return Options{}, errors.New("options: options.json is not valid JSON")
	}
	return r.validate()
}

func (r raw) validate() (Options, error) {
	o := Defaults()
	if r.ConnectToken != nil {
		o.ConnectToken = strings.TrimSpace(*r.ConnectToken)
	}
	if r.ConnectURL != nil {
		o.ConnectURL = strings.TrimRight(strings.TrimSpace(*r.ConnectURL), "/")
	}
	if o.ConnectURL != "" {
		u, err := url.Parse(o.ConnectURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return Options{}, errors.New("options: connect_url must be an http(s) URL")
		}
	}
	if r.ConnectCredentials != nil {
		creds, err := decodeCredentials(*r.ConnectCredentials)
		if err != nil {
			return Options{}, err
		}
		o.ConnectCredentials = creds
		o.CredentialsSet = len(creds) > 0
	}
	if len(r.Vaults) > 0 {
		o.Vaults = nil
		for _, v := range r.Vaults {
			v = strings.TrimSpace(v)
			if v == "" || strings.ContainsAny(v, "/\n") || len(v) > 128 {
				return Options{}, errors.New("options: vaults holds an empty or invalid vault name")
			}
			o.Vaults = append(o.Vaults, v)
		}
		if len(o.Vaults) > maxListLen {
			return Options{}, errors.New("options: too many vaults")
		}
	}
	// Entries are named by position, never quoted: a value pasted into
	// the wrong field must not reach the log.
	seenKeys := map[string]bool{}
	for i, ref := range r.References {
		ref.Key = strings.TrimSpace(ref.Key)
		ref.Ref = strings.TrimSpace(ref.Ref)
		if !discover.KeyPattern.MatchString(ref.Key) {
			return Options{}, fmt.Errorf("options: references[%d]: key must be lowercase letters, digits and underscores", i)
		}
		if seenKeys[ref.Key] {
			return Options{}, fmt.Errorf("options: references[%d]: the key is listed twice", i)
		}
		seenKeys[ref.Key] = true
		if _, err := discover.ParseRef(ref.Ref); err != nil {
			return Options{}, fmt.Errorf("options: references[%d]: ref must be op://vault/item/field or op://vault/item/section/field", i)
		}
		o.References = append(o.References, ref)
	}
	if len(o.References) > 4*maxListLen {
		return Options{}, errors.New("options: too many references")
	}
	if r.SecretsFiles != nil {
		files, err := validateFiles(r.SecretsFiles)
		if err != nil {
			return Options{}, err
		}
		o.SecretsFiles = files
	}
	if r.PollIntervalSeconds != nil {
		p := *r.PollIntervalSeconds
		if p < MinPollSeconds || p > MaxPollSeconds {
			return Options{}, fmt.Errorf("options: poll_interval_seconds must be %d-%d", MinPollSeconds, MaxPollSeconds)
		}
		o.PollIntervalSeconds = p
	}
	if r.RestartAddons != nil {
		o.RestartAddons = *r.RestartAddons
	}
	if r.CoreRestart != nil {
		switch *r.CoreRestart {
		case "auto", "notify":
			o.CoreRestart = *r.CoreRestart
		default:
			return Options{}, errors.New("options: core_restart must be auto or notify")
		}
	}
	if r.DryRun != nil {
		o.DryRun = *r.DryRun
	}
	if r.LogLevel != nil {
		switch *r.LogLevel {
		case "debug", "info", "warning", "error":
			o.LogLevel = *r.LogLevel
		default:
			return Options{}, errors.New("options: log_level must be debug, info, warning or error")
		}
	}
	return o, nil
}

// Configured reports whether there is enough to reach 1Password at all.
func (o Options) Configured() bool {
	if o.ConnectToken == "" {
		return false
	}
	return o.ConnectURL != "" || o.CredentialsSet
}

// Embedded reports whether the add-on runs its own Connect server.
func (o Options) Embedded() bool { return o.ConnectURL == "" }

// validateFiles normalizes secrets_files: config-relative, no escape, named
// secrets.yaml / secret.yaml (or .yml), root secrets.yaml first.
func validateFiles(in []string) ([]string, error) {
	out := []string{"secrets.yaml"}
	seen := map[string]bool{"secrets.yaml": true}
	for i, f := range in {
		f = strings.TrimSpace(strings.ReplaceAll(f, "\\", "/"))
		clean := path.Clean(strings.TrimPrefix(f, "/"))
		if f == "" || clean == "." || strings.HasPrefix(clean, "../") || clean == ".." {
			return nil, fmt.Errorf("options: secrets_files[%d] is not a path inside the config directory", i)
		}
		// Only the names Home Assistant, ESPHome and Zigbee2MQTT read
		// secrets from, which are also the names the GitOps agent never
		// syncs: any other name could be overwritten from git.
		switch strings.ToLower(path.Base(clean)) {
		case "secrets.yaml", "secrets.yml", "secret.yaml", "secret.yml":
		default:
			return nil, fmt.Errorf("options: secrets_files[%d] must be named secrets.yaml or secret.yaml", i)
		}
		for _, seg := range strings.Split(clean, "/") {
			if strings.HasPrefix(seg, ".") {
				return nil, fmt.Errorf("options: secrets_files[%d] may not be in a hidden directory", i)
			}
		}
		if seen[clean] {
			continue
		}
		seen[clean] = true
		out = append(out, clean)
	}
	if len(out) > maxListLen {
		return nil, errors.New("options: too many secrets_files")
	}
	return out, nil
}

// decodeCredentials accepts the credentials file as raw JSON or base64
// (what `base64 < 1password-credentials.json` prints), and checks it is a
// JSON object Connect can read. The error never quotes the input.
func decodeCredentials(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	data := []byte(s)
	if !strings.HasPrefix(s, "{") {
		compact := strings.Join(strings.Fields(s), "")
		decoded, err := base64.StdEncoding.DecodeString(compact)
		if err != nil {
			decoded, err = base64.RawStdEncoding.DecodeString(compact)
		}
		if err != nil {
			return nil, errors.New("options: connect_credentials is neither JSON nor base64")
		}
		data = decoded
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, errors.New("options: connect_credentials is not a JSON object (paste the whole 1password-credentials.json)")
	}
	if _, ok := obj["version"]; !ok {
		return nil, errors.New("options: connect_credentials has no \"version\": is it really 1password-credentials.json?")
	}
	return data, nil
}

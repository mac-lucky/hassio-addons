// Package devices plans reconciliation of Home Assistant's device registry
// against gitops/devices.yaml.
//
// It is internal/entities' device-registry sibling, UPDATE-ONLY in the same
// way and emitting the same registries.RegOp shape. Devices are created and
// removed by integrations, never over the WebSocket API, so what the agent
// manages is four customization fields on devices that already exist: the
// user-facing name, area, labels and disabled.
//
// # Manifest format
//
// A device id is a random hex string nobody can read off the UI, so an
// entry carries a manifest key of its own plus a match block naming the
// live device it means:
//
//	devices:
//	  - id: kitchen_strip            # manifest key, ^[a-z0-9_]+$
//	    match:
//	      name: LED Kitchen          # the integration's device name, any case
//	      identifier: "esphome:aa:bb:cc:dd:ee:ff"  # "domain:value"
//	      device_id: f66eece92f36df1e909d27f0d866b1b7
//	    name: Kitchen LED strip      # -> name_by_user; null clears it
//	    area: kitchen                # a registries.yaml area id or a live
//	                                 # area_id; null clears it
//	    labels: [lighting]           # same resolution rule as area
//	    disabled: false              # false -> disabled_by null; true -> "user"
//
// match takes any of its three keys and needs at least one. Every key given
// must hold, and together they must pick out exactly one live device.
// match.name is compared against the integration's own "name", never
// name_by_user: the entry would otherwise stop matching the moment it
// renamed the device. identifier matches when it equals one [domain, value]
// pair of the device's identifiers joined as "domain:value", exactly. It is
// never split: either half may hold colons of its own - a MAC address in
// the value, or HomeKit Controller's "homekit_controller:accessory-id"
// domain.
//
// Like entities.yaml this is an ALLOWLIST: only those four fields are
// accepted, and each maps onto one config/device_registry/update param.
//
// # Ownership
//
// The "managed" side is state["device_originals"], keyed
// "device:<device_id>" rather than by manifest key: once an entry is gone,
// the live id is the only thing left saying which device to restore.
//
//  1. An entry whose match picks out no device, several, or a device another
//     entry also picked -> KindError op, naming the problem. This layer
//     NEVER creates a device.
//  2. An entry declaring disabled, on a device whose live disabled_by is
//     neither null nor "user" -> KindError op: the integration or its config
//     entry disabled it, and flipping it would fight that. An entry that
//     leaves disabled out may still set the other three fields.
//  3. Otherwise only the entry's OWN declared fields are compared or sent.
//     A KindUpdate op is emitted when a declared field differs from live, or
//     when it has no entry in device_originals yet - so the applier has
//     something to execute that records the original.
//  4. A device with recorded originals that no field-declaring entry
//     resolves to any more (the entry was removed, re-pointed at another
//     device, or declares no fields) gets a KindRestore op putting every
//     recorded original back.
//  5. A device with recorded originals that is gone from the registry
//     (removed, or re-added under a new id) gets a KindForget op: there is
//     nothing left to restore, so the applier only drops its
//     device_originals entry, sending nothing to Home Assistant. An error
//     op instead would repeat every cycle with no way out short of editing
//     state.json. No entry can resolve to a missing device, so this never
//     competes with rules 1-3.
//
// Rule 4 has one hold. While an entry that declares fields matches no
// device at all, no restore is planned: that entry may well mean one of the
// managed devices - an integration renaming its device is enough to break a
// name-only match - and restoring would undo everything it set, a device it
// disabled included, only for the next cycle to redo it once the match is
// fixed. An entry matching several devices holds only those devices. Rule
// 5 is never held: a forget changes nothing live.
//
// area/labels resolve exactly as entities.yaml's do, through
// entities.RefResolver; one created in this SAME cycle does not resolve.
package devices

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/difftext"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/entities"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/registries"
	yaml "go.yaml.in/yaml/v3"
)

// idPattern validates a manifest key. It names the entry only; the live
// device is whatever match picks out.
var idPattern = regexp.MustCompile(`^[a-z0-9_]+$`)

// allowedFields are the only per-item fields besides id and match; anything
// else is a validation error.
var allowedFields = map[string]bool{"name": true, "area": true, "labels": true, "disabled": true}

// allowedMatchKeys are the only keys a match block accepts.
var allowedMatchKeys = map[string]bool{"device_id": true, "name": true, "identifier": true}

// Kinds devices.Plan's ops carry: the three entities.Plan uses, plus
// registries' bookkeeping-only KindForget for a managed device that is gone.
const (
	KindUpdate  = registries.KindUpdate
	KindError   = registries.KindError
	KindRestore = entities.KindRestore
	KindForget  = registries.KindForget
)

// ManifestError is returned when gitops/devices.yaml fails to parse or
// validate. Error() aggregates every problem, not just the first.
type ManifestError struct {
	Problems []string
}

func (e *ManifestError) Error() string {
	return strings.Join(e.Problems, "; ")
}

// Match is an entry's match block; a key not given is "".
type Match struct {
	DeviceID   string
	Name       string
	Identifier string
}

// Device is one validated manifest entry.
type Device struct {
	ID    string
	Match Match
	// Fields holds the declared allowedFields exactly as written; a key
	// present with a nil value is an explicit null. Empty when the entry
	// declares nothing, which un-manages its device (rule 4).
	Fields map[string]any
}

// Desired is the parsed, validated gitops/devices.yaml, in manifest order.
type Desired struct {
	Devices []Device
}

func emptyDesired() Desired { return Desired{Devices: []Device{}} }

// LoadManifest loads and validates <workdir>/gitops/devices.yaml. A missing
// file returns an empty Desired, not an error: the layer is idle.
func LoadManifest(workdir string) (Desired, error) {
	path := filepath.Join(workdir, "gitops", "devices.yaml")
	info, statErr := os.Stat(path)
	if statErr != nil || !info.Mode().IsRegular() {
		return emptyDesired(), nil
	}

	data, err := os.ReadFile(path) // #nosec G304 -- path is workdir-relative, constructed by this package only
	if err != nil {
		return Desired{}, &ManifestError{Problems: []string{fmt.Sprintf("devices.yaml: could not read file: %v", err)}}
	}

	var parsed any
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		return Desired{}, &ManifestError{Problems: []string{fmt.Sprintf("devices.yaml: invalid YAML: %v", err)}}
	}
	if parsed == nil {
		return emptyDesired(), nil
	}
	obj, ok := parsed.(map[string]any)
	if !ok {
		return Desired{}, &ManifestError{Problems: []string{"devices.yaml: top level must be a mapping"}}
	}

	itemsRaw, present := obj["devices"]
	if !present || itemsRaw == nil {
		return emptyDesired(), nil
	}
	items, ok := itemsRaw.([]any)
	if !ok {
		return Desired{}, &ManifestError{Problems: []string{"devices.yaml: devices must be a list"}}
	}

	var errs []string
	seen := map[string]bool{}
	result := []Device{}

	for idx, rawItem := range items {
		itemMap, ok := rawItem.(map[string]any)
		if !ok {
			errs = append(errs, fmt.Sprintf("devices.yaml: devices[%d] is not a mapping", idx))
			continue
		}

		id, idIsString := itemMap["id"].(string)
		if !idIsString || !idPattern.MatchString(id) {
			errs = append(errs, fmt.Sprintf("devices.yaml: devices[%d] has an invalid or missing 'id' (lowercase letters, digits and _)", idx))
			continue
		}
		if seen[id] {
			errs = append(errs, fmt.Sprintf("devices.yaml: duplicate id '%s'", id))
			continue
		}

		device, itemErrs := validateItem(id, itemMap)
		if len(itemErrs) > 0 {
			errs = append(errs, itemErrs...)
			continue
		}

		seen[id] = true
		result = append(result, device)
	}

	if len(errs) > 0 {
		return Desired{}, &ManifestError{Problems: errs}
	}
	return Desired{Devices: result}, nil
}

// validateItem validates one manifest item apart from id (the caller did
// that), reporting every problem it finds rather than the first.
//
// The type checks are also what keeps rendered fields to strings, bools,
// nulls and string lists - the whole value domain fieldDiff hands to
// difftext.ReprValue. Widening this (and allowedFields) changes plan text,
// so add a diff-text assertion, not just a validation one.
func validateItem(id string, itemMap map[string]any) (Device, []string) {
	device := Device{ID: id, Fields: map[string]any{}}
	var errs []string
	var unknown []string

	for k, v := range itemMap {
		switch {
		case k == "id" || k == "match":
			continue
		case !allowedFields[k]:
			unknown = append(unknown, k)
		default:
			device.Fields[k] = v
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		errs = append(errs, fmt.Sprintf(
			"devices.yaml: device '%s' has unsupported field(s) %s", id, strings.Join(unknown, ", ")))
	}

	match, matchErrs := validateMatch(id, itemMap["match"])
	errs = append(errs, matchErrs...)
	device.Match = match

	if v, ok := device.Fields["name"]; ok && v != nil {
		if s, ok := v.(string); !ok || s == "" {
			errs = append(errs, fmt.Sprintf("devices.yaml: device '%s' name must be a non-empty string or null", id))
		}
	}
	if v, ok := device.Fields["area"]; ok && v != nil {
		if s, ok := v.(string); !ok || s == "" {
			errs = append(errs, fmt.Sprintf("devices.yaml: device '%s' area must be a non-empty string or null", id))
		}
	}
	if v, ok := device.Fields["labels"]; ok && v != nil {
		if !isStringList(v) {
			errs = append(errs, fmt.Sprintf("devices.yaml: device '%s' labels must be a list of non-empty strings", id))
		}
	}
	if v, ok := device.Fields["disabled"]; ok && v != nil {
		if _, ok := v.(bool); !ok {
			errs = append(errs, fmt.Sprintf("devices.yaml: device '%s' disabled must be a boolean", id))
		}
	}

	if len(errs) > 0 {
		return Device{}, errs
	}
	return device, nil
}

// validateMatch validates an item's match block: a mapping of
// allowedMatchKeys to non-empty strings, at least one of them given, with
// identifier shaped "domain:value".
func validateMatch(id string, raw any) (Match, []string) {
	matchMap, ok := raw.(map[string]any)
	if !ok {
		return Match{}, []string{fmt.Sprintf(
			"devices.yaml: device '%s' needs a 'match' mapping with at least one of device_id, name, identifier", id)}
	}

	var errs []string
	var unknown []string
	for k := range matchMap {
		if !allowedMatchKeys[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		errs = append(errs, fmt.Sprintf(
			"devices.yaml: device '%s' match has unsupported key(s) %s", id, strings.Join(unknown, ", ")))
	}

	var m Match
	for _, k := range difftext.SortedKeys(allowedMatchKeys) {
		v, present := matchMap[k]
		if !present {
			continue
		}
		s, ok := v.(string)
		if !ok || s == "" {
			// An all-digit device_id or a bare number in name parses as an
			// int; the quote hint is the fix in both cases.
			errs = append(errs, fmt.Sprintf(
				"devices.yaml: device '%s' match.%s must be a non-empty string (quote it if YAML reads it as a number)", id, k))
			continue
		}
		switch k {
		case "device_id":
			m.DeviceID = s
		case "name":
			m.Name = s
		case "identifier":
			// Only the shape is checked. Where "domain" ends is unknowable
			// here (see hasIdentifier), and a domain pattern would refuse
			// HomeKit Controller's legacy "accessory-id" outright.
			if !strings.Contains(s, ":") || strings.HasPrefix(s, ":") || strings.HasSuffix(s, ":") {
				errs = append(errs, fmt.Sprintf(
					"devices.yaml: device '%s' match.identifier must be \"domain:value\", e.g. \"esphome:aa:bb:cc:dd:ee:ff\"", id))
				continue
			}
			m.Identifier = s
		}
	}

	if m == (Match{}) && len(errs) == 0 {
		errs = append(errs, fmt.Sprintf(
			"devices.yaml: device '%s' match needs at least one of device_id, name, identifier", id))
	}
	return m, errs
}

func isStringList(v any) bool {
	list, ok := v.([]any)
	if !ok {
		return false
	}
	for _, item := range list {
		if s, ok := item.(string); !ok || s == "" {
			return false
		}
	}
	return true
}

// resolution is what one entry's match picked out of the live registry:
// deviceID when exactly one device matched, otherwise errMsg, with
// candidates holding every device an ambiguous match picked.
type resolution struct {
	deviceID   string
	errMsg     string
	candidates []string
}

// Plan computes the device registry ops reconciling live devices toward
// desired, given state.DeviceOriginals and the ownership rules in the
// package doc comment.
func Plan(
	desired Desired, liveDevices []map[string]any, originals map[string]map[string]any, refs entities.RefResolver,
) []registries.RegOp {
	if originals == nil {
		originals = map[string]map[string]any{}
	}
	liveByID := map[string]map[string]any{}
	for _, obj := range liveDevices {
		if id, ok := obj["id"].(string); ok && id != "" {
			liveByID[id] = obj
		}
	}

	// Every entry is matched before any is planned, so a device two entries
	// both picked is known when the first of them comes up.
	resolved := make([]resolution, len(desired.Devices))
	claimedBy := map[string][]string{}
	for i, d := range desired.Devices {
		ids := matchDevices(d.Match, liveDevices)
		switch len(ids) {
		case 0:
			resolved[i] = resolution{errMsg: "no device matches " + describeMatch(d.Match)}
		case 1:
			resolved[i] = resolution{deviceID: ids[0]}
			claimedBy[ids[0]] = append(claimedBy[ids[0]], d.ID)
		default:
			resolved[i] = resolution{
				errMsg: fmt.Sprintf("%d devices match %s: %s; add an identifier or device_id to the match to pick one",
					len(ids), describeMatch(d.Match), strings.Join(ids, ", ")),
				candidates: ids,
			}
		}
	}

	// protected: devices a field-declaring entry means, errors included -
	// never restored. restoreKey: the manifest id of a no-field entry that
	// resolved to a device, which is restore-eligible like "not declared",
	// and labels that restore. unmatched: field-declaring entries matching
	// no device at all, which hold every restore (see the package doc).
	protected := map[string]bool{}
	restoreKey := map[string]string{}
	var unmatched []string
	var ops []registries.RegOp

	for i, d := range desired.Devices {
		declares := len(d.Fields) > 0
		res := resolved[i]
		if res.errMsg != "" {
			ops = append(ops, errorOp(d.ID, res.errMsg))
			if declares {
				if len(res.candidates) == 0 {
					unmatched = append(unmatched, d.ID)
				}
				for _, id := range res.candidates {
					protected[id] = true
				}
			}
			continue
		}

		deviceID := res.deviceID
		liveObj := liveByID[deviceID]
		if declares {
			protected[deviceID] = true
		}
		if claimants := claimedBy[deviceID]; len(claimants) > 1 {
			ops = append(ops, errorOp(d.ID, fmt.Sprintf(
				"device %s is matched by entries %s; a device can be managed by one entry only",
				describeDevice(deviceID, liveObj), quoteJoin(claimants))))
			continue
		}
		if !declares {
			restoreKey[deviceID] = d.ID
			continue
		}

		if _, sendsDisabled := d.Fields["disabled"]; sendsDisabled {
			if msg := disabledByGuard(liveObj); msg != "" {
				ops = append(ops, errorOp(d.ID, fmt.Sprintf(
					"cannot manage %s: device %s is %s; drop 'disabled' from this entry to manage its other fields",
					d.ID, describeDevice(deviceID, liveObj), msg)))
				continue
			}
		}

		params, err := buildParams(d.Fields, refs)
		if err != nil {
			ops = append(ops, errorOp(d.ID, err.Error()))
			continue
		}

		existingOriginals, hasOriginals := originals["device:"+deviceID]
		firstRecording := !hasOriginals || hasNewField(params, existingOriginals)
		diffText := fieldDiff(deviceID, d.ID, params, liveObj)
		if diffText == "" && !firstRecording {
			continue
		}
		if diffText == "" {
			diffText = adoptedNoChangeText(d.ID, deviceID, liveObj)
		}
		ops = append(ops, registries.RegOp{
			Kind: KindUpdate, RType: "device", Key: d.ID, Params: params, LiveID: deviceID, DiffText: diffText,
		})
	}

	var restoreIDs []string
	for key := range originals {
		deviceID, ok := strings.CutPrefix(key, "device:")
		if !ok || deviceID == "" || protected[deviceID] {
			continue
		}
		restoreIDs = append(restoreIDs, deviceID)
	}
	sort.Strings(restoreIDs)

	for _, deviceID := range restoreIDs {
		opKey := deviceID
		if k, ok := restoreKey[deviceID]; ok {
			opKey = k
		}
		liveObj, exists := liveByID[deviceID]
		if !exists {
			// Rule 5, ahead of the hold. restoreKey cannot name a missing
			// device (only a live one resolves), so opKey is the device id.
			ops = append(ops, registries.RegOp{
				Kind: KindForget, RType: "device", Key: deviceID, Params: map[string]any{}, LiveID: deviceID,
				DiffText: fmt.Sprintf("stop tracking device %s: it no longer exists in Home Assistant; nothing to restore", deviceID),
			})
			continue
		}
		if len(unmatched) > 0 {
			ops = append(ops, errorOp(opKey, heldRestoreText(deviceID, liveObj, unmatched)))
			continue
		}

		restoreParams := sanitizeRestoreParams(deviceID, originals["device:"+deviceID])
		if _, sendsDisabled := restoreParams["disabled_by"]; sendsDisabled {
			if msg := disabledByGuard(liveObj); msg != "" {
				ops = append(ops, errorOp(opKey, fmt.Sprintf(
					"cannot restore device %s: %s; refusing to touch it", describeDevice(deviceID, liveObj), msg)))
				continue
			}
		}

		diffText := fieldDiff(deviceID, opKey, restoreParams, liveObj)
		if diffText == "" {
			diffText = restoreNoChangeText(deviceID, liveObj)
		}
		ops = append(ops, registries.RegOp{
			Kind: KindRestore, RType: "device", Key: opKey, Params: restoreParams, LiveID: deviceID, DiffText: diffText,
		})
	}

	return ops
}

// matchDevices returns the sorted ids of every live device satisfying all
// of m's given keys.
func matchDevices(m Match, liveDevices []map[string]any) []string {
	var ids []string
	for _, obj := range liveDevices {
		id, _ := obj["id"].(string)
		if id == "" {
			continue
		}
		if m.DeviceID != "" && id != m.DeviceID {
			continue
		}
		if m.Name != "" {
			name, _ := obj["name"].(string)
			if !strings.EqualFold(name, m.Name) {
				continue
			}
		}
		if m.Identifier != "" && !hasIdentifier(obj["identifiers"], m.Identifier) {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// hasIdentifier reports whether identifiers (the registry's
// [[domain, value], ...]) holds a pair that joins to want as
// "domain:value". Each live pair is joined rather than want split: both
// halves can carry colons (a MAC address value; HomeKit Controller's
// "homekit_controller:accessory-id" domain), so no split point is right
// for every integration.
func hasIdentifier(identifiers any, want string) bool {
	list, _ := identifiers.([]any)
	for _, item := range list {
		pair, ok := item.([]any)
		if !ok || len(pair) != 2 {
			continue
		}
		d, _ := pair[0].(string)
		v, _ := pair[1].(string)
		if d+":"+v == want {
			return true
		}
	}
	return false
}

// describeMatch renders m's given keys for a plan message, in the order
// the package doc lists them.
func describeMatch(m Match) string {
	var parts []string
	if m.DeviceID != "" {
		parts = append(parts, fmt.Sprintf("device_id %q", m.DeviceID))
	}
	if m.Name != "" {
		parts = append(parts, fmt.Sprintf("name %q", m.Name))
	}
	if m.Identifier != "" {
		parts = append(parts, fmt.Sprintf("identifier %q", m.Identifier))
	}
	return strings.Join(parts, " and ")
}

// describeDevice names a live device for a plan message by what the UI
// shows (name_by_user, else the integration's name) plus its id, which is
// all a device with neither gets.
func describeDevice(deviceID string, liveObj map[string]any) string {
	name, _ := liveObj["name_by_user"].(string)
	if name == "" {
		name, _ = liveObj["name"].(string)
	}
	if name == "" {
		return deviceID
	}
	return fmt.Sprintf("%q (%s)", name, deviceID)
}

func quoteJoin(ids []string) string {
	quoted := make([]string, len(ids))
	for i, id := range ids {
		quoted[i] = "'" + id + "'"
	}
	return strings.Join(quoted, ", ")
}

// hasNewField reports whether params declares a field missing from
// existingOriginals - rule 3's trigger for an update op with no drift, so
// the applier records that field's original value.
func hasNewField(params, existingOriginals map[string]any) bool {
	for field := range params {
		if _, already := existingOriginals[field]; !already {
			return true
		}
	}
	return false
}

// disabledByGuard returns "" if liveObj's disabled_by is unset or "user"
// (safe to touch), otherwise a phrase naming the value refusing it - rule
// 2. Unlike entities' guard it runs only when disabled_by is being sent: a
// device the integration disabled can still be renamed, moved or labelled.
func disabledByGuard(liveObj map[string]any) string {
	v, ok := liveObj["disabled_by"]
	if !ok || v == nil {
		return ""
	}
	s, _ := v.(string)
	if s == "" || s == "user" {
		return ""
	}
	return fmt.Sprintf("disabled by %q, not by a user", s)
}

// restoreFields are the config/device_registry/update params this layer
// ever records an original for. HA's update schema refuses unknown keys,
// so a hand-edited state.json holding anything else would jam that restore
// every cycle; sanitizeRestoreParams drops it instead.
var restoreFields = map[string]bool{"name_by_user": true, "area_id": true, "labels": true, "disabled_by": true}

// sanitizeRestoreParams copies a device's recorded originals into restore
// params, dropping any field restoreFields does not list and a disabled_by
// that is neither null nor "user" - the only two values the update schema
// accepts. regapply clamps what it records for disabled_by, so the second
// case needs a hand-edited state.json too. Dropping a field rather than
// the op still deletes the device's device_originals entry.
func sanitizeRestoreParams(deviceID string, recorded map[string]any) map[string]any {
	out := make(map[string]any, len(recorded))
	for f, v := range recorded {
		if !restoreFields[f] {
			slog.Warn("devices: restore: recorded original is not a device field this layer manages; skipping it",
				"device_id", deviceID, "field", f)
			continue
		}
		if f == "disabled_by" {
			if s, ok := v.(string); ok && s != "" && s != "user" {
				slog.Warn(
					"devices: restore: recorded disabled_by is neither null nor \"user\"; skipping this field rather than sending a value Home Assistant would reject",
					"device_id", deviceID, "value", s)
				continue
			}
		}
		out[f] = v
	}
	return out
}

// buildParams translates an entry's declared fields into
// config/device_registry/update params: name becomes name_by_user, area/
// labels resolve through refs into area_id/labels, disabled becomes
// disabled_by ("user"/null). A null labels clears them like an empty list.
//
// Resolved label ids are deduplicated, first occurrence kept: HA stores a
// device's labels as a set and lists each once, so a repeat - written
// twice, or a manifest id and its live id side by side - would read as
// drift on every cycle.
func buildParams(fields map[string]any, refs entities.RefResolver) (map[string]any, error) {
	params := map[string]any{}

	if v, ok := fields["name"]; ok {
		params["name_by_user"] = v
	}
	if v, ok := fields["area"]; ok {
		if v == nil {
			params["area_id"] = nil
		} else {
			ref, _ := v.(string)
			liveID, err := refs.Resolve("area", ref)
			if err != nil {
				return nil, err
			}
			params["area_id"] = liveID
		}
	}
	if v, ok := fields["labels"]; ok {
		list, _ := v.([]any)
		resolved := make([]any, 0, len(list))
		seen := make(map[string]bool, len(list))
		for _, lv := range list {
			ref, _ := lv.(string)
			liveID, err := refs.Resolve("label", ref)
			if err != nil {
				return nil, err
			}
			if seen[liveID] {
				continue
			}
			seen[liveID] = true
			resolved = append(resolved, liveID)
		}
		params["labels"] = resolved
	}
	if v, ok := fields["disabled"]; ok {
		if b, _ := v.(bool); b {
			params["disabled_by"] = "user"
		} else {
			params["disabled_by"] = nil
		}
	}
	return params, nil
}

func errorOp(key, msg string) registries.RegOp {
	return registries.RegOp{Kind: registries.KindError, RType: "device", Key: key, Params: map[string]any{}, Error: msg}
}

func adoptedNoChangeText(key, deviceID string, liveObj map[string]any) string {
	return fmt.Sprintf("now managing %s as device %s; no field changes needed", key, describeDevice(deviceID, liveObj))
}

func restoreNoChangeText(deviceID string, liveObj map[string]any) string {
	return fmt.Sprintf("restoring original values for device %s; live values already match", describeDevice(deviceID, liveObj))
}

func heldRestoreText(deviceID string, liveObj map[string]any, unmatched []string) string {
	subject, verb, fix := "manifest entry "+quoteJoin(unmatched), "matches", "fix its match or remove it"
	if len(unmatched) > 1 {
		subject, verb, fix = "manifest entries "+quoteJoin(unmatched), "match", "fix their match or remove them"
	}
	return fmt.Sprintf(
		"not restoring device %s yet: %s %s no device right now and may be meant for this one; %s first",
		describeDevice(deviceID, liveObj), subject, verb, fix)
}

// fieldsEqual is drift-detection equality: a []any (only ever "labels")
// compares order-insensitively, since HA stores device labels as a set and
// echoes them back in no particular order; everything else compares as a
// plain scalar.
func fieldsEqual(a, b any) bool {
	aList, aIsList := a.([]any)
	bList, bIsList := b.([]any)
	if aIsList || bIsList {
		if len(aList) != len(bList) {
			return false
		}
		as, bs := stringsOf(aList), stringsOf(bList)
		sort.Strings(as)
		sort.Strings(bs)
		for i := range as {
			if as[i] != bs[i] {
				return false
			}
		}
		return true
	}
	return a == b
}

func stringsOf(list []any) []string {
	out := make([]string, len(list))
	for i, v := range list {
		s, _ := v.(string)
		out[i] = s
	}
	return out
}

// fieldDiff renders params (declared fields, or a restore's recorded
// originals) against liveObj's matching fields as a unified diff, or ""
// when nothing differs. The "from" header carries the live device id, so
// the plan shows which device an entry's match picked.
func fieldDiff(deviceID, key string, params, liveObj map[string]any) string {
	changed := false
	fields := difftext.SortedKeys(params)
	before := make([]string, 0, len(fields))
	after := make([]string, 0, len(fields))
	for _, f := range fields {
		beforeVal := liveObj[f]
		afterVal := params[f]
		if !fieldsEqual(beforeVal, afterVal) {
			changed = true
		}
		before = append(before, fmt.Sprintf("%s: %s\n", f, difftext.ReprValue(beforeVal)))
		after = append(after, fmt.Sprintf("%s: %s\n", f, difftext.ReprValue(afterVal)))
	}
	if !changed {
		return ""
	}
	return difftext.UnifiedDiff(before, after, "live/device/"+deviceID, "manifest/device/"+key)
}

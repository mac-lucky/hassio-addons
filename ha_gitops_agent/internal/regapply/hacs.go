// Package regapply's hacs file is internal/hacs' execution counterpart,
// the way subentries.go is for internal/subentries.
//
// HACS registers its commands only on Core's WebSocket API and exposes no
// REST route, so unlike the flows and subentry layers this one speaks
// nothing but WebSocket. Verified against hacs/integration's own source
// (websocket/repositories.py and repository.py, HACS 2.x):
//
//   - hacs/repositories/list {"categories": ["integration"]} -> one object
//     per repository ("id", "full_name", "installed", "domain", ...). The
//     WHOLE store, thousands of entries, so it is only sent when a
//     full_name search is unavoidable (FetchHacsLive, hacsSession).
//   - hacs/repositories/add {"repository": "owner/name", "category": ...}
//     registers a custom repository without downloading it, and does not
//     return the new id - which is why a list follows.
//   - hacs/repository/info {"repository_id": "<id>"} -> that one
//     repository's object, the same shape a list entry has.
//   - hacs/repository/download {"repository": "<id>", "version": "3.1.0"} -
//     "repository" is the id despite the name; "version" is optional.
//   - get_config -> Core's own, whose "components" names every domain the
//     running instance has SET UP. What a restart reminder clears against.
//
// Both add and download answer {} on FAILURE too, reporting the reason out
// of band as a dispatched event, so every write here is followed by a
// read-back of the repository object - that object is the only evidence.
//
// download is synchronous (it unpacks release archives from GitHub), so it
// runs under hacsDownloadTimeout while every other command keeps the tight
// default, which would otherwise remember a running install as failed.
//
// No stash file: the only live action is a download, whose inverse would be
// an uninstall that internal/hacs refuses to plan (it would take the
// entities and their history), so the Roll Back button skips this layer.
package regapply

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/hacs"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/registries"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/wsclient"
)

// The HACS WebSocket commands this layer sends, plus Core's get_config -
// see the package doc comment for each one's verified shape.
const (
	msgHacsRepositoriesList   = "hacs/repositories/list"
	msgHacsRepositoriesAdd    = "hacs/repositories/add"
	msgHacsRepositoryInfo     = "hacs/repository/info"
	msgHacsRepositoryDownload = "hacs/repository/download"
	msgCoreGetConfig          = "get_config"
)

// hacsDownloadTimeout bounds one hacs/repository/download (see the package
// doc comment). Ten minutes is far past any plausible success on slow
// storage, so a timeout means the download is never coming back.
const hacsDownloadTimeout = 10 * time.Minute

// hacsListTimeout bounds one hacs/repositories/list - the WHOLE store,
// thousands of entries, which HACS serializes on the fly. On a Pi that can
// outlast the tight transport default, and a timeout there is classified
// as transport, so nothing would be remembered and the same listing would
// be retried every cycle.
const hacsListTimeout = time.Minute

// wsCodeUnknownCommand is Core's ERR_UNKNOWN_COMMAND, answered for a
// command type nothing has registered. For a hacs/* command it means one
// thing: HACS is not installed on this box.
const wsCodeUnknownCommand = "unknown_command"

// ErrHacsNotInstalled is what every hacs/* command failing with
// unknown_command is reported as. A sentinel because it is a standing
// misconfiguration that never clears on its own: recon.planHacsLayer skips
// the layer and raises a health flag rather than failing every reconcile.
var ErrHacsNotInstalled = errors.New("hacs is not installed")

// HacsLive is one cycle's reading of the box, as internal/hacs.Plan and
// internal/hacs.PruneRestartPending need it.
type HacsLive struct {
	// Repositories is every "integration" repository this cycle looked at -
	// the whole store, or just the declared items read one by one. Plan
	// treats both the same, since it only asks about a declared full_name.
	Repositories []map[string]any
	// Components is every domain the RUNNING instance has set up
	// (get_config's "components"), nil when no restart reminder stands. A
	// domain HACS calls installed but missing here is downloaded, not loaded.
	Components []string
}

// HacsFetchRequest is what the cycle already knows - the parsed manifest,
// state.HacsManaged and state.HacsRestartPending - handed down so the fetch
// can read as little of the box as that knowledge allows.
type HacsFetchRequest struct {
	Desired        hacs.Desired
	Managed        map[string]string
	RestartPending []string
}

// FetchHacsLive reads what this cycle needs of the box over one dialed
// connection, as cheaply as its own knowledge allows: when every declared
// item has a recorded repository id, each is read alone through
// hacs/repository/info rather than listing the whole store every five
// minutes. The full listing is still fetched when any of these holds:
//
//   - an item has no recorded id yet (findable only by full_name);
//   - a recorded id resolves to a DIFFERENT full_name (the manifest was
//     repointed, and hacs.Plan's rule 5 needs the new id to refuse it);
//   - a recorded id is no longer installed, or does not answer (the listing
//     is what tells adopt from install, and duplicate full_names apart).
//
// Only a transport failure short-circuits that fallback. get_config is read
// only when a restart reminder stands - the sole consumer of Components.
//
// A missing HACS is reported once here as ErrHacsNotInstalled, not per
// item. With nothing declared this asks HACS nothing and cannot tell.
func FetchHacsLive(ctx context.Context, dialer Dialer, req HacsFetchRequest) (HacsLive, error) {
	if dialer == nil {
		return HacsLive{}, errors.New("no websocket dialer was configured for this call")
	}
	ws, err := dialer(ctx)
	if err != nil {
		return HacsLive{}, err
	}
	defer ws.Close()

	repos, err := fetchHacsRepositories(ctx, ws, req)
	if err != nil {
		return HacsLive{}, err
	}

	var components []string
	if len(req.RestartPending) > 0 {
		config, configErr := ws.Cmd(ctx, msgCoreGetConfig, nil)
		if configErr != nil {
			return HacsLive{}, fmt.Errorf("could not read home assistant's loaded components: %w", configErr)
		}
		components = loadedComponents(config)
	}

	return HacsLive{Repositories: repos, Components: components}, nil
}

// fetchHacsRepositories reads the declared repositories the cheapest way
// this cycle's state allows - see FetchHacsLive for the rules.
func fetchHacsRepositories(ctx context.Context, ws WSClient, req HacsFetchRequest) ([]map[string]any, error) {
	if len(req.Desired.Repos) == 0 {
		return nil, nil
	}
	repos, sufficient, err := fetchManagedHacsRepositories(ctx, ws, req)
	if err != nil {
		return nil, err
	}
	if sufficient {
		return repos, nil
	}

	result, err := ws.CmdTimeout(ctx, msgHacsRepositoriesList, hacsListParams(), hacsListTimeout)
	if err != nil {
		return nil, fmt.Errorf("could not read the HACS repository list: %w",
			hacsCommandError(msgHacsRepositoriesList, err))
	}
	return toObjectList(result), nil
}

// fetchManagedHacsRepositories reads one object per declared item through
// hacs/repository/info, off the recorded ids. sufficient false means the
// caller must fetch the full listing after all; an error is returned only
// for a failure the listing could not survive either.
func fetchManagedHacsRepositories(
	ctx context.Context, ws WSClient, req HacsFetchRequest,
) (repos []map[string]any, sufficient bool, err error) {
	out := make([]map[string]any, 0, len(req.Desired.Repos))
	for _, item := range req.Desired.Repos {
		id, _ := item["id"].(string)
		declared, _ := item["repository"].(string)
		repositoryID := req.Managed[hacs.KeyPrefix+id]
		if repositoryID == "" {
			return nil, false, nil
		}

		result, cmdErr := ws.Cmd(ctx, msgHacsRepositoryInfo, hacsInfoParams(repositoryID))
		if cmdErr != nil {
			if isTransportOrTimeoutError(cmdErr) {
				return nil, false, fmt.Errorf("could not read HACS repository %s: %w",
					repositoryID, hacsCommandError(msgHacsRepositoryInfo, cmdErr))
			}
			// HACS answered that it cannot say - a stale id, or no hacs/*
			// commands at all. The listing decides both.
			return nil, false, nil
		}

		repo, _ := result.(map[string]any)
		fullName, _ := repo["full_name"].(string)
		installed, _ := repo["installed"].(bool)
		if !installed || !strings.EqualFold(fullName, declared) {
			return nil, false, nil
		}
		out = append(out, repo)
	}
	return out, true, nil
}

// hacsListParams narrows the store listing to this layer's one supported
// category - the list is the most expensive call here, and other categories
// would only make it bigger.
func hacsListParams() map[string]any {
	return map[string]any{"categories": []any{hacs.CategoryIntegration}}
}

// hacsInfoParams names the one repository to read - the field is
// "repository_id" here and plain "repository" on the download, which is why
// neither is spelled at a call site.
func hacsInfoParams(repositoryID string) map[string]any {
	return map[string]any{"repository_id": repositoryID}
}

// loadedComponents reads get_config's "components" defensively: anything
// not a list of strings contributes nothing rather than failing the cycle,
// since keeping a restart reminder up one cycle too long is the harmless
// direction. Core reports bare domains ("hacs") and platform pairs
// ("sensor.hacs"); only the bare domains are kept.
func loadedComponents(result any) []string {
	config, ok := result.(map[string]any)
	if !ok {
		return nil
	}
	list, ok := config["components"].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		component, _ := item.(string)
		if component == "" || strings.Contains(component, ".") {
			continue
		}
		out = append(out, component)
	}
	return out
}

// hacsCommandError maps unknown_command - the whole command family
// unregistered - onto HACS not being installed. Anything else comes back
// untouched, since every call site already names what it was attempting.
func hacsCommandError(msgType string, err error) error {
	var wsErr *wsclient.Error
	if errors.As(err, &wsErr) && wsErr.Code == wsCodeUnknownCommand {
		return hacsMissingError(msgType)
	}
	return err
}

func hacsMissingError(msgType string) error {
	return fmt.Errorf(
		"%w: home assistant does not know the '%s' command, so HACS does not look installed on this box - "+
			"install HACS first, or turn the reconcile.hacs option off", ErrHacsNotInstalled, msgType)
}

// hacsStateKey namespaces one manifest id inside the shared state maps -
// internal/hacs' own KeyPrefix.
func hacsStateKey(id string) string { return hacs.KeyPrefix + id }

// ApplyHacsPlan executes ops (from hacs.Plan) against HACS over one
// connection dialed from dialer. KindError ops come back in SkippedErrors.
//
// Per-op isolation like ApplySubentryPlan: declared repositories are
// independent, so every op is attempted regardless of what came before and
// failures are joined into Error. RolledBack is always false (no stash).
// Isolation depends on lazyConn redialing after a transport failure - a
// cached dead socket would remember each op as the item's own failure.
//
// managed/attempts/restartPending are state.Hacs*, mutated in place (the
// last through a pointer); the caller persists them. The connection is
// dialed lazily so an adopt-only plan cannot fail on an unreachable box.
func ApplyHacsPlan(
	ctx context.Context, dialer Dialer, ops []registries.RegOp,
	managed map[string]string, attempts map[string]map[string]any, restartPending *[]string,
) RegistryApplyResult {
	return applyLayerPlan(ops, func(executable []registries.RegOp) RegistryApplyResult {
		return applyHacsPlanInner(ctx, dialer, executable, managed, attempts, restartPending)
	})
}

func applyHacsPlanInner(
	ctx context.Context, dialer Dialer, executable []registries.RegOp,
	managed map[string]string, attempts map[string]map[string]any, restartPending *[]string,
) (result RegistryApplyResult) {
	defer recoverToResult(&result, "regapply: apply_hacs_plan failed")

	session := &hacsSession{conn: newLazyConn(dialer)}
	defer session.conn.close()

	var applied, failures []string
	for _, op := range executable {
		if execErr := executeHacsOp(ctx, session, op, managed, attempts, restartPending); execErr != nil {
			failures = append(failures, fmt.Sprintf("%s hacs:%s failed: %v", op.Kind, op.Key, execErr))
			continue
		}
		applied = append(applied, fmt.Sprintf("%s hacs:%s", op.Kind, op.Key))
	}

	if len(failures) > 0 {
		errMsg := strings.Join(failures, "; ")
		slog.Warn("regapply: apply_hacs_plan", "error", errMsg)
		return RegistryApplyResult{OK: false, Applied: applied, Error: errMsg}
	}

	slog.Info("regapply: apply_hacs_plan executed", "applied", len(applied))
	return RegistryApplyResult{OK: true, Applied: applied}
}

// hacsSession is one ApplyHacsPlan call's connection. It asks for the
// expensive store listing in exactly one situation - straight after an add,
// the only way a never-seen repository acquires a readable id. Everything
// else reads one repository through hacs/repository/info.
type hacsSession struct {
	conn *lazyConn
}

// idOf returns the HACS repository id for fullName, or "" when a freshly
// read listing does not carry it. The match is hacs.FindRepository's -
// case-insensitive, preferring an installed duplicate - so the driver
// cannot resolve a manifest entry to a different id than the planner did.
func (s *hacsSession) idOf(ctx context.Context, fullName string) (string, error) {
	result, err := s.conn.cmd(ctx, msgHacsRepositoriesList, hacsListParams(), hacsListTimeout)
	if err != nil {
		return "", fmt.Errorf("could not read the HACS repository list: %w",
			hacsCommandError(msgHacsRepositoriesList, err))
	}
	id, _ := hacs.FindRepository(toObjectList(result), fullName)["id"].(string)
	return id, nil
}

// info reads one repository's own object - the read-back that turns an
// empty download reply into evidence, without listing the whole store.
func (s *hacsSession) info(ctx context.Context, repositoryID string) (map[string]any, error) {
	result, err := s.conn.cmd(ctx, msgHacsRepositoryInfo, hacsInfoParams(repositoryID), 0)
	if err != nil {
		return nil, fmt.Errorf("could not read HACS repository %s back: %w",
			repositoryID, hacsCommandError(msgHacsRepositoryInfo, err))
	}
	repo, _ := result.(map[string]any)
	return repo, nil
}

// executeHacsOp executes a single adopt / install op and updates the state
// it touches. Failure memory, the rule hacs.Plan reads back: a failed
// install records the entry's hash plus a reason into attempts so the next
// plan refuses to re-download, and a success clears the key. An adopt
// records nothing - it sends nothing, so it cannot be what failed.
func executeHacsOp(
	ctx context.Context, session *hacsSession, op registries.RegOp,
	managed map[string]string, attempts map[string]map[string]any, restartPending *[]string,
) error {
	key := hacsStateKey(op.Key)

	// Adopt is bookkeeping only - no WebSocket. The live repository is
	// already installed and left on whatever version it is on.
	if hacs.IsAdopt(op) {
		repositoryID, _ := op.Params["repository_id"].(string)
		if repositoryID == "" {
			return errors.New("refusing to adopt: the planned op carries no HACS repository id")
		}
		managed[key] = repositoryID
		delete(attempts, key)
		return nil
	}

	hash, _ := op.Params["hash"].(string)
	repositoryID, domain, err := downloadHacsRepository(ctx, session, op)
	if err != nil {
		// Remembered only when HACS actually answered: a transport failure
		// says the BOX was unreachable, and recording it would strand the
		// item behind a hash that will never change on its own.
		if !isTransportOrTimeoutError(err) {
			attempts[key] = map[string]any{"hash": hash, "error": err.Error()}
		}
		return err
	}

	delete(attempts, key)
	managed[key] = repositoryID
	if domain != "" {
		// Downloaded, therefore not loaded: custom_components are imported
		// at startup, so this domain stays invisible until a restart sets it
		// up. hacs.PruneRestartPending clears it when it shows up loaded.
		*restartPending = appendRestartPending(*restartPending, domain)
	}
	return nil
}

// downloadHacsRepository adds the repository as a custom one if HACS has
// never heard of it, downloads it, and reads it back to confirm.
//
// The read-back is the point: both writes answer {} either way, so the
// repository's object is the only evidence the integration is on disk and
// the only readable source of its domain. HACS marks it installed
// immediately, so "installed" still false is reported as a failure rather
// than becoming a download repeated every cycle.
func downloadHacsRepository(
	ctx context.Context, session *hacsSession, op registries.RegOp,
) (repositoryID, domain string, err error) {
	repository, _ := op.Params["repository"].(string)
	category, _ := op.Params["category"].(string)
	version, _ := op.Params["version"].(string)
	repositoryID, _ = op.Params["repository_id"].(string)

	if repositoryID == "" {
		if _, addErr := session.conn.cmd(ctx, msgHacsRepositoriesAdd, map[string]any{
			"repository": repository, "category": category,
		}, 0); addErr != nil {
			return "", "", fmt.Errorf("could not add custom repository %s: %w",
				repository, hacsCommandError(msgHacsRepositoriesAdd, addErr))
		}
		// The add replies {} either way, so this listing is what says
		// whether it worked - and the only way to learn the assigned id.
		id, listErr := session.idOf(ctx, repository)
		if listErr != nil {
			return "", "", listErr
		}
		if id == "" {
			return "", "", fmt.Errorf(
				"HACS accepted the custom repository %s but does not list it afterwards; "+
					"check the owner/name and that the repository is public", repository)
		}
		repositoryID = id
	}

	params := map[string]any{"repository": repositoryID}
	if version != "" {
		params["version"] = version
	}
	// The one command in this package with a budget of its own - see
	// hacsDownloadTimeout.
	if _, downloadErr := session.conn.cmd(
		ctx, msgHacsRepositoryDownload, params, hacsDownloadTimeout,
	); downloadErr != nil {
		return "", "", fmt.Errorf("could not download %s: %w",
			repository, hacsCommandError(msgHacsRepositoryDownload, downloadErr))
	}

	installed, infoErr := session.info(ctx, repositoryID)
	if infoErr != nil {
		return "", "", infoErr
	}
	if installed == nil {
		return "", "", fmt.Errorf("HACS reported %s as downloaded but no longer knows the repository", repository)
	}
	if isInstalled, _ := installed["installed"].(bool); !isInstalled {
		return "", "", fmt.Errorf(
			"HACS reported %s as downloaded but does not mark it installed; "+
				"check the HACS panel for what it says about this repository", repository)
	}
	domain, _ = installed["domain"].(string)
	// The plan's hint, only when HACS gave nothing: without a domain there
	// is no restart reminder to raise, and no way to clear one.
	if domain == "" {
		domain, _ = op.Params["domain"].(string)
	}
	return repositoryID, domain, nil
}

// appendRestartPending adds domain to the reminder list, sorted and
// deduplicated because it is persisted in state.json and rendered into a
// polled fragment compared byte for byte. That invariant is
// hacs.PruneRestartPending's, so the whole list goes back through it; nil
// components means nothing is pruned.
func appendRestartPending(pending []string, domain string) []string {
	return hacs.PruneRestartPending(append(append([]string{}, pending...), domain), nil)
}

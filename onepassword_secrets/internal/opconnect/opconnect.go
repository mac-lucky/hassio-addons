// Package opconnect is a minimal client for the 1Password Connect REST API:
// the handful of read-only calls this add-on makes (health, vaults, item
// summaries, one item). Hand-written rather than the Connect Go SDK so the
// module keeps its one dependency, and so every place a response body can
// carry a secret is visible: only Item reads values, and they go straight
// into secret.Value.
package opconnect

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mac-lucky/hassio-addons/onepassword_secrets/internal/httperr"
	"github.com/mac-lucky/hassio-addons/onepassword_secrets/internal/httpx"
	"github.com/mac-lucky/hassio-addons/onepassword_secrets/internal/secret"
)

// RequestTimeout bounds one API call.
const RequestTimeout = 15 * time.Second

// maxBody caps a response read. A vault listing of a few thousand items is
// well under it; anything larger is not a Connect answer.
const maxBody = 16 << 20

// Client talks to one Connect server.
type Client struct {
	// BaseURL is the server root, e.g. "http://127.0.0.1:8080".
	BaseURL string
	// Token is the Connect access token. Sent only in the Authorization
	// header, never logged.
	Token string
	// HTTP is the transport; nil means http.DefaultClient.
	HTTP httpx.Doer
}

// StatusError is a non-2xx answer. Message is Connect's own error text
// (its {"status","message"} envelope), bounded and on one line; Connect
// error bodies carry no item content.
type StatusError struct {
	Code    int
	Message string
}

func (e *StatusError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("connect: HTTP %d", e.Code)
	}
	return fmt.Sprintf("connect: HTTP %d: %s", e.Code, e.Message)
}

// IsUnauthorized reports whether err is Connect refusing the token.
func IsUnauthorized(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && (se.Code == http.StatusUnauthorized || se.Code == http.StatusForbidden)
}

// IsNotFound reports whether err is a 404.
func IsNotFound(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Code == http.StatusNotFound
}

// Dependency is one row of /health's "dependencies".
type Dependency struct {
	Service string `json:"service"`
	Status  string `json:"status"`
	Message string `json:"message"`
}

// Health is GET /health: the server version and the state of what it
// depends on (its database, the sync process, the account data).
type Health struct {
	Name         string       `json:"name"`
	Version      string       `json:"version"`
	Dependencies []Dependency `json:"dependencies"`
}

// Dependency returns the named dependency's status, "" when absent.
func (h Health) Dependency(service string) Dependency {
	for _, d := range h.Dependencies {
		if d.Service == service {
			return d
		}
	}
	return Dependency{}
}

// Synced reports whether the account data is available, which is what
// every vault and item call needs.
func (h Health) Synced() bool {
	return strings.EqualFold(h.Dependency("account_data").Status, "ACTIVE")
}

// Vault is one entry of GET /v1/vaults.
type Vault struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	ContentVersion int    `json:"contentVersion"`
	Items          int    `json:"items"`
}

// ItemSummary is one entry of GET /v1/vaults/{id}/items: no fields.
type ItemSummary struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Category  string    `json:"category"`
	Version   int       `json:"version"`
	Tags      []string  `json:"tags"`
	UpdatedAt time.Time `json:"updatedAt"`
	Vault     struct {
		ID string `json:"id"`
	} `json:"vault"`
}

// Field is one item field. Value is wrapped the moment it is decoded.
type Field struct {
	ID      string
	Type    string
	Purpose string
	Label   string
	Section string // the section's label, "" for none
	Value   secret.Value
}

// Item is GET /v1/vaults/{vault}/items/{item}.
type Item struct {
	ID        string
	Title     string
	Category  string
	Version   int
	VaultID   string
	Tags      []string
	UpdatedAt time.Time
	Fields    []Field
}

// rawItem is the wire shape. Value is a json.RawMessage, not a string, so
// the plain value lives in exactly one transient buffer before New wraps
// it.
type rawItem struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Category  string    `json:"category"`
	Version   int       `json:"version"`
	Tags      []string  `json:"tags"`
	UpdatedAt time.Time `json:"updatedAt"`
	Vault     struct {
		ID string `json:"id"`
	} `json:"vault"`
	Sections []struct {
		ID    string `json:"id"`
		Label string `json:"label"`
	} `json:"sections"`
	Fields []struct {
		ID      string          `json:"id"`
		Type    string          `json:"type"`
		Purpose string          `json:"purpose"`
		Label   string          `json:"label"`
		Value   json.RawMessage `json:"value"`
		Section *struct {
			ID string `json:"id"`
		} `json:"section"`
	} `json:"fields"`
}

// Health calls GET /health, which needs no token.
func (c *Client) Health(ctx context.Context) (Health, error) {
	var h Health
	err := c.get(ctx, "/health", false, &h)
	return h, err
}

// Vaults lists the vaults the token can read.
func (c *Client) Vaults(ctx context.Context) ([]Vault, error) {
	var v []Vault
	err := c.get(ctx, "/v1/vaults", true, &v)
	return v, err
}

// Items lists one vault's item summaries.
func (c *Client) Items(ctx context.Context, vaultID string) ([]ItemSummary, error) {
	var items []ItemSummary
	err := c.get(ctx, "/v1/vaults/"+url.PathEscape(vaultID)+"/items", true, &items)
	return items, err
}

// Item fetches one item with its field values.
func (c *Client) Item(ctx context.Context, vaultID, itemID string) (Item, error) {
	var raw rawItem
	if err := c.get(ctx, "/v1/vaults/"+url.PathEscape(vaultID)+"/items/"+url.PathEscape(itemID), true, &raw); err != nil {
		return Item{}, err
	}
	sections := make(map[string]string, len(raw.Sections))
	for _, s := range raw.Sections {
		sections[s.ID] = s.Label
	}
	item := Item{
		ID:        raw.ID,
		Title:     raw.Title,
		Category:  raw.Category,
		Version:   raw.Version,
		VaultID:   raw.Vault.ID,
		Tags:      raw.Tags,
		UpdatedAt: raw.UpdatedAt,
		Fields:    make([]Field, 0, len(raw.Fields)),
	}
	for _, f := range raw.Fields {
		var value string
		if len(f.Value) > 0 {
			// A value that is not a JSON string (Connect sends strings for
			// every field type) is dropped rather than echoed in an error.
			if json.Unmarshal(f.Value, &value) != nil {
				value = ""
			}
		}
		field := Field{
			ID:      f.ID,
			Type:    f.Type,
			Purpose: f.Purpose,
			Label:   f.Label,
			Value:   secret.New(value),
		}
		if f.Section != nil {
			field.Section = sections[f.Section.ID]
		}
		item.Fields = append(item.Fields, field)
	}
	return item, nil
}

// get performs one GET and decodes the JSON answer into out. Decode errors
// are reported without the body: an item body holds values.
func (c *Client) get(ctx context.Context, path string, auth bool, out any) error {
	reqCtx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, strings.TrimRight(c.BaseURL, "/")+path, nil)
	if err != nil {
		return fmt.Errorf("connect: building request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if auth {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("connect: GET %s: %w", path, scrubURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &StatusError{Code: resp.StatusCode, Message: httperr.Detail(resp)}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return fmt.Errorf("connect: GET %s: reading body: %w", path, err)
	}
	if len(body) > maxBody {
		return fmt.Errorf("connect: GET %s: response over %d bytes", path, maxBody)
	}
	if err := json.Unmarshal(body, out); err != nil {
		var syntax *json.SyntaxError
		var typeErr *json.UnmarshalTypeError
		switch {
		case errors.As(err, &syntax):
			return fmt.Errorf("connect: GET %s: invalid JSON at offset %d", path, syntax.Offset)
		case errors.As(err, &typeErr):
			return fmt.Errorf("connect: GET %s: unexpected JSON type for %s", path, typeErr.Field)
		default:
			return fmt.Errorf("connect: GET %s: invalid JSON", path)
		}
	}
	return nil
}

// scrubURLError drops the URL from a *url.Error: it is only ever the
// configured server plus a path, but a user-supplied connect_url may carry
// credentials in its userinfo.
func scrubURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// TokenExpiry reads the "exp" claim of a Connect token (a JWT) without
// verifying it - the server does that; this is for the expiry warning.
// ok is false for a token with no readable exp claim.
func TokenExpiry(token string) (exp time.Time, ok bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp json.Number `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Exp == "" {
		return time.Time{}, false
	}
	secs, err := claims.Exp.Int64()
	if err != nil || secs <= 0 {
		return time.Time{}, false
	}
	return time.Unix(secs, 0).UTC(), true
}

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
)

// QuBe Sync staff: asking for an act-as grant (POST /api/cli/act-as) and reading the event
// stream (GET /api/admin/events). The other /api/admin routes are plain reads, done with Do.
// Both always carry the session's own token (StaffOnly), never an act-as grant's.

// ActAsRequest asks to act in a customer's organization. Minutes 0 leaves the length to the
// server (60 minutes, or less where the grant's maximum is less); WriteBudget nil leaves it to
// the server too.
type ActAsRequest struct {
	OrganizationID string `json:"organization_id"`
	Reason         string `json:"reason"`
	Minutes        int    `json:"minutes,omitempty"`
	Scope          string `json:"scope,omitempty"`  // sandbox (default) or all
	Access         string `json:"access,omitempty"` // read_only, configure or read_write
	WriteBudget    *int   `json:"write_budget,omitempty"`
}

// Grant is an act-as grant as the server describes it: a CLI session of kind act_as, with its
// organization and who approved it.
type Grant struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	Scope       string `json:"scope"`
	Access      string `json:"access"`
	Reason      string `json:"reason"`
	WriteBudget *int   `json:"write_budget"`
	ExpiresAt   string `json:"expires_at"`
	Actor       string `json:"actor"`
	AgentName   string `json:"agent_name"`
	ParentID    string `json:"parent_id"`
	// Organization is the customer's organization the grant reaches.
	Organization struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"organization"`
	ApprovedBy *struct {
		Email string `json:"email"`
	} `json:"approved_by"`
}

// Approval is the device flow a grant waits on: a person approves UserCode at
// VerificationURIComplete, and DeviceCode redeems the grant on POST /api/cli/token.
type Approval struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// ActAsAnswer is POST /api/cli/act-as's answer: Status "active" (201) with the grant's Token,
// the Grant and the Apps it reaches; or "pending_approval" (202) with the Approval to wait on
// and the Request as the server read it.
type ActAsAnswer struct {
	Status   string    `json:"status"`
	Token    string    `json:"token"`
	Grant    *Grant    `json:"grant"`
	Apps     []App     `json:"apps"`
	Approval *Approval `json:"approval"`
	Request  *struct {
		OrganizationID string `json:"organization_id"`
		Scope          string `json:"scope"`
		Access         string `json:"access"`
		Minutes        int    `json:"minutes"`
		Reason         string `json:"reason"`
		WriteBudget    *int   `json:"write_budget"`
	} `json:"request"`
}

// RequestActAs asks for a grant with the session's own token.
func (c *Client) RequestActAs(r ActAsRequest) (*ActAsAnswer, error) {
	var out struct {
		Data ActAsAnswer `json:"data"`
	}
	if err := c.Do("POST", "/api/cli/act-as", nil, r, &out); err != nil {
		return nil, err
	}
	return &out.Data, nil
}

// EventsPage is one answer from GET /api/admin/events: the events (oldest first), and the
// cursor to pass back as `after`, read from the x-qube-cursor header every answer carries (a
// 204 included) or else from meta.cursor. Status is 200, or 204 when a long poll ended with
// nothing.
type EventsPage struct {
	Events []map[string]interface{}
	Cursor string
	Status int
}

// AdminEvents reads the event stream. With `wait` in q it is a long poll the server holds up
// to 25 s, so it goes without the client's timeout: ctx carries the deadline.
func (c *Client) AdminEvents(ctx context.Context, q url.Values) (*EventsPage, error) {
	var out struct {
		Data []map[string]interface{} `json:"data"`
		Meta struct {
			Cursor json.Number `json:"cursor"`
		} `json:"meta"`
	}
	hc := c.HTTP
	if q.Get("wait") != "" {
		hc = pollHTTP
	}
	status, header, err := c.send(ctx, hc, "GET", "/api/admin/events", q, nil, &out)
	if e, ok := err.(*Error); ok && hc == c.HTTP && c.retryRead(ctx, "GET", e) {
		status, header, err = c.send(ctx, hc, "GET", "/api/admin/events", q, nil, &out)
	}
	if err != nil {
		return nil, err
	}
	page := &EventsPage{Events: out.Data, Status: status, Cursor: header.Get("x-qube-cursor")}
	if page.Cursor == "" {
		page.Cursor = out.Meta.Cursor.String()
	}
	if status == http.StatusNoContent {
		page.Events = nil
	}
	return page, nil
}

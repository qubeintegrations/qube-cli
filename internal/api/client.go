// Package api is a thin HTTP client for QuBe Sync. It sends the session token from `qube login`
// as a bearer token: to the CLI endpoints, and to the v1/v2 API along with the app it acts as
// (X-Qube-App), so the server can hold a read-only session to reads. While the session acts
// in a customer's organization (`qube act-as`), the grant's token goes in its place everywhere
// but the staff routes (StaffOnly).
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

// Version is set by the release build (-X ...api.Version=0.1.0, see .goreleaser.yaml). A
// `go install ...@v0.1.0` build has no ldflags, so it falls back to the module version Go
// records in the binary; a plain `go build` in a checkout stays "0.0.0-dev".
var Version = "0.0.0-dev"

func init() {
	if Version != "0.0.0-dev" {
		return
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		Version = strings.TrimPrefix(info.Main.Version, "v")
	}
}

func userAgent() string { return "qube-cli/" + Version }

const DefaultTimeout = 60 * time.Second

type Client struct {
	Host  string
	Token string // CLI session token (qct_...)
	// ActAsToken is an act-as grant's token (`qube act-as`). While it is set it is sent instead
	// of Token on every route but the staff ones (StaffOnly), so a command acts in the
	// customer's organization.
	ActAsToken string
	// OnActAsRefused is called when the server answers 401 to a request that carried
	// ActAsToken: the grant has ended (expired, revoked, or its session ended). It may rewrite
	// the error's Message.
	OnActAsRefused func(*Error)
	AppID          string // the app /api/v1 and /api/v2 calls act as
	HTTP           *http.Client
	Ctx            context.Context // cancelled on Ctrl-C; every request carries it
}

// Error is a non-2xx answer, with whatever the server said.
type Error struct {
	Status     int
	Code       string
	Message    string
	Body       string
	RetryAfter int  // seconds, from a 429's Retry-After header (0 if none)
	ActAs      bool // the request carried an act-as grant's token
}

func (e *Error) Error() string {
	switch {
	case e.Message != "" && e.Code != "":
		return fmt.Sprintf("HTTP %d %s: %s", e.Status, e.Code, e.Message)
	case e.Message != "":
		return fmt.Sprintf("HTTP %d: %s", e.Status, e.Message)
	}
	return fmt.Sprintf("HTTP %d: %s", e.Status, strings.TrimSpace(e.Body))
}

// Retryable says whether a request that failed this way may succeed if simply repeated:
// a network error, a 5xx, or a 429.
func Retryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Status >= 500 || e.Status == 429
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	var ue *url.Error
	return errors.As(err, &ue)
}

func New(host string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &Client{Host: host, HTTP: &http.Client{Timeout: timeout}, Ctx: context.Background()}
}

// The two endpoints of the device flow are the only /api/cli calls made before a login.
func isDeviceFlow(path string) bool {
	return path == "/api/cli/device" || path == "/api/cli/token"
}

// StaffOnly says whether path always carries the session's own token, never an act-as
// grant's: the staff API (/api/admin) and asking for a grant (/api/cli/act-as), which the
// server refuses from a grant.
func StaffOnly(path string) bool {
	return path == "/api/admin" || strings.HasPrefix(path, "/api/admin/") ||
		path == "/api/cli/act-as" || strings.HasPrefix(path, "/api/cli/act-as/")
}

// tokenFor is the token a request to path carries: the act-as grant's while there is one,
// except on the staff routes.
func (c *Client) tokenFor(path string) (token string, actAs bool) {
	if c.ActAsToken != "" && !StaffOnly(path) {
		return c.ActAsToken, true
	}
	return c.Token, false
}

func (c *Client) newRequest(method, path string, query url.Values, body io.Reader) (*http.Request, error) {
	return c.newRequestWithContext(c.Ctx, method, path, query, body)
}

func (c *Client) newRequestWithContext(ctx context.Context, method, path string, query url.Values, body io.Reader) (*http.Request, error) {
	u := c.Host + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent())
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// The session token goes only to the host that issued it (a Client is made per host, from
	// the session stored for that host), and only to /api/cli, /api/admin and, with the app it
	// acts as, to /api/v1 and /api/v2. The OpenAPI document is public and goes without it.
	token, _ := c.tokenFor(path)
	switch {
	case isDeviceFlow(path) || path == "/api/v2/openapi.json":
	case strings.HasPrefix(path, "/api/cli") || strings.HasPrefix(path, "/api/admin"):
		if token == "" {
			return nil, errors.New("not logged in: run `qube login`")
		}
		req.Header.Set("Authorization", "Bearer "+token)
	case strings.HasPrefix(path, "/api/v1/") || strings.HasPrefix(path, "/api/v2/"):
		if token == "" {
			return nil, errors.New("not logged in: run `qube login`")
		}
		if c.AppID == "" {
			return nil, errors.New("no app to act as: run `qube use <app>` or pass --app")
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Qube-App", c.AppID)
	}
	return req, nil
}

// carriesActAs says whether a request to path carries the act-as grant's token.
func (c *Client) carriesActAs(path string) bool {
	if isDeviceFlow(path) || path == "/api/v2/openapi.json" {
		return false
	}
	_, actAs := c.tokenFor(path)
	return actAs
}

// Do performs a request. body may be nil, a []byte of JSON, or any value to marshal.
// out (if non-nil) receives the decoded JSON body. A read refused with 429 and a Retry-After
// (a staff session's rate limit) is sent once more after that wait.
func (c *Client) Do(method, path string, query url.Values, body interface{}, out interface{}) error {
	_, err := c.do(c.Ctx, c.HTTP, method, path, query, body, out)
	if e, ok := err.(*Error); ok && c.retryRead(c.Ctx, method, e) {
		_, err = c.do(c.Ctx, c.HTTP, method, path, query, body, out)
	}
	return err
}

// MaxReadRetryWait is the longest Do waits on a 429's Retry-After before sending a read again.
var MaxReadRetryWait = 60 * time.Second

// retryAfterUnit is what one Retry-After second waits (shortened in tests).
var retryAfterUnit = time.Second

// retryRead waits out a 429's Retry-After for a read (GET or HEAD), once, and says whether to
// send it again; a write isn't repeated, and Ctrl-C stops the wait.
func (c *Client) retryRead(ctx context.Context, method string, e *Error) bool {
	if e.Status != http.StatusTooManyRequests || e.RetryAfter <= 0 || (method != "GET" && method != "HEAD") {
		return false
	}
	wait := time.Duration(e.RetryAfter) * retryAfterUnit
	if time.Duration(e.RetryAfter)*time.Second > MaxReadRetryWait {
		return false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(wait):
		return true
	}
}

// do is Do with the context and HTTP client given, and no retry; it also returns the answer's
// status (0 when there was none).
func (c *Client) do(ctx context.Context, hc *http.Client, method, path string, query url.Values, body interface{}, out interface{}) (int, error) {
	status, _, err := c.send(ctx, hc, method, path, query, body, out)
	return status, err
}

// send is do that also returns the answer's headers.
func (c *Client) send(ctx context.Context, hc *http.Client, method, path string, query url.Values, body interface{}, out interface{}) (int, http.Header, error) {
	var reader io.Reader
	if body != nil {
		var data []byte
		switch b := body.(type) {
		case []byte:
			data = b
		default:
			var err error
			data, err = json.Marshal(b)
			if err != nil {
				return 0, nil, err
			}
		}
		reader = bytes.NewReader(data)
	}
	req, err := c.newRequestWithContext(ctx, method, path, query, reader)
	if err != nil {
		return 0, nil, err
	}
	res, err := hc.Do(req)
	if err != nil {
		return 0, nil, unwrapCanceled(err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		return 0, res.Header, unwrapCanceled(err)
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return res.StatusCode, res.Header, c.refused(errorFrom(res, data, path), path)
	}
	if out != nil && len(data) > 0 {
		return res.StatusCode, res.Header, json.Unmarshal(data, out)
	}
	return res.StatusCode, res.Header, nil
}

// refused marks an error from a request that carried the act-as grant's token, and on a 401
// (the grant has ended) tells OnActAsRefused.
func (c *Client) refused(e *Error, path string) *Error {
	if !c.carriesActAs(path) {
		return e
	}
	e.ActAs = true
	if e.Status == http.StatusUnauthorized && c.OnActAsRefused != nil {
		c.OnActAsRefused(e)
	}
	return e
}

// Raw is Do without decoding: the response body as-is (for `qube api`).
// A read refused with 429 and a Retry-After is sent once more, as Do does.
func (c *Client) Raw(method, path string, query url.Values, body []byte) ([]byte, int, error) {
	data, status, e, err := c.raw(method, path, query, body)
	if err == nil && e != nil && c.retryRead(c.Ctx, method, e) {
		data, status, _, err = c.raw(method, path, query, body)
	}
	return data, status, err
}

// raw is one Raw attempt; e describes a non-2xx answer.
func (c *Client) raw(method, path string, query url.Values, body []byte) ([]byte, int, *Error, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := c.newRequest(method, path, query, reader)
	if err != nil {
		return nil, 0, nil, err
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return nil, 0, nil, unwrapCanceled(err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		return data, res.StatusCode, nil, unwrapCanceled(err)
	}
	var e *Error
	if res.StatusCode < 200 || res.StatusCode > 299 {
		e = c.refused(errorFrom(res, data, path), path)
	}
	return data, res.StatusCode, e, nil
}

// Ctrl-C shows up wrapped in a *url.Error; callers want context.Canceled.
func unwrapCanceled(err error) error {
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	return err
}

// errorFrom reads the two envelopes the server uses -- {"error": {"code", "message"}} on the
// CLI endpoints and {"errors": {"detail": ...}} or {"errors": {field: [...]}} elsewhere --
// and turns a 401 on the CLI endpoints into the one instruction that fixes it.
func errorFrom(res *http.Response, data []byte, path string) *Error {
	e := &Error{Status: res.StatusCode, Body: string(data)}
	if ra := res.Header.Get("Retry-After"); ra != "" {
		e.RetryAfter, _ = strconv.Atoi(ra)
	}
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Errors json.RawMessage `json:"errors"`
	}
	if json.Unmarshal(data, &env) == nil {
		e.Code, e.Message = env.Error.Code, env.Error.Message
		if e.Message == "" && len(env.Errors) > 0 {
			e.Message = flattenErrors(env.Errors)
			var coded struct {
				Code string `json:"code"`
			}
			if json.Unmarshal(env.Errors, &coded) == nil {
				e.Code = coded.Code
			}
		}
	}
	switch {
	case res.StatusCode == 401 && strings.HasPrefix(path, "/api/admin"):
		e.Message = "this staff session has expired or was revoked; run `qube login --admin` again"
	case res.StatusCode == 401 && strings.HasPrefix(path, "/api/cli"):
		if e.Code == "cli_token_expired" {
			e.Message = "this session has expired; run `qube login` again"
		} else {
			e.Message = "this session is no longer valid (revoked, or for another host); run `qube login` again"
		}
	default:
		explainRefusal(e)
	}
	return e
}

// explainRefusal adds what to do next to the refusals of QuBe Sync's staff features: an act-as
// grant's limits, the staff rate limit, the audit log and the switches that turn staff access
// off. The server's own message is kept; where it is missing, one is supplied.
func explainRefusal(e *Error) {
	add := func(fallback, next string) {
		if e.Message == "" {
			e.Message = fallback
		}
		if next != "" {
			e.Message = strings.TrimRight(e.Message, " ") + " " + next
		}
	}
	switch e.Code {
	case "act_as_access":
		add("This act-as grant doesn't allow that.",
			"(`qube act-as --status` shows the grant; for more, `qube act-as --end`, then `qube act-as ORG --access ...`.)")
	case "write_budget_spent":
		add("This act-as grant has used its write budget: every change in a production app spends one.",
			"(`qube act-as --end`, then `qube act-as ORG ...` for a new grant.)")
	case "rate_limited":
		fallback := "This staff session is over its rate limit (a guard against loops)."
		if e.RetryAfter > 0 {
			fallback += fmt.Sprintf(" Retry in %d s.", e.RetryAfter)
		}
		add(fallback, "")
	case "audit_unavailable":
		add("The staff audit log can't be written, so this request was refused.",
			"Nothing was done; try again shortly.")
	case "permission_required":
		add("This session doesn't hold the staff permission that needs.",
			"")
	case "staff_access_disabled":
		add("Staff sessions are switched off on this server (:cli_staff_access).",
			"A superuser turns them on at /admin/feature-flags.")
	case "act_as_disabled":
		add("Act-as grants are switched off on this server (:cli_act_as).",
			"A superuser turns them on at /admin/feature-flags.")
	case "act_as_no_credentials":
		e.Message = "an act-as grant never reads an app's API key or webhook secret, so `qube env` doesn't work while acting as an organization: " +
			"commands use the API through the grant itself (`qube act-as --end` returns to your own apps)"
	}
}

func flattenErrors(raw json.RawMessage) string {
	var detail struct {
		Detail string `json:"detail"`
	}
	if json.Unmarshal(raw, &detail) == nil && detail.Detail != "" {
		return detail.Detail
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var fields map[string]interface{}
	if json.Unmarshal(raw, &fields) == nil && len(fields) > 0 {
		var parts []string
		for k, v := range fields {
			parts = append(parts, fmt.Sprintf("%s %v", k, v))
		}
		return strings.Join(parts, "; ")
	}
	return strings.TrimSpace(string(raw))
}

// ---- CLI endpoints

type DeviceStart struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// TokenGrant is what POST /api/cli/token answers once a code is approved: a login's session,
// or an act-as grant (Kind "act_as").
type TokenGrant struct {
	Token       string   `json:"token"`
	ExpiresAt   string   `json:"expires_at"`
	Scope       string   `json:"scope"`
	Access      string   `json:"access"` // read_only, read_write, or configure for a grant (empty from servers before access levels)
	Kind        string   `json:"kind"`   // login or act_as (empty from servers before staff sessions)
	Actor       string   `json:"actor"`  // human or agent
	AgentName   string   `json:"agent_name"`
	Permissions []string `json:"permissions"` // the staff permissions it holds now
	Reason      string   `json:"reason"`
	WriteBudget *int     `json:"write_budget"`
	ParentID    string   `json:"parent_id"`
	ApprovedBy  *struct {
		Email string `json:"email"`
	} `json:"approved_by"`
	User struct {
		Email string `json:"email"`
		Name  string `json:"name"`
	} `json:"user"`
	Organization struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"organization"`
}

// The staff permissions `qube login --admin` asks for (every one a CLI session can hold), and
// the most an AI agent's session can hold (`qube login --agent NAME --admin`).
var (
	StaffPermissions = []string{"admin.read", "admin.read_customer_data", "admin.read_audit", "org.act", "org.act_production", "agents.manage"}
	AgentPermissions = []string{"admin.read", "admin.read_customer_data", "org.act", "org.act_production"}
)

// DeviceRequest is what a login asks for. Access is read_only or read_write; Permissions are
// staff permissions (QuBe Sync staff only); AgentName says an AI agent will hold the session.
// The person approving sees each preselected and has the last word.
type DeviceRequest struct {
	ClientName  string
	Access      string
	Permissions []string
	AgentName   string
}

// StartDevice begins a login (POST /api/cli/device). An ordinary login sends only
// client_name and access, as before staff sessions.
func (c *Client) StartDevice(r DeviceRequest) (*DeviceStart, error) {
	body := map[string]interface{}{"client_name": r.ClientName, "access": r.Access}
	if len(r.Permissions) > 0 {
		body["permissions"] = r.Permissions
	}
	if r.AgentName != "" {
		body["actor"] = "agent"
		body["agent_name"] = r.AgentName
	}
	var out DeviceStart
	err := c.Do("POST", "/api/cli/device", nil, body, &out)
	return &out, err
}

// ErrPending is PollToken's answer while the code has not been approved yet.
var ErrPending = errors.New("authorization pending")

// PollToken asks whether the device code has been approved: the grant, ErrPending, a
// terminal *Error (access_denied, expired_token, invalid_grant) or a transport error.
func (c *Client) PollToken(deviceCode string) (*TokenGrant, error) {
	var out TokenGrant
	err := c.Do("POST", "/api/cli/token", nil, map[string]string{"device_code": deviceCode}, &out)
	if err != nil {
		var e *Error
		if errors.As(err, &e) && e.Code == "authorization_pending" {
			return nil, ErrPending
		}
		return nil, err
	}
	return &out, nil
}

type App struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Sandbox    bool   `json:"sandbox"`
	SupportURL string `json:"support_url"`
}

func (c *Client) Apps() ([]App, string, error) {
	var out struct {
		Data  []App  `json:"data"`
		Scope string `json:"scope"`
	}
	err := c.Do("GET", "/api/cli/apps", nil, nil, &out)
	return out.Data, out.Scope, err
}

type Credentials struct {
	App           App    `json:"app"`
	APIKey        string `json:"api_key"`
	WebhookSecret string `json:"webhook_secret"`
	APIBaseURL    string `json:"api_base_url"`
	MCPURL        string `json:"mcp_url"`
}

func (c *Client) Credentials(appID string) (*Credentials, error) {
	var out struct {
		Data Credentials `json:"data"`
	}
	err := c.Do("GET", "/api/cli/apps/"+url.PathEscape(appID)+"/credentials", nil, nil, &out)
	return &out.Data, err
}

// ResolveApp accepts an app id, exact name, or unique name prefix (case-insensitive).
func ResolveApp(apps []App, ref string) (*App, error) {
	if ref == "" {
		if len(apps) == 1 {
			return &apps[0], nil
		}
		if len(apps) == 0 {
			return nil, errors.New("this session can reach no apps (run `qube apps`)")
		}
		return nil, errors.New("several apps: pass --app <name|id> or run `qube use <app>`")
	}
	lower := strings.ToLower(ref)
	var prefix []App
	for i := range apps {
		if apps[i].ID == ref || strings.ToLower(apps[i].Name) == lower {
			return &apps[i], nil
		}
		if strings.HasPrefix(strings.ToLower(apps[i].Name), lower) {
			prefix = append(prefix, apps[i])
		}
	}
	if len(prefix) == 1 {
		return &prefix[0], nil
	}
	if len(prefix) > 1 {
		return nil, fmt.Errorf("%q matches several apps; use the id or the full name", ref)
	}
	return nil, fmt.Errorf("no app matches %q (run `qube apps`)", ref)
}

// Package api is a thin HTTP client for QuBe Sync: the CLI endpoints (bearer session token)
// and the v1/v2 API (basic auth with an app's API key).
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
	"strconv"
	"strings"
	"time"
)

// Version is set by the release build (-X ...api.Version=v0.1.0); see .goreleaser.yaml.
var Version = "0.0.0-dev"

func userAgent() string { return "qube-cli/" + Version }

const DefaultTimeout = 60 * time.Second

type Client struct {
	Host   string
	Token  string // CLI session token (qct_...)
	APIKey string // an app's API key (sk_...), for /api/v1 and /api/v2
	HTTP   *http.Client
	Ctx    context.Context // cancelled on Ctrl-C; every request carries it
}

// Error is a non-2xx answer, with whatever the server said.
type Error struct {
	Status     int
	Code       string
	Message    string
	Body       string
	RetryAfter int // seconds, from a 429's Retry-After header (0 if none)
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

func (c *Client) newRequest(method, path string, query url.Values, body io.Reader) (*http.Request, error) {
	u := c.Host + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	ctx := c.Ctx
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
	// The session token is only ever sent to /api/cli; an app's key only to /api/v1 and /api/v2
	// -- both on the host the credentials came from (see appClient in cmd/qube).
	if strings.HasPrefix(path, "/api/cli") {
		if !isDeviceFlow(path) {
			if c.Token == "" {
				return nil, errors.New("not logged in: run `qube login`")
			}
			req.Header.Set("Authorization", "Bearer "+c.Token)
		}
	} else if c.APIKey != "" {
		req.SetBasicAuth(c.APIKey, "")
	}
	return req, nil
}

// Do performs a request. body may be nil, a []byte of JSON, or any value to marshal.
// out (if non-nil) receives the decoded JSON body.
func (c *Client) Do(method, path string, query url.Values, body interface{}, out interface{}) error {
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
				return err
			}
		}
		reader = bytes.NewReader(data)
	}
	req, err := c.newRequest(method, path, query, reader)
	if err != nil {
		return err
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return unwrapCanceled(err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		return unwrapCanceled(err)
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return errorFrom(res, data, path)
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

// Raw is Do without decoding: the response body as-is (for `qube api`).
func (c *Client) Raw(method, path string, query url.Values, body []byte) ([]byte, int, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := c.newRequest(method, path, query, reader)
	if err != nil {
		return nil, 0, err
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return nil, 0, unwrapCanceled(err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	return data, res.StatusCode, unwrapCanceled(err)
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
		}
	}
	if res.StatusCode == 401 && strings.HasPrefix(path, "/api/cli") {
		if e.Code == "cli_token_expired" {
			e.Message = "this session has expired; run `qube login` again"
		} else {
			e.Message = "this session is no longer valid (revoked, or for another host); run `qube login` again"
		}
	}
	return e
}

func flattenErrors(raw json.RawMessage) string {
	var detail struct {
		Detail string `json:"detail"`
	}
	if json.Unmarshal(raw, &detail) == nil && detail.Detail != "" {
		return detail.Detail
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

type TokenGrant struct {
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"`
	Scope     string `json:"scope"`
	User      struct {
		Email string `json:"email"`
		Name  string `json:"name"`
	} `json:"user"`
	Organization struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"organization"`
}

func (c *Client) StartDevice(clientName string) (*DeviceStart, error) {
	var out DeviceStart
	err := c.Do("POST", "/api/cli/device", nil, map[string]string{"client_name": clientName}, &out)
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

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
)

// `qube listen`'s routes under /api/cli/apps/:app_id/relays. A relay is a QuBe-hosted
// webhook_url for one app, one user and one machine (the device id); QuBe Sync hands each
// delivery to it to the CLI's long poll instead of POSTing it over the internet, and the CLI
// says how the local app answered.

// Relay is a `qube listen` URL. ID is its public id (rl_...).
type Relay struct {
	ID    string `json:"id"`
	URL   string `json:"url"`
	AppID string `json:"app_id"`
}

// ReplacedRelay is the relay a new machine took over when the user already had the most
// the server allows for the app. Its URL no longer works.
type ReplacedRelay struct {
	ID           string `json:"id"`
	URL          string `json:"url"`
	LastPolledAt string `json:"last_polled_at"` // RFC 3339; empty when it was never polled
}

// RelayGrant is the answer to CreateRelay.
type RelayGrant struct {
	Relay    Relay          `json:"relay"`
	Replaced *ReplacedRelay `json:"replaced,omitempty"`
}

// Delivery is one webhook for the relay, as a poll hands it over. Body is the payload exactly
// as QuBe Sync signed it; Path is the webhook_url's path and query after the relay's own
// (often ""); Summary names what it is (kind, ids and event, never record data).
type Delivery struct {
	AttemptID string                 `json:"attempt_id"`
	Path      string                 `json:"path"`
	Headers   map[string]string      `json:"headers"`
	Body      string                 `json:"body"`
	Summary   map[string]interface{} `json:"summary"`
}

// ForwardError says why the local app couldn't be reached. Kind is one of connect, timeout,
// tls and other; Detail (at most 200 characters) is Go's error text.
type ForwardError struct {
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
}

// DeliveryResult is how the local app answered one delivery: its Status, with the first
// 1,000 bytes of its Body and, for a redirect, its Location; or Error when it couldn't be
// reached. It is sent as {"status", "body", "location"?} or {"error": {"kind", "detail"}},
// the two shapes the server accepts.
type DeliveryResult struct {
	Status   int
	Body     string
	Location string
	Error    *ForwardError
}

func (r DeliveryResult) MarshalJSON() ([]byte, error) {
	if r.Error != nil {
		return json.Marshal(map[string]interface{}{"error": r.Error})
	}
	out := map[string]interface{}{"status": r.Status, "body": r.Body}
	if r.Location != "" {
		out["location"] = r.Location
	}
	return json.Marshal(out)
}

// pollHTTP carries the long polls. It has no Timeout, which would cover reading the body
// too and so cut a poll the server holds open; each poll carries a deadline instead. It
// uses the default transport, which asks for gzip and undoes it, since nothing here sets
// Accept-Encoding.
var pollHTTP = &http.Client{}

func relaysPath(appID string) string {
	return "/api/cli/apps/" + url.PathEscape(appID) + "/relays"
}

func relayPath(appID, relayID string) string {
	return relaysPath(appID) + "/" + url.PathEscape(relayID)
}

// CreateRelay gets this machine's relay for the app, creating it if there is none
// (POST /api/cli/apps/:app_id/relays).
func (c *Client) CreateRelay(appID, deviceID string) (*RelayGrant, error) {
	var out RelayGrant
	err := c.Do("POST", relaysPath(appID), nil, map[string]string{"device_id": deviceID}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ForgetRelay deletes a relay (`qube listen --forget`): deliveries to its URL are refused
// from then on, and the machine's next CreateRelay gets a new one.
func (c *Client) ForgetRelay(appID, relayID string) error {
	return c.Do("DELETE", relayPath(appID, relayID), nil, nil, nil)
}

// PollDeliveries waits for the relay's next deliveries, as listening session `session`. The
// server answers as soon as there is work, and with none (a 204: nil, nil) after about 25 s.
// ctx should carry the poll's deadline. Another session taking over the URL is an *Error
// with Code relay_taken_over (409).
func (c *Client) PollDeliveries(ctx context.Context, appID, relayID, session string) ([]Delivery, error) {
	var out struct {
		Deliveries []Delivery `json:"deliveries"`
	}
	status, err := c.do(ctx, pollHTTP, "GET", relayPath(appID, relayID)+"/deliveries", url.Values{"session": {session}}, nil, &out)
	if err != nil || status == http.StatusNoContent {
		return nil, err
	}
	return out.Deliveries, nil
}

// ReportDelivery tells QuBe Sync how the local app answered one delivery. An *Error with Code
// attempt_not_pending (404) means it came too late: QuBe Sync had stopped waiting and
// retries the webhook.
func (c *Client) ReportDelivery(ctx context.Context, appID, relayID, attemptID string, result DeliveryResult) error {
	_, err := c.do(ctx, c.HTTP, "POST", relayPath(appID, relayID)+"/deliveries/"+url.PathEscape(attemptID), nil, result, nil)
	return err
}

// StopListening tells QuBe Sync that session stopped (Ctrl-C), so deliveries waiting on it
// fail at once instead of at their deadline.
func (c *Client) StopListening(ctx context.Context, appID, relayID, session string) error {
	_, err := c.do(ctx, c.HTTP, "DELETE", relayPath(appID, relayID)+"/listen", url.Values{"session": {session}}, nil, nil)
	return err
}

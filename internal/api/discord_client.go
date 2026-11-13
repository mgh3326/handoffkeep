package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// The Discord client the decision notices, the task threads and the bot
// share: one webhook or bot request with mentions disabled, a timeout, and
// errors that never quote the webhook URL or the bot token.

// discordRequestTimeout bounds one request to Discord.
const discordRequestTimeout = 10 * time.Second

// discordContentMaxRunes is Discord's limit on one message.
const discordContentMaxRunes = 2000

// decisionDeadlineZone renders deadlines in the operator's time zone. It is a
// fixed offset so a server image without tzdata renders the same text.
var decisionDeadlineZone = time.FixedZone("KST", 9*60*60)

// so no error it returns may contain the URL.
type DiscordWebhook struct {
	URL  string
	HTTP *http.Client
}

// ParseDiscordWebhookURL accepts only an https URL, so the credential never
// crosses the network in the clear. The message says why without quoting it.
func ParseDiscordWebhookURL(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return "", errors.New("discord webhook url must be an https url")
	}
	return raw, nil
}

// Post sends one message. allowed_mentions is empty so text from a question can
// never ping @everyone.
func (d DiscordWebhook) Post(ctx context.Context, content string) error {
	return d.send(ctx, http.MethodPost, d.URL, content, nil)
}

// discordStatusError is a non-2xx answer from Discord. retryAfter is a
// 429's wait, zero when Discord gave none. api names the caller in the
// message; empty means the webhook.
type discordStatusError struct {
	status     int
	retryAfter time.Duration
	api        string
}

// discordRetryAfterMax bounds a 429's wait, so a malformed header cannot
// stall a worker for hours.
const discordRetryAfterMax = 10 * time.Minute

// discordRetryAfter reads a 429's Retry-After header, in seconds (Discord
// may send a fraction).
func discordRetryAfter(resp *http.Response) time.Duration {
	if resp.StatusCode != http.StatusTooManyRequests {
		return 0
	}
	secs, err := strconv.ParseFloat(strings.TrimSpace(resp.Header.Get("Retry-After")), 64)
	if err != nil || secs <= 0 || math.IsNaN(secs) {
		return 0
	}
	return min(time.Duration(secs*float64(time.Second)), discordRetryAfterMax)
}

func (e discordStatusError) Error() string {
	api := e.api
	if api == "" {
		api = discordWebhookAPI
	}
	return fmt.Sprintf("%s status %d", api, e.status)
}

// discordResponseMaxBytes bounds what is read of a response: a message object
// is a few kilobytes.
const discordResponseMaxBytes = 1 << 20

// discordWebhookAPI names the webhook in errors.
const discordWebhookAPI = "discord webhook"

// send is every call to the webhook: the content with mentions disabled, a
// timeout, and errors that never quote the URL. out, when set, receives the
// JSON response. users, when given, are the only ids the message may ping;
// parse stays empty either way, so no @everyone or role ever pings.
func (d DiscordWebhook) send(ctx context.Context, method, target, content string, out any, users ...string) error {
	payload := map[string]any{"content": content, "allowed_mentions": discordAllowedMentions(users)}
	return discordRequest(ctx, d.HTTP, discordWebhookAPI, method, target, "", payload, out)
}

// discordAllowedMentions pings only users: parse stays empty, so no
// @everyone or role ever pings.
func discordAllowedMentions(users []string) map[string]any {
	allowed := map[string]any{"parse": []string{}}
	if len(users) > 0 {
		allowed["users"] = users
	}
	return allowed
}

// discordRequest is one request to Discord, for the webhook and the bot alike:
// a timeout, a size-capped JSON answer into out (when set), a non-2xx as
// discordStatusError, and errors that quote neither the URL (a webhook URL
// is a credential) nor auth (the bot token). api prefixes the errors.
// payload nil sends no body.
func discordRequest(ctx context.Context, client *http.Client, api, method, target, auth string, payload, out any) error {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	ctx, cancel := context.WithTimeout(ctx, discordRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return errors.New(api + " request could not be built")
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
		req.Header.Set("User-Agent", discordBotUserAgent)
	}
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		// *url.Error quotes the full URL; keep only its cause.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return fmt.Errorf("%s: %w", api, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		status := discordStatusError{status: resp.StatusCode, retryAfter: discordRetryAfter(resp)}
		if api != discordWebhookAPI {
			status.api = api
		}
		return status
	}
	if out != nil {
		if err := json.NewDecoder(io.LimitReader(resp.Body, discordResponseMaxBytes)).Decode(out); err != nil {
			return errors.New(api + " response could not be read")
		}
	}
	return nil
}

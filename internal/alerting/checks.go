package alerting

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/alertmanager/config"
	amcommoncfg "github.com/prometheus/alertmanager/config/common"
	"github.com/prometheus/alertmanager/config/receiver"
	"github.com/prometheus/alertmanager/notify"
	"github.com/prometheus/alertmanager/notify/telegram"
	"github.com/prometheus/alertmanager/types"
	commoncfg "github.com/prometheus/common/config"
	"github.com/prometheus/common/model"
)

// Detection without delivery is not detection. Counting sends (deliveries.go)
// only shows a broken contact once an alert has tried it, and a contact that
// went quiet weeks ago is found when someone misses the alert that mattered.
// So every integration is checked on a schedule in two ways:
//
//   - a silent check every Interval that posts nothing: Telegram is asked
//     whether the bot token is valid and the bot sees the chat (getChat);
//     integrations with an address are reached (DNS, TCP, TLS); email
//     connects to its smarthost. Kinds without such a check say so.
//   - a real message every SendInterval (a week by default), silent on
//     Telegram, and only when nothing else has gone through that integration
//     in that time: an alert that was delivered proves the path just as well.
//
// "Not checked yet" and "cannot be checked" are their own states and are
// never shown as working.

// CheckAlertName is the alertname of the scheduled real message.
const CheckAlertName = "VigilDeliveryCheck"

// Check states.
const (
	CheckOK      = "ok"
	CheckFailing = "failing"
	CheckNone    = "cannot_check" // no check that posts nothing exists for this kind
	CheckPending = "not_checked"  // not checked since the server started
)

// CheckOptions say how often each part of the check runs. Zero turns that
// part off.
type CheckOptions struct {
	Interval     time.Duration // silent check
	SendInterval time.Duration // real message when nothing else got through
}

// DefaultCheckOptions: silent every 15 minutes, a real message once a week.
var DefaultCheckOptions = CheckOptions{Interval: 15 * time.Minute, SendInterval: 7 * 24 * time.Hour}

// CheckResult is the last scheduled check of one integration.
type CheckResult struct {
	State     string    `json:"state"`
	Method    string    `json:"method,omitempty"` // what the silent check does
	LastCheck time.Time `json:"last_check,omitzero"`
	Error     string    `json:"error,omitempty"`
	// LastProof is the last time a real message was accepted by this
	// integration: an alert, a test or the scheduled message. It survives
	// a restart.
	LastProof time.Time `json:"last_proof,omitzero"`
	// NextSend is when the scheduled message goes out unless something
	// else gets through first; zero when real messages are off.
	NextSend  time.Time `json:"next_send,omitzero"`
	SendError string    `json:"send_error,omitempty"` // the last scheduled message failed
}

type checks struct {
	mu      sync.Mutex
	file    string
	results map[string]*CheckResult
	tried   map[string]time.Time // last scheduled message, sent or not
	// Persisted: when each integration was last proved, and since when it
	// is known (the first scheduled message is due a SendInterval after
	// that, so an upgrade or a new contact does not send one at once).
	saved struct {
		Proof map[string]time.Time `json:"proof"`
		Since map[string]time.Time `json:"since"`
	}
}

func (c *checks) load() {
	c.saved.Proof, c.saved.Since = map[string]time.Time{}, map[string]time.Time{}
	if c.file == "" {
		return
	}
	if b, err := os.ReadFile(c.file); err == nil {
		_ = json.Unmarshal(b, &c.saved)
	}
	if c.saved.Proof == nil {
		c.saved.Proof = map[string]time.Time{}
	}
	if c.saved.Since == nil {
		c.saved.Since = map[string]time.Time{}
	}
}

func (c *checks) store() error {
	if c.file == "" {
		return nil
	}
	b, err := json.Marshal(c.saved)
	if err != nil {
		return err
	}
	tmp := c.file + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, c.file)
}

// ChecksOn says the server runs the checks, so Deliveries reports an
// integration not checked yet as such rather than as checks being off.
func (a *Alertmanager) ChecksOn() { a.checksOn.Store(true) }

func (c *checks) proofOf(k string) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.saved.Proof[k]
}

// checkResults returns the scheduled check of every integration in use, by key.
func (a *Alertmanager) checkResults() map[string]CheckResult {
	a.checks.mu.Lock()
	defer a.checks.mu.Unlock()
	out := make(map[string]CheckResult, len(a.checks.results))
	for k, r := range a.checks.results {
		out[k] = *r
	}
	return out
}

// RunChecks checks every integration in use now and sends the scheduled
// real messages that are due. The server calls it on a timer.
func (a *Alertmanager) RunChecks(ctx context.Context, opts CheckOptions) {
	a.ChecksOn()
	rt := a.routing.Load()
	if rt == nil || rt.conf == nil {
		return
	}
	now := time.Now()
	sent := map[string]Delivery{}
	for _, d := range a.Deliveries() {
		sent[Target{d.Receiver, d.Integration, d.Index}.key()] = d
	}

	type job struct {
		t   Target
		rcv *config.Receiver
	}
	var jobs []job
	for ri := range rt.conf.Receivers {
		rcv := &rt.conf.Receivers[ri]
		for _, in := range rt.receivers[rcv.Name] {
			jobs = append(jobs, job{Target{rcv.Name, in.Name(), in.Index()}, rcv})
		}
	}

	c := &a.checks
	c.mu.Lock()
	inUse := map[string]bool{}
	for _, j := range jobs {
		k := j.t.key()
		inUse[k] = true
		if c.results[k] == nil {
			c.results[k] = &CheckResult{State: CheckPending}
		}
		if _, ok := c.saved.Since[k]; !ok {
			c.saved.Since[k] = now
		}
		if d := sent[k]; d.LastOK.After(c.saved.Proof[k]) {
			c.saved.Proof[k] = d.LastOK
		}
	}
	for k := range c.results {
		if !inUse[k] {
			delete(c.results, k)
		}
	}
	for k := range c.saved.Since { // a removed contact starts afresh if it comes back
		if !inUse[k] {
			delete(c.saved.Since, k)
			delete(c.saved.Proof, k)
		}
	}
	c.mu.Unlock()

	for _, j := range jobs {
		if ctx.Err() != nil {
			return
		}
		k := j.t.key()
		if opts.Interval > 0 {
			method, err := a.probe(ctx, j.rcv, j.t)
			c.mu.Lock()
			r := c.results[k]
			r.Method, r.LastCheck = method, time.Now()
			switch {
			case errors.Is(err, errNoProbe):
				r.State, r.Error = CheckNone, ""
			case err != nil:
				r.State, r.Error = CheckFailing, cleanError(err)
			default:
				r.State, r.Error = CheckOK, ""
			}
			c.mu.Unlock()
		}

		c.mu.Lock()
		r := c.results[k]
		r.NextSend = time.Time{}
		due := false
		if opts.SendInterval > 0 {
			from := c.saved.Proof[k]
			if from.IsZero() {
				from = c.saved.Since[k]
			}
			r.NextSend = from.Add(opts.SendInterval)
			if r.SendError != "" {
				if c.saved.Proof[k].After(c.tried[k]) { // something else got through since
					r.SendError = ""
				} else {
					// After a failed scheduled message, try again within the
					// hour rather than in a week, but not at every check: a
					// contact that half works is not flooded.
					r.NextSend = c.tried[k].Add(min(retryGap, opts.SendInterval))
				}
			}
			due = !now.Before(r.NextSend)
		}
		c.mu.Unlock()
		if !due {
			c.mu.Lock()
			c.results[k].LastProof = c.saved.Proof[k]
			c.mu.Unlock()
			continue
		}

		err := a.sendCheck(ctx, j.rcv, j.t)
		a.sent.record(j.t.Receiver, j.t.Integration, j.t.Index, err, time.Now())
		c.mu.Lock()
		r = c.results[k]
		c.tried[k] = time.Now()
		if err != nil {
			r.SendError = cleanError(err)
			r.NextSend = c.tried[k].Add(min(retryGap, opts.SendInterval))
		} else {
			r.SendError = ""
			c.saved.Proof[k] = time.Now()
			r.NextSend = c.saved.Proof[k].Add(opts.SendInterval)
		}
		r.LastProof = c.saved.Proof[k]
		c.mu.Unlock()
	}

	c.mu.Lock()
	err := c.store()
	c.mu.Unlock()
	if err != nil {
		a.log.Warn("saving delivery check times failed", "err", err)
	}
}

var errNoProbe = errors.New("no check without sending exists for this kind")

const (
	probeTimeout = 15 * time.Second
	retryGap     = time.Hour // between scheduled messages after one failed
)

// probe checks one integration without posting anything, and says how.
func (a *Alertmanager) probe(ctx context.Context, rcv *config.Receiver, t Target) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	i := t.Index
	at := func(n int) bool { return i < n }
	switch t.Integration {
	case "telegram":
		if at(len(rcv.TelegramConfigs)) {
			return "asks Telegram whether the bot sees the chat", telegramProbe(ctx, rcv.TelegramConfigs[i].APIUrl.String(),
				string(rcv.TelegramConfigs[i].BotToken), rcv.TelegramConfigs[i].BotTokenFile,
				rcv.TelegramConfigs[i].ChatID, rcv.TelegramConfigs[i].ChatIDFile, rcv.TelegramConfigs[i].HTTPConfig)
		}
	case "email":
		if at(len(rcv.EmailConfigs)) {
			return "connects to the mail server", reach(ctx, "tcp://"+rcv.EmailConfigs[i].Smarthost.String(), nil)
		}
	}
	addr, httpConf, err := address(rcv, t)
	if err != nil {
		return "reaches the address", err
	}
	if addr == "" {
		return "", errNoProbe
	}
	return "reaches the address (DNS, connection, TLS); posts nothing", reach(ctx, addr, httpConf)
}

// address is where an integration with a URL sends to.
func address(rcv *config.Receiver, t Target) (string, *commoncfg.HTTPClientConfig, error) {
	i := t.Index
	var s, file string
	var hc *commoncfg.HTTPClientConfig
	switch t.Integration {
	case "slack":
		if i < len(rcv.SlackConfigs) {
			c := rcv.SlackConfigs[i]
			s, file, hc = secretURL(c.APIURL), c.APIURLFile, c.HTTPConfig
		}
	case "webhook":
		if i < len(rcv.WebhookConfigs) {
			c := rcv.WebhookConfigs[i]
			s, file, hc = string(c.URL), c.URLFile, c.HTTPConfig
		}
	case "discord":
		if i < len(rcv.DiscordConfigs) {
			c := rcv.DiscordConfigs[i]
			s, file, hc = secretURL(c.WebhookURL), c.WebhookURLFile, c.HTTPConfig
		}
	case "msteams":
		if i < len(rcv.MSTeamsConfigs) {
			c := rcv.MSTeamsConfigs[i]
			s, file, hc = secretURL(c.WebhookURL), c.WebhookURLFile, c.HTTPConfig
		}
	case "msteamsv2":
		if i < len(rcv.MSTeamsV2Configs) {
			c := rcv.MSTeamsV2Configs[i]
			s, file, hc = secretURL(c.WebhookURL), c.WebhookURLFile, c.HTTPConfig
		}
	case "mattermost":
		if i < len(rcv.MattermostConfigs) {
			c := rcv.MattermostConfigs[i]
			s, file, hc = secretURL(c.WebhookURL), c.WebhookURLFile, c.HTTPConfig
		}
	}
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", nil, fmt.Errorf("reading the address file: %w", err)
		}
		s = strings.TrimSpace(string(b))
	}
	if s == "" || strings.Contains(s, "{{") { // none, or filled in per alert: no one address to try
		return "", nil, nil
	}
	return s, hc, nil
}

func secretURL(u *amcommoncfg.SecretURL) string {
	if u == nil || u.URL == nil {
		return ""
	}
	return u.URL.String()
}

// reach resolves, connects and (for https) completes a TLS handshake to an
// address, through the integration's proxy when it has one. Nothing is
// sent over the connection.
func reach(ctx context.Context, raw string, hc *commoncfg.HTTPClientConfig) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return errors.New("the address is not a valid URL")
	}
	port := u.Port()
	if port == "" {
		port = map[string]string{"https": "443", "http": "80"}[u.Scheme]
	}
	if port == "" {
		return fmt.Errorf("no port in %s://%s", u.Scheme, u.Hostname())
	}
	if hc != nil && hc.ProxyURL.URL != nil && hc.ProxyURL.URL.Host != "" {
		// The connection goes through a proxy; reaching the proxy is what
		// can be checked without a request.
		u, port = hc.ProxyURL.URL, hc.ProxyURL.URL.Port()
		if port == "" {
			port = map[string]string{"https": "443", "http": "80"}[u.Scheme]
		}
	}
	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(u.Hostname(), port))
	if err != nil {
		return err
	}
	defer conn.Close()
	if u.Scheme != "https" {
		return nil
	}
	tc := &tls.Config{ServerName: u.Hostname()}
	if hc != nil {
		if hc.TLSConfig.InsecureSkipVerify {
			tc.InsecureSkipVerify = true
		}
		if hc.TLSConfig.ServerName != "" {
			tc.ServerName = hc.TLSConfig.ServerName
		}
		if hc.TLSConfig.CA != "" || hc.TLSConfig.CAFile != "" {
			// A private authority: build the full client to get its pool.
			rt, err := commoncfg.NewTLSConfig(&hc.TLSConfig)
			if err != nil {
				return err
			}
			tc = rt
			if tc.ServerName == "" {
				tc.ServerName = u.Hostname()
			}
		}
	}
	tconn := tls.Client(conn, tc)
	_ = conn.SetDeadline(time.Now().Add(probeTimeout))
	return tconn.HandshakeContext(ctx)
}

// telegramProbe asks the Bot API about the chat: a wrong token, a chat the
// bot was removed from or a wrong chat id all fail here, and nothing is
// posted.
func telegramProbe(ctx context.Context, apiURL, token, tokenFile string, chat int64, chatFile string, hc *commoncfg.HTTPClientConfig) error {
	if tokenFile != "" {
		b, err := os.ReadFile(tokenFile)
		if err != nil {
			return fmt.Errorf("reading the bot token file: %w", err)
		}
		token = strings.TrimSpace(string(b))
	}
	if chatFile != "" {
		b, err := os.ReadFile(chatFile)
		if err != nil {
			return fmt.Errorf("reading the chat id file: %w", err)
		}
		if chat, err = strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err != nil {
			return errors.New("the chat id file does not hold a number")
		}
	}
	if apiURL == "" {
		apiURL = "https://api.telegram.org"
	}
	client := http.DefaultClient
	if hc != nil {
		c, err := commoncfg.NewClientFromConfig(*hc, "vigil-check")
		if err != nil {
			return err
		}
		client = c
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(apiURL, "/")+"/bot"+token+"/getChat?chat_id="+strconv.FormatInt(chat, 10), nil)
	if err != nil {
		return errors.New("the Telegram address is not valid")
	}
	resp, err := client.Do(req)
	if err != nil {
		return err // cleanError cuts the token out of the address
	}
	defer resp.Body.Close()
	var body struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err := json.Unmarshal(b, &body); err != nil {
		return fmt.Errorf("Telegram answered %s", resp.Status)
	}
	if !body.OK {
		return fmt.Errorf("Telegram: %s", body.Description)
	}
	return nil
}

// sendCheck sends the scheduled real message through one integration, past
// routing and silences like a test. The integration is built afresh from
// its configuration, so the send is counted once, by the caller; on
// Telegram it is silent (no sound).
func (a *Alertmanager) sendCheck(ctx context.Context, rcv *config.Receiver, t Target) error {
	one := *rcv
	if t.Integration == "telegram" && t.Index < len(rcv.TelegramConfigs) {
		one.TelegramConfigs = append([]*telegram.TelegramConfig(nil), rcv.TelegramConfigs...)
		c := *rcv.TelegramConfigs[t.Index]
		c.DisableNotifications = true
		one.TelegramConfigs[t.Index] = &c
	}
	built, err := receiver.BuildReceiverIntegrations(one, a.routing.Load().tmpl, a.log)
	if err != nil {
		return err
	}
	for _, in := range built {
		if in.Name() == t.Integration && in.Index() == t.Index {
			return notifyOnce(ctx, in, t, checkAlert(t))
		}
	}
	return ErrNoIntegration
}

func checkAlert(t Target) *types.Alert {
	now := time.Now()
	al := &types.Alert{UpdatedAt: now}
	al.Labels = model.LabelSet{"alertname": CheckAlertName, "severity": "info", "receiver": model.LabelValue(t.Receiver)}
	al.Annotations = model.LabelSet{
		"summary": model.LabelValue(fmt.Sprintf("Scheduled delivery check from vigil to %s (%s): this contact works. Nothing is wrong.", t.Receiver, t.Integration)),
	}
	al.StartsAt, al.EndsAt = now, now.Add(5*time.Minute)
	return al
}

// notifyOnce sends one alert through one integration directly, the way a
// test does.
func notifyOnce(ctx context.Context, in notify.Integration, t Target, al *types.Alert) error {
	ctx, cancel := context.WithTimeout(ctx, directTimeout)
	defer cancel()
	now := time.Now()
	ctx = notify.WithReceiverName(ctx, t.Receiver)
	ctx = notify.WithGroupKey(ctx, "vigil-check/"+t.key())
	ctx = notify.WithGroupLabels(ctx, al.Labels)
	ctx = notify.WithNow(ctx, now)
	ctx = notify.WithRepeatInterval(ctx, 0)
	ctx = notify.WithNotificationReason(ctx, notify.ReasonFirstNotification)
	_, err := in.Notify(ctx, al)
	return err
}

func checksFile(dataDir string) string { return filepath.Join(dataDir, "delivery-checks.json") }

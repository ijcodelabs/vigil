# Alerts

Three things make an alert reach someone:

1. a **rule** says what is wrong;
2. **routing** says who is told;
3. a **silence** can hold it back for a while.

Rules are evaluated by the server. Notifications, grouping and silences are
done by Alertmanager, which runs inside the server and takes its usual
configuration.

## The Alerts page

- **Alerts**: what is firing, pending (the condition holds, but not yet for
  as long as the rule asks) or silenced. **Silence** on a row starts a
  silence for exactly that alert.
- **Rules**: every rule with its state and, if its query fails, the error.
- **Silences**: active and ended ones.
- **Contacts & routing** (admins): the Alertmanager configuration.

## Rules

**Alerts → Rules → Edit rule file.** The file is in Prometheus' rule format.
Saving replaces all rules. Every expression is run once against its store
first; if anything is wrong nothing changes and the reason is shown.

A single alert can also be made from a query in
[Explore](explore.md#from-a-query-to-a-dashboard-or-an-alert) with **Add
to → Alert rules…**: a form writes the rule into this file, which is
checked in the same way.

```yaml
groups:
  - name: hosts
    interval: 30s
    rules:
      - alert: DiskAlmostFull
        expr: 100 * (1 - host_filesystem_free_bytes / host_filesystem_size_bytes) > 90
        for: 10m                         # must hold this long before it fires
        labels: {severity: warning}
        annotations:
          summary: '{{ $labels.mountpoint }} on {{ $labels.agent }} is {{ $value | humanize }}% full'
```

Every series the expression returns is one alert, with that series' labels.

### Rules on logs

A group with `type: logs` holds LogsQL. The query must end in `| stats …`;
every row it returns is an alert.

```yaml
  - name: logs
    type: logs
    interval: 1m
    rules:
      - alert: ErrorBurst
        expr: '_time:5m level:error | stats by (agent, app) count() as errors | filter errors:>50'
        annotations:
          summary: '{{ $value }} error lines from {{ $labels.app }} in 5 minutes'
      - alert: NginxSilent                # no lines at all is also a signal
        expr: '_time:10m app:nginx | stats count() as lines | filter lines:0'
        for: 5m
```

### Recording rules

`record:` instead of `alert:` stores a query's result as a new metric. In a
`type: logs` group this turns log counts into metrics you can chart cheaply.

```yaml
      - record: app:log_errors:count5m
        expr: '_time:5m level:error | stats by (app) count() as errors'
```

### What a rule can use

| Field | Meaning |
|---|---|
| `expr` | the query |
| `for` | how long it must hold before firing |
| `keep_firing_for` | keep firing this long after it stops holding |
| `labels` | added to the alert; routing matches on these |
| `annotations` | text for people |

In labels and annotations: `{{ $labels.name }}`, `{{ $value }}`, and the
functions `humanize` (1234567 → 1.235M), `humanize1024`, `humanizeDuration`
(seconds → 2h3m), `humanizePercentage` (0.153 → 15.3%), `toUpper`,
`toLower`.

### Rules that come with vigil

A server with an empty rule file still tells you when something is down.
The group **builtin** always runs:

| Alert | Fires when |
|---|---|
| `HostDown` | an agent has not reported for a minute |
| `ProbeFailing` | a check (HTTP, TCP, TLS, ping) has failed for two minutes |
| `CertificateExpiresSoon` | a checked certificate has less than 14 days left |
| `CertificateExpired` | a checked certificate has expired |
| `AgentDroppingData` | an agent had to drop data it could not send or was not allowed to |

Integrations bring their own rules, shown as `<integration>/<group>`, and
run while a profile uses the integration:

| What is down | Alert | Comes with |
|---|---|---|
| a Docker container | `ContainerDown`: it stopped with an exit code other than 0 and has not come back for two minutes. `ContainerRestarting`: more than three restarts in 15 minutes | Docker |
| a pod | `PodNotReady`, `PodCrashLooping`, `PodStuckPending` | Kubernetes node |
| a node | `NodeNotReady` | Kubernetes cluster |
| a deployment, stateful set or daemon set | `DeploymentReplicasUnavailable`, `StatefulSetNotReady`, `DaemonSetUnavailable` | Kubernetes cluster |

A container that ended with exit code 0 (a migration, a one-off job) does
not alert. Removing a stopped container ends its alert, a few minutes
after the removal. `ContainerDown` needs
agents from the version that introduced it; older ones do not report
stopped containers.

To change a built-in rule, define an alert **of the same name** in the rule
file; yours runs instead. A group named `builtin` in the rule file replaces
all of them, and with `rules: []` switches them off:

```yaml
groups:
  - name: hosts
    rules:
      - alert: HostDown                  # replaces the built-in HostDown
        expr: vigil_agent_up == 0
        for: 5m
        labels: {severity: critical}
```

`GET /api/v1/rules/builtin` returns the built-in rules as they are in the
running version, to copy from.

### Rules in git

The rule file is one document, so keep it in a repository and let a
pipeline upload it:

```bash
curl -fsS -X PUT -H "Authorization: Bearer $VIGIL_ADMIN_TOKEN" \
     --data-binary @rules.yaml https://vigil.example.com/api/v1/rules/config
```

The upload is refused, with the reason, if anything in it is wrong, so a
bad commit fails the pipeline and the running rules stay. The repository
then holds only what differs from the built-in rules, which change with the
version of vigil you run.

`configs/rules.example.yaml` has more rules for hosts, services, logs and
the agents themselves.

## Who is told: contacts and routing

**Alerts → Contacts & routing → Edit.** This is Alertmanager's configuration
format; its documentation applies unchanged.

```yaml
route:
  receiver: team-email                 # default
  group_by: [alertname, agent]
  group_wait: 30s                      # collect related alerts before the first message
  group_interval: 5m
  repeat_interval: 4h                  # remind while still firing
  routes:
    - matchers: [severity="critical"]
      receiver: oncall

receivers:
  - name: team-email
    email_configs:
      - to: ops@example.com
        send_resolved: true
  - name: oncall
    webhook_configs:
      - url: https://hooks.example.com/pager

inhibit_rules:                         # a down host does not also page for what runs on it
  - source_matchers: [alertname="HostDown"]
    target_matchers: [severity="warning"]
    equal: [agent]
```

Email, Slack, webhooks, PagerDuty, Opsgenie, Microsoft Teams, Telegram and
the rest of Alertmanager's receivers work. `configs/alerting.example.yaml`
is a fuller example.

> On a new server the only contact is a placeholder called `none`: alerts
> are evaluated and shown, but nobody is notified until you add a receiver.

Passwords and webhook URLs in this configuration are stored on the server
as written. To keep a secret out of it, use the `_file` form most receivers
have and put the secret in a file only the server can read:

```yaml
receivers:
  - name: telegram
    telegram_configs:
      - bot_token_file: /etc/vigil/telegram-token   # the file holds just the token
        chat_id: -1001234567890
        parse_mode: HTML
        send_resolved: true
        message: |-
          {{ if eq .Status "firing" }}🔴 <b>FIRING</b>{{ else }}🟢 <b>resolved</b>{{ end }}
          {{ range .Alerts }}<b>{{ .Labels.alertname }}</b> [{{ .Labels.severity }}] on {{ .Labels.agent }}
          {{ .Annotations.summary }}
          {{ end }}
```

### Slack

In Slack, create an app with an **incoming webhook** for the channel
(api.slack.com/apps → your app → Incoming Webhooks) and put its URL in a
file on the server. The URL is the whole secret: anyone who has it can post
to the channel.

```yaml
route:
  receiver: slack-ops                  # everything goes to #ops-alerts ...
  group_by: [alertname, agent]
  routes:
    - matchers: [severity="critical"]  # ... and what is critical to #oncall instead
      receiver: slack-oncall

receivers:
  - name: slack-ops
    slack_configs:
      - api_url_file: /etc/vigil/slack-ops-webhook   # the file holds just the URL
        send_resolved: true
        username: vigil
        title_link: '{{ reReplaceAll "/alertmanager$" "" .ExternalURL }}/#/alerts'
        fallback: '{{ template "slack.default.title" . }}'   # the text of the push notification
        text: |-
          {{ range .Alerts }}{{ if eq .Status "resolved" }}:white_check_mark: {{ end }}*{{ .Labels.alertname }}*{{ with .Labels.agent }} on `{{ . }}`{{ end }}: {{ .Annotations.summary }}
          {{ end }}
  - name: slack-oncall
    slack_configs:
      - api_url_file: /etc/vigil/slack-oncall-webhook
        send_resolved: true
        username: vigil
        title_link: '{{ reReplaceAll "/alertmanager$" "" .ExternalURL }}/#/alerts'
        fallback: '{{ template "slack.default.title" . }}'   # the text of the push notification
        text: |-
          <!channel>
          {{ range .Alerts }}{{ if eq .Status "resolved" }}:white_check_mark: {{ end }}*{{ .Labels.alertname }}*{{ with .Labels.agent }} on `{{ . }}`{{ end }}: {{ .Annotations.summary }}
          {{ end }}
```

- Set `title_link`, `fallback` and `text` as above. Left alone, the title
  and the push notification link to Alertmanager's own web page, which
  vigil does not have, and the message has no text, only a title like
  `[FIRING:2] DiskAlmostFull web-01`. The
  link above opens vigil's Alerts page; it relies on `external_url` in the
  server config being the address people use.
- A webhook belongs to one channel, so a second channel is a second
  webhook. `channel:` only has an effect with the old "legacy" webhooks and
  with a bot token.
- With a bot token instead (`app_token_file: /etc/vigil/slack-token` in
  place of `api_url_file`, scope `chat:write`, the bot invited to the
  channel) one token serves every channel, and each receiver names its own
  with `channel: '#ops-alerts'`.
- The bar next to the message is red while firing and green once resolved.
  `<!channel>` notifies everyone in the channel; leave it out where that is
  too loud.

To check a contact without waiting for trouble, hand Alertmanager an alert
yourself; it goes the same way a real one does:

```bash
curl -X POST -H "Authorization: Bearer $VIGIL_ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d '[{"labels":{"alertname":"Test","severity":"info"},"annotations":{"summary":"test"}}]' \
  https://vigil.example.com/alertmanager/api/v2/alerts
```

## Silences

A silence holds back notifications for alerts whose labels match, for a
chosen time, with a reason.

- **Silence** on an alert's row fills in its labels.
- **Silences → New silence**: one matcher per line.

  ```
  alertname=DiskAlmostFull
  agent=~db-.*
  ```

`amtool` works too, since the server exposes Alertmanager's API:

```bash
amtool --alertmanager.url=https://vigil.example.com/alertmanager silence add alertname=HostDown agent=web-01 -c "maintenance" -d 2h
```

(give it the admin token with `--http.config.file`).

### Who hears about a silence

Alertmanager itself says nothing about a silenced alert. vigil adds two
messages, sent to the contacts the alerts are routed to:

- **When a silence is made**: who silenced what, until when, and the
  comment. It goes to the contacts of the alerts the silence covers at that
  moment. If it covers none yet (planned work), it goes to the contacts an
  alert with the silence's own labels would reach. The message is an alert
  named `AlertsSilenced` with severity `info`, so routing and templates
  treat it like any other.
  A message template of your own can give it a heading that is not
  "firing":
  `{{ if eq .GroupLabels.alertname "AlertsSilenced" }}Silenced{{ else }}…{{ end }}`.
- **When an alert ends while silenced**: a "resolved" message with the note
  that it ended while silenced, but only to contacts that had been told it
  was firing (and whose receiver has `send_resolved`). An alert that began
  and ended inside a silence was never announced, so its end is not either.

When a silence ends and the alert still fires, Alertmanager notifies as
usual; nothing extra is sent.

A silence made by someone signed in carries their account name, whatever
the request says. A script using a token may name whom it acts for.

Good to know: what was announced is remembered in the server's memory.
After a restart of the server, alerts that end while silenced do so
quietly. `notify_silences: false` in the server config switches both
messages off.

## Alerts that do not come from a rule

Security detections from agents ([Runtime events](security.md)) are raised
as alerts the moment they arrive, named after the rule that matched, with a
link to the capture if there is one. They clear ten minutes after the last
detection.

## Good to know

- Rules see data about 30 seconds late (see [Explore](explore.md)).
- Pending and firing alerts, silences, and the record of what was already
  notified all survive a server restart.
- Firing and pending alerts are also stored as the metric
  `ALERTS{alertname, alertstate}`, so they can be charted.
- A rule file can only be saved while both stores are reachable, because
  each expression is test-run.

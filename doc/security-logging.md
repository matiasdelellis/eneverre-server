# Security logging (fail2ban / CrowdSec)

Eneverre can write authentication failures to a dedicated log file so an
intrusion-prevention tool can ban abusive IPs (repeated password guessing).

## Enabling

Set a path in `eneverre.ini`:

```ini
[auth]
security_log = /var/log/eneverre/security.log
```

or via environment variable:

```
ENEVERRE_SECURITY_LOG=/var/log/eneverre/security.log
```

The directory must exist and be writable by the eneverre process; it is not
created for you. When the path is unset, events still appear in the main log at
`WARN` level (so `journalctl` captures them) — the dedicated file just makes
tailing trivial for fail2ban/CrowdSec.

Every event is also mirrored to the main log regardless of this setting.

## Log format

One line per event, fields space-separated, quoted where a value may contain
spaces:

```
<RFC3339 timestamp> eneverre <event> ip=<client-ip> user="<username>" path=<path> reason=<reason>
```

Example:

```
2026-07-10T14:23:01-03:00 eneverre authentication_failure ip=203.0.113.5 user="admin" path=/api/login reason=invalid_credentials
```

The client IP honors `X-Forwarded-For` / `X-Real-IP` **only when the request
comes from a trusted proxy**, so the banned address is the real client, not
the proxy — and a direct client cannot spoof the header to get an innocent IP
banned (or evade a ban); untrusted peers are logged by their socket address.
By default only loopback peers are trusted, which covers the same-host Caddy
setup; a proxy on another host must be listed in `[server] trusted_proxies`
(comma-separated IPs or CIDRs, or `none` to trust nobody). See
[`doc/example/README.md`](example/README.md).

### Events currently emitted

| event                    | reason                | when                                                        |
|--------------------------|-----------------------|-------------------------------------------------------------|
| `authentication_failure` | `invalid_credentials` | wrong username/password at `POST /api/login`                |
| `authentication_failure` | `basic_auth_failed`   | a wrong HTTP Basic password on any protected API endpoint   |

Expired Bearer tokens and requests with no credentials are **not** logged: they
are normal (a lapsed browser session) and banning on them would lock out
legitimate users.

## Log rotation

Open once, append forever. Use `copytruncate` so eneverre keeps writing to the
same file descriptor after rotation:

```
/var/log/eneverre/security.log {
    weekly
    rotate 8
    missingok
    notifempty
    copytruncate
}
```

## fail2ban

Filter — `/etc/fail2ban/filter.d/eneverre.conf`:

```ini
[Definition]
failregex = ^.* eneverre authentication_failure ip=<HOST> .*$
ignoreregex =
```

fail2ban auto-detects the leading RFC3339 timestamp. Verify the filter against
the live log before enabling the jail:

```
fail2ban-regex /var/log/eneverre/security.log /etc/fail2ban/filter.d/eneverre.conf
```

Jail — add to `/etc/fail2ban/jail.local`:

```ini
[eneverre]
enabled  = true
filter   = eneverre
logpath  = /var/log/eneverre/security.log
maxretry = 5
findtime = 10m
bantime  = 1h
# If eneverre listens on a non-standard port, set it so the ban covers it:
# port   = 8080
```

Reload: `fail2ban-client reload`. Inspect: `fail2ban-client status eneverre`.

## CrowdSec

Acquisition — `/etc/crowdsec/acquis.d/eneverre.yaml`:

```yaml
filenames:
  - /var/log/eneverre/security.log
labels:
  type: eneverre
```

Parser — `/etc/crowdsec/parsers/s01-parse/eneverre.yaml`:

```yaml
onsuccess: next_stage
filter: "evt.Parsed.program == 'eneverre' || evt.Line.Labels.type == 'eneverre'"
name: eneverre/auth-failure
grok:
  pattern: '%{TIMESTAMP_ISO8601:timestamp} eneverre authentication_failure ip=%{IP:source_ip} user=%{QUOTEDSTRING:username} path=%{NOTSPACE:path} reason=%{NOTSPACE:reason}'
  apply_on: message
statics:
  - meta: log_type
    value: eneverre_auth_failure
  - meta: source_ip
    expression: evt.Parsed.source_ip
```

Scenario — `/etc/crowdsec/scenarios/eneverre-bruteforce.yaml`:

```yaml
type: leaky
name: eneverre/http-bruteforce
description: "Detect eneverre login brute-force"
filter: "evt.Meta.log_type == 'eneverre_auth_failure'"
groupby: "evt.Meta.source_ip"
capacity: 5
leakspeed: "10m"
blackhole: 1m
labels:
  service: eneverre
  type: bruteforce
  remediation: true
```

Reload: `systemctl reload crowdsec`. Confirm parsing with
`cscli explain --file /var/log/eneverre/security.log --type eneverre`.

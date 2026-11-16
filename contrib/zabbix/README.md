# Zabbix media type

A Zabbix webhook (`phonelink.yaml`, built from `phonelink.js`) that sends
alerts through gotify-phonelink: a push to the Gotify user who linked the
phone number, and an SMS if an important problem is not opened in time.
Tested against the script engine API of Zabbix 7.x; the export imports into
Zabbix 7.0 and later.

## Plugin

Add a sender for Zabbix to the plugin configuration, for example:

```yaml
senders:
  - name: zabbix
    token: ...            # at least 16 characters
    failover: 20m         # SMS if a problem push is not opened within 20 minutes
    failover_max: 1h
    sms: true
    sms_min_priority: 8   # only High (8) and Disaster (10) problems by SMS
```

## Zabbix

1. *Alerts → Media types → Import*: `phonelink.yaml`.
2. Open the media type and set the parameters
   - `url`: the send endpoint shown on the plugin page in Gotify
   - `token`: the token of the zabbix sender
   - `failover` (optional): e.g. `5m`, capped at the sender's `failover_max`
   - `http_proxy` (optional)
3. Per user (*Users → Media*): type *Gotify phonelink*, *Send to* either the
   phone number (`+436761234567`, the one the user linked in Gotify) or
   `user:<gotify user name>`.
4. Add the media type to an action's operations as usual.

Severity → Gotify priority: Not classified 2, Information 3, Warning 5,
Average 6, High 8, Disaster 10. Recoveries and updates (acknowledgements) get
priority 3 and never fall back to SMS.

A recipient who did not link the number gets nothing (HTTP 404 in the action
log) unless the sender has `sms_unlinked: true`.

## Development

`phonelink.yaml` is generated: edit `phonelink.js`, then run
`python3 build.py`. `node test.js` runs the script as Zabbix does, with a
mocked HTTP side.

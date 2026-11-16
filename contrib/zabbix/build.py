#!/usr/bin/env python3
"""Writes the Zabbix media type export (phonelink.yaml) from phonelink.js."""
import json
import os

here = os.path.dirname(os.path.abspath(__file__))
script = open(os.path.join(here, "phonelink.js")).read()

params = [
    ("url", "<send endpoint, see the plugin page in Gotify>"),
    ("token", "<token of the zabbix sender in the plugin configuration>"),
    ("send_to", "{ALERT.SENDTO}"),
    ("subject", "{ALERT.SUBJECT}"),
    ("message", "{ALERT.MESSAGE}"),
    ("event_source", "{EVENT.SOURCE}"),
    ("event_value", "{EVENT.VALUE}"),
    ("event_severity", "{EVENT.NSEVERITY}"),
    ("event_update_status", "{EVENT.UPDATE.STATUS}"),
    ("failover", ""),
    ("http_proxy", ""),
]

templates = [
    ("TRIGGERS", "PROBLEM", "Problem: {EVENT.NAME}",
     "{HOST.NAME}: {EVENT.NAME}\nSeverity: {EVENT.SEVERITY}\nStarted: {EVENT.DATE} {EVENT.TIME}\nOperational data: {EVENT.OPDATA}\nEvent ID: {EVENT.ID}"),
    ("TRIGGERS", "RECOVERY", "Resolved in {EVENT.DURATION}: {EVENT.NAME}",
     "{HOST.NAME}: {EVENT.NAME}\nResolved: {EVENT.RECOVERY.DATE} {EVENT.RECOVERY.TIME}\nEvent ID: {EVENT.ID}"),
    ("TRIGGERS", "UPDATE", "Updated problem in {EVENT.AGE}: {EVENT.NAME}",
     "{USER.FULLNAME} {EVENT.UPDATE.ACTION} the problem.\n{EVENT.UPDATE.MESSAGE}\nCurrent status: {EVENT.STATUS}"),
    ("SERVICE", "PROBLEM", "Service \"{SERVICE.NAME}\" problem: {EVENT.NAME}",
     "Service: {SERVICE.NAME}\nSeverity: {EVENT.SEVERITY}\nStarted: {EVENT.DATE} {EVENT.TIME}"),
    ("SERVICE", "RECOVERY", "Service \"{SERVICE.NAME}\" resolved in {EVENT.DURATION}: {EVENT.NAME}",
     "Service: {SERVICE.NAME}\nResolved: {EVENT.RECOVERY.DATE} {EVENT.RECOVERY.TIME}"),
]


def q(s):
    # JSON strings are valid YAML double-quoted scalars
    return json.dumps(s, ensure_ascii=False)


out = ["zabbix_export:", "  version: '7.0'", "  media_types:",
       "    - name: 'Gotify phonelink'", "      type: WEBHOOK", "      parameters:"]
for n, v in params:
    out += ["        - name: " + q(n), "          value: " + q(v)]
out += ["      script: |"]
out += [("        " + l) if l else "" for l in script.rstrip("\n").split("\n")]
out += ["      timeout: 30s",
        "      description: " + q("Alerts to the Gotify user who linked the phone number in the user's media "
                                 "(gotify-phonelink plugin), SMS failover per the plugin's zabbix sender. "
                                 "Send to: +<phone number> or user:<gotify user>. "
                                 "https://github.com/geosone/gotify-phonelink"),
        "      message_templates:"]
for src, mode, subj, msg in templates:
    out += ["        - event_source: " + src, "          operation_mode: " + mode,
            "          subject: " + q(subj), "          message: " + q(msg)]
open(os.path.join(here, "phonelink.yaml"), "w").write("\n".join(out) + "\n")

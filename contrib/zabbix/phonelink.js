// Zabbix webhook for gotify-phonelink: sends alerts to the Gotify user who
// linked the phone number in the user's media ("Send to": a phone number such
// as +436761234567, or "user:<gotify user name>").
var Phonelink = {
    // Zabbix severity (0-5) -> Gotify priority
    priorities: [2, 3, 5, 6, 8, 10],

    params: {},

    setParams: function (params) {
        ['url', 'token', 'send_to', 'message'].forEach(function (k) {
            if (typeof params[k] !== 'string' || params[k].trim() === '' || params[k].indexOf('{ALERT.') === 0 ||
                params[k].indexOf('<') === 0) {
                throw 'Parameter "' + k + '" is missing or was not replaced by Zabbix.';
            }
        });
        Phonelink.params = params;
    },

    isRecovery: function () {
        var p = Phonelink.params;
        return p.event_value === '0';
    },

    priority: function () {
        var p = Phonelink.params;
        if (p.event_source !== undefined && p.event_source !== '0') {
            return 5;
        }
        if (Phonelink.isRecovery() || p.event_update_status === '1') {
            return 3;
        }
        var s = parseInt(p.event_severity, 10);
        if (isNaN(s) || s < 0 || s > 5) {
            return 5;
        }
        return Phonelink.priorities[s];
    },

    body: function () {
        var p = Phonelink.params,
            to = p.send_to.trim(),
            body = {
                title: p.subject || '',
                message: p.message,
                priority: Phonelink.priority()
            };

        if (to.indexOf('user:') === 0) {
            body.user = to.substring(5).trim();
        }
        else {
            body.to = to;
        }
        // recoveries and updates never fall back to SMS
        if (Phonelink.isRecovery() || p.event_update_status === '1') {
            body.failover = 0;
        }
        else if (typeof p.failover === 'string' && p.failover.trim() !== '' && p.failover.indexOf('{') !== 0) {
            body.failover = p.failover.trim();
        }
        return body;
    },

    send: function () {
        var req = new HttpRequest(),
            body = Phonelink.body();

        if (typeof Phonelink.params.http_proxy === 'string' && Phonelink.params.http_proxy.trim() !== '') {
            req.setProxy(Phonelink.params.http_proxy.trim());
        }
        req.addHeader('Content-Type: application/json');
        req.addHeader('Authorization: Bearer ' + Phonelink.params.token);

        Zabbix.log(4, '[ Phonelink Webhook ] Sending to ' + (body.user ? 'user ' + body.user : body.to) +
            ', priority ' + body.priority);
        var resp = req.post(Phonelink.params.url, JSON.stringify(body)),
            status = req.getStatus(),
            data = null;

        try {
            data = JSON.parse(resp);
        }
        catch (e) {
            data = null;
        }
        if (status !== 200) {
            throw 'phonelink answered HTTP ' + status + ((data && data.error) ? ': ' + data.error : '');
        }
        return data;
    }
};

try {
    var params = JSON.parse(value);
    Phonelink.setParams(params);
    var result = Phonelink.send();
    Zabbix.log(4, '[ Phonelink Webhook ] delivered by ' + (result && result.status));
    return 'OK';
}
catch (error) {
    Zabbix.log(3, '[ Phonelink Webhook ] Sending failed: ' + error);
    throw 'Sending failed: ' + error;
}

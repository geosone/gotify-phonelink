// Runs phonelink.js the way Zabbix does (value = JSON of the parameters,
// HttpRequest and Zabbix objects provided), with a mocked HTTP side.
// node contrib/zabbix/test.js
'use strict';
const fs = require('fs');
const path = require('path');
const assert = require('assert');

const script = fs.readFileSync(path.join(__dirname, 'phonelink.js'), 'utf8');

function run(params, answer) {
    const sent = {};
    function HttpRequest() {
        this.headers = [];
        this.status = 0;
    }
    HttpRequest.prototype.addHeader = function (h) { this.headers.push(h); };
    HttpRequest.prototype.setProxy = function (p) { sent.proxy = p; };
    HttpRequest.prototype.post = function (url, body) {
        sent.url = url;
        sent.headers = this.headers;
        sent.body = JSON.parse(body);
        this.status = answer.status;
        return answer.body;
    };
    HttpRequest.prototype.getStatus = function () { return this.status; };
    const Zabbix = { log: function () {} };
    const fn = new Function('value', 'HttpRequest', 'Zabbix', script);
    let result, error;
    try {
        result = fn(JSON.stringify(params), HttpRequest, Zabbix);
    } catch (e) {
        error = String(e);
    }
    return { sent, result, error };
}

const base = {
    url: 'https://gotify.example.org/plugin/1/custom/x/send',
    token: 'zabbix-token',
    send_to: '+436761234567',
    subject: 'Problem: disk full',
    message: 'host1: disk full',
    event_source: '0',
    event_value: '1',
    event_severity: '4',
    event_update_status: '0',
    failover: '',
    http_proxy: ''
};
const ok = { status: 200, body: '{"status":"push","id":"1"}' };

// problem, severity high
let r = run(base, ok);
assert.strictEqual(r.result, 'OK', r.error);
assert.strictEqual(r.sent.body.to, '+436761234567');
assert.strictEqual(r.sent.body.priority, 8);
assert.strictEqual(r.sent.body.failover, undefined);
assert.ok(r.sent.headers.includes('Authorization: Bearer zabbix-token'));

// disaster with a failover override
r = run(Object.assign({}, base, { event_severity: '5', failover: '5m' }), ok);
assert.strictEqual(r.sent.body.priority, 10);
assert.strictEqual(r.sent.body.failover, '5m');

// recovery: low priority, never SMS
r = run(Object.assign({}, base, { event_value: '0', failover: '5m' }), ok);
assert.strictEqual(r.sent.body.priority, 3);
assert.strictEqual(r.sent.body.failover, 0);

// update (acknowledge)
r = run(Object.assign({}, base, { event_update_status: '1' }), ok);
assert.strictEqual(r.sent.body.priority, 3);
assert.strictEqual(r.sent.body.failover, 0);

// gotify user instead of a number
r = run(Object.assign({}, base, { send_to: 'user:geos_one' }), ok);
assert.strictEqual(r.sent.body.user, 'geos_one');
assert.strictEqual(r.sent.body.to, undefined);

// non-trigger event (e.g. discovery): normal priority
r = run(Object.assign({}, base, { event_source: '1', event_severity: '{EVENT.NSEVERITY}' }), ok);
assert.strictEqual(r.sent.body.priority, 5);

// unreplaced failover macro is ignored
r = run(Object.assign({}, base, { failover: '{$PHONELINK.FAILOVER}' }), ok);
assert.strictEqual(r.sent.body.failover, undefined);

// errors
r = run(base, { status: 404, body: '{"error":"the recipient has not linked this phone number"}' });
assert.ok(/HTTP 404: the recipient has not linked/.test(r.error), r.error);
r = run(Object.assign({}, base, { url: '<send endpoint, see the plugin page in Gotify>' }), ok);
assert.ok(/"url" is missing/.test(r.error), r.error);
r = run(Object.assign({}, base, { send_to: '{ALERT.SENDTO}' }), ok);
assert.ok(/"send_to" is missing/.test(r.error), r.error);

console.log('zabbix webhook: all tests passed');

// Exercise the actual form script against a minimal DOM and a fake Supervisor
// response. No browser or third-party packages are needed for payload checks.
const {test} = require('node:test');
const assert = require('node:assert/strict');
const {readFileSync} = require('node:fs');
const {join} = require('node:path');
const {runInNewContext} = require('node:vm');

async function formFixture(view) {
  const elements = new Map();
  class Element {
    constructor() { this.value = ''; this.dataset = {}; this.children = []; this.events = {}; }
    set id(id) { this.nameID = id; elements.set(id, this); }
    get id() { return this.nameID; }
    append(...children) { this.children.push(...children); }
    replaceChildren() { this.children = []; }
    addEventListener(event, fn) { this.events[event] = fn; }
    querySelectorAll(selector) {
      return [...elements.values(), ...elements.get('setup-fields').children].filter(e =>
        selector === '[data-mode]' ? e.dataset.mode : e.type === 'password');
    }
  }
  for (const id of ['setup-form', 'setup-message', 'setup-fields', 'setup-mode', 'reload-settings', 'save-settings']) {
    const element = new Element(); element.id = id;
  }
  const requests = [];
  runInNewContext(readFileSync(join(__dirname, '../../elastic_agent/status/web/setup.js'), 'utf8'), {
    document: {getElementById: id => elements.get(id), createElement: () => new Element(), createTextNode: s => s},
    fetch: async (url, init) => {
      if (!init || !init.method) return {ok: true, json: async () => view};
      requests.push({url, body: JSON.parse(init.body)});
      return {ok: true};
    },
  });
  await new Promise(resolve => setImmediate(resolve));
  return {elements, requests, save: () => elements.get('setup-form').events.submit({preventDefault() {}})};
}

for (const mode of ['standalone', 'fleet']) {
  for (const displayed of ['!secret destination_url', '']) {
    test(`${mode}: unchanged ${displayed || 'protected URL'} is omitted from saves`, async () => {
      const key = mode === 'fleet' ? 'fleet_url' : 'elasticsearch_url';
      const {elements, requests, save} = await formFixture({
        mode, options: {[key]: displayed, tags: []}, configured_urls: {[key]: true},
        configured_secrets: {enrollment_token: true, api_key: true}, revision: 'revision', csrf_token: 'csrf',
      });
      assert.equal(elements.get('setup-mode').value, mode);
      assert.equal(elements.get(`setting-${key}`).type, 'text'); // !secret is not a browser URL.
      elements.get('setting-tags').value = 'changed';
      await save();
      assert.equal(requests[0].url, 'api/config');
      assert.equal(key in requests[0].body.options, false);
      assert.equal((mode === 'fleet' ? 'enrollment_token' : 'api_key') in requests[0].body.options, false);
      assert.deepEqual(requests[0].body.options.tags, ['changed']);
      assert.equal(requests[1].url, 'api/restart');
    });
  }
}

test('explicit replacement and clearing use the entered value', async () => {
  for (const clear of [false, true]) {
    const {elements, requests, save} = await formFixture({mode: 'fleet', options: {fleet_url: '', tags: []},
      configured_urls: {fleet_url: true}, configured_secrets: {enrollment_token: true}});
    elements.get('setting-fleet_url').value = '!secret replacement_url';
    elements.get('clear-fleet_url').checked = clear;
    await save();
    assert.equal(requests[0].body.options.fleet_url, clear ? '' : '!secret replacement_url');
  }
});

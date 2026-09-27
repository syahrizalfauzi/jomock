const $ = (sel) => document.querySelector(sel);

async function api(path, options) {
  const res = await fetch(path, options);
  const text = await res.text();
  const body = text ? JSON.parse(text) : null;
  if (!res.ok) throw new Error((body && body.error) || res.statusText);
  return body;
}

const sendJSON = (path, value, method = 'POST') =>
  api(path, {
    method,
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(value),
  });

const esc = (s) =>
  String(s ?? '').replace(/[&<>"]/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c]));

const statusClass = (s) => (s >= 500 ? 'bad' : s >= 400 ? 'warn' : 'ok');

// localStorage throws in some privacy modes; a lost preference is not worth an error.
const pref = {
  get: (k) => { try { return localStorage.getItem(k); } catch { return null; } },
  set: (k, v) => { try { localStorage.setItem(k, v); } catch { /* ignore */ } },
};

// gRPC reports 0 for success; HTTP reports the status code directly.
const entryStatus = (e) =>
  e.protocol === 'grpc' ? (e.status === 0 ? 'OK' : String(e.status)) : String(e.status || '-');
const entryStatusClass = (e) =>
  e.protocol === 'grpc' ? (e.status === 0 ? 'ok' : 'bad') : statusClass(e.status);

// Status labels live in the form's <option> text, so there is one source of truth.
function optionLabel(selectId, code) {
  const opt = [...$('#' + selectId).options].find((o) => Number(o.value) === Number(code));
  return opt ? opt.textContent : String(code);
}

const stubStatusText = (s) =>
  s.type === 'grpc' ? String(s.response.grpcStatus || 0) : String(s.response.status || 200);
const stubStatusLabel = (s) =>
  s.type === 'grpc'
    ? optionLabel('f-grpc-status', s.response.grpcStatus || 0)
    : optionLabel('f-status-select', s.response.status || 200);
const stubStatusClass = (s) =>
  s.type === 'grpc' ? ((s.response.grpcStatus || 0) === 0 ? 'ok' : 'bad') : statusClass(s.response.status || 200);

let stubs = [];
let entries = [];
let editingId = null;
let warningsHidden = pref.get('jomock.hideWarnings') === '1';

// ---- stub list -------------------------------------------------------------

async function refreshStubs() {
  stubs = await api('/__admin/mappings');
  renderStubs();
}

function renderStubs() {
  const tbody = $('#stub-table tbody');
  tbody.innerHTML = '';
  $('#stub-empty').classList.toggle('hidden', stubs.length > 0);

  stubs.forEach((s, i) => {
    const type = s.type || 'http';
    const tr = document.createElement('tr');
    tr.innerHTML =
      '<td class="muted">' + (i + 1) + '</td>' +
      '<td class="mono muted">' + esc(type) + '</td>' +
      '<td class="mono">' + esc(type === 'grpc' ? '-' : s.request.method || '*') + '</td>' +
      '<td class="mono">' + esc(s.request.pathPattern ? '~ ' + s.request.pathPattern : s.request.path || '*') + '</td>' +
      '<td class="mono ' + stubStatusClass(s) + '" title="' + esc(stubStatusLabel(s)) + '">' +
        esc(stubStatusText(s)) + '</td>' +
      '<td class="mono muted">' + esc(s.id) + '</td>';

    const actions = document.createElement('td');
    actions.className = 'actions';
    actions.append(
      iconButton('up', () => move(s.id, 'up')),
      iconButton('down', () => move(s.id, 'down')),
      iconButton('edit', () => openEditor(s.id)),
      iconButton('del', () => remove(s.id))
    );
    tr.append(actions);
    tbody.append(tr);
  });
}

function iconButton(label, onClick) {
  const b = document.createElement('button');
  b.type = 'button';
  b.className = 'icon';
  b.textContent = label;
  b.addEventListener('click', onClick);
  return b;
}

async function remove(id) {
  if (!confirm('Delete stub ' + id + '?')) return;
  await api('/__admin/mappings/' + encodeURIComponent(id), { method: 'DELETE' });
  if (editingId === id) closeEditor();
  await refreshStubs();
}

async function move(id, direction) {
  await sendJSON('/__admin/mappings/' + encodeURIComponent(id) + '/move', { direction });
  await refreshStubs();
}

// ---- key/value rows --------------------------------------------------------

function kvRow(box, key = '', value = '') {
  const row = document.createElement('div');
  row.className = 'kv';

  const k = document.createElement('input');
  k.className = 'kv-key';
  k.placeholder = 'key';
  k.value = key;

  const v = document.createElement('input');
  v.className = 'kv-val';
  v.placeholder = 'value';
  v.value = value;

  const del = document.createElement('button');
  del.type = 'button';
  del.className = 'icon ghost';
  del.textContent = '-';
  del.addEventListener('click', () => row.remove());

  row.append(k, v, del);
  box.append(row);
}

function fillKV(sel, obj) {
  const box = $(sel);
  box.innerHTML = '';
  Object.entries(obj || {}).forEach(([k, v]) => kvRow(box, k, v));
}

function readKV(sel) {
  const out = {};
  $(sel).querySelectorAll('.kv').forEach((row) => {
    const k = row.querySelector('.kv-key').value.trim();
    if (k) out[k] = row.querySelector('.kv-val').value;
  });
  return out;
}

// ---- form <-> stub ---------------------------------------------------------

const setSelect = (sel, value) => { $(sel).value = value; };
const isGRPC = () => $('#f-type').value === 'grpc';

function bodyMode(body) {
  if (!body) return 'none';
  if (body.equalJson !== undefined) return 'equalJson';
  if (body.contains) return 'contains';
  return 'equal';
}

function bodyText(body) {
  if (!body) return '';
  if (body.equalJson !== undefined) return JSON.stringify(body.equalJson, null, 2);
  return body.contains || body.equal || '';
}

function responseMode(res) {
  if (res.jsonBody !== undefined) return 'json';
  if (res.body) return 'text';
  return 'none';
}

function responseText(res) {
  if (res.jsonBody !== undefined) return JSON.stringify(res.jsonBody, null, 2);
  return res.body || '';
}

function syncBodyVisibility() {
  $('#f-body-wrap').classList.toggle('hidden', $('#f-bodymode').value === 'none');
  $('#f-resp-wrap').classList.toggle('hidden', $('#f-respmode').value === 'none');
}

// The form shows one protocol at a time: a gRPC stub has no HTTP method and its
// own status space, so those fields would only be noise.
function syncTypeVisibility() {
  const grpc = isGRPC();
  $('#f-method-field').classList.toggle('hidden', grpc);
  $('#f-status-field').classList.toggle('hidden', grpc);
  $('#f-grpc-block').classList.toggle('hidden', !grpc);
  $('#f-path-label').textContent = grpc ? 'Method (/package.Service/Method)' : 'Path';
  $('#f-path').placeholder = grpc ? '/package.Service/Method' : '/orders or ^/orders/[0-9]+$';
}

function setHTTPStatus(code) {
  const value = Number(code) || 200;
  const known = [...$('#f-status-select').options].some((o) => Number(o.value) === value);
  setSelect('#f-status-select', known ? String(value) : 'custom');
  $('#f-status').classList.toggle('hidden', $('#f-status-select').value !== 'custom');
  if ($('#f-status-select').value === 'custom') $('#f-status').value = value;
}

function readHTTPStatus() {
  const sel = $('#f-status-select').value;
  return sel === 'custom' ? Number($('#f-status').value) || 200 : Number(sel);
}

function syncCustomStatus() {
  $('#f-status').classList.toggle('hidden', $('#f-status-select').value !== 'custom');
}

// openEditorFromEntry seeds the form from a journal entry, so an unmatched
// request becomes a stub in one click. Only method and path are copied: query
// and headers are per-call values that would make the stub stop matching.
function openEditorFromEntry(e) {
  openEditor(null);
  setSelect('#f-type', e.protocol === 'grpc' ? 'grpc' : 'http');
  syncTypeVisibility();
  setSelect('#f-method', e.method);
  setSelect('#f-pathmode', 'path');
  $('#f-path').value = new URL(e.url, location.origin).pathname;
  $('#editor-msg').textContent = 'generated from ' + e.method + ' ' + e.url;
  $('#editor').scrollIntoView({ block: 'nearest', behavior: 'smooth' });
  $('#f-path').focus();
}

function openEditor(id) {
  editingId = id;
  const s = id == null ? null : stubs.find((x) => x.id === id) ?? null;
  const req = (s && s.request) || {};
  const res = (s && s.response) || {};

  $('#f-id').value = s ? s.id : '(new)';
  setSelect('#f-type', (s && s.type) || 'http');
  setSelect('#f-method', req.method || '');

  const pattern = !!req.pathPattern;
  setSelect('#f-pathmode', pattern ? 'pathPattern' : 'path');
  $('#f-path').value = pattern ? req.pathPattern : req.path || '';

  fillKV('#f-query', req.query);
  fillKV('#f-headers', req.headers);

  setSelect('#f-bodymode', bodyMode(req.body));
  $('#f-body').value = bodyText(req.body);

  setHTTPStatus(res.status || 200);
  setSelect('#f-grpc-status', String(res.grpcStatus || 0));
  $('#f-grpc-message').value = res.grpcMessage || '';
  fillKV('#f-resp-headers', res.headers);

  const mode = responseMode(res);
  setSelect('#f-respmode', mode);
  $('#f-resp').value = responseText(res);
  $('#f-delay').value = res.delayMs || 0;

  syncBodyVisibility();
  syncTypeVisibility();
  $('#editor').classList.remove('hidden');
  $('#editor-msg').textContent = s ? 'editing ' + s.id : 'new stub';
  $('#f-type').focus();
}

function closeEditor() {
  editingId = null;
  $('#editor').classList.add('hidden');
  $('#editor-msg').textContent = '';
}

// readForm throws SyntaxError when a JSON field does not parse.
function readForm() {
  const grpc = isGRPC();

  const request = {};
  if (!grpc) {
    const method = $('#f-method').value;
    if (method) request.method = method;
  }

  const path = $('#f-path').value.trim();
  if (path) request[$('#f-pathmode').value] = path;

  const query = readKV('#f-query');
  if (Object.keys(query).length) request.query = query;
  const headers = readKV('#f-headers');
  if (Object.keys(headers).length) request.headers = headers;

  const bm = $('#f-bodymode').value;
  const rawBody = $('#f-body').value;
  if (bm === 'equalJson') request.body = { equalJson: JSON.parse(rawBody) };
  else if (bm !== 'none' && rawBody !== '') request.body = { [bm]: rawBody };

  const response = {};
  if (grpc) {
    const code = Number($('#f-grpc-status').value) || 0;
    if (code !== 0) response.grpcStatus = code;
    const message = $('#f-grpc-message').value.trim();
    if (message) response.grpcMessage = message;
  } else {
    response.status = readHTTPStatus();
  }

  const respHeaders = readKV('#f-resp-headers');
  if (Object.keys(respHeaders).length) response.headers = respHeaders;

  const rm = $('#f-respmode').value;
  if (rm === 'json') response.jsonBody = JSON.parse($('#f-resp').value);
  else if (rm === 'text') response.body = $('#f-resp').value;

  const delay = Number($('#f-delay').value) || 0;
  if (delay > 0) response.delayMs = delay;

  return { type: $('#f-type').value, request, response };
}

async function saveStub() {
  const msg = $('#editor-msg');
  let stub;
  try {
    stub = readForm();
  } catch (err) {
    msg.textContent = 'invalid JSON: ' + err.message;
    return;
  }
  try {
    if (editingId == null) await sendJSON('/__admin/mappings', stub);
    else await sendJSON('/__admin/mappings/' + encodeURIComponent(editingId), stub, 'PUT');
    closeEditor();
    await refreshStubs();
  } catch (err) {
    msg.textContent = err.message;
  }
}

function formatJSON(sel) {
  const el = $(sel);
  const msg = $('#editor-msg');
  try {
    el.value = JSON.stringify(JSON.parse(el.value), null, 2);
    msg.textContent = 'formatted';
  } catch (err) {
    msg.textContent = 'invalid JSON: ' + err.message;
  }
}

// ---- save / open stubs.json ------------------------------------------------

function saveToFile() {
  const blob = new Blob([JSON.stringify(stubs, null, 2)], { type: 'application/json' });
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = 'stubs.json';
  a.click();
  URL.revokeObjectURL(url);
}

async function openFromFile(file) {
  const parsed = JSON.parse(await file.text());
  const list = Array.isArray(parsed) ? parsed : parsed.stubs;
  if (!Array.isArray(list)) throw new Error('expected a JSON array of stubs');

  if (stubs.length && !confirm('Replace all ' + stubs.length + ' stubs with ' + list.length + ' from ' + file.name + '?')) return;

  stubs = await sendJSON('/__admin/mappings', list, 'PUT');
  closeEditor();
  renderStubs();
}

// ---- protos ----------------------------------------------------------------

function applyWarningsVisibility() {
  const has = $('#proto-warnings').children.length > 0;
  const toggle = $('#proto-warn-toggle');
  toggle.classList.toggle('hidden', !has);
  toggle.textContent = warningsHidden ? 'show warnings' : 'hide warnings';
  $('#proto-warnings').classList.toggle('hidden', warningsHidden);
}

async function refreshSchema() {
  try {
    renderProtoSummary(await api('/__admin/grpc/schema'));
  } catch {
    // gRPC is optional; nothing loaded and no -proto flag is a valid state
  }
}

function renderProtoSummary(s) {
  const list = $('#grpc-methods');
  list.innerHTML = '';
  (s.methods || []).forEach((m) => {
    const opt = document.createElement('option');
    opt.value = m;
    list.append(opt);
  });

  const box = $('#proto-warnings');
  box.innerHTML = '';
  (s.warnings || []).forEach((warn) => {
    const p = document.createElement('p');
    p.textContent = warn;
    box.append(p);
  });
  applyWarningsVisibility();

  if (!s.methods || !s.methods.length) {
    $('#proto-summary').textContent = 'no protos loaded';
    return;
  }
  $('#proto-summary').textContent =
    s.methods.length + ' rpcs / ' + s.services.length + ' services' +
    (s.warnings.length ? ' - ' + s.warnings.length + ' folder(s) failed' : '');
}

async function importProtos(fileList) {
  const files = {};
  for (const file of fileList) {
    if (!file.name.endsWith('.proto')) continue;
    files[file.webkitRelativePath || file.name] = await file.text();
  }
  if (!Object.keys(files).length) {
    $('#proto-summary').textContent = 'no .proto files in that selection';
    return;
  }

  $('#proto-summary').textContent = 'compiling ' + Object.keys(files).length + ' files...';
  try {
    renderProtoSummary(await sendJSON('/__admin/grpc/protos', { files }));
  } catch (err) {
    $('#proto-summary').textContent = err.message;
    $('#proto-warnings').innerHTML = '';
    applyWarningsVisibility();
  }
}

// ---- journal ---------------------------------------------------------------

async function refreshJournal() {
  try {
    entries = await api('/__admin/requests?limit=100');
    renderJournal();
  } catch {
    // admin server busy - try again next tick
  }
}

function renderJournal() {
  const tbody = $('#journal-table tbody');
  tbody.innerHTML = '';
  $('#journal-count').textContent = entries.length + ' shown';

  entries.forEach((e) => {
    const tr = document.createElement('tr');
    tr.innerHTML =
      '<td class="muted">' + new Date(e.time).toLocaleTimeString() + '</td>' +
      '<td class="muted">' + esc(e.protocol || 'http') + '</td>' +
      '<td class="mono">' + esc(e.method) + '</td>' +
      '<td class="mono">' + esc(e.url) + '</td>' +
      '<td class="mono ' + entryStatusClass(e) + '">' + esc(entryStatus(e)) + '</td>' +
      '<td class="mono muted">' + (e.stubId ? esc(e.stubId) : 'unmatched') + '</td>' +
      '<td class="mono muted">' + e.durationMs + '</td>';

    tr.addEventListener('click', () => {
      [...tbody.children].forEach((c) => c.classList.remove('selected'));
      tr.classList.add('selected');
      showEntry(e);
    });

    const actions = document.createElement('td');
    actions.className = 'actions';
    if (!e.stubId) {
      const btn = document.createElement('button');
      btn.type = 'button';
      btn.className = 'icon';
      btn.textContent = 'stub';
      btn.title = 'create a stub from this request';
      btn.addEventListener('click', (ev) => {
        ev.stopPropagation();
        openEditorFromEntry(e);
      });
      actions.append(btn);
    }
    tr.append(actions);
    tbody.append(tr);
  });
}

function showEntry(e) {
  const headers = Object.entries(e.headers || {}).map(([k, v]) => k + ': ' + v).join('\n');
  $('#entry-detail').textContent =
    e.method + ' ' + e.url + '\n' +
    'protocol: ' + (e.protocol || 'http') + '\n' +
    'stub: ' + (e.stubId || '(none)') + '\n' +
    (e.protocol === 'grpc' ? 'grpc-status: ' + e.status : 'status: ' + e.status) +
    '  duration: ' + e.durationMs + 'ms\n\n' +
    headers + (e.body ? '\n\n' + e.body : '');
}

// ---- health + verify -------------------------------------------------------

async function refreshHealth() {
  try {
    const h = await api('/__admin/health');
    $('#health').textContent = h.stubs + ' stubs - ok';
  } catch {
    $('#health').textContent = 'admin unreachable';
  }
}

async function runVerify() {
  const body = { stubId: $('#verify-id').value.trim() };
  const raw = $('#verify-count').value.trim();
  if (raw !== '') body.expectedCount = Number(raw);

  const msg = $('#verify-msg');
  try {
    const r = await sendJSON('/__admin/verify', body);
    msg.textContent = 'matched ' + r.matched + ' -> ' + (r.passed ? 'PASS' : 'FAIL');
    msg.className = 'muted ' + (r.passed ? 'ok' : 'bad');
  } catch (err) {
    msg.textContent = err.message;
    msg.className = 'muted bad';
  }
}

// ---- boot ------------------------------------------------------------------

$('#new-stub').addEventListener('click', () => openEditor(null));
$('#cancel-stub').addEventListener('click', closeEditor);
$('#verify-run').addEventListener('click', runVerify);
$('#save-stubs').addEventListener('click', saveToFile);
$('#open-stubs').addEventListener('click', () => $('#open-file').click());
$('#format-request').addEventListener('click', () => formatJSON('#f-body'));
$('#format-response').addEventListener('click', () => formatJSON('#f-resp'));
$('#f-bodymode').addEventListener('change', syncBodyVisibility);
$('#f-respmode').addEventListener('change', syncBodyVisibility);
$('#f-type').addEventListener('change', syncTypeVisibility);
$('#f-status-select').addEventListener('change', syncCustomStatus);

$('#proto-dir').addEventListener('click', () => $('#proto-dir-input').click());
$('#proto-files').addEventListener('click', () => $('#proto-files-input').click());
$('#proto-warn-toggle').addEventListener('click', () => {
  warningsHidden = !warningsHidden;
  pref.set('jomock.hideWarnings', warningsHidden ? '1' : '0');
  applyWarningsVisibility();
});
['proto-dir-input', 'proto-files-input'].forEach((id) => {
  $('#' + id).addEventListener('change', async (ev) => {
    const picked = [...ev.target.files];
    ev.target.value = '';
    await importProtos(picked);
  });
});

document.querySelectorAll('[data-add]').forEach((btn) => {
  btn.addEventListener('click', () => kvRow($('#' + btn.dataset.add)));
});

$('#open-file').addEventListener('change', async (ev) => {
  const file = ev.target.files[0];
  ev.target.value = '';
  if (!file) return;
  try {
    await openFromFile(file);
  } catch (err) {
    alert('Open failed: ' + err.message);
  }
});

$('#editor').addEventListener('submit', (ev) => { ev.preventDefault(); saveStub(); });

refreshStubs().catch((err) => { $('#health').textContent = err.message; });
refreshHealth();
refreshSchema();
refreshJournal();
setInterval(refreshJournal, 1500);
setInterval(refreshHealth, 5000);

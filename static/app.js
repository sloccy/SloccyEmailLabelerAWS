// ---- Panel toggle ----
function togglePanel(id) {
  document.getElementById(id).classList.toggle('d-none');
}

// ---- Mobile sidebar ----
function toggleSidebar() {
  const sidebar = document.querySelector('.sidebar');
  const backdrop = document.getElementById('sidebar-backdrop');
  sidebar.classList.toggle('open');
  backdrop.classList.toggle('visible');
}
function closeSidebar() {
  const sidebar = document.querySelector('.sidebar');
  const backdrop = document.getElementById('sidebar-backdrop');
  if (sidebar) sidebar.classList.remove('open');
  if (backdrop) backdrop.classList.remove('visible');
}

// ---- Sync account selects ----
function syncAccountSelects() {
  const src = document.getElementById('prompt-filter-account');
  const accountOptions = [...src.querySelectorAll('option')].slice(1).map(o => o.outerHTML).join('');
  const newPrompt = document.getElementById('new-prompt-account');
  if (newPrompt) newPrompt.innerHTML = '<option value="">All accounts (global)</option>' + accountOptions;
}

// ---- Toast (Bootstrap Toast) ----
function toast(msg, type = 'success') {
  const el = document.getElementById('toast');
  const body = document.getElementById('toast-body');
  el.className = 'toast align-items-center border-0 text-bg-' + (type === 'error' ? 'danger' : type === 'warning' ? 'warning' : 'success');
  body.textContent = msg;
  bootstrap.Toast.getOrCreateInstance(el, { delay: 3500 }).show();
}

// ---- Navigation ----
function setActivePage(page, el) {
  document.querySelectorAll('.page').forEach(p => p.classList.remove('active'));
  document.querySelectorAll('[data-page]').forEach(n => n.classList.remove('active'));
  document.getElementById('page-' + page).classList.add('active');
  if (el) el.classList.add('active');
  if (location.hash !== '#' + page) history.replaceState(null, '', '#' + page);
  closeSidebar();
}

function _navToHash() {
  const page = location.hash.replace('#', '') || 'dashboard';
  const nav = document.querySelector(`[data-page="${page}"]`);
  if (nav) setActivePage(page, nav);
}

window.addEventListener('DOMContentLoaded', _navToHash);
window.addEventListener('hashchange', _navToHash);

let _oauthStep2Initial = null;
window.addEventListener('DOMContentLoaded', function() {
  const el = document.getElementById('oauth-step-2-body');
  if (el) _oauthStep2Initial = el.innerHTML;
});

// ---- Export prompts ----
function exportPrompts() {
  const accountId = document.getElementById('prompt-filter-account')?.value || '';
  window.location.href = accountId
    ? `/api/prompts/export?account_id=${encodeURIComponent(accountId)}&name=${encodeURIComponent(accountId)}`
    : `/api/prompts/export?name=all`;
}

// ---- Toggle log download panel ----
function toggleLogDownloadPanel() {
  const p = document.getElementById('log-download-panel');
  p.classList.toggle('d-none');
  if (!p.classList.contains('d-none')) {
    const now = new Date();
    const yesterday = new Date(now - 86400000);
    const toLocal = d => new Date(d - d.getTimezoneOffset() * 60000).toISOString().slice(0, 16);
    document.getElementById('log-dl-end').value = toLocal(now);
    document.getElementById('log-dl-start').value = toLocal(yesterday);
  }
}

// ---- Download logs ----
function downloadLogs() {
  const start = document.getElementById('log-dl-start').value;
  const end = document.getElementById('log-dl-end').value;
  if (!start || !end) { toast('Select a start and end time.', 'error'); return; }
  if (start >= end) { toast('Start must be before end.', 'error'); return; }
  const toUTC = s => new Date(s).toISOString().replace('T', ' ').slice(0, 19);
  window.location.href = `/api/logs/download?start=${encodeURIComponent(toUTC(start))}&end=${encodeURIComponent(toUTC(end))}`;
}

// ---- Drag to reorder (native HTML drag-and-drop) ----
let _dragEl = null;
let _dragPlaceholder = null;

function _initDragReorder() {
  const list = document.getElementById('prompts-list');
  if (!list) return;

  list.querySelectorAll('.drag-handle').forEach(handle => {
    const card = handle.closest('.card[data-id]');
    if (!card) return;
    card.draggable = true;

    card.addEventListener('dragstart', e => {
      _dragEl = card;
      card.classList.add('drag-ghost');
      e.dataTransfer.effectAllowed = 'move';
      // Needed for Firefox
      e.dataTransfer.setData('text/plain', '');
    });

    card.addEventListener('dragend', async () => {
      card.classList.remove('drag-ghost');
      if (_dragPlaceholder && _dragPlaceholder.parentNode) {
        _dragPlaceholder.parentNode.removeChild(_dragPlaceholder);
      }
      _dragPlaceholder = null;
      const dropped = _dragEl;
      _dragEl = null;
      if (!dropped) return;

      const orderedIds = [...list.querySelectorAll('.card[data-id]')].map(c => parseInt(c.dataset.id));
      const resp = await fetch('/api/prompts/reorder', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ ordered_ids: orderedIds }),
      });
      if (!resp.ok) {
        toast('Failed to save order.', 'error');
        const accountId = document.getElementById('prompt-filter-account')?.value || '';
        htmx.ajax('GET', accountId ? `/fragments/prompts?account_id=${accountId}` : '/fragments/prompts', { target: '#prompts-list', swap: 'innerHTML' });
      }
    });
  });

  list.addEventListener('dragover', e => {
    e.preventDefault();
    e.dataTransfer.dropEffect = 'move';
    if (!_dragEl) return;
    const target = _getDropTarget(e.clientY, list);
    if (target && target !== _dragEl) {
      const rect = target.getBoundingClientRect();
      if (e.clientY < rect.top + rect.height / 2) {
        list.insertBefore(_dragEl, target);
      } else {
        list.insertBefore(_dragEl, target.nextSibling);
      }
    }
  });
}

function _getDropTarget(y, list) {
  const cards = [...list.querySelectorAll('.card[data-id]')].filter(c => c !== _dragEl);
  let closest = null;
  let closestDist = Infinity;
  for (const card of cards) {
    const rect = card.getBoundingClientRect();
    const mid = rect.top + rect.height / 2;
    const dist = Math.abs(y - mid);
    if (dist < closestDist) { closestDist = dist; closest = card; }
  }
  return closest;
}

document.getElementById('prompts-list').addEventListener('htmx:afterSwap', _initDragReorder);

// ---- Builder SSE ----
let _builderEs = null;

function _builderDone() {
  clearTimeout(_builderEs && _builderEs._timeout);
  if (_builderEs) { _builderEs.close(); _builderEs = null; }
  const btn = document.getElementById('btn-generate');
  btn.disabled = false; btn.innerHTML = '&#9670; Generate Instruction'; btn.classList.remove('btn-generating');
  document.getElementById('btn-use-prompt').disabled = false;
}

function generatePrompt() {
  const desc = document.getElementById('builder-description').value.trim();
  if (!desc) { toast('Describe the emails first.', 'error'); return; }
  const btn = document.getElementById('btn-generate');
  btn.disabled = true; btn.innerHTML = '<span class="btn-spinner"></span>Generating...'; btn.classList.add('btn-generating');
  document.getElementById('builder-result').style.display = 'block';
  document.getElementById('builder-instruction').value = '';

  if (_builderEs) { _builderEs.close(); }

  const es = new EventSource('/api/prompts/generate-stream?description=' + encodeURIComponent(desc));
  _builderEs = es;

  // Reset timeout on each event — fires only after 2 min of inactivity, not 2 min total.
  function resetTimeout() {
    clearTimeout(es._timeout);
    es._timeout = setTimeout(() => {
      toast('Generation timed out (no activity for 2 minutes). Try again.', 'error');
      _builderDone();
    }, 120000);
  }
  resetTimeout();

  es.addEventListener('content', function(e) {
    document.getElementById('builder-instruction').value += e.data;
    resetTimeout();
  });
  es.addEventListener('done', function() { _builderDone(); });
  es.addEventListener('error', function(e) {
    if (e.data) toast('Generation failed: ' + e.data, 'error');
    _builderDone();
  });
  es.onerror = function() {
    if (es.readyState === EventSource.CLOSED) _builderDone();
  };
}

function useBuilderInstruction() {
  const instruction = document.getElementById('builder-instruction').value.trim();
  if (!instruction) return;
  const promptsNav = document.querySelector('[data-page="prompts"]');
  setActivePage('prompts', promptsNav);
  const list = document.getElementById('prompts-list');
  const doPopulate = () => {
    const panel = document.getElementById('add-prompt-panel');
    if (panel.classList.contains('d-none')) panel.classList.remove('d-none');
    const ta = document.getElementById('new-prompt-instructions');
    if (ta) { ta.value = instruction; ta.scrollIntoView({ behavior: 'smooth', block: 'center' }); }
  };
  if (list && list.querySelector('.card, .empty')) {
    doPopulate();
  } else {
    list.addEventListener('htmx:afterSwap', function handler() {
      list.removeEventListener('htmx:afterSwap', handler);
      doPopulate();
    });
  }
}

// ---- Copy OAuth URL ----
function copyAuthUrl(btn) {
  const urlBox = btn.previousElementSibling;
  const url = urlBox.dataset.url;
  if (!url) return;
  if (navigator.clipboard && window.isSecureContext) {
    navigator.clipboard.writeText(url).then(() => toast('Link copied to clipboard.'));
  } else {
    const ta = document.createElement('textarea');
    ta.value = url;
    ta.style.position = 'fixed'; ta.style.opacity = '0';
    document.body.appendChild(ta);
    ta.focus(); ta.select();
    try { document.execCommand('copy'); toast('Link copied to clipboard.'); }
    catch (e) { toast('Copy failed — select and copy the link manually.', 'error'); }
    document.body.removeChild(ta);
  }
}

// ---- Config import ----
function handleConfigImport(input) {
  const file = input.files[0];
  if (!file) return;
  const fd = new FormData();
  fd.append('file', file);
  input.value = '';
  fetch('/api/config/import', { method: 'POST', body: fd })
    .then(r => r.json())
    .then(data => {
      const el = document.getElementById('import-result');
      el.style.display = '';
      if (data.error) {
        const err = document.createElement('div');
        err.className = 'small text-danger';
        err.textContent = data.error;
        el.replaceChildren(err);
        return;
      }
      const s = data.summary;
      const ok = document.createElement('div');
      ok.className = 'small text-muted';
      ok.textContent = `Import complete — accounts: +${s.accounts.added} (${s.accounts.skipped} skipped), ` +
        `prompts: +${s.prompts.added} (${s.prompts.skipped} skipped), ` +
        `settings: +${s.settings.added} (${s.settings.skipped} skipped), ` +
        `retention: +${s.retention.added} (${s.retention.skipped} skipped).`;
      el.replaceChildren(ok);
      if (window.htmx) htmx.trigger(document.body, 'showToast',
        { message: 'Configuration imported.', type: 'success' });
    })
    .catch(() => {
      const el = document.getElementById('import-result');
      el.style.display = '';
      const fail = document.createElement('div');
      fail.className = 'small text-danger';
      fail.textContent = 'Import failed. Check the file and try again.';
      el.replaceChildren(fail);
    });
}

// ---- Dashboard poller status ----
document.body.addEventListener('htmx:afterSwap', function(e) {
  if (e.detail.target.id !== 'dashboard-content') return;
  const el = e.detail.target.querySelector('[data-poller-running]');
  if (!el) return;
  const running = JSON.parse(el.dataset.pollerRunning);
  document.getElementById('pollerDot').classList.toggle('active', running);
  document.getElementById('pollerLabel').textContent = running ? 'running' : 'stopped';
});

// ---- Hx-Trigger event handlers ----
document.body.addEventListener('showToast', function(e) {
  const { message, type } = e.detail || {};
  if (message) toast(message, type || 'success');
});

// ---- Recategorize modal: "changed" detection + "Improve" checkbox toggle ----
function recategorizeToggle(checkbox) {
  const row = checkbox.closest('.recategorize-row');
  if (!row) return;
  const initialChecked = checkbox.defaultChecked;
  const changed = checkbox.checked !== initialChecked;
  const improveWrap = row.querySelector('.improve-check-wrap');
  if (improveWrap) {
    improveWrap.classList.toggle('d-none', !changed);
    if (!changed) {
      const improveCheck = improveWrap.querySelector('input[type="checkbox"]');
      if (improveCheck) improveCheck.checked = false;
    }
  }
}

// ---- Suggestions badge ----
function _refreshSuggestionsBadge() {
  fetch('/fragments/prompt-suggestions')
    .then(r => r.text())
    .then(html => {
      const count = (html.match(/class="suggestion-card"/g) || []).filter((_, i, a) => true).length;
      const badge = document.getElementById('suggestions-badge');
      if (!badge) return;
      if (count > 0) {
        badge.textContent = count;
        badge.classList.remove('d-none');
      } else {
        badge.classList.add('d-none');
      }
    }).catch(() => {});
}

document.body.addEventListener('refreshSuggestionBadge', _refreshSuggestionsBadge);
window.addEventListener('refreshSuggestions', function() {
  _refreshSuggestionsBadge();
  // Reload suggestions list if on that page
  const listContainer = document.getElementById('suggestions-list-container');
  if (listContainer && document.getElementById('page-prompt-suggestions').classList.contains('active')) {
    htmx.ajax('GET', '/fragments/prompt-suggestions', { target: '#suggestions-list-container', swap: 'innerHTML' });
  }
});

document.body.addEventListener('closeModal', function(e) {
  const modalId = typeof e.detail === 'string' ? e.detail : (e.detail && e.detail.value);
  if (!modalId) return;
  const el = document.getElementById(modalId);
  if (el) bootstrap.Modal.getInstance(el)?.hide();
});

document.body.addEventListener('closeOAuthPanel', function() {
  document.getElementById('add-account-panel').classList.add('d-none');
  const step2 = document.getElementById('oauth-step-2-body');
  if (_oauthStep2Initial !== null) step2.innerHTML = _oauthStep2Initial;
  document.getElementById('oauth-step-1').classList.remove('done');
});

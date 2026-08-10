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
  // The suggestions list's own poll only fires while this page is already active (see
  // #suggestions-poll's hx-trigger filter in prompt_suggestions_list.html), so navigating
  // here from elsewhere would otherwise wait up to its own interval for the first update —
  // navigation itself is a good enough reason to refresh immediately.
  if (page === 'prompt-suggestions') {
    window.dispatchEvent(new CustomEvent('refreshSuggestions'));
  }
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
    // Only the handle grabs the card into drag mode - if the whole card were
    // draggable, click-drag inside its inputs/textareas would be hijacked as a
    // drag gesture instead of text selection.
    card.draggable = false;

    if (!handle.dataset.dragBound) {
      handle.dataset.dragBound = '1';
      const arm = () => { card.draggable = true; };
      const disarm = () => { card.draggable = false; };
      handle.addEventListener('mousedown', arm);
      handle.addEventListener('touchstart', arm);
      handle.addEventListener('mouseup', disarm);
      handle.addEventListener('touchend', disarm);
    }

    card.addEventListener('dragstart', e => {
      _dragEl = card;
      card.classList.add('drag-ghost');
      e.dataTransfer.effectAllowed = 'move';
      // Needed for Firefox
      e.dataTransfer.setData('text/plain', '');
    });

    card.addEventListener('dragend', async () => {
      card.classList.remove('drag-ghost');
      card.draggable = false;
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

  es.onmessage = function(e) {
    let msg;
    try { msg = JSON.parse(e.data); } catch { return; }
    if (msg.type === 'content') {
      document.getElementById('builder-instruction').value += msg.text;
      resetTimeout();
    } else if (msg.type === 'done') {
      _builderDone();
    } else if (msg.type === 'error') {
      if (msg.text) toast('Generation failed: ' + msg.text, 'error');
      _builderDone();
    }
  };
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

// ---- Classification / prompt-improver models: Standard/Flex tier toggles ----
// The settings form is HTMX-swapped, so this is a document-level delegated handler rather
// than one bound to the (re-rendered) button elements directly. The toggle's id prefix
// ("classify" or "improve") selects which hidden input and dropdown pair it drives.
document.addEventListener('click', function(e) {
  const btn = e.target.closest('#classify-tier-toggle button[data-tier], #improve-tier-toggle button[data-tier]');
  if (!btn) return;
  const tier = btn.dataset.tier;
  const form = btn.closest('form');
  if (!form) return;
  const prefix = btn.closest('.btn-group').id.replace('-tier-toggle', '');

  // Resolve everything relative to the clicked button's own form, rather than via
  // document-wide getElementById — that's what makes this robust to any duplicate/stale
  // copy of these ids elsewhere in the DOM (the prior global lookup could silently mutate
  // the wrong instance, making the button appear to do nothing).
  btn.closest('.btn-group').querySelectorAll('button').forEach(b => b.classList.toggle('active', b === btn));
  const tierInput = form.querySelector('[name="' + prefix + '_tier"]');
  if (tierInput) tierInput.value = tier;

  const standardSel = form.querySelector('#' + prefix + '-model-standard');
  const flexSel = form.querySelector('#' + prefix + '-model-flex');
  const showFlex = tier === 'flex';
  standardSel.classList.toggle('d-none', showFlex);
  standardSel.disabled = showFlex;
  flexSel.classList.toggle('d-none', !showFlex);
  flexSel.disabled = !showFlex;
});

// ---- Model pricing table: Normal/Flex toggle ----
// Same delegated-handler pattern as the tier toggles above (the settings form is
// HTMX-swapped, so a direct binding wouldn't survive a re-render).
document.addEventListener('click', function(e) {
  const btn = e.target.closest('#pricing-tier-toggle button[data-ptier]');
  if (!btn) return;
  const group = btn.closest('.btn-group');
  group.querySelectorAll('button').forEach(b => b.classList.toggle('active', b === btn));
  const showFlex = btn.dataset.ptier === 'flex';
  document.getElementById('model-pricing-standard').classList.toggle('d-none', showFlex);
  document.getElementById('model-pricing-flex').classList.toggle('d-none', !showFlex);
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

// ---- Bulk recategorize: history table multi-select ----
// #hist-body is htmx-swapped (filters, refreshHistory), so selection listeners are
// document-level delegated handlers rather than bound to the (re-rendered) checkbox
// elements directly — same pattern as the settings tier toggles above. Checked state
// itself lives in the DOM (each .hist-select checkbox), not a separate JS-tracked set: a
// swap wipes out stale checkboxes and their checked state together, so there's nothing to
// resync by hand — bulkUpdateToolbar() just re-reads the DOM whenever something might have
// changed.
const BULK_RECATEGORIZE_MAX = 50;

// bulkSelectedKeys returns the deduped "accountId:messageId" pairs for every checked
// .hist-select box currently in #hist-body. Multiple history rows can share a messageId
// (one row per rule that matched that email), so the same email can appear checked via more
// than one row — Set dedupes that down to one entry per email, matching what the server
// counts (recategorize_bulk.go's parseBulkSelections dedupes the same way).
function bulkSelectedKeys() {
  const boxes = document.querySelectorAll('#hist-body .hist-select:checked');
  const keys = new Set();
  boxes.forEach(b => keys.add(b.dataset.aid + ':' + b.dataset.mid));
  return [...keys];
}

function bulkUpdateToolbar() {
  const count = bulkSelectedKeys().length;
  const toolbar = document.getElementById('hist-bulk-toolbar');
  const countEl = document.getElementById('hist-bulk-count');
  if (!toolbar || !countEl) return;
  toolbar.classList.toggle('d-none', count === 0);
  toolbar.classList.toggle('d-flex', count > 0);
  countEl.textContent = count + (count === 1 ? ' email selected' : ' emails selected');
}

document.addEventListener('change', function(e) {
  if (e.target.matches('#hist-body .hist-select')) {
    bulkUpdateToolbar();
  } else if (e.target.id === 'hist-select-all') {
    document.querySelectorAll('#hist-body .hist-select').forEach(b => { b.checked = e.target.checked; });
    bulkUpdateToolbar();
  }
});

// Any swap of #hist-body (initial load, a filter change, or the refreshHistory trigger
// fired after applying a recategorization) discards whatever was checked — reset the
// header checkbox and toolbar to match rather than showing a stale selected count.
document.getElementById('hist-body')?.addEventListener('htmx:afterSwap', function() {
  const selectAll = document.getElementById('hist-select-all');
  if (selectAll) selectAll.checked = false;
  bulkUpdateToolbar();
});

function bulkRecategorizeClearSelection() {
  document.querySelectorAll('#hist-body .hist-select:checked').forEach(b => { b.checked = false; });
  const selectAll = document.getElementById('hist-select-all');
  if (selectAll) selectAll.checked = false;
  bulkUpdateToolbar();
}

// bulkActionToggle enforces "Apply to all" / "Remove from all" as mutually exclusive per
// rule (styled as a segmented btn-check pair, but they're two independent checkboxes —
// see bulk_recategorize_form.html — so nothing else keeps them from both being checked at
// once) and shows/hides that rule's "Improve prompt with AI" checkbox based on whether
// either is checked, mirroring recategorizeToggle's behavior for the single-email modal.
function bulkActionToggle(checkbox) {
  const row = checkbox.closest('.bulk-recategorize-row');
  if (!row) return;
  const other = checkbox.name === 'apply_prompt_ids'
    ? row.querySelector('input[name="remove_prompt_ids"]')
    : row.querySelector('input[name="apply_prompt_ids"]');
  if (checkbox.checked && other) other.checked = false;

  const anyChecked = row.querySelector('input[name="apply_prompt_ids"]:checked, input[name="remove_prompt_ids"]:checked');
  const improveWrap = row.querySelector('.bulk-improve-wrap');
  if (improveWrap) {
    improveWrap.classList.toggle('d-none', !anyChecked);
    if (!anyChecked) {
      const improveCheck = improveWrap.querySelector('input[type="checkbox"]');
      if (improveCheck) improveCheck.checked = false;
    }
  }
}

function bulkRecategorizeOpen() {
  const keys = bulkSelectedKeys();
  if (keys.length === 0) return;
  if (keys.length > BULK_RECATEGORIZE_MAX) {
    toast(`Select at most ${BULK_RECATEGORIZE_MAX} emails at a time (${keys.length} selected).`, 'error');
    return;
  }
  const params = new URLSearchParams();
  keys.forEach(k => params.append('selections', k));
  htmx.ajax('GET', '/fragments/history/bulk-recategorize?' + params.toString(),
    { target: '#recategorize-modal-body', swap: 'innerHTML' }
  ).then(() => {
    new bootstrap.Modal(document.getElementById('recategorize-modal')).show();
  });
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

// ---- Suggestion live trace ----
//
// A generating suggestion's detail card (prompt_suggestion_detail.html) renders a
// .trace-live pane instead of a static spinner. This polls its progress log
// (GET .../trace?after=N) every 1.5s, appends deltas into that pane, and — the piece that
// actually fixes "Generating… forever until refresh" — swaps in the finished card and
// notifies the list/badge the instant the trace reports done, instead of waiting on any
// fixed interval to notice on its own.
const _suggestionTracers = {};

function _startSuggestionTrace(el) {
  const id = el.dataset.suggestionId;
  if (!id || _suggestionTracers[id]) return; // already tracking this one

  const answerEl = el.querySelector('.trace-answer');
  const thinkingEl = el.querySelector('.trace-thinking');
  const stepsEl = el.querySelector('.trace-steps');
  const stalledEl = el.querySelector('.trace-stalled');
  let after = 0;
  let answerStarted = false;

  const stepLabels = {
    round_start: 'Starting…', candidate: 'Draft ready, validating…',
    replay_start: 'Checking against past examples…', replay_done: 'Validation',
    note: 'Note', error: 'Error',
  };

  function appendStep(kind, text) {
    if (!stepsEl || !(kind in stepLabels)) return;
    const li = document.createElement('li');
    li.textContent = text ? stepLabels[kind] + ': ' + text : stepLabels[kind];
    stepsEl.appendChild(li);
  }

  function poll() {
    // The element (or the whole card) may have been removed from the DOM since the last
    // tick — "Back to list" clears #suggestion-detail-container's innerHTML directly, with
    // no htmx swap event to hook a cleanup callback onto, so this check is the only thing
    // that stops the interval in that case.
    if (!document.body.contains(el)) {
      clearInterval(_suggestionTracers[id]);
      delete _suggestionTracers[id];
      return;
    }
    fetch('/fragments/prompt-suggestions/' + id + '/trace?after=' + after)
      .then(r => r.ok ? r.json() : Promise.reject(r.status))
      .then(data => {
        after = data.lastSeq;
        (data.events || []).forEach(ev => {
          if (ev.kind === 'answer') {
            if (!answerStarted) { answerEl.textContent = ''; answerEl.classList.remove('text-muted', 'fst-italic'); answerStarted = true; }
            answerEl.textContent += ev.text;
          } else if (ev.kind === 'thinking' && thinkingEl) {
            thinkingEl.textContent += ev.text;
          } else {
            appendStep(ev.kind, ev.text);
          }
        });
        if (stalledEl) stalledEl.classList.toggle('d-none', !data.stalled);

        if (data.status !== 'generating') {
          clearInterval(_suggestionTracers[id]);
          delete _suggestionTracers[id];
          // Swap in the finished card (pending/failed/dismissed) in place of this one, then
          // let the list and badge know — this is the completion signal that never existed
          // before: nothing used to fire when the worker finished, so the UI waited on
          // whatever poll happened to be running next, if any were running at all.
          const card = document.getElementById('suggestion-detail-' + id);
          if (card && window.htmx) {
            htmx.ajax('GET', '/fragments/prompt-suggestions/' + id, { target: '#' + card.id, swap: 'outerHTML' });
          }
          window.dispatchEvent(new CustomEvent('refreshSuggestions'));
        }
      })
      .catch(() => {}); // a transient fetch failure just waits for the next tick
  }

  _suggestionTracers[id] = setInterval(poll, 1500);
  poll();
}

// Listened for on #suggestion-detail-container (never itself replaced) rather than on the
// .trace-live element directly, so this covers both ways a generating card can appear:
// the initial GET (targets #suggestion-detail-container, swap innerHTML) and a regenerate
// POST (targets #suggestion-detail-{id}, swap outerHTML, nested inside the same container).
// Re-scans the whole container rather than reading the swap target off the event, matching
// the htmx:afterSwap pattern already used for #hist-body/#prompts-list elsewhere in this
// file — _startSuggestionTrace is idempotent (skips an id it's already tracking), so
// re-scanning a small subtree on every swap is cheap and simple rather than depending on
// an event-detail shape.
document.getElementById('suggestion-detail-container')?.addEventListener('htmx:afterSwap', function() {
  document.querySelectorAll('#suggestion-detail-container .trace-live[data-suggestion-id]').forEach(_startSuggestionTrace);
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

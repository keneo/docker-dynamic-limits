(function () {
    'use strict';

    // --- State ---
    let containers = [];
    let globalLimits = {};
    let globalUsage = {};
    let globalEnforced = {};
    let selectedID = null;
    let refreshTimer = null;
    let selectedIDs = new Set();

    let segments = [];

    // Segment scope from URL query parameter
    var urlParams = new URLSearchParams(window.location.search);
    var segmentScope = urlParams.get('segment') || '';
    var isSegmentScoped = segmentScope !== '';

    // --- DOM refs ---
    const tbody = document.getElementById('container-tbody');
    const emptyMsg = document.getElementById('empty-msg');
    const detailPanel = document.getElementById('detail-panel');
    const detailTitle = document.getElementById('detail-title');
    const detailLimits = document.getElementById('detail-limits');
    const statusIndicator = document.getElementById('status-indicator');
    const lastRefreshEl = document.getElementById('last-refresh');
    const refreshSelect = document.getElementById('refresh-interval');
    const toastContainer = document.getElementById('toast-container');
    const offlineBanner = document.getElementById('offline-banner');

    // --- Value formatting (mirrors Go format.go) ---
    const LIMIT_TYPES = ['cpu', 'ram', 'net', 'disk', 'disk-io-bytes', 'disk-io-ops', 'spending',
        'ram-usage-bsec', 'disk-usage-bsec', 'ram-request-bsec', 'disk-request-bsec'];

    const LIMIT_LABELS = {
        'cpu': 'CPU',
        'ram': 'RAM',
        'disk': 'Disk',
        'net': 'Network',
        'disk-io-bytes': 'Disk I/O Bytes',
        'disk-io-ops': 'Disk I/O Ops',
        'spending': 'Spending',
        'ram-usage-bsec': 'RAM Usage B·s',
        'disk-usage-bsec': 'Disk Usage B·s',
        'ram-request-bsec': 'RAM Request B·s',
        'disk-request-bsec': 'Disk Request B·s'
    };

    function formatByteSeconds(b) {
        var TB = 1024 * 1024 * 1024 * 1024;
        var GB = 1024 * 1024 * 1024;
        var MB = 1024 * 1024;
        var KB = 1024;
        if (b >= TB) return (b / TB).toFixed(1) + 'T\u00b7s';
        if (b >= GB) return (b / GB).toFixed(1) + 'G\u00b7s';
        if (b >= MB) return (b / MB).toFixed(1) + 'M\u00b7s';
        if (b >= KB) return (b / KB).toFixed(1) + 'K\u00b7s';
        return b + 'B\u00b7s';
    }

    function formatValue(type, v) {
        if (v === 0) return '-';
        switch (type) {
            case 'cpu': return formatCPU(v);
            case 'ram':
            case 'disk':
            case 'net':
            case 'disk-io-bytes':
                return formatBytes(v);
            case 'ram-usage-bsec':
            case 'disk-usage-bsec':
            case 'ram-request-bsec':
            case 'disk-request-bsec':
                return formatByteSeconds(v);
            case 'spending':
                return '$' + (v / 100000).toFixed(2);
            case 'disk-io-ops':
                return v.toLocaleString();
            default:
                return String(v);
        }
    }

    function formatCPU(s) {
        if (s >= 3600) {
            var h = Math.floor(s / 3600);
            var m = Math.floor((s % 3600) / 60);
            var sec = s % 60;
            return h + 'h' + m + 'm' + sec + 's';
        }
        if (s >= 60) {
            return Math.floor(s / 60) + 'm' + (s % 60) + 's';
        }
        return s + 's';
    }

    function formatBytes(b) {
        var TB = 1024 * 1024 * 1024 * 1024;
        var GB = 1024 * 1024 * 1024;
        var MB = 1024 * 1024;
        var KB = 1024;
        if (b >= TB) return (b / TB).toFixed(1) + 'T';
        if (b >= GB) return (b / GB).toFixed(1) + 'G';
        if (b >= MB) return (b / MB).toFixed(1) + 'M';
        if (b >= KB) return (b / KB).toFixed(1) + 'K';
        return b + 'B';
    }

    function parseValue(type, s) {
        s = s.trim();
        switch (type) {
            case 'cpu': return parseDuration(s);
            case 'ram':
            case 'disk':
            case 'net':
            case 'disk-io-bytes':
                return parseBytes(s);
            case 'ram-usage-bsec':
            case 'disk-usage-bsec':
            case 'ram-request-bsec':
            case 'disk-request-bsec':
                return parseBytes(s);
            case 'disk-io-ops':
                return parseInt(s, 10);
            case 'spending':
                return Math.round(parseFloat(s) * 100000);
            default:
                return parseInt(s, 10);
        }
    }

    function parseDuration(s) {
        if (s.length === 0) return NaN;
        var suffix = s[s.length - 1];
        var num = s.slice(0, -1);
        switch (suffix) {
            case 's': return parseInt(num, 10);
            case 'm': return parseInt(num, 10) * 60;
            case 'h': return parseInt(num, 10) * 3600;
            default: return parseInt(s, 10);
        }
    }

    function parseBytes(s) {
        if (s.length === 0) return NaN;
        var suffix = s[s.length - 1].toLowerCase();
        var num = s.slice(0, -1);
        var mul = 1;
        switch (suffix) {
            case 'k': mul = 1024; break;
            case 'm': mul = 1024 * 1024; break;
            case 'g': mul = 1024 * 1024 * 1024; break;
            case 't': mul = 1024 * 1024 * 1024 * 1024; break;
            default: num = s; break;
        }
        return Math.round(parseFloat(num) * mul);
    }

    // --- Toast ---
    function toast(msg, isError) {
        var el = document.createElement('div');
        el.className = 'toast' + (isError ? ' error' : '');
        el.textContent = msg;
        toastContainer.appendChild(el);
        setTimeout(function () { el.remove(); }, 3000);
    }

    // --- API helpers ---
    // When segment-scoped, rewrite paths to go through /segments/{id}/...
    function scopedPath(path) {
        if (!isSegmentScoped) return path;
        // Paths that should NOT be rewritten (segment management itself, global endpoints)
        if (path.indexOf('/segments') === 0) return path;
        if (path === '/config') return '/segments/' + segmentScope + '/config';
        if (path === '/containers') return '/segments/' + segmentScope + '/containers';
        // Rewrite /containers/{id}... -> /segments/{seg}/containers/{id}...
        if (path.indexOf('/containers/') === 0) {
            return '/segments/' + segmentScope + path;
        }
        if (path === '/events') return '/segments/' + segmentScope + '/events';
        // Host limits: not available on segment scope — should use segment limits
        if (path === '/host-limits' || path === '/global-limits') {
            return '/segments/' + segmentScope + '/limits';
        }
        // /register: assign to segment on register
        return path;
    }

    function api(method, path, body) {
        var opts = { method: method, headers: {} };
        if (body !== undefined) {
            opts.headers['Content-Type'] = 'application/json';
            opts.body = JSON.stringify(body);
        }
        return fetch('/api' + scopedPath(path), opts).then(function (resp) {
            if (!resp.ok) {
                return resp.json().catch(function () { return {}; }).then(function (err) {
                    throw new Error(err.error || 'HTTP ' + resp.status);
                });
            }
            return resp.json().catch(function () { return null; });
        });
    }

    // --- Rendering ---
    function renderContainers() {
        tbody.innerHTML = '';
        if (containers.length === 0) {
            emptyMsg.hidden = false;
            updateBulkToolbar();
            return;
        }
        emptyMsg.hidden = true;

        // Prune selectedIDs to only keep IDs still in the list
        var currentIDs = {};
        containers.forEach(function (cs) { currentIDs[cs.container.id] = true; });
        selectedIDs.forEach(function (id) {
            if (!currentIDs[id]) selectedIDs.delete(id);
        });

        containers.forEach(function (cs) {
            var c = cs.container;
            var enforcedCount = 0;
            var limitCount = 0;
            for (var key in cs.limits) {
                if (cs.limits[key] > 0) limitCount++;
            }
            for (var key2 in cs.enforced) {
                if (cs.enforced[key2]) enforcedCount++;
            }

            var tr = document.createElement('tr');
            var classes = [];
            if (enforcedCount > 0) classes.push('enforced-row');
            if (selectedIDs.has(c.id)) classes.push('selected-row');
            tr.className = classes.join(' ');

            var state = cs.state || 'unknown';
            var stateClass = 'state-badge state-' + state;

            var frozenBadge = cs.frozen ? ' <span class="frozen-badge">FROZEN</span>' : '';

            var segBadge = c.segment_id ? '<span class="segment-badge">' + esc(c.segment_id) + '</span>' : '<span class="segment-none">-</span>';

            tr.innerHTML =
                '<td class="col-check"><input type="checkbox"' + (selectedIDs.has(c.id) ? ' checked' : '') + '></td>' +
                '<td><code>' + esc(c.id) + '</code></td>' +
                '<td>' + esc(c.name || '-') + frozenBadge + '</td>' +
                '<td>' + segBadge + '</td>' +
                '<td><span class="' + stateClass + '">' + esc(state) + '</span></td>' +
                '<td>' + limitCount + '</td>' +
                '<td>' + enforcedCount + (enforcedCount > 0 ? ' <span class="enforced-badge">ENFORCED</span>' : '') + '</td>' +
                '<td class="actions"></td>';

            // Wire checkbox
            var cb = tr.querySelector('input[type="checkbox"]');
            cb.onchange = (function (id) {
                return function () {
                    if (this.checked) {
                        selectedIDs.add(id);
                    } else {
                        selectedIDs.delete(id);
                    }
                    // Update row style
                    var row = this.closest('tr');
                    if (this.checked) {
                        row.classList.add('selected-row');
                    } else {
                        row.classList.remove('selected-row');
                    }
                    updateBulkToolbar();
                };
            })(c.id);

            var actions = tr.querySelector('.actions');

            var detailBtn = document.createElement('button');
            detailBtn.className = 'btn btn-sm';
            detailBtn.textContent = 'Details';
            detailBtn.onclick = function () { selectContainer(c.id); };
            actions.appendChild(detailBtn);

            var freezeBtn = document.createElement('button');
            freezeBtn.className = 'btn btn-sm' + (cs.frozen ? ' btn-primary' : '');
            freezeBtn.textContent = cs.frozen ? 'Unfreeze' : 'Freeze';
            freezeBtn.onclick = cs.frozen
                ? function () { unfreezeContainer(c.id); }
                : function () { freezeContainer(c.id); };
            actions.appendChild(freezeBtn);

            if (!isSegmentScoped) {
                var segBtn = document.createElement('button');
                segBtn.className = 'btn btn-sm';
                segBtn.textContent = 'Segment';
                segBtn.onclick = (function (id) { return function () { openAssignDialog(id); }; })(c.id);
                actions.appendChild(segBtn);
            }

            var cloneBtn = document.createElement('button');
            cloneBtn.className = 'btn btn-sm';
            cloneBtn.textContent = 'Clone';
            cloneBtn.onclick = function () { cloneContainer(c.id); };
            actions.appendChild(cloneBtn);

            var removeBtn = document.createElement('button');
            removeBtn.className = 'btn btn-sm btn-danger';
            removeBtn.textContent = 'Remove';
            removeBtn.onclick = function () { removeContainer(c.id, c.name); };
            actions.appendChild(removeBtn);

            tbody.appendChild(tr);
        });

        // Wire select-all checkbox
        var selectAllCb = document.getElementById('select-all');
        selectAllCb.onchange = function () {
            var checked = this.checked;
            containers.forEach(function (cs) {
                if (checked) {
                    selectedIDs.add(cs.container.id);
                } else {
                    selectedIDs.delete(cs.container.id);
                }
            });
            // Update all row checkboxes
            tbody.querySelectorAll('input[type="checkbox"]').forEach(function (cb) {
                cb.checked = checked;
                var row = cb.closest('tr');
                if (checked) {
                    row.classList.add('selected-row');
                } else {
                    row.classList.remove('selected-row');
                }
            });
            updateBulkToolbar();
        };

        updateBulkToolbar();
    }

    function renderDetail() {
        if (!selectedID) {
            detailPanel.hidden = true;
            return;
        }

        var cs = containers.find(function (c) { return c.container.id === selectedID; });
        if (!cs) {
            detailPanel.hidden = true;
            selectedID = null;
            return;
        }

        detailPanel.hidden = false;
        detailTitle.textContent = (cs.container.name || cs.container.id) + ' (' + cs.container.id + ')';

        var html = '';
        LIMIT_TYPES.forEach(function (type) {
            var usage = (cs.usage && cs.usage[type]) || 0;
            var limit = (cs.limits && cs.limits[type]) || 0;
            var enforced = cs.enforced && cs.enforced[type];
            var pct = limit > 0 ? Math.min(100, (usage / limit) * 100) : 0;

            html +=
                '<div class="limit-row">' +
                    '<span class="limit-type">' + esc(LIMIT_LABELS[type]) + '</span>' +
                    '<span>' + formatValue(type, usage) + '</span>' +
                    '<span>' + formatValue(type, limit) + '</span>' +
                    '<span class="limit-pct">' + (limit > 0 ? pct.toFixed(0) + '%' : '-') + '</span>' +
                    '<div class="progress-bar"><div class="progress-fill' + (enforced ? ' enforced' : '') + '" style="width:' + pct + '%"></div></div>' +
                    '<div class="limit-actions">' +
                        '<button class="btn btn-sm" data-type="' + type + '" data-op="set">Set</button>' +
                        '<button class="btn btn-sm" data-type="' + type + '" data-op="increase">+</button>' +
                        '<button class="btn btn-sm" data-type="' + type + '" data-op="decrease">-</button>' +
                    '</div>' +
                '</div>';
        });
        detailLimits.innerHTML = html;

        // Add activity section if not present
        if (!document.getElementById('detail-activity')) {
            var actDiv = document.createElement('div');
            actDiv.id = 'detail-activity';
            detailLimits.parentNode.appendChild(actDiv);
        }

        detailLimits.querySelectorAll('[data-op]').forEach(function (btn) {
            btn.addEventListener('click', function () {
                openLimitDialog(selectedID, btn.dataset.type, btn.dataset.op);
            });
        });
    }

    function esc(s) {
        var d = document.createElement('div');
        d.textContent = s;
        return d.innerHTML;
    }

    // --- Data fetching ---
    function refresh() {
        api('GET', '/containers').then(function (data) {
            if (data && typeof data === 'object' && !Array.isArray(data) && ('containers' in data)) {
                containers = Array.isArray(data.containers) ? data.containers : [];
                globalLimits = data.host_limits || data.global_limits || data.scope_limits || {};
                globalUsage = data.host_usage || data.global_usage || data.scope_usage || {};
                globalEnforced = data.host_enforced || data.global_enforced || data.scope_enforced || {};
            } else {
                containers = Array.isArray(data) ? data : [];
                globalLimits = {};
                globalUsage = {};
                globalEnforced = {};
            }
            offlineBanner.hidden = true;
            statusIndicator.className = 'status connected';
            statusIndicator.title = 'Connected';
            lastRefreshEl.textContent = new Date().toLocaleTimeString();
            renderContainers();
            renderDetail();
            renderGlobalLimits();
            if (selectedID) fetchActivity(selectedID);
            // Fetch segments
            if (!isSegmentScoped) {
                api('GET', '/segments').then(function (data) {
                    segments = (data && data.segments) || [];
                    renderSegments();
                });
            }
        }).catch(function (err) {
            offlineBanner.hidden = false;
            statusIndicator.className = 'status disconnected';
            statusIndicator.title = 'Disconnected: ' + err.message;
        });
    }

    function startAutoRefresh() {
        stopAutoRefresh();
        var interval = parseInt(refreshSelect.value, 10);
        if (interval > 0) {
            refreshTimer = setInterval(refresh, interval);
        }
    }

    function stopAutoRefresh() {
        if (refreshTimer) {
            clearInterval(refreshTimer);
            refreshTimer = null;
        }
    }

    // --- Activity ---
    let currentActivity = [];
    let expandedActivityRows = {};  // track which rows are expanded across re-renders

    function fetchActivity(containerID) {
        api('GET', '/containers/' + containerID + '/activity').then(function (data) {
            currentActivity = data || [];
            renderActivity();
        }).catch(function () {
            currentActivity = [];
            renderActivity();
        });
    }

    function renderActivity() {
        var el = document.getElementById('detail-activity');
        if (!el) return;
        if (!currentActivity || currentActivity.length === 0) {
            el.innerHTML = '<p class="empty-msg">No proxy activity yet.</p>';
            return;
        }

        // Save scroll positions of expanded activity bodies before re-render
        var savedScrolls = {};
        el.querySelectorAll('.activity-body-row:not([hidden])').forEach(function (row) {
            var idx = row.dataset.idx;
            var pres = row.querySelectorAll('.activity-body');
            pres.forEach(function (pre, j) {
                if (pre.scrollTop || pre.scrollLeft) {
                    savedScrolls[idx + ':' + j] = { top: pre.scrollTop, left: pre.scrollLeft };
                }
            });
        });

        var html = '<table class="activity-table"><thead><tr>' +
            '<th>Time</th><th>Host</th><th>Path</th><th>Model</th>' +
            '<th>Tokens</th><th>Cost</th><th>Status</th><th>Duration</th>' +
            '</tr></thead><tbody>';
        // Show most recent first
        for (var i = currentActivity.length - 1; i >= 0; i--) {
            var a = currentActivity[i];
            var time = a.timestamp ? new Date(a.timestamp).toLocaleTimeString() : '-';
            var tokens = (a.input_tokens || a.output_tokens) ?
                (a.input_tokens || 0) + '/' + (a.output_tokens || 0) : '-';
            var cost = a.cost_micro ? '$' + (a.cost_micro / 100000000).toFixed(4) : '-';
            var statusClass = a.error ? 'activity-error' : (a.status_code >= 400 ? 'activity-warn' : '');
            var statusText = a.error || (a.status_code || '-');
            var dur = a.duration_ms ? (a.duration_ms / 1000).toFixed(1) + 's' : '-';
            var isExpanded = !!expandedActivityRows[i];

            html += '<tr class="activity-row ' + statusClass + '" data-idx="' + i + '">' +
                '<td>' + esc(time) + '</td>' +
                '<td>' + esc(a.host || '-') + '</td>' +
                '<td>' + esc(a.path || '-') + '</td>' +
                '<td>' + esc(a.model || '-') + '</td>' +
                '<td>' + tokens + '</td>' +
                '<td>' + cost + '</td>' +
                '<td>' + esc(String(statusText)) + '</td>' +
                '<td>' + dur + '</td>' +
                '</tr>' +
                '<tr class="activity-body-row" data-idx="' + i + '"' + (isExpanded ? '' : ' hidden') + '>' +
                '<td colspan="8"><div class="activity-bodies">' +
                '<div><strong>Request:</strong><pre class="activity-body">' + highlightJSON(a.request_body) + '</pre></div>' +
                '<div><strong>Response:</strong><pre class="activity-body">' + highlightJSON(a.response_body) + '</pre></div>' +
                '</div></td></tr>';
        }
        html += '</tbody></table>';
        el.innerHTML = html;

        // Restore scroll positions
        el.querySelectorAll('.activity-body-row:not([hidden])').forEach(function (row) {
            var idx = row.dataset.idx;
            var pres = row.querySelectorAll('.activity-body');
            pres.forEach(function (pre, j) {
                var saved = savedScrolls[idx + ':' + j];
                if (saved) {
                    pre.scrollTop = saved.top;
                    pre.scrollLeft = saved.left;
                }
            });
        });

        // Click to expand/collapse
        el.querySelectorAll('.activity-row').forEach(function (row) {
            row.addEventListener('click', function () {
                var idx = row.dataset.idx;
                var bodyRow = el.querySelector('.activity-body-row[data-idx="' + idx + '"]');
                if (bodyRow) {
                    bodyRow.hidden = !bodyRow.hidden;
                    if (bodyRow.hidden) {
                        delete expandedActivityRows[idx];
                    } else {
                        expandedActivityRows[idx] = true;
                    }
                }
            });
        });
    }

    function escHTML(s) {
        return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
    }

    function highlightJSON(s) {
        if (!s) return '<span class="json-null">(empty)</span>';
        var text;
        try {
            text = JSON.stringify(JSON.parse(s), null, 2);
        } catch (e) {
            // Truncated or invalid JSON — best-effort format the raw text
            text = s;
        }
        return colorizeJSON(escHTML(text));
    }

    function colorizeJSON(escaped) {
        return escaped.replace(
            /"([^"]+)"(\s*:)/g, '<span class="json-key">"$1"</span>$2'
        ).replace(
            /:\s*"([^"]*)"/g, function (m, val) {
                return ': <span class="json-str">"' + val + '"</span>';
            }
        ).replace(
            /:\s*(-?\d+\.?\d*([eE][+-]?\d+)?)\b/g, ': <span class="json-num">$1</span>'
        ).replace(
            /:\s*(true|false)\b/g, ': <span class="json-bool">$1</span>'
        ).replace(
            /:\s*(null)\b/g, ': <span class="json-null">$1</span>'
        );
    }

    function formatJSON(s) {
        if (!s) return '(empty)';
        try {
            return JSON.stringify(JSON.parse(s), null, 2);
        } catch (e) {
            return s;
        }
    }

    // --- Actions ---
    function selectContainer(id) {
        selectedID = id;
        renderDetail();
        fetchActivity(id);
    }

    function cloneContainer(id) {
        api('POST', '/containers/' + id + '/clone', {}).then(function (result) {
            toast('Cloned: ' + result.id);
            refresh();
        }).catch(function (err) {
            toast('Clone failed: ' + err.message, true);
        });
    }

    function freezeContainer(id) {
        api('POST', '/containers/' + id + '/freeze', {}).then(function () {
            toast('Container frozen');
            refresh();
        }).catch(function (err) {
            toast('Freeze failed: ' + err.message, true);
        });
    }

    function unfreezeContainer(id) {
        api('POST', '/containers/' + id + '/unfreeze', {}).then(function (result) {
            if (result && result.enforcement_active) {
                toast('Container unfrozen (enforcement still active)');
            } else {
                toast('Container unfrozen');
            }
            refresh();
        }).catch(function (err) {
            toast('Unfreeze failed: ' + err.message, true);
        });
    }

    function removeContainer(id, name) {
        var dialog = document.getElementById('confirm-dialog');
        document.getElementById('confirm-title').textContent = 'Remove Container';
        document.getElementById('confirm-message').textContent = 'Remove ' + (name || id) + ' from management?';
        dialog.showModal();
        dialog.onclose = function () {
            if (dialog.returnValue === 'confirm') {
                api('DELETE', '/containers/' + id).then(function () {
                    toast('Container removed');
                    if (selectedID === id) selectedID = null;
                    refresh();
                }).catch(function (err) {
                    toast('Remove failed: ' + err.message, true);
                });
            }
        };
    }

    // --- Bulk selection ---
    function updateBulkToolbar() {
        var toolbar = document.getElementById('bulk-toolbar');
        var countEl = document.getElementById('bulk-count');
        var selectAllCb = document.getElementById('select-all');

        if (selectedIDs.size === 0) {
            toolbar.hidden = true;
            if (selectAllCb) {
                selectAllCb.checked = false;
                selectAllCb.indeterminate = false;
            }
            return;
        }

        toolbar.hidden = false;
        countEl.textContent = selectedIDs.size + ' selected';

        if (selectAllCb) {
            if (selectedIDs.size === containers.length) {
                selectAllCb.checked = true;
                selectAllCb.indeterminate = false;
            } else {
                selectAllCb.checked = false;
                selectAllCb.indeterminate = true;
            }
        }
    }

    function bulkOperation(ids, opFn, label) {
        var total = ids.length;
        var ok = 0;
        var fail = 0;
        var chain = Promise.resolve();
        ids.forEach(function (id) {
            chain = chain.then(function () {
                return opFn(id).then(function () {
                    ok++;
                }).catch(function () {
                    fail++;
                });
            });
        });
        return chain.then(function () {
            var msg = label + ': ' + ok + '/' + total + ' succeeded';
            if (fail > 0) msg += ', ' + fail + ' failed';
            toast(msg, fail > 0);
            selectedIDs.clear();
            refresh();
        });
    }

    function bulkFreeze() {
        var ids = Array.from(selectedIDs);
        bulkOperation(ids, function (id) {
            return api('POST', '/containers/' + id + '/freeze', {});
        }, 'Freeze');
    }

    function bulkUnfreeze() {
        var ids = Array.from(selectedIDs);
        bulkOperation(ids, function (id) {
            return api('POST', '/containers/' + id + '/unfreeze', {});
        }, 'Unfreeze');
    }

    function bulkClone() {
        var ids = Array.from(selectedIDs);
        bulkOperation(ids, function (id) {
            return api('POST', '/containers/' + id + '/clone', {});
        }, 'Clone');
    }

    function bulkRemove() {
        var count = selectedIDs.size;
        var dialog = document.getElementById('confirm-dialog');
        document.getElementById('confirm-title').textContent = 'Remove ' + count + ' Containers';
        document.getElementById('confirm-message').textContent = 'Remove ' + count + ' selected containers from management?';
        dialog.showModal();
        dialog.onclose = function () {
            if (dialog.returnValue === 'confirm') {
                var ids = Array.from(selectedIDs);
                bulkOperation(ids, function (id) {
                    return api('DELETE', '/containers/' + id);
                }, 'Remove');
            }
        };
    }

    function bulkSetLimit() {
        document.getElementById('bulk-limit-mode').value = '1';
        document.getElementById('limit-container-id').value = '';
        document.getElementById('limit-type').value = 'cpu';
        document.getElementById('limit-operation').value = 'set';
        document.getElementById('limit-value').value = '';
        document.getElementById('limit-dialog').showModal();
    }

    // Wire bulk toolbar buttons
    document.getElementById('bulk-freeze').addEventListener('click', bulkFreeze);
    document.getElementById('bulk-unfreeze').addEventListener('click', bulkUnfreeze);
    document.getElementById('bulk-clone').addEventListener('click', bulkClone);
    document.getElementById('bulk-remove').addEventListener('click', bulkRemove);
    document.getElementById('bulk-set-limit').addEventListener('click', bulkSetLimit);

    function openLimitDialog(containerID, type, operation) {
        document.getElementById('bulk-limit-mode').value = '';
        document.getElementById('limit-container-id').value = containerID;
        document.getElementById('limit-type').value = type;
        document.getElementById('limit-operation').value = operation;
        document.getElementById('limit-value').value = '';
        document.getElementById('limit-dialog').showModal();
    }

    // --- Event listeners ---
    document.getElementById('btn-register').addEventListener('click', function () {
        document.getElementById('register-id').value = '';
        document.getElementById('register-dialog').showModal();
    });

    document.getElementById('register-dialog').addEventListener('close', function () {
        var dialog = this;
        if (dialog.returnValue !== 'cancel') {
            var containerID = document.getElementById('register-id').value.trim();
            if (!containerID) return;
            var body = { container_id: containerID };
            if (isSegmentScoped) body.segment_id = segmentScope;
            api('POST', '/register', body).then(function (result) {
                toast('Registered: ' + result.id);
                refresh();
            }).catch(function (err) {
                toast('Register failed: ' + err.message, true);
            });
        }
    });

    document.getElementById('limit-dialog').addEventListener('close', function () {
        var dialog = this;
        if (dialog.returnValue !== 'cancel') {
            var isBulk = document.getElementById('bulk-limit-mode').value === '1';
            var type = document.getElementById('limit-type').value;
            var operation = document.getElementById('limit-operation').value;
            var rawValue = document.getElementById('limit-value').value.trim();
            if (!rawValue) return;

            var value = parseValue(type, rawValue);
            if (isNaN(value)) {
                toast('Invalid value: ' + rawValue, true);
                return;
            }

            if (isBulk) {
                var ids = Array.from(selectedIDs);
                bulkOperation(ids, function (id) {
                    return api('PUT', '/containers/' + id + '/limits', {
                        type: type,
                        value: value,
                        operation: operation
                    });
                }, 'Set limit ' + type);
            } else {
                var containerID = document.getElementById('limit-container-id').value;
                api('PUT', '/containers/' + containerID + '/limits', {
                    type: type,
                    value: value,
                    operation: operation
                }).then(function () {
                    toast('Limit ' + operation + ': ' + type);
                    refresh();
                }).catch(function (err) {
                    toast('Limit failed: ' + err.message, true);
                });
            }
        }
        document.getElementById('bulk-limit-mode').value = '';
    });

    document.getElementById('detail-close').addEventListener('click', function () {
        selectedID = null;
        currentActivity = [];
        expandedActivityRows = {};
        var actEl = document.getElementById('detail-activity');
        if (actEl) actEl.remove();
        detailPanel.hidden = true;
    });

    refreshSelect.addEventListener('change', startAutoRefresh);

    // Pause refresh when tab is hidden
    document.addEventListener('visibilitychange', function () {
        if (document.hidden) {
            stopAutoRefresh();
        } else {
            refresh();
            startAutoRefresh();
        }
    });

    // --- Global Limits ---
    var globalPanel = document.getElementById('global-panel');
    var globalLimitsEl = document.getElementById('global-limits');

    function renderGlobalLimits() {
        var hasAnyLimit = false;
        for (var k in globalLimits) {
            if (globalLimits[k] > 0) { hasAnyLimit = true; break; }
        }
        globalPanel.hidden = !hasAnyLimit;
        if (!hasAnyLimit) return;

        var html = '';
        LIMIT_TYPES.forEach(function (type) {
            var limit = (globalLimits && globalLimits[type]) || 0;
            if (limit === 0) return;
            var usage = (globalUsage && globalUsage[type]) || 0;
            var enforced = globalEnforced && globalEnforced[type];
            var pct = limit > 0 ? Math.min(100, (usage / limit) * 100) : 0;

            html +=
                '<div class="limit-row">' +
                    '<span class="limit-type">' + esc(LIMIT_LABELS[type]) + '</span>' +
                    '<span>' + formatValue(type, usage) + '</span>' +
                    '<span>' + formatValue(type, limit) + '</span>' +
                    '<span class="limit-pct">' + (limit > 0 ? pct.toFixed(0) + '%' : '-') + '</span>' +
                    '<div class="progress-bar"><div class="progress-fill' + (enforced ? ' enforced' : '') + '" style="width:' + pct + '%"></div></div>' +
                    '<div class="limit-actions">' +
                        '<button class="btn btn-sm" data-global-type="' + type + '" data-global-op="set">Set</button>' +
                        '<button class="btn btn-sm" data-global-type="' + type + '" data-global-op="increase">+</button>' +
                        '<button class="btn btn-sm" data-global-type="' + type + '" data-global-op="decrease">-</button>' +
                    '</div>' +
                '</div>';
        });
        globalLimitsEl.innerHTML = html;

        globalLimitsEl.querySelectorAll('[data-global-op]').forEach(function (btn) {
            btn.addEventListener('click', function () {
                openGlobalLimitDialog(btn.dataset.globalType, btn.dataset.globalOp);
            });
        });
    }

    function openGlobalLimitDialog(type, operation) {
        document.getElementById('global-limit-type').value = type;
        document.getElementById('global-limit-value').value = '';
        document.getElementById('global-limit-operation').value = operation;
        document.getElementById('global-limit-dialog').showModal();
    }

    document.getElementById('global-limit-dialog').addEventListener('close', function () {
        var dialog = this;
        if (dialog.returnValue !== 'cancel') {
            var type = document.getElementById('global-limit-type').value;
            var operation = document.getElementById('global-limit-operation').value;
            var rawValue = document.getElementById('global-limit-value').value.trim();
            if (!rawValue) return;

            var value = parseValue(type, rawValue);
            if (isNaN(value)) {
                toast('Invalid value: ' + rawValue, true);
                return;
            }

            api('PUT', '/host-limits', {
                type: type,
                value: value,
                operation: operation
            }).then(function () {
                var label = isSegmentScoped ? 'Segment' : 'Host';
                toast(label + ' limit ' + operation + ': ' + type);
                refresh();
            }).catch(function (err) {
                toast('Limit failed: ' + err.message, true);
            });
        }
    });

    // --- Config panel ---
    var configTbody = document.getElementById('config-tbody');
    var configToggle = document.getElementById('config-toggle');
    var configBody = document.getElementById('config-body');
    var configArrow = document.getElementById('config-arrow');
    var configOpen = false;

    var CONFIG_DISPLAY_ORDER = [
        { key: 'anthropic-enabled', json: 'anthropic_enabled' },
        { key: 'openai-enabled', json: 'openai_enabled' },
        { key: 'ollama-enabled', json: 'ollama_enabled' },
        { key: 'anthropic-key', json: 'anthropic_key_set', isKey: true },
        { key: 'openai-key', json: 'openai_key_set', isKey: true },
        { key: 'ollama-url', json: 'ollama_url' },
        { key: 'ollama-models', json: 'ollama_models', isList: true },
        { key: 'ollama-queue-size', json: 'ollama_queue_size' },
        { key: 'ollama-timeout', json: 'ollama_timeout' },
        { key: 'ollama-default-bid', json: 'ollama_default_bid' },
        { key: 'error-webhooks', json: 'error_webhooks', isList: true },
        { key: 'keep-limits-consistent', json: 'keep_limits_consistent' }
    ];

    configToggle.addEventListener('click', function () {
        configOpen = !configOpen;
        configBody.hidden = !configOpen;
        configArrow.className = 'config-arrow' + (configOpen ? ' open' : '');
        if (configOpen) refreshConfig();
    });

    function refreshConfig() {
        api('GET', '/config').then(function (data) {
            renderConfig(data || {});
        }).catch(function () {
            configTbody.innerHTML = '<tr><td colspan="2">Failed to load config</td></tr>';
        });
    }

    function renderConfig(data) {
        configTbody.innerHTML = '';
        CONFIG_DISPLAY_ORDER.forEach(function (item) {
            var tr = document.createElement('tr');
            var val;
            if (item.isKey) {
                val = data[item.json] ? '****' : '(not set)';
            } else if (item.isList) {
                var arr = data[item.json];
                val = (arr && arr.length > 0) ? arr.join(', ') : '(none)';
            } else {
                var v = data[item.json];
                val = (v !== undefined && v !== null) ? String(v) : '(not configured)';
            }
            tr.innerHTML = '<td><code>' + esc(item.key) + '</code></td><td>' + esc(val) + '</td>';
            configTbody.appendChild(tr);
        });
    }

    // --- Segments ---
    var segmentsPanel = document.getElementById('segments-panel');
    var segmentsList = document.getElementById('segments-list');

    function renderSegments() {
        if (!segmentsPanel) return;
        segmentsPanel.hidden = segments.length === 0;
        if (segments.length === 0) return;

        var html = '';
        segments.forEach(function (seg) {
            // Count containers in this segment
            var memberCount = 0;
            containers.forEach(function (cs) {
                if (cs.container.segment_id === seg.id) memberCount++;
            });

            html +=
                '<div class="segment-card" data-seg-id="' + esc(seg.id) + '">' +
                    '<div class="segment-card-header">' +
                        '<strong>' + esc(seg.id) + '</strong>' +
                        '<span class="segment-meta">' + esc(seg.name || '') + ' &middot; ' + memberCount + ' containers</span>' +
                    '</div>' +
                    '<div class="segment-card-actions">' +
                        '<button class="btn btn-sm seg-set-limit" data-seg="' + esc(seg.id) + '">Set Limit</button>' +
                        '<button class="btn btn-sm seg-view" data-seg="' + esc(seg.id) + '">View</button>' +
                        '<button class="btn btn-sm btn-danger seg-delete" data-seg="' + esc(seg.id) + '">Delete</button>' +
                    '</div>' +
                '</div>';
        });
        segmentsList.innerHTML = html;

        segmentsList.querySelectorAll('.seg-set-limit').forEach(function (btn) {
            btn.addEventListener('click', function () {
                openSegmentLimitDialog(btn.dataset.seg);
            });
        });

        segmentsList.querySelectorAll('.seg-view').forEach(function (btn) {
            btn.addEventListener('click', function () {
                window.location.href = '?segment=' + encodeURIComponent(btn.dataset.seg);
            });
        });

        segmentsList.querySelectorAll('.seg-delete').forEach(function (btn) {
            btn.addEventListener('click', function () {
                var segId = btn.dataset.seg;
                if (confirm('Delete segment "' + segId + '"?')) {
                    api('DELETE', '/segments/' + segId).then(function () {
                        toast('Segment deleted: ' + segId);
                        refresh();
                    }).catch(function (err) {
                        toast('Delete failed: ' + err.message, true);
                    });
                }
            });
        });
    }

    // Create segment
    document.getElementById('btn-create-segment').addEventListener('click', function () {
        document.getElementById('segment-id').value = '';
        document.getElementById('segment-name').value = '';
        document.getElementById('create-segment-dialog').showModal();
    });

    document.getElementById('create-segment-dialog').addEventListener('close', function () {
        if (this.returnValue === 'cancel') return;
        var id = document.getElementById('segment-id').value.trim();
        var name = document.getElementById('segment-name').value.trim() || id;
        if (!id) return;

        api('POST', '/segments', { id: id, name: name }).then(function () {
            toast('Segment created: ' + id);
            refresh();
        }).catch(function (err) {
            toast('Create failed: ' + err.message, true);
        });
    });

    // Segment limit dialog
    function openSegmentLimitDialog(segId) {
        document.getElementById('segment-limit-seg-id').value = segId;
        document.getElementById('segment-limit-value').value = '';
        document.getElementById('segment-limit-dialog').showModal();
    }

    document.getElementById('segment-limit-dialog').addEventListener('close', function () {
        if (this.returnValue === 'cancel') return;
        var segId = document.getElementById('segment-limit-seg-id').value;
        var type = document.getElementById('segment-limit-type').value;
        var operation = document.getElementById('segment-limit-operation').value;
        var rawValue = document.getElementById('segment-limit-value').value.trim();
        if (!rawValue) return;

        var value = parseValue(type, rawValue);
        if (isNaN(value)) {
            toast('Invalid value: ' + rawValue, true);
            return;
        }

        api('PUT', '/segments/' + segId + '/limits', {
            type: type, value: value, operation: operation
        }).then(function () {
            toast('Segment limit ' + operation + ': ' + type);
            refresh();
        }).catch(function (err) {
            toast('Segment limit failed: ' + err.message, true);
        });
    });

    // Assign segment dialog
    function openAssignDialog(containerID) {
        document.getElementById('assign-container-id').value = containerID;
        var sel = document.getElementById('assign-segment-select');
        sel.innerHTML = '<option value="">(none)</option>';
        segments.forEach(function (seg) {
            sel.innerHTML += '<option value="' + esc(seg.id) + '">' + esc(seg.id) + ' - ' + esc(seg.name) + '</option>';
        });
        // Pre-select current segment
        containers.forEach(function (cs) {
            if (cs.container.id === containerID && cs.container.segment_id) {
                sel.value = cs.container.segment_id;
            }
        });
        document.getElementById('assign-segment-dialog').showModal();
    }

    document.getElementById('assign-segment-dialog').addEventListener('close', function () {
        if (this.returnValue === 'cancel') return;
        var containerID = document.getElementById('assign-container-id').value;
        var segId = document.getElementById('assign-segment-select').value;

        // Find current segment
        var currentSeg = '';
        containers.forEach(function (cs) {
            if (cs.container.id === containerID) currentSeg = cs.container.segment_id || '';
        });

        if (segId === currentSeg) return; // No change

        var promise;
        if (currentSeg && segId) {
            // Unassign from old, then assign to new
            promise = api('POST', '/segments/' + currentSeg + '/containers/' + containerID + '/unassign')
                .then(function () { return api('POST', '/segments/' + segId + '/containers/' + containerID + '/assign'); });
        } else if (segId) {
            promise = api('POST', '/segments/' + segId + '/containers/' + containerID + '/assign');
        } else if (currentSeg) {
            promise = api('POST', '/segments/' + currentSeg + '/containers/' + containerID + '/unassign');
        } else {
            return;
        }

        promise.then(function () {
            toast(segId ? 'Assigned to ' + segId : 'Unassigned');
            refresh();
        }).catch(function (err) {
            toast('Assign failed: ' + err.message, true);
        });
    });

    // --- Init ---
    // Set scope badge and title
    var scopeBadge = document.getElementById('scope-badge');
    if (isSegmentScoped && scopeBadge) {
        scopeBadge.innerHTML = '<a href="/" style="color:var(--accent);text-decoration:none;" title="Back to all containers">\u2190 ' + esc(segmentScope) + '</a>';
        scopeBadge.style.cssText = 'font-size:0.6em;font-weight:normal;';
        document.title = 'DDL - ' + segmentScope;
    }
    // Update panel header
    var globalPanelHeader = document.querySelector('#global-panel h2');
    if (globalPanelHeader) {
        globalPanelHeader.textContent = isSegmentScoped ? 'Segment Limits' : 'Host Limits';
    }

    refresh();
    startAutoRefresh();
})();

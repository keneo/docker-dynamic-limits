(function () {
    'use strict';

    // --- State ---
    let containers = [];
    let selectedID = null;
    let refreshTimer = null;

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
                return '$' + (v / 100).toFixed(2);
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
                return Math.round(parseFloat(s) * 100);
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
    function api(method, path, body) {
        var opts = { method: method, headers: {} };
        if (body !== undefined) {
            opts.headers['Content-Type'] = 'application/json';
            opts.body = JSON.stringify(body);
        }
        return fetch('/api' + path, opts).then(function (resp) {
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
            return;
        }
        emptyMsg.hidden = true;

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
            if (enforcedCount > 0) tr.className = 'enforced-row';

            var state = cs.state || 'unknown';
            var stateClass = 'state-badge state-' + state;

            tr.innerHTML =
                '<td><code>' + esc(c.id) + '</code></td>' +
                '<td>' + esc(c.name || '-') + '</td>' +
                '<td><span class="' + stateClass + '">' + esc(state) + '</span></td>' +
                '<td>' + limitCount + '</td>' +
                '<td>' + enforcedCount + (enforcedCount > 0 ? ' <span class="enforced-badge">ENFORCED</span>' : '') + '</td>' +
                '<td class="actions"></td>';

            var actions = tr.querySelector('.actions');

            var detailBtn = document.createElement('button');
            detailBtn.className = 'btn btn-sm';
            detailBtn.textContent = 'Details';
            detailBtn.onclick = function () { selectContainer(c.id); };
            actions.appendChild(detailBtn);

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
            containers = data || [];
            offlineBanner.hidden = true;
            statusIndicator.className = 'status connected';
            statusIndicator.title = 'Connected';
            lastRefreshEl.textContent = new Date().toLocaleTimeString();
            renderContainers();
            renderDetail();
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

    // --- Actions ---
    function selectContainer(id) {
        selectedID = id;
        renderDetail();
    }

    function cloneContainer(id) {
        api('POST', '/containers/' + id + '/clone', {}).then(function (result) {
            toast('Cloned: ' + result.id);
            refresh();
        }).catch(function (err) {
            toast('Clone failed: ' + err.message, true);
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

    function openLimitDialog(containerID, type, operation) {
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
            api('POST', '/register', { container_id: containerID }).then(function (result) {
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
            var containerID = document.getElementById('limit-container-id').value;
            var type = document.getElementById('limit-type').value;
            var operation = document.getElementById('limit-operation').value;
            var rawValue = document.getElementById('limit-value').value.trim();
            if (!rawValue) return;

            var value = parseValue(type, rawValue);
            if (isNaN(value)) {
                toast('Invalid value: ' + rawValue, true);
                return;
            }

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
    });

    document.getElementById('detail-close').addEventListener('click', function () {
        selectedID = null;
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

    // --- Init ---
    refresh();
    startAutoRefresh();
})();

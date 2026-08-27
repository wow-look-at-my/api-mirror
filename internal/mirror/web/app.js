// The dashboard.
//
// Plain ES modules, no build step and no dependencies. The page is served from
// the binary, so a toolchain between the source and what ships would be one
// more thing that can be stale in a way nothing checks.

const token = new URLSearchParams(location.search).get('token') || '';

// api fetches one admin endpoint, carrying the token the page was opened with.
async function api(path, options = {}) {
	const headers = { ...(options.headers || {}) };
	if (token) headers['X-Mirror-Token'] = token;
	const resp = await fetch(path.replace(/^\//, ''), { ...options, headers });
	if (!resp.ok) throw new Error(`${path}: ${resp.status} ${await resp.text()}`);
	if (resp.status === 204) return null;
	return resp.json();
}

// el builds one element. Children may be nodes or text; an object of attributes
// is applied first. It is here so no view has to touch innerHTML with a value
// that came off the wire.
function el(tag, attrs = {}, ...children) {
	const node = document.createElement(tag);
	for (const [k, v] of Object.entries(attrs)) {
		if (v === null || v === undefined || v === false) continue;
		if (k === 'class') node.className = v;
		else if (k.startsWith('on')) node.addEventListener(k.slice(2), v);
		else node.setAttribute(k, v === true ? '' : String(v));
	}
	for (const c of children.flat()) {
		if (c === null || c === undefined || c === false) continue;
		node.append(c instanceof Node ? c : document.createTextNode(String(c)));
	}
	return node;
}

const fmt = {
	int: (n) => (n ?? 0).toLocaleString(),
	bytes(n) {
		if (!n) return '0 B';
		const units = ['B', 'kB', 'MB', 'GB'];
		let i = 0;
		while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
		return `${n < 10 && i > 0 ? n.toFixed(1) : Math.round(n)} ${units[i]}`;
	},
	// Durations arrive as nanoseconds, which is what Go writes and what keeps
	// the wire honest about sub-millisecond work.
	dur(ns) {
		if (!ns) return '-';
		const ms = ns / 1e6;
		if (ms < 1) return `${(ns / 1000).toFixed(0)}us`;
		if (ms < 1000) return `${ms.toFixed(ms < 10 ? 1 : 0)}ms`;
		return `${(ms / 1000).toFixed(1)}s`;
	},
	when(iso) {
		if (!iso || iso.startsWith('0001-')) return '-';
		const d = new Date(iso);
		if (Number.isNaN(d.getTime())) return '-';
		return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' });
	},
	ago(iso) {
		if (!iso || iso.startsWith('0001-')) return 'never';
		const secs = (Date.now() - new Date(iso).getTime()) / 1000;
		if (!Number.isFinite(secs)) return 'never';
		const past = secs >= 0;
		const s = Math.abs(secs);
		const unit = s < 60 ? [s, 's'] : s < 3600 ? [s / 60, 'm'] : s < 86400 ? [s / 3600, 'h'] : [s / 86400, 'd'];
		const text = `${Math.round(unit[0])}${unit[1]}`;
		return past ? `${text} ago` : `in ${text}`;
	},
};

function tile(label, value, note) {
	return el('div', { class: 'tile' },
		el('div', { class: 'label' }, label),
		el('div', { class: 'value' }, value),
		note ? el('div', { class: 'note' }, note) : null);
}

function panel(...children) {
	return el('div', { class: 'panel scroll' }, ...children);
}

function table(headers, rows) {
	if (!rows.length) return panel(el('div', { class: 'empty' }, 'Nothing yet.'));
	return panel(el('table', {},
		el('thead', {}, el('tr', {}, headers.map((h) => el('th', {}, h)))),
		el('tbody', {}, rows.map((cells) => el('tr', {}, cells.map((c) =>
			el('td', { class: typeof c === 'number' ? 'num' : null },
				typeof c === 'number' ? fmt.int(c) : c)))))));
}

function pill(text) {
	return el('span', { class: `pill ${String(text).toLowerCase()}` }, text);
}

function section(title, ...body) {
	return el('section', {}, el('h2', {}, title), ...body);
}

// dispositions renders one group's tally as a row of pills, so a shape that is
// half hit and half passthrough reads as exactly that.
function dispositions(map) {
	const entries = Object.entries(map || {}).filter(([, n]) => n > 0);
	if (!entries.length) return el('span', { class: 'muted' }, '-');
	entries.sort((a, b) => b[1] - a[1]);
	return el('span', { class: 'row' }, entries.map(([k, n]) =>
		el('span', { class: `pill ${k}` }, `${k} ${fmt.int(n)}`)));
}

const views = {};

views.overview = async () => {
	const o = await api('api/overview');
	document.getElementById('title').textContent = o.title || o.mirror;
	document.getElementById('upstream').textContent = o.upstream;

	const total = o.answered + o.passthrough;
	const modelled = total ? Math.round((o.answered / total) * 100) : 0;
	const out = el('div', {});

	out.append(section('This run', el('div', { class: 'tiles' },
		tile('Answered', fmt.int(o.answered), 'from stored state'),
		tile('Passed through', fmt.int(o.passthrough), 'still unmodelled'),
		tile('Modelled', `${modelled}%`, 'of requests answered here'),
		tile('Pulled upstream', fmt.bytes(o.upstream_bytes), 'this process'),
		tile('Principals', fmt.int(o.principals), `${fmt.int(o.denials)} live denials`),
		tile('Deliveries', fmt.int(o.deliveries.total), `last ${fmt.ago(o.deliveries.last)}`),
		tile('Started', fmt.ago(o.started), o.fingerprint.slice(0, 12)))));

	out.append(section('Traffic by lane', table(
		['Lane', 'Requests', 'Errors', 'Bytes', 'Mean', 'Last'],
		(o.lanes || []).map((l) => [pill(l.lane), l.count, l.errors, fmt.bytes(l.bytes),
			fmt.dur(l.mean_ns), fmt.ago(l.last)]))));

	out.append(section('Cache contents', table(
		['Kind', 'Keys held', 'Errored', 'Newest fetch'],
		(o.kinds || []).map((k) => [el('span', { class: 'mono' }, k.kind), k.rows, k.errored, fmt.ago(k.newest)]))));

	out.append(section('Background work', table(
		['Job', 'State', 'Detail'],
		[
			['Refresh', o.refresh.enabled ? pill('ok') : el('span', { class: 'muted' }, 'not declared'),
				o.refresh.enabled ? `every ${o.refresh.interval}, ${fmt.int(o.refresh.cycles)} cycles, ${fmt.int(o.refresh.swept)} swept, ${fmt.int(o.refresh.errors)} errors` : '-'],
			// A declared replayer that is not running has a reason, and `off`
			// carries it. Reading that state as "not declared" is the mystery
			// the field exists to prevent.
			['Replay',
				o.replay.enabled ? pill('ok')
					: o.replay.off ? pill('off')
						: el('span', { class: 'muted' }, 'not declared'),
				o.replay.enabled ? `every ${o.replay.interval}, ${fmt.int(o.replay.resent)} re-sent of ${fmt.int(o.replay.found)} listed`
					: o.replay.off ? `declared, but ${o.replay.off} is empty -- a lost delivery stays lost`
						: '-'],
			['Notify', o.notify.enabled ? pill('ok') : el('span', { class: 'muted' }, 'not declared'),
				o.notify.enabled ? `${fmt.int(o.notify.subscriptions)} subscriptions, ${fmt.int(o.notify.sent)} sent, ${fmt.int(o.notify.failed)} failed` : '-'],
		])));
	return out;
};

views.requests = async () => {
	const v = await api('api/requests');
	const out = el('div', {});
	out.append(section('By route shape', table(
		['Method', 'Shape', 'Count', 'Dispositions', 'Mean', 'Bytes', 'Last'],
		v.groups.map((g) => [g.method, el('span', { class: 'mono wrap' }, g.shape), g.count,
			dispositions(g.dispositions), fmt.dur(v.means_ns[`${g.method} ${g.shape}`]),
			fmt.bytes(g.bytes), fmt.ago(g.last)]))));

	out.append(section('Recent', table(
		['At', 'Method', 'Path', 'Disposition', 'Reason', 'Status', 'Took', 'Principal'],
		v.recent.map((r) => [fmt.when(r.at), r.method, el('span', { class: 'mono wrap' }, r.path),
			pill(r.disposition), r.reason || '-', r.status, fmt.dur(r.duration_ns),
			el('span', { class: 'mono' }, r.principal || '-')]))));
	return out;
};

views.passthrough = async () => {
	const v = await api('api/brief');
	const out = el('div', {});
	out.append(section('What is still leaving', el('p', { class: 'muted' },
		`A passthrough is unfinished work, not a settled state. ${fmt.int(v.total)} requests have been forwarded rather than answered here.`)));
	if (!v.items.length) {
		out.append(panel(el('div', { class: 'empty' }, 'Nothing has been forwarded. Every request this mirror saw, it answered.')));
		return out;
	}
	for (const item of v.items) {
		out.append(section(`${item.method} ${item.shape}`,
			el('div', { class: 'row' },
				el('span', {}, `${fmt.int(item.count)} forwarded`),
				dispositions(item.reasons),
				item.samples && item.samples.length
					? el('span', { class: 'muted mono' }, `e.g. ${item.samples.join('  ')}`)
					: null),
			el('pre', {}, item.sketch)));
	}
	return out;
};

views.timeline = async () => {
	const v = await api('api/timeline');
	const out = el('div', {});
	const s = v.stats;
	out.append(section('The ring', el('div', { class: 'tiles' },
		tile('Frames', fmt.int(s.frames), `window ${s.window}`),
		tile('Dropped', fmt.int(s.dropped), s.dropped ? 'oldest frames evicted' : 'nothing lost'),
		tile('Since', fmt.ago(s.since), 'resets on restart'))));

	if (!v.frames.length) {
		out.append(panel(el('div', { class: 'empty' }, 'No traffic recorded yet.')));
		return out;
	}

	// One row per lane, bars positioned by time. Everything the mirror
	// exchanged is here; a gap in a lane is a real gap, not a filter.
	const lanes = new Map();
	for (const f of v.frames) {
		if (!lanes.has(f.lane)) lanes.set(f.lane, []);
		lanes.get(f.lane).push(f);
	}
	const times = v.frames.map((f) => new Date(f.at).getTime());
	const first = Math.min(...times);
	const last = Math.max(...times, Date.now());
	const span = Math.max(last - first, 1);

	const rows = el('div', { class: 'lanes' });
	for (const [lane, frames] of [...lanes].sort((a, b) => b[1].length - a[1].length)) {
		const track = el('div', { class: 'track' });
		for (const f of frames) {
			const left = ((new Date(f.at).getTime() - first) / span) * 100;
			const width = Math.max((f.duration_ns / 1e6 / span) * 100, 0.4);
			track.append(el('div', {
				class: `bar${f.error || f.status >= 400 ? ' err' : ''}`,
				style: `left:${left}%;width:${width}%`,
				title: `${f.method} ${f.path} -> ${f.status || 'error'} (${fmt.dur(f.duration_ns)})`,
			}));
		}
		rows.append(el('div', { class: 'lane' },
			el('span', {}, pill(lane), ' ', el('span', { class: 'muted' }, fmt.int(frames.length))),
			track));
	}
	out.append(section('Everything exchanged', rows,
		el('div', { class: 'axis' },
			el('span', {}, new Date(first).toLocaleTimeString()),
			el('span', {}, new Date(last).toLocaleTimeString()))));

	const recent = v.frames.slice(-100).reverse();
	out.append(section('Latest frames', table(
		['At', 'Lane', 'Method', 'Path', 'Status', 'Bytes', 'Took', 'Detail'],
		recent.map((f) => [fmt.when(f.at), pill(f.lane), f.method,
			el('span', { class: 'mono wrap' }, f.path), f.error ? pill('error') : f.status,
			fmt.bytes(f.bytes), fmt.dur(f.duration_ns), f.error || f.detail || '-']))));
	return out;
};

views.rates = async () => {
	const v = await api('api/rates');
	const out = el('div', {});
	if (!v.declared) {
		out.append(panel(el('div', { class: 'empty' },
			'This spec names no rate-limit headers, so no budget can be read. Declare <ratelimit> on <upstream> to fill this tab.')));
		return out;
	}
	out.append(section('Budgets observed', table(
		['Principal', 'Resource', 'Remaining', 'Limit', 'Used', 'Resets', 'Seen'],
		v.budgets.map((b) => {
			const share = b.limit ? (b.remaining / b.limit) * 100 : 0;
			const meter = el('div', { class: `meter${share < 20 ? ' low' : ''}` },
				el('span', { style: `width:${Math.max(share, 2)}%` }));
			return [
				el('span', { class: 'mono' }, b.principal),
				b.resource,
				el('div', { class: 'row' }, meter, el('span', { class: 'num' }, fmt.int(b.remaining))),
				b.limit,
				b.used,
				// A reset already in the past is said plainly. Rendering it as
				// "resets now" would read as a budget about to refresh.
				b.stale ? el('span', { class: 'muted' }, `${fmt.ago(b.reset)} - stale`) : fmt.ago(b.reset),
				fmt.ago(b.observed_at),
			];
		}))));
	return out;
};

views.webhooks = async () => {
	const v = await api('api/events');
	const out = el('div', {});
	if (!v.configured) {
		out.append(panel(el('div', { class: 'empty' }, 'This spec declares no <events>, so nothing tells this mirror what changed.')));
		return out;
	}
	const s = v.stats;
	out.append(section('Deliveries', el('div', { class: 'tiles' },
		tile('Received', fmt.int(s.total), `at ${v.path}`),
		tile('Last', fmt.ago(s.last), `reorder window ${s.reorder_window}`),
		tile('Since', fmt.ago(s.since), 'counts reset on restart'))));

	out.append(section('Outcomes', el('div', { class: 'row' }, dispositions(s.dispositions))));

	// Declared types with a zero are the point of this table: a type the
	// provider was never subscribed to looks exactly like a quiet week.
	const seen = new Map((s.types || []).map((t) => [t.type, t.count]));
	out.append(section('Event types', table(
		['Type', 'Resource', 'Clock', 'Sets', 'Received'],
		v.declared.map((d) => [
			el('span', { class: 'mono' }, d.type),
			d.resource,
			d.invalidate ? el('span', { class: 'muted' }, `invalidates: ${d.invalidate}`)
				: (d.unordered ? el('span', { class: 'muted' }, 'unordered') : el('span', { class: 'mono' }, d.clock)),
			d.sets,
			seen.get(d.type) || 0,
		]))));

	out.append(section('Delivery-gap replay', v.replay.enabled
		? table(['Interval', 'Cycles', 'Listed', 'Re-sent', 'Errors', 'Last'],
			[[v.replay.interval, v.replay.cycles, v.replay.found, v.replay.resent, v.replay.errors, fmt.ago(v.replay.last)]])
		: panel(el('div', { class: 'empty' },
			'No <replay> declared. A delivery the provider could not hand over is never re-sent, and nothing reports the gap.'))));
	return out;
};

views.resources = async () => {
	const params = new URLSearchParams(location.hash.split('?')[1] || '');
	const want = params.get('resource') || '';
	const list = await api(`api/resources${want ? `?resource=${encodeURIComponent(want)}` : ''}`);
	const out = el('div', {});
	out.append(section('Declared resources', table(
		['Resource', 'Store', 'TTL', 'Keys', 'Fields', 'Reveal', 'Routes'],
		list.map((r) => [
			el('a', { href: `#resources?resource=${encodeURIComponent(r.name)}`, onclick: () => setTimeout(render, 0) }, r.name),
			r.store,
			r.ttl || '-',
			el('span', { class: 'mono' }, r.keys.join(', ')),
			r.fields.length,
			revealSummary(r.reveal),
			el('span', { class: 'mono wrap' }, r.routes.join('  ')),
		]))));

	const chosen = list.find((r) => r.name === want);
	if (chosen) {
		out.append(section(`${chosen.name} columns`, table(
			['Column', 'Type', 'From'],
			chosen.fields.map((f) => [el('span', { class: 'mono' }, f.name), f.type,
				el('span', { class: 'mono' }, f.from || '(expr)')]))));
		const rows = chosen.rows || [];
		const cols = rows.length ? Object.keys(rows[0]) : [];
		out.append(section(`${chosen.name} rows${chosen.truncated ? ' (truncated)' : ''}`,
			table(cols, rows.map((row) => cols.map((c) => el('span', { class: 'mono wrap' }, String(row[c] ?? '')))))));
	}
	return out;
};

function revealSummary(rv) {
	if (!rv) return el('span', { class: 'muted' }, 'none');
	if (rv.credential) return pill('credential');
	const parts = [];
	if (rv.public) parts.push('public predicate');
	if (rv.probe) parts.push('probe');
	if (rv.grant_ttl) parts.push(`grant ${rv.grant_ttl}`);
	if (rv.deny_ttl) parts.push(`deny ${rv.deny_ttl}`);
	return el('span', { class: 'muted' }, parts.join(', ') || 'none');
}

views.principals = async () => {
	const params = new URLSearchParams(location.hash.split('?')[1] || '');
	const want = params.get('principal') || '';
	const v = await api(`api/principals${want ? `?principal=${encodeURIComponent(want)}` : ''}`);
	const out = el('div', {});
	out.append(section('Who has proven what', el('p', { class: 'muted' },
		`Grants and denials are the only per-caller tables. Everything else this mirror stores is global. ${fmt.int(v.denials)} refusals are currently being replayed without asking upstream.`)));
	out.append(table(['Principal', 'Grants', 'Newest'],
		v.principals.map((p) => [
			el('a', { href: `#principals?principal=${encodeURIComponent(p.principal)}`, onclick: () => setTimeout(render, 0) },
				el('span', { class: 'mono' }, p.principal)),
			p.grants, fmt.ago(p.newest)])));

	if (v.standing) {
		out.append(section(`${v.standing.principal} grants`, table(
			['Resource', 'Key', 'Source', 'Expires'],
			v.standing.grants.map((g) => [g.resource, el('span', { class: 'mono wrap' }, g.key), g.source, fmt.ago(g.expires_at)]))));
		out.append(section(`${v.standing.principal} denials`, table(
			['Resource', 'Key', 'Status', 'Expires'],
			v.standing.denials.map((d) => [d.resource, el('span', { class: 'mono wrap' }, d.key), d.status, fmt.ago(d.expires_at)]))));
	}
	return out;
};

views.subscriptions = async () => {
	const out = el('div', {});
	let list;
	try {
		list = await api('api/subscriptions');
	} catch (e) {
		out.append(panel(el('div', { class: 'empty' },
			'This spec declares no <notify>, so consumers cannot ask to be told when a delivery lands.')));
		return out;
	}
	out.append(section('Registered consumers', el('p', { class: 'muted' },
		'A subscriber is told AFTER a delivery is applied, so they stop racing this mirror’s ingestion with their own copy of the upstream’s webhooks.')));
	out.append(table(['Principal', 'URL', 'Events', 'State', 'Failures', 'Last delivered'],
		list.map((s) => [
			el('span', { class: 'mono' }, s.principal),
			el('span', { class: 'mono wrap' }, s.url),
			(s.events || []).join(', ') || el('span', { class: 'muted' }, 'all'),
			s.disabled ? pill('error') : pill('ok'),
			s.failures,
			el('span', {}, fmt.ago(s.last_ok), s.last_error ? el('div', { class: 'muted wrap' }, s.last_error) : null),
		])));
	return out;
};

views.spec = async () => {
	const v = await api('api/spec');
	return el('div', {},
		section('Schema fingerprint', el('pre', {}, v.fingerprint)),
		section('Derived DDL', el('pre', {}, v.ddl)));
};

const tabs = [
	['overview', 'Overview'],
	['requests', 'Requests'],
	['passthrough', 'Passthrough'],
	['timeline', 'Timeline'],
	['rates', 'Rate limit'],
	['webhooks', 'Webhooks'],
	['resources', 'Resources'],
	['principals', 'Principals'],
	['subscriptions', 'Subscriptions'],
	['spec', 'Schema'],
];

function currentTab() {
	const name = location.hash.replace('#', '').split('?')[0];
	return views[name] ? name : 'overview';
}

function drawTabs() {
	const nav = document.getElementById('tabs');
	nav.replaceChildren(...tabs.map(([id, label]) => el('button', {
		type: 'button',
		role: 'tab',
		'aria-selected': id === currentTab(),
		onclick: () => { location.hash = id; },
	}, label)));
}

async function render() {
	drawTabs();
	const view = document.getElementById('view');
	try {
		view.replaceChildren(await views[currentTab()]());
	} catch (e) {
		// A failed panel says so. A tab that silently stays on its last content
		// is a tab that lies about how current it is.
		view.replaceChildren(el('div', { class: 'err' }, String(e.message || e)));
	}
	document.getElementById('clock').textContent = `updated ${new Date().toLocaleTimeString()}`;
}

window.addEventListener('hashchange', render);
document.getElementById('reload').addEventListener('click', render);
render();
// The page re-reads on a fixed cadence. It is a live view of a running process,
// so a stale one is worse than a slow one.
setInterval(render, 15000);

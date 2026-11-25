// The workflow canvas. Drawflow draws the nodes and links and handles dragging and zoom; this adds
// what is Autobot's: a right-click menu to place nodes, a click to open a node's settings, named
// outputs, and saving every change. The server checks every save (links may fan out and merge but
// not loop), keeps each node's settings, and refuses a save made from an out-of-date copy.
(function () {
  'use strict';
  var root = document.getElementById('wf');
  if (!root || typeof Drawflow === 'undefined') { return; }

  var state = JSON.parse(root.dataset.state);
  var base = root.dataset.base, csrf = root.dataset.csrf;
  var menu = document.getElementById('wf-menu');
  var ghost = document.getElementById('wf-ghost');
  var modal = document.getElementById('wf-modal');
  var statusEl = document.getElementById('wf-status');
  var types = {};
  state.types.forEach(function (t) { types[t.type] = t; });

  var editor = new Drawflow(root);
  editor.reroute = false;
  editor.zoom_min = 0.3; editor.zoom_max = 2;
  editor.zoom_enter = function () {};   // the wheel zooms by our rule below, not only with Ctrl
  editor.start();

  // The dot grid is the canvas's background, so it is moved and scaled with the canvas: a drag on
  // the background then plainly pans everything, rather than seeming to move a node. (Drawflow
  // reads the first class of the element pressed, so "parent-drawflow" comes first in the page.)
  // Drawflow scales about the canvas's centre, so the canvas's origin sits at
  // translate + half its size × (1 − zoom).
  function moveGrid(x, y) {
    var z = editor.zoom, size = 22 * z, pc = editor.precanvas;
    root.style.backgroundSize = size + 'px ' + size + 'px';
    root.style.backgroundPosition = (x + pc.clientWidth / 2 * (1 - z)) + 'px ' + (y + pc.clientHeight / 2 * (1 - z)) + 'px';
  }
  editor.on('translate', function (p) { moveGrid(p.x, p.y); keepView(editor.zoom, p.x, p.y); });

  // The wheel zooms about the point under the mouse, which stays where it is.
  root.addEventListener('wheel', function (e) {
    e.preventDefault();
    var z = Math.min(editor.zoom_max, Math.max(editor.zoom_min, editor.zoom * Math.exp(-e.deltaY * 0.002)));
    if (z === editor.zoom) { return; }
    var at = canvasPoint(e.clientX, e.clientY);
    editor.zoom = z;
    editor.zoom_refresh();
    var r = editor.precanvas.getBoundingClientRect();
    editor.canvas_x += e.clientX - (r.x + at.x * z);
    editor.canvas_y += e.clientY - (r.y + at.y * z);
    editor.precanvas.style.transform = 'translate(' + editor.canvas_x + 'px, ' + editor.canvas_y + 'px) scale(' + z + ')';
    moveGrid(editor.canvas_x, editor.canvas_y);
    keepView(z, editor.canvas_x, editor.canvas_y);
  }, { passive: false });

  var dfToID = {}, idToDf = {};   // Drawflow's numeric ids and ours
  var loading = true;

  function newID() {
    var s = '';
    while (s.length < 12) { s += Math.random().toString(36).slice(2); }
    return s.slice(0, 12);
  }

  function nodeEl(dfid) { return document.getElementById('node-' + dfid); }

  // Node content is fixed markup; the label and summary are set as text, never as HTML.
  function addNode(id, type, x, y) {
    var t = types[type];
    var outs = (state.outputs || {})[id] || t.outputs || ['out'];
    var dfid = editor.addNode(type, t.event ? 0 : 1, outs.length, x, y, 'wf-' + type, { id: id },
      '<div class="wf-node"><div class="wf-name"></div><div class="wf-title"></div><div class="wf-sub"></div><div class="wf-ref"></div></div>');
    dfToID[dfid] = id; idToDf[id] = dfid;
    var el = nodeEl(dfid);
    el.querySelector('.wf-title').textContent = t.label;
    if (!state.outputs) { state.outputs = {}; }
    state.outputs[id] = outs;
    labelPorts(id);
    setSummary(id);
    return dfid;
  }

  // A node with named outputs (a decision's choices) shows each name beside its port.
  function labelPorts(id) {
    var outs = outputsOf(id), ports = nodeEl(idToDf[id]).querySelectorAll('.outputs .output');
    ports.forEach(function (p, i) {
      if (outs.length > 1 || outs[0] !== 'out') { p.dataset.label = outs[i] || ''; } else { delete p.dataset.label; }
    });
  }

  // After a save, a node's outputs and the links from them are set to what the server stored: a
  // decision's choices may have changed, and links from choices it no longer has are gone.
  function syncPorts(id, links) {
    var dfid = idToDf[id], outs = outputsOf(id);
    if (!dfid) { return; }
    loading = true;   // these changes are the server's own; nothing to save
    var n = editor.getNodeFromId(dfid);
    Object.keys(n.outputs).forEach(function (cls) {
      n.outputs[cls].connections.slice().forEach(function (c) {
        editor.removeSingleConnection(dfid, c.node, cls, c.output);
      });
    });
    var have = Object.keys(editor.getNodeFromId(dfid).outputs).length;
    for (; have < outs.length; have++) { editor.addNodeOutput(dfid); }
    for (; have > outs.length; have--) { editor.removeNodeOutput(dfid, 'output_' + have); }
    links.forEach(function (l) {
      if (l.from === id && idToDf[l.to]) { editor.addConnection(dfid, idToDf[l.to], outClass(id, l.out), 'input_1'); }
    });
    labelPorts(id);
    loading = false; lastLink = null;
  }

  // The name above a node and the summary inside it come from the server's last answer.
  function setSummary(id) {
    var el = nodeEl(idToDf[id]);
    if (!el) { return; }
    el.querySelector('.wf-sub').textContent = state.summaries[id] || '';
    // A block that continues another's conversation has a dashed border and says whose.
    var ref = (state.continues || {})[id] || '';
    el.classList.toggle('wf-reference', !!ref);
    el.querySelector('.wf-ref').textContent = ref;
    // A block whose templates use a tag the rules do not allow is marked, with the reason beside its summary.
    var bad = (state.problems || {})[id] || '';
    el.classList.toggle('wf-problem', !!bad);
    el.querySelector('.wf-sub').title = bad;
    el.querySelector('.wf-name').textContent = (state.names || {})[id] || types[editor.getNodeFromId(idToDf[id]).name].label;
  }

  function show(msg, bad) {
    statusEl.textContent = msg;
    statusEl.className = 'wf-status' + (bad ? ' bad' : '');
  }

  // The bar says what is wrong with the blocks' templates, naming each block, tag and reason (the
  // server's text), after a save (prefix "Saved") or when the page loads (prefix "").
  function showSaved(prefix) {
    var list = Object.keys(state.problems || {}).map(function (id) { return state.problems[id]; });
    if (!list.length) { show(prefix || ''); return; }
    var text = list.slice(0, 3).join(' · ') + (list.length > 3 ? ' · and ' + (list.length - 3) + ' more' : '');
    show((prefix ? prefix + '. ' : '') + 'Needs a fix: ' + text, true);
  }

  // ---- loading the saved workflow ----
  state.graph.nodes.forEach(function (n) { addNode(n.id, n.type, n.x, n.y); });
  state.graph.links.forEach(function (l) {
    if (idToDf[l.from] && idToDf[l.to]) { editor.addConnection(idToDf[l.from], idToDf[l.to], outClass(l.from, l.out), 'input_1'); }
  });
  loading = false;
  showSaved('');

  // ---- Fit: the whole workflow in view, centred, at no more than full size ----
  function setView(z, x, y) {
    editor.zoom = editor.zoom_last_value = z;
    editor.canvas_x = x; editor.canvas_y = y;
    editor.precanvas.style.transform = 'translate(' + x + 'px, ' + y + 'px) scale(' + z + ')';
    moveGrid(x, y);
    keepView(z, x, y);
  }

  // The view (pan and zoom) is remembered in this browser, per workflow, so a reload keeps it.
  var viewKey = 'autobot-view:' + base, viewTimer = null;
  function keepView(z, x, y) {
    clearTimeout(viewTimer);
    viewTimer = setTimeout(function () {
      try { localStorage.setItem(viewKey, JSON.stringify({ z: z, x: x, y: y })); } catch (e) { /* not kept */ }
    }, 200);
  }
  function keptView() {
    try {
      var v = JSON.parse(localStorage.getItem(viewKey));
      if (v && isFinite(v.z) && isFinite(v.x) && isFinite(v.y)) { return v; }
    } catch (e) { /* none */ }
    return null;
  }

  // The nodes' extent on the canvas, the name above each included, or null when there are none.
  function extent() {
    var box = null;
    Object.keys(dfToID).forEach(function (dfid) {
      var el = nodeEl(dfid), name = el.querySelector('.wf-name');
      var x0 = el.offsetLeft, y0 = el.offsetTop - (name ? name.offsetHeight : 0);
      var x1 = el.offsetLeft + el.offsetWidth, y1 = el.offsetTop + el.offsetHeight;
      box = box ? { x0: Math.min(box.x0, x0), y0: Math.min(box.y0, y0), x1: Math.max(box.x1, x1), y1: Math.max(box.y1, y1) } : { x0: x0, y0: y0, x1: x1, y1: y1 };
    });
    return box;
  }

  function fit() {
    var b = extent(), pc = editor.precanvas;
    if (!b) { setView(1, 0, 0); return; }
    var pad = 60, W = root.clientWidth, H = root.clientHeight;
    var z = Math.max(editor.zoom_min, Math.min(1, (W - 2 * pad) / (b.x1 - b.x0), (H - 2 * pad) / (b.y1 - b.y0)));
    // A canvas point c shows at translate + half the canvas × (1 − zoom) + c × zoom.
    var x = W / 2 - (b.x0 + b.x1) / 2 * z - pc.clientWidth / 2 * (1 - z);
    var y = H / 2 - (b.y0 + b.y1) / 2 * z - pc.clientHeight / 2 * (1 - z);
    setView(z, x, y);
  }
  document.getElementById('wf-fit').addEventListener('click', fit);

  // A workflow opens in the view last left; failing that, fitted if any node would be out of sight.
  (function () {
    var v = keptView(), b = extent();
    if (v) {
      setView(Math.min(editor.zoom_max, Math.max(editor.zoom_min, v.z)), v.x, v.y);
    } else if (b && (b.x0 < 0 || b.y0 < 0 || b.x1 > root.clientWidth || b.y1 > root.clientHeight)) {
      fit();
    }
  })();

  // ---- saving ----
  var saveTimer = null, saving = false, again = false;
  function scheduleSave() {
    if (loading) { return; }
    clearTimeout(saveTimer);
    saveTimer = setTimeout(save, 300);
  }

  // New reference blocks not yet saved: block id -> the id of the block whose conversation it continues.
  var newRefs = {}, sentRefs = {};

  function layout() {
    var data = editor.export().drawflow.Home.data, nodes = [], links = [];
    sentRefs = {};
    Object.keys(data).forEach(function (k) {
      var n = data[k], id = dfToID[n.id], node = { id: id, type: n.name, x: Math.round(n.pos_x), y: Math.round(n.pos_y) };
      if (newRefs[id]) { node.continue_of = sentRefs[id] = newRefs[id]; }
      nodes.push(node);
      Object.keys(n.outputs).forEach(function (cls) {
        n.outputs[cls].connections.forEach(function (c) {
          links.push({ from: id, out: outName(id, cls), to: dfToID[c.node], in: 'in' });
        });
      });
    });
    return { version: state.version, nodes: nodes, links: links };
  }

  // A node's outputs, in Drawflow's order (output_1, output_2, …), by name.
  function outputsOf(id) {
    var o = (state.outputs || {})[id];
    if (o) { return o; }
    var n = idToDf[id] && editor.getNodeFromId(idToDf[id]);
    return (n && types[n.name].outputs) || ['out'];
  }
  function outName(id, cls) { return outputsOf(id)[parseInt(cls.replace('output_', ''), 10) - 1] || 'out'; }
  function outClass(id, name) { return 'output_' + (Math.max(0, outputsOf(id).indexOf(name)) + 1); }

  function post(url, body, isForm) {
    var headers = { 'X-Autobot-CSRF': csrf };
    if (!isForm) { headers['Content-Type'] = 'application/json'; body = JSON.stringify(body); }
    return fetch(url, { method: 'POST', headers: headers, body: body, credentials: 'same-origin' });
  }

  function save() {
    if (saving) { again = true; return; }
    saving = true;
    show('Saving…');
    post(base + '/layout', layout()).then(function (resp) {
      return resp.json().then(function (body) { return { status: resp.status, body: body }; });
    }).then(function (r) {
      if (r.status === 200) {
        state.version = r.body.version; state.summaries = r.body.summaries; state.names = r.body.names;
        state.outputs = r.body.outputs; state.continues = r.body.continues; state.problems = r.body.problems || {};
        Object.keys(sentRefs).forEach(function (id) { delete newRefs[id]; });
        Object.keys(idToDf).forEach(setSummary);
        // A block whose templates use a tag the rules do not allow is marked, and the bar says which; the layout is saved anyway.
        showSaved('Saved');
      } else {
        show(r.body.error || 'Not saved.', true);
        if (r.status === 409) { offerReload(); }
        // A refused link (a loop, say) comes off the canvas again.
        if (lastLink) { editor.removeSingleConnection(lastLink.output_id, lastLink.input_id, lastLink.output_class, lastLink.input_class); }
      }
      lastLink = null;
    }).catch(function () { show('Not saved: Autobot could not be reached.', true); })
      .then(function () { saving = false; if (again) { again = false; save(); } });
  }

  function offerReload() {
    var b = document.createElement('button');
    b.className = 'secondary'; b.textContent = 'Reload';
    b.onclick = function () { location.reload(); };
    statusEl.appendChild(document.createTextNode(' ')); statusEl.appendChild(b);
  }

  editor.on('nodeMoved', scheduleSave);
  editor.on('nodeRemoved', function (dfid) { delete idToDf[dfToID[dfid]]; delete dfToID[dfid]; scheduleSave(); });
  editor.on('connectionRemoved', scheduleSave);
  // Links may fan out and collapse; the server refuses only a loop, and then the link comes off.
  var lastLink = null;
  editor.on('connectionCreated', function (c) { lastLink = c; scheduleSave(); });

  // ---- the right-click menu, and placing a node with the mouse ----
  var placing = null, placingRef = null;   // placingRef: the block a reference being placed continues

  function hideMenu() { menu.hidden = true; menu.replaceChildren(); }

  function openMenu(x, y, items) {
    menu.replaceChildren();
    items.forEach(function (it) {
      var b = document.createElement('button');
      b.type = 'button'; b.textContent = it.label;
      b.onclick = function () { hideMenu(); it.run(); };
      menu.appendChild(b);
    });
    menu.style.left = x + 'px'; menu.style.top = y + 'px';
    menu.hidden = false;
  }

  function startPlacing(type, x, y, ref) {
    placing = type; placingRef = ref || null;
    var what = ref ? 'a reference to ' + ((state.names || {})[ref] || 'a block') : types[type].label;
    ghost.textContent = ref ? 'Reference to ' + ((state.names || {})[ref] || 'a block') : types[type].label;
    moveGhost(x, y);
    ghost.hidden = false;
    show('Click to place ' + what + ' (Esc cancels)');
  }

  function moveGhost(x, y) { ghost.style.left = x + 'px'; ghost.style.top = y + 'px'; }

  function stopPlacing() { placing = null; placingRef = null; ghost.hidden = true; show(''); }

  // Where a point on the screen falls on the canvas, allowing for its pan and zoom.
  function canvasPoint(clientX, clientY) {
    var r = editor.precanvas.getBoundingClientRect();
    return { x: (clientX - r.x) / editor.zoom, y: (clientY - r.y) / editor.zoom };
  }

  // ---- copy & paste blocks across workflows of the same workspace ----
  var wsBase = base.replace(/\/workflows\/[^/]+$/, '');
  var currentKind = (base.match(/\/workflows\/([^/]+)$/) || ['', ''])[1];
  var clipKey = 'autobot:copied-block:' + wsBase;

  function readClip() {
    try { return JSON.parse(localStorage.getItem(clipKey) || 'null'); } catch (e) { return null; }
  }
  function copyBlock(nid, ntype) {
    var name = (state.names || {})[nid] || (types[ntype] && types[ntype].label) || ntype;
    try { localStorage.setItem(clipKey, JSON.stringify({ kind: currentKind, node: nid, type: ntype, name: name })); } catch (e) {}
    show('Copied “' + name + '” — right-click any canvas to paste');
  }
  function pasteBlock(clip, pt) {
    show('Pasting…');
    post(base + '/paste', { version: state.version, source_kind: clip.kind, source_node: clip.node, x: Math.round(pt.x), y: Math.round(pt.y) })
      .then(function (resp) { return resp.json().then(function (body) { return { status: resp.status, body: body }; }); })
      .then(function (r) {
        if (r.status === 200) {
          state.version = r.body.version; state.summaries = r.body.summaries; state.names = r.body.names;
          state.outputs = r.body.outputs; state.continues = r.body.continues; state.problems = r.body.problems || {};
          loading = true;
          addNode(r.body.node.id, r.body.node.type, r.body.node.x, r.body.node.y);
          loading = false;
          Object.keys(idToDf).forEach(setSummary);
          showSaved(r.body.note || 'Pasted');
        } else {
          show(r.body.error || 'Not pasted.', true);
          if (r.status === 409) { offerReload(); }
        }
      })
      .catch(function () { show('Not pasted: Autobot could not be reached.', true); });
  }

  root.addEventListener('contextmenu', function (e) {
    e.preventDefault();
    if (placing) { stopPlacing(); return; }
    var conn = e.target.closest('.connection');
    if (conn) {
      // A link: Drawflow names its two ends and their ports in its classes.
      var cls = Array.prototype.slice.call(conn.classList);
      var end = function (prefix) { var c = cls.filter(function (x) { return x.indexOf(prefix) === 0; })[0]; return c ? c.slice(prefix.length) : ''; };
      var port = function (re) { return cls.filter(function (x) { return re.test(x); })[0]; };
      openMenu(e.clientX, e.clientY, [{ label: 'Delete link', run: function () {
        editor.removeSingleConnection(end('node_out_node-'), end('node_in_node-'), port(/^output_\d+$/), port(/^input_\d+$/));
      } }]);
      return;
    }
    var node = e.target.closest('.drawflow-node');
    if (node) {
      var dfid = node.id.replace('node-', '');
      var nid = dfToID[dfid], ntype = editor.getNodeFromId(dfid).name;
      var items = [{ label: 'Copy', run: function () { copyBlock(nid, ntype); } }];
      // An Agent block that starts its own conversations can be referred to: a new block that continues it.
      if (ntype === 'agent' && !(state.continues || {})[nid]) {
        items.push({ label: 'New reference', run: function () { startPlacing('agent', e.clientX, e.clientY, nid); } });
      }
      items.push({ label: 'Delete', run: function () { editor.removeNodeId('node-' + dfid); } });
      openMenu(e.clientX, e.clientY, items);
      return;
    }
    var canvasItems = [];
    var clip = readClip();
    if (clip && clip.node && types[clip.type]) {
      var pt = canvasPoint(e.clientX, e.clientY);
      canvasItems.push({ label: 'Paste “' + (clip.name || types[clip.type].label) + '”', run: function () { pasteBlock(clip, pt); } });
    }
    state.types.forEach(function (t) {
      canvasItems.push({ label: t.label, run: function () { startPlacing(t.type, e.clientX, e.clientY); } });
    });
    openMenu(e.clientX, e.clientY, canvasItems);
  }, true);

  document.addEventListener('mousemove', function (e) { if (placing) { moveGhost(e.clientX, e.clientY); } });
  document.addEventListener('keydown', function (e) {
    if (e.key === 'Escape') { if (placing) { stopPlacing(); } hideMenu(); }
  });
  document.addEventListener('mousedown', function (e) { if (!menu.contains(e.target)) { hideMenu(); } });

  // A click places a node being placed, or opens the settings of the node clicked (not dragged).
  var down = null;
  root.addEventListener('mousedown', function (e) {
    if (placing && e.button === 0) {
      e.stopPropagation(); e.preventDefault();
      var p = canvasPoint(e.clientX, e.clientY), type = placing, ref = placingRef, id = newID();
      stopPlacing();
      if (ref) {
        newRefs[id] = ref;
        if (!state.continues) { state.continues = {}; }
        state.continues[id] = 'continues ' + ((state.names || {})[ref] || '');
      }
      addNode(id, type, p.x, p.y);
      scheduleSave();
      return;
    }
    var node = e.target.closest('.drawflow-node');
    down = node && e.button === 0 && !e.target.closest('.output, .input') ? { node: node, x: e.clientX, y: e.clientY } : null;
  }, true);
  root.addEventListener('mouseup', function (e) {
    if (down && Math.abs(e.clientX - down.x) < 4 && Math.abs(e.clientY - down.y) < 4) {
      var id = dfToID[down.node.id.replace('node-', '')];
      if (e.shiftKey) { openTest(id); } else { openSettings(id); }
    }
    down = null;
  });

  // ---- a node's settings, in a modal ----
  function openSettings(id) {
    fetch(base + '/nodes/' + id, { credentials: 'same-origin' }).then(function (r) { return r.text(); }).then(function (html) {
      modal.innerHTML = html;   // our own server's page, escaped by its templates
      modal.dataset.node = id;
      modal.showPopover();
    });
  }

  // Shift-click tests a block: a form asks for the tags it uses, then it runs once with them.
  function openTest(id) {
    fetch(base + '/nodes/' + id + '/test', { credentials: 'same-origin' }).then(function (r) { return r.text(); }).then(function (html) {
      modal.innerHTML = html;   // our own server's page, escaped by its templates
      modal.dataset.node = id;
      modal.showPopover();
    });
  }

  modal.addEventListener('click', function (e) { if (e.target.closest('[data-close]')) { modal.hidePopover(); } });
  var ko = document.getElementById('wf-kickoff');
  if (ko && ko.dataset.node) { ko.addEventListener('click', function () { openTest(ko.dataset.node); }); }

  // An (i)'s note opens inside the dialog: wrapped, as wide as the dialog allows, shifted left when the
  // icon is near the right edge, and above the icon when there is more room there.
  function placeTip(e) {
    var icon = e.target.closest && e.target.closest('.info');
    if (!icon) { return; }
    var m = modal.getBoundingClientRect(), r = icon.getBoundingClientRect();
    var max = Math.min(288, m.width - 24), below = m.bottom - r.bottom, above = r.top - m.top;
    icon.style.setProperty('--tip-max', max + 'px');
    icon.style.setProperty('--tip-left', Math.min(0, m.right - 12 - (r.left + max)) + 'px');
    icon.classList.toggle('tip-above', below < 110 && above > below);
  }
  modal.addEventListener('mouseover', placeTip);
  modal.addEventListener('focusin', placeTip);

  // A refused save leaves its message in the dialog and on the bar; closing the dialog without saving
  // takes the bar's message away, since there is nothing left to see or save.
  var refused = '';
  modal.addEventListener('toggle', function (e) {
    if (e.newState === 'closed' && refused) {
      if (statusEl.textContent === refused) { show(''); }
      refused = '';
    }
  });
  // An agent's preset fills its Instruction and Done when, read-only; Custom unlocks them, starting
  // from the text that was there.
  modal.addEventListener('change', function (e) {
    // Continuing another block's thread: the model starts as that block's.
    var cont = e.target.closest('select[data-continue]');
    if (cont) {
      var on = cont.value !== '', form = cont.form;
      form.querySelectorAll('[data-when-continuing]').forEach(function (el) { el.hidden = !on; });
      // The model starts as the other block's, and may be changed.
      var model = cont.options[cont.selectedIndex].dataset.model;
      if (on && model) { form.querySelector('select[data-model]').value = model; }
      return;
    }
    var limit = e.target.closest('input[data-limit-subs]');
    if (limit) {
      limit.form.querySelectorAll('[data-when-limit]').forEach(function (el) { el.hidden = !limit.checked; });
      return;
    }
    var closing = e.target.closest('select[data-closing]');
    if (closing) {
      var cform = closing.form;
      cform.querySelectorAll("[data-when-report]").forEach(function (el) { el.hidden = closing.value !== "report"; });
      return;
    }
    var sel = e.target.closest('select[data-preset]');
    if (!sel) { return; }
    var form = sel.form, opt = sel.options[sel.selectedIndex], custom = sel.value === 'custom';
    [['instruction', 'instruction'], ['goal', 'goal'], ['summary_format', 'summary']].forEach(function (pair) {
      var box = form.elements[pair[0]];
      if (!custom) { box.value = opt.dataset[pair[1]] || ''; }
      box.readOnly = !custom;
    });
    var step = form.elements['closing'];
    if (!custom) { step.value = opt.dataset.closing || ''; }
    step.disabled = !custom;
    // The preset's starting Additional instructions go in an empty box, or one that still holds the
    // previous preset's, which the person has not touched.
    var extra = form.elements['system_prompt'], was = sel.dataset.prevAdditional;
    if (was === undefined) { var first = sel.querySelector('option[selected]'); was = first ? first.dataset.additional || '' : ''; }
    var now = custom ? '' : opt.dataset.additional || '';
    if (!custom && (extra.value.trim() === '' || extra.value === was)) { extra.value = now; }
    sel.dataset.prevAdditional = now;
    // Its starting run settings fill the inputs it has a value for; fields left untouched from a
    // previous preset's defaults reset to the dialog's baseline so switching away from e.g. Lens Team
    // does not leave sub_agents=6 stuck on a single-agent preset.
    var numFields = ['max_turns', 'max_minutes', 'time_warn_percent', 'sub_agents'];
    if (sel.dataset.baseRunDefaults === undefined) {
      var base = {};
      numFields.forEach(function (name) { base[name] = form.elements[name].value; });
      base.limit_sub_agents = form.elements['limit_sub_agents'].checked;
      base.sub_agents_per_run = form.elements['sub_agents_per_run'].value;
      sel.dataset.baseRunDefaults = JSON.stringify(base);
    }
    var baseRD = {}, prevRD = {}, rd = {};
    try { baseRD = JSON.parse(sel.dataset.baseRunDefaults); } catch (err) { baseRD = {}; }
    try { prevRD = sel.dataset.prevRunDefaults ? JSON.parse(sel.dataset.prevRunDefaults) : {}; } catch (err) { prevRD = {}; }
    try { rd = custom || !opt.dataset.runDefaults ? {} : JSON.parse(opt.dataset.runDefaults); } catch (err) { rd = {}; }
    if (!custom) {
      numFields.forEach(function (name) {
        if (typeof rd[name] === 'number') {
          form.elements[name].value = rd[name];
        } else if (typeof prevRD[name] === 'number' && String(form.elements[name].value) === String(prevRD[name])) {
          form.elements[name].value = baseRD[name];
        }
      });
      var lim = form.elements['limit_sub_agents'];
      if (typeof rd.sub_agents_per_run === 'number') {
        lim.checked = rd.sub_agents_per_run > 0;
        if (lim.checked) { form.elements['sub_agents_per_run'].value = rd.sub_agents_per_run; }
        form.querySelectorAll('[data-when-limit]').forEach(function (el) { el.hidden = !lim.checked; });
      } else if (typeof prevRD.sub_agents_per_run === 'number') {
        lim.checked = !!baseRD.limit_sub_agents;
        form.elements['sub_agents_per_run'].value = baseRD.sub_agents_per_run;
        form.querySelectorAll('[data-when-limit]').forEach(function (el) { el.hidden = !lim.checked; });
      }
      sel.dataset.prevRunDefaults = JSON.stringify(rd);
    }
    form.querySelectorAll('[data-when-report]').forEach(function (el) { el.hidden = step.value !== 'report'; });
  });
  modal.addEventListener('submit', function (e) {
    var form = e.target, id = modal.dataset.node;
    if (form.hasAttribute('data-native')) { return; }   // an agent's run opens its own page, in a new tab
    e.preventDefault();
    if (form.classList.contains('test-form')) {
      show('Testing…');
      post(form.action, new URLSearchParams(new FormData(form)), true).then(function (resp) { return resp.text(); })
        .then(function (html) { modal.innerHTML = html; show(''); })
        .catch(function () { show('Not tested: Autobot could not be reached.', true); });
      return;
    }
    form.version.value = state.version;
    show('Saving…');
    // Sent as an ordinary form (not multipart), which is what the server reads.
    post(form.action, new URLSearchParams(new FormData(form)), true).then(function (resp) {
      if (resp.status === 200) {
        return resp.json().then(function (body) {
          state.version = body.version; state.summaries = body.summaries; state.names = body.names;
          state.outputs = body.outputs; state.continues = body.continues; state.problems = body.problems || {};
          syncPorts(id, body.links || []);
          refused = '';
          Object.keys(idToDf).forEach(setSummary); modal.hidePopover(); showSaved(body.note ? 'Saved. ' + body.note : 'Saved');
        });
      }
      if (resp.headers.get('Content-Type').indexOf('application/json') === 0) {
        return resp.json().then(function (body) { show(body.error, true); if (resp.status === 409) { offerReload(); } modal.hidePopover(); });
      }
      return resp.text().then(function (html) {
        modal.innerHTML = html;
        refused = (state.names[id] || 'This block') + ': settings not saved (see the message in the dialog)';
        show(refused, true);
      });
    }).catch(function () { show('Not saved: Autobot could not be reached.', true); });
  });
})();

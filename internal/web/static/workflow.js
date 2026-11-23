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
      '<div class="wf-node"><div class="wf-name"></div><div class="wf-title"></div><div class="wf-sub"></div></div>');
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
    el.querySelector('.wf-name').textContent = (state.names || {})[id] || types[editor.getNodeFromId(idToDf[id]).name].label;
  }

  function show(msg, bad) {
    statusEl.textContent = msg;
    statusEl.className = 'wf-status' + (bad ? ' bad' : '');
  }

  // ---- loading the saved workflow ----
  state.graph.nodes.forEach(function (n) { addNode(n.id, n.type, n.x, n.y); });
  state.graph.links.forEach(function (l) {
    if (idToDf[l.from] && idToDf[l.to]) { editor.addConnection(idToDf[l.from], idToDf[l.to], outClass(l.from, l.out), 'input_1'); }
  });
  loading = false;

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

  function layout() {
    var data = editor.export().drawflow.Home.data, nodes = [], links = [];
    Object.keys(data).forEach(function (k) {
      var n = data[k], id = dfToID[n.id];
      nodes.push({ id: id, type: n.name, x: Math.round(n.pos_x), y: Math.round(n.pos_y) });
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
        state.outputs = r.body.outputs;
        Object.keys(idToDf).forEach(setSummary);
        show('Saved');
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
  var placing = null;

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

  function startPlacing(type, x, y) {
    placing = type;
    ghost.textContent = types[type].label;
    moveGhost(x, y);
    ghost.hidden = false;
    show('Click to place ' + types[type].label + ' (Esc cancels)');
  }

  function moveGhost(x, y) { ghost.style.left = x + 'px'; ghost.style.top = y + 'px'; }

  function stopPlacing() { placing = null; ghost.hidden = true; show(''); }

  // Where a point on the screen falls on the canvas, allowing for its pan and zoom.
  function canvasPoint(clientX, clientY) {
    var r = editor.precanvas.getBoundingClientRect();
    return { x: (clientX - r.x) / editor.zoom, y: (clientY - r.y) / editor.zoom };
  }

  root.addEventListener('contextmenu', function (e) {
    e.preventDefault();
    if (placing) { stopPlacing(); return; }
    var node = e.target.closest('.drawflow-node');
    if (node) {
      var dfid = node.id.replace('node-', '');
      openMenu(e.clientX, e.clientY, [{ label: 'Delete', run: function () { editor.removeNodeId('node-' + dfid); } }]);
      return;
    }
    openMenu(e.clientX, e.clientY, state.types.map(function (t) {
      return { label: t.label, run: function () { startPlacing(t.type, e.clientX, e.clientY); } };
    }));
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
      var p = canvasPoint(e.clientX, e.clientY), type = placing;
      stopPlacing();
      addNode(newID(), type, p.x, p.y);
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
  // An agent's preset fills its Instruction and Done when, read-only; Custom unlocks them, starting
  // from the text that was there.
  modal.addEventListener('change', function (e) {
    var sel = e.target.closest('select[data-preset]');
    if (!sel) { return; }
    var form = sel.form, opt = sel.options[sel.selectedIndex], custom = sel.value === 'custom';
    ['instruction', 'goal'].forEach(function (name) {
      var box = form.elements[name];
      if (!custom) { box.value = opt.dataset[name]; }
      box.readOnly = !custom;
    });
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
          state.outputs = body.outputs;
          syncPorts(id, body.links || []);
          setSummary(id); modal.hidePopover(); show('Saved');
        });
      }
      if (resp.headers.get('Content-Type').indexOf('application/json') === 0) {
        return resp.json().then(function (body) { show(body.error, true); if (resp.status === 409) { offerReload(); } modal.hidePopover(); });
      }
      return resp.text().then(function (html) { modal.innerHTML = html; show('Not saved: see the settings.', true); });
    }).catch(function () { show('Not saved: Autobot could not be reached.', true); });
  });
})();

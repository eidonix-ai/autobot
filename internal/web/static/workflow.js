// The workflow canvas. Drawflow draws the nodes and links and handles dragging and zoom; this adds
// what is Autobot's: a right-click menu to place nodes, a click to open a node's settings, the rule
// that chains are straight lines, and saving every change. The server checks every save, keeps
// each node's settings, and refuses a save made from an out-of-date copy.
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
  editor.start();

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
    var dfid = editor.addNode(type, t.event ? 0 : 1, 1, x, y, 'wf-' + type, { id: id },
      '<div class="wf-node"><div class="wf-title"></div><div class="wf-sub"></div></div>');
    dfToID[dfid] = id; idToDf[id] = dfid;
    var el = nodeEl(dfid);
    el.querySelector('.wf-title').textContent = t.label;
    setSummary(id);
    return dfid;
  }

  function setSummary(id) {
    var el = nodeEl(idToDf[id]);
    if (el) { el.querySelector('.wf-sub').textContent = state.summaries[id] || ''; }
  }

  function show(msg, bad) {
    statusEl.textContent = msg;
    statusEl.className = 'wf-status' + (bad ? ' bad' : '');
  }

  // ---- loading the saved workflow ----
  state.graph.nodes.forEach(function (n) { addNode(n.id, n.type, n.x, n.y); });
  state.graph.links.forEach(function (l) {
    if (idToDf[l.from] && idToDf[l.to]) { editor.addConnection(idToDf[l.from], idToDf[l.to], 'output_1', 'input_1'); }
  });
  loading = false;

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
      var out = n.outputs.output_1;
      (out ? out.connections : []).forEach(function (c) { links.push({ from: id, to: dfToID[c.node] }); });
    });
    return { version: state.version, nodes: nodes, links: links };
  }

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
        state.version = r.body.version; state.summaries = r.body.summaries;
        Object.keys(idToDf).forEach(setSummary);
        show('Saved');
      } else {
        show(r.body.error || 'Not saved.', true);
        if (r.status === 409) { offerReload(); }
      }
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
  // A chain is a straight line: one link out of a node, one link into it.
  editor.on('connectionCreated', function (c) {
    var from = editor.getNodeFromId(c.output_id), to = editor.getNodeFromId(c.input_id);
    if (from.outputs[c.output_class].connections.length > 1 || to.inputs[c.input_class].connections.length > 1) {
      editor.removeSingleConnection(c.output_id, c.input_id, c.output_class, c.input_class);
      show('A chain is a straight line: each step links to one next step.', true);
      return;
    }
    scheduleSave();
  });

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
      openSettings(dfToID[down.node.id.replace('node-', '')]);
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

  modal.addEventListener('click', function (e) { if (e.target.closest('[data-close]')) { modal.hidePopover(); } });
  modal.addEventListener('submit', function (e) {
    e.preventDefault();
    var form = e.target, id = modal.dataset.node;
    form.version.value = state.version;
    show('Saving…');
    // Sent as an ordinary form (not multipart), which is what the server reads.
    post(form.action, new URLSearchParams(new FormData(form)), true).then(function (resp) {
      if (resp.status === 200) {
        return resp.json().then(function (body) {
          state.version = body.version; state.summaries = body.summaries;
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

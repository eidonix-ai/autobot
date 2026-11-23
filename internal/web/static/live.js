// A page that is still going (a run) loads this script. Every few seconds it fetches the page again
// and puts in only what changed: each element marked data-live is matched by id; in a list marked
// data-live-list only the items that changed are replaced and new ones added. The page never
// reloads, so open details stay open and the scroll stays where it is, following the end only when
// it was already there. Once the fetched page no longer loads this script, the run is over: that last
// version is put in and the polling stops.
(function () {
  'use strict';
  var every = 3000;
  var self = document.currentScript && document.currentScript.getAttribute('src');

  // The markup of an item, as the page was sent: a details a person has opened is still the same item.
  function markup(el) {
    var copy = el.cloneNode(true);
    copy.querySelectorAll('details[open]').forEach(function (d) { d.removeAttribute('open'); });
    if (copy.hasAttribute('open')) copy.removeAttribute('open');
    return copy.outerHTML;
  }

  // Replaces an item with its new version, the details a person had opened still open.
  function replaceKeepingOpen(list, fresh, current) {
    var opened = Array.prototype.map.call(current.querySelectorAll('details'), function (d) { return d.open; });
    var node = document.importNode(fresh, true);
    node.querySelectorAll('details').forEach(function (d, i) { if (opened[i]) d.open = true; });
    list.replaceChild(node, current);
  }

  function sync(old, next) {
    if (!old.hasAttribute('data-live-list')) {
      if (old.innerHTML !== next.innerHTML) old.innerHTML = next.innerHTML;
      return;
    }
    var have = old.children, want = next.children;
    for (var i = 0; i < want.length; i++) {
      if (i >= have.length) old.appendChild(document.importNode(want[i], true));
      else if (markup(have[i]) !== markup(want[i])) replaceKeepingOpen(old, want[i], have[i]);
    }
    while (have.length > want.length) old.removeChild(old.lastElementChild);
  }

  function tick() {
    fetch(location.pathname + location.search, { credentials: 'same-origin', cache: 'no-store' })
      .then(function (r) { if (!r.ok) throw new Error(r.status); return r.text(); })
      .then(function (html) {
        var doc = new DOMParser().parseFromString(html, 'text/html');
        var root = document.scrollingElement || document.documentElement;
        var atEnd = root.scrollTop + root.clientHeight >= root.scrollHeight - 40;
        doc.querySelectorAll('[data-live][id]').forEach(function (next) {
          var old = document.getElementById(next.id);
          if (old) sync(old, next);
        });
        if (atEnd && root.scrollHeight > root.clientHeight) root.scrollTop = root.scrollHeight;
        if (doc.querySelector('script[src="' + self + '"]')) setTimeout(tick, every);
      })
      .catch(function () { setTimeout(tick, every * 2); });
  }
  setTimeout(tick, every);
})();

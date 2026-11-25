// Agent token: Copy puts the token on the clipboard; New token asks first.
(function () {
  document.addEventListener("click", function (e) {
    var b = e.target.closest && e.target.closest(".js-copy");
    if (!b) return;
    var input = document.getElementById(b.getAttribute("data-target"));
    if (!input) return;
    input.select();
    var done = function () { b.textContent = "Copied"; window.setTimeout(function () { b.textContent = "Copy"; }, 2000); };
    if (navigator.clipboard && navigator.clipboard.writeText) navigator.clipboard.writeText(input.value).then(done, function () {});
    else if (document.execCommand("copy")) done();
  });
  document.addEventListener("submit", function (e) {
    var f = e.target;
    var q = f.getAttribute && f.getAttribute("data-confirm");
    if (q && !window.confirm(q)) e.preventDefault();
  }, true);
})();

// The device-code sign-in: while a code is waiting, ask the server every few
// seconds whether the person has finished, and reload the page when it has.
(function () {
  var box = document.getElementById("flow");
  if (!box) return;
  var stopped = false;
  function check() {
    if (stopped) return;
    fetch("/connectors/graph/status", { cache: "no-store", credentials: "same-origin" })
      .then(function (r) { return r.json(); })
      .then(function (s) {
        if (s.state !== "pending") {
          stopped = true;
          window.location.assign("/connectors#graph");
          window.location.reload();
        }
      })
      .catch(function () {});
  }
  window.setInterval(check, 3000);
})();

// While an import runs the page shows how it goes: reload every few seconds.
(function () {
  if (!document.getElementById("import-running") && !document.querySelector("#history-state[data-running]")) return;
  window.setTimeout(function () { window.location.reload(); }, 4000);
})();

// The list page. Nothing here builds markup from text: the server renders the
// rows (its templates escape everything), and this script only places the
// pieces it is given, sets textContent and toggles classes.
(function () {
  var page = document.getElementById("list");
  if (!page) return;
  document.documentElement.classList.add("js");
  var since = page.getAttribute("data-since");
  var OPEN_KEY = "one.open";
  var GROUPS_KEY = "one.groups"; // only across the reload that the new-items button makes
  var HEADERS = { "X-Requested-With": "one" };
  var phone = window.matchMedia("(max-width: 55.99rem)");
  // On a laptop the list page has three panes (SPEC 1): the stages are in the left
  // pane, the chosen stage's rows in the middle one. The groups stay in the page;
  // there they are only the lists of rows, one shown at a time.
  var panes = document.body.classList.contains("page-panes");
  var laptop = window.matchMedia("(min-width: 68rem)");
  var STAGE_KEY = "one.stage";
  var STAGES = ["new", "in_progress", "pending", "resolved"];
  var stage = "new";

  function openSet() {
    try { return JSON.parse(localStorage.getItem(OPEN_KEY) || "[]"); } catch (e) { return []; }
  }
  function remember(id, on) {
    var set = openSet().filter(function (x) { return x !== id; });
    if (on) set.push(id);
    try { localStorage.setItem(OPEN_KEY, JSON.stringify(set.slice(-200))); } catch (e) {}
  }
  function toNodes(html) {
    var t = document.createElement("template");
    t.innerHTML = html;
    return t.content;
  }
  function text(res) {
    if (!res.ok) return res.text().then(function (m) { throw new Error(m || "Something went wrong."); });
    return res.text();
  }

  // On a laptop only the middle pane scrolls: showing something moves that pane and never the window (UX review 2, 14).
  // The top of the pane is under the search and the heading, so what counts as visible starts below them.
  function lapPanes() { return panes && laptop.matches; }
  function reveal(el, center) {
    if (!el) return;
    if (!lapPanes()) { el.scrollIntoView({ block: center ? "center" : "nearest" }); return; }
    var pr = page.getBoundingClientRect(), r = el.getBoundingClientRect();
    var top = pr.top + 80, bottom = pr.bottom - 8, d = 0;
    if (center) d = (r.top + r.height / 2) - (top + bottom) / 2;
    else if (r.top < top) d = r.top - top;
    else if (r.bottom > bottom) d = Math.min(r.bottom - bottom, r.top - top);
    if (d) page.scrollTop += d;
  }
  // put the cursor in a box and show it, without the browser scrolling the window
  function focusBox(el, center) {
    if (!el) return;
    if (!lapPanes()) { if (center) el.scrollIntoView({ block: "center" }); el.focus(); return; }
    el.focus({ preventScroll: true });
    reveal(el, center);
  }

  // --- numbers and the new-items button ---
  var button = document.getElementById("new-items");
  function applyState(s) {
    var h = document.getElementById("heading");
    if (h && s.heading) h.textContent = s.heading;
    Object.keys(s.counts || {}).forEach(function (stage) {
      document.querySelectorAll('[data-count="' + stage + '"]').forEach(function (el) { el.textContent = String(s.counts[stage]); });
    });
    var w = document.getElementById("g-wait");
    if (w) w.textContent = s.wait || "";
    Object.keys(s.approve || {}).forEach(function (stage) {
      document.querySelectorAll('[data-approve="' + stage + '"]').forEach(function (el) { el.textContent = s.approve[stage]; });
    });
    var todo = document.getElementById("todo-link");
    if (todo) { todo.textContent = s.todo || ""; todo.hidden = !s.todo; }
    document.querySelectorAll('[data-count="todo"]').forEach(function (el) { el.textContent = String(parseInt(s.todo, 10) || 0); });
    var nstrip = document.getElementById("night-strip");
    if (nstrip) {
      var nt = document.getElementById("night-text");
      if (nt) nt.textContent = s.night || "";
      nstrip.hidden = !s.night;
    }
    var pstrip = document.getElementById("people-strip");
    if (pstrip) {
      pstrip.textContent = "";
      if (s.people) {
        var pa = document.createElement("a");
        pa.href = "/settings#h-proposals";
        pa.textContent = s.people;
        pstrip.appendChild(pa);
      }
      pstrip.hidden = !s.people;
    }
    var strip = document.getElementById("claude-strip");
    if (strip) { strip.textContent = s.claude || ""; strip.hidden = !s.claude; }
    var alerts = document.getElementById("alerts");
    if (alerts && s.alerts) {
      alerts.textContent = "";
      s.alerts.forEach(function (t) {
        var p = document.createElement("p");
        p.className = "strip strip-alert";
        p.setAttribute("role", "status");
        var a = document.createElement("a");
        a.href = "/connectors";
        a.textContent = t;
        p.appendChild(a);
        alerts.appendChild(p);
      });
    }
    document.dispatchEvent(new Event("one:strips"));
    var closed = document.getElementById("closed-link");
    if (closed) {
      closed.hidden = !s.closed;
      closed.textContent = "";
      if (s.closed) {
        var cl = document.createElement("a");
        cl.href = "/closed";
        cl.textContent = s.closed_text || "Closed (" + s.closed + ")";
        closed.appendChild(cl);
      }
    }
    var cn = document.getElementById("stage-closed-n");
    if (cn) cn.textContent = String(s.closed || 0);
    midHead();
    if (button) {
      if (s.waiting > 0) { button.textContent = s.button; button.hidden = false; } else { button.hidden = true; }
    }
  }
  // The block `Connections` of the left pane is renewed with the numbers, at most
  // every 20 seconds unless Refresh asked for it.
  var connsAt = 0;
  // the connection the list is narrowed to (SPEC 23) rides on every request of the page
  var onlyEl = document.getElementById("only-conn");
  var only = onlyEl ? onlyEl.getAttribute("data-conn") : "";
  function withOnly(url) { return only ? url + (url.indexOf("?") < 0 ? "?" : "&") + "conn=" + encodeURIComponent(only) : url; }
  function refreshConns(force) {
    if (!panes || !laptop.matches) return;
    var now = Date.now();
    if (!force && now - connsAt < 20000) return;
    connsAt = now;
    fetch(withOnly("/list/connections"), { credentials: "same-origin", cache: "no-store" }).then(text).then(function (html) {
      var slot = document.getElementById("conns-slot");
      if (!slot) return;
      slot.textContent = "";
      slot.appendChild(toNodes(html));
    }).catch(function () {});
  }
  function refreshState(force) {
    document.dispatchEvent(new Event("one-changed"));
    refreshConns(force === true);
    return fetch(withOnly("/list/state?since=" + encodeURIComponent(since)), { credentials: "same-origin", cache: "no-store" })
      .then(function (r) { return r.json(); }).then(applyState).catch(function () {});
  }
  if (button) {
    button.addEventListener("click", function () {
      var open = [];
      page.querySelectorAll(".g-head[aria-expanded=true]").forEach(function (g) { open.push(g.parentNode.parentNode.getAttribute("data-stage")); });
      try { sessionStorage.setItem(GROUPS_KEY, JSON.stringify(open)); } catch (e) {}
      window.location.reload();
    });
  }
  window.setInterval(function () { if (!document.hidden) refreshState(); }, 60000);
  document.addEventListener("one-refreshed", function () { refreshState(true); });

  // --- groups: folded until opened, loaded when opened, paged ---
  function loadGroup(list, url, replaceMore) {
    return fetch(url, { credentials: "same-origin", cache: "no-store" }).then(text).then(function (html) {
      if (replaceMore) replaceMore.remove();
      list.appendChild(toNodes(html));
      restoreOpen(list);
      rowsLoaded();
    });
  }
  function showGroup(head, on) {
    var list = document.getElementById(head.getAttribute("aria-controls"));
    head.setAttribute("aria-expanded", on ? "true" : "false");
    list.hidden = !on;
    if (!on || list.getAttribute("data-loaded")) return;
    var stage = head.parentNode.parentNode.getAttribute("data-stage");
    list.innerHTML = "";
    var msg = document.createElement("li");
    msg.className = "loading";
    msg.textContent = "Loading…";
    list.appendChild(msg);
    fetch(withOnly("/list/" + stage), { credentials: "same-origin", cache: "no-store" }).then(text).then(function (html) {
      list.innerHTML = "";
      list.appendChild(toNodes(html));
      list.setAttribute("data-loaded", "1");
      restoreOpen(list);
      rowsLoaded();
    }).catch(function () {
      msg.className = "loading error";
      msg.textContent = "The list could not be loaded. Press the group name to try again.";
      list.innerHTML = "";
      list.appendChild(msg);
    });
  }
  page.addEventListener("click", function (e) {
    var t = e.target;
    var head = t.closest && t.closest(".js-group");
    if (head) { showGroup(head, head.getAttribute("aria-expanded") !== "true"); return; }
    var more = t.closest && t.closest(".js-more");
    if (more) {
      var li = more.closest(".more-row");
      var list = li.parentNode;
      var stage = list.parentNode.getAttribute("data-stage");
      more.disabled = true;
      loadGroup(list, withOnly("/list/" + stage + "?offset=" + li.getAttribute("data-next")), li).catch(function () { more.disabled = false; });
      return;
    }
    var older = t.closest && t.closest(".js-older");
    if (older) {
      var wrap = older.closest(".msgs-wrap");
      older.disabled = true;
      fetch(older.getAttribute("data-url"), { credentials: "same-origin", cache: "no-store" }).then(text).then(function (html) {
        wrap.innerHTML = "";
        wrap.appendChild(toNodes(html));
      }).catch(function () { older.disabled = false; });
      return;
    }
    var fold = t.closest && t.closest(".fold-toggle");
    if (fold) {
      var box = fold.previousElementSibling;
      var folded = box.classList.toggle("folded");
      fold.textContent = folded ? "Show all" : "Show less";
      fold.setAttribute("aria-expanded", folded ? "false" : "true");
      return;
    }
    var line = t.closest && t.closest(".js-row-toggle");
    var when = !line && t.closest && t.closest(".line-when");
    if (line || when) {
      var li = (line || when).closest(".row");
      if (panes && laptop.matches) chooseRow(li);
      toggleRow(li);
    }
  });
  // The groups unfolded before the new-items button reloaded the page.
  try {
    var again = JSON.parse(sessionStorage.getItem(GROUPS_KEY) || "[]");
    sessionStorage.removeItem(GROUPS_KEY);
    if (panes && laptop.matches) again = [];
    again.forEach(function (stage) {
      var head = page.querySelector('.group[data-stage="' + stage + '"] .js-group');
      if (head) showGroup(head, true);
    });
  } catch (e) {}

  // --- rows: opened in place, never moved ---
  function swapRow(li, html) {
    var wasFocused = li.contains(document.activeElement);
    var frag = toNodes(html);
    var fresh = frag.firstElementChild;
    if (!fresh) return li;
    var keep = li.classList.contains("kbd");
    li.replaceWith(fresh);
    if (keep) fresh.classList.add("kbd");
    if (wasFocused) { var b = fresh.querySelector(".js-row-toggle"); if (b) b.focus(); }
    updateRibbon();
    pinNote();
    return fresh;
  }
  // On a phone the note box of the open thread is fixed at the bottom of the screen (SPEC 5), for the row opened last.
  var lastOpened = "";
  function pinNote() {
    var body = document.body;
    page.querySelectorAll(".row.pinned").forEach(function (r) { r.classList.remove("pinned"); });
    var open = page.querySelectorAll(".row.open"), li = null;
    for (var i = 0; i < open.length; i++) if (open[i].getAttribute("data-id") === lastOpened) li = open[i];
    if (!li && open.length) li = open[open.length - 1];
    var say = phone.matches && li && li.querySelector(".thread .say-bar");
    if (!say) { body.classList.remove("say-pinned"); body.style.removeProperty("--say-h"); return; }
    li.classList.add("pinned");
    body.classList.add("say-pinned");
    body.style.setProperty("--say-h", say.offsetHeight + "px");
  }
  phone.addEventListener("change", pinNote);
  // Opening a row in New makes the item In progress: the server answers with
  // the row as it is now, and the row stays where it is.
  function openRow(li, restoring) {
    var id = li.getAttribute("data-id");
    var req = (li.getAttribute("data-stage") === "new" && !restoring)
      ? fetch("/items/" + id + "/open", { method: "POST", headers: HEADERS, credentials: "same-origin", body: new URLSearchParams() })
      : fetch("/items/" + id + "/row?open=1", { credentials: "same-origin", cache: "no-store" });
    var old = li.querySelector(".row-error");
    if (old) old.remove();
    return req.then(text).then(function (html) {
      remember(id, true);
      if (!restoring) lastOpened = id;
      var fresh = swapRow(li, html);
      refreshState();
      if (!restoring) reveal(fresh, false);
    }).catch(function () {
      // a row that did not open says so in its own line (UX review 15)
      if (restoring || !li.isConnected) return;
      var p = document.createElement("p");
      p.className = "error row-error";
      p.setAttribute("role", "alert");
      p.textContent = "This did not open. Try again.";
      li.appendChild(p);
    });
  }
  function closeRow(li) {
    var id = li.getAttribute("data-id");
    remember(id, false);
    fetch("/items/" + id + "/row", { credentials: "same-origin", cache: "no-store" }).then(text).then(function (html) { swapRow(li, html); }).catch(function () {});
  }
  function toggleRow(li) {
    if (li.classList.contains("open")) closeRow(li); else openRow(li, false);
  }
  function restoreOpen(list) {
    var ids = openSet();
    list.querySelectorAll(".row:not(.open)").forEach(function (li) {
      if (ids.indexOf(li.getAttribute("data-id")) >= 0) openRow(li, true);
    });
  }

  // An open chat asks the server every few seconds whether it has changed (SPEC 17):
  // a plain request, no long-lived connection. The row is fetched again and put in
  // its place unless the owner is writing in it, so nothing he typed is lost.
  function rowIsBeingEdited(li) {
    if (li.querySelector(".attach-list li")) return true;
    var tas = li.querySelectorAll("textarea");
    for (var i = 0; i < tas.length; i++) {
      if (tas[i] === document.activeElement || tas[i].value !== tas[i].defaultValue) return true;
    }
    return false;
  }
  function pulseRow(li) {
    var id = li.getAttribute("data-id");
    fetch("/items/" + id + "/pulse", { credentials: "same-origin", cache: "no-store" }).then(text).then(function (v) {
      if (v === li.getAttribute("data-pulse") || rowIsBeingEdited(li) || !li.classList.contains("open")) return;
      return fetch("/items/" + id + "/row?open=1", { credentials: "same-origin", cache: "no-store" }).then(text).then(function (html) {
        if (!rowIsBeingEdited(li) && li.isConnected) { swapRow(li, html); refreshState(); }
      });
    }).catch(function () {});
  }
  window.setInterval(function () {
    if (document.hidden) return;
    page.querySelectorAll(".row.open[data-chat]").forEach(pulseRow);
  }, 8000);

  // The reply box grows with its text, so the owner sees all of what would be sent.
  function growReply(ta) { ta.style.height = "auto"; ta.style.height = (ta.scrollHeight + 2) + "px"; }
  function growAll(root) { if (root.querySelectorAll) root.querySelectorAll("textarea.reply-text").forEach(growReply); }
  document.addEventListener("input", function (e) {
    if (e.target && e.target.classList && e.target.classList.contains("reply-text")) growReply(e.target);
  });
  if (window.MutationObserver) {
    new MutationObserver(function (list) {
      list.forEach(function (m) { m.addedNodes.forEach(function (n) { if (n.nodeType === 1) growAll(n); }); });
    }).observe(document.body, { childList: true, subtree: true });
  }
  growAll(document);

  // Enter sends a note, as in any chat; Shift+Enter makes a new line.
  document.addEventListener("keydown", function (e) {
    if (e.key !== "Enter" || e.shiftKey || e.ctrlKey || e.metaKey || e.altKey || e.isComposing) return;
    var ta = e.target;
    if (!ta || (ta.tagName || "").toLowerCase() !== "textarea") return;
    var form = ta.closest && ta.closest("form.say");
    if (!form) return;
    e.preventDefault();
    if (form.requestSubmit) form.requestSubmit(); else form.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true }));
  });

  // --- the owner writes in a chat himself, files included (SPEC 17) ---
  // The files he chose are kept here, so one can be taken off the list before
  // it goes. The limits are told in a sentence at once; the server checks them again.
  var MAX_FILES = 5, MAX_BYTES = 20 * 1024 * 1024;
  function chosen(form) { return form._files || (form._files = []); }
  function showChosen(form) {
    var ul = form.querySelector(".attach-list");
    ul.textContent = "";
    chosen(form).forEach(function (f, i) {
      var li = document.createElement("li");
      var n = document.createElement("span"); n.className = "fname"; n.textContent = f.name;
      var x = document.createElement("button"); x.type = "button"; x.className = "tertiary js-unattach";
      x.setAttribute("data-i", String(i)); x.setAttribute("aria-label", "Remove " + f.name); x.textContent = "×";
      li.appendChild(n); li.appendChild(x); ul.appendChild(li);
    });
  }
  function say(form, msg) {
    var err = form.querySelector(".say-error");
    err.textContent = msg || "";
    err.hidden = !msg;
  }
  page.addEventListener("click", function (e) {
    var a = e.target.closest && e.target.closest(".js-attach, .js-unattach");
    if (!a) return;
    var form = a.closest("form.chatbox, form.notebox");
    if (!form) return;
    if (a.classList.contains("js-attach")) { form.querySelector(".attach-input").click(); return; }
    chosen(form).splice(parseInt(a.getAttribute("data-i"), 10), 1);
    say(form, "");
    showChosen(form);
  });
  page.addEventListener("change", function (e) {
    var input = e.target;
    if (!input.classList || !input.classList.contains("attach-input")) return;
    var form = input.closest("form.chatbox, form.notebox");
    var list = chosen(form), problem = "";
    Array.prototype.slice.call(input.files).forEach(function (f) {
      if (list.length >= MAX_FILES) { problem = "At most " + MAX_FILES + " files can be sent at once."; return; }
      if (f.size > MAX_BYTES) { problem = f.name + " is larger than 20 MB."; return; }
      if (f.size === 0) { problem = f.name + " is empty."; return; }
      list.push(f);
    });
    input.value = "";
    say(form, problem);
    showChosen(form);
  });
  function sendWrite(form) {
    var box = form.querySelector("textarea");
    var files = chosen(form);
    if (box.value.trim() === "" && files.length === 0) { say(form, "Write something first."); box.focus(); return; }
    if (form.dataset.busy) return;
    form.dataset.busy = "1";
    say(form, "");
    var data = new FormData();
    data.append("text", box.value);
    files.forEach(function (f) { data.append("file", f, f.name); });
    var li = form.closest(".row");
    form.querySelectorAll("button").forEach(function (b) { b.disabled = true; });
    fetch(form.action, { method: "POST", headers: HEADERS, credentials: "same-origin", body: data })
      .then(text).then(function (html) { swapRow(li, html); refreshState(); })
      .catch(function (x) {
        delete form.dataset.busy;
        form.querySelectorAll("button").forEach(function (b) { b.disabled = false; });
        say(form, x.message);
      });
  }

  // --- forms inside rows: a stage change or a note, answered in place ---
  page.addEventListener("submit", function (e) {
    var form = e.target.closest && e.target.closest("form[data-ajax]");
    if (!form) return;
    e.preventDefault();
    var kind = form.getAttribute("data-ajax");
    if (kind === "chatwrite") { sendWrite(form); return; }
    if (kind === "thread" && chosen(form).length) { sendWrite(form); return; }
    if (kind === "thread") {
      var box = form.querySelector("textarea");
      var err = form.querySelector(".say-error");
      if (box.value.trim() === "") { err.textContent = "Write something first."; err.hidden = false; box.focus(); return; }
      err.hidden = true;
      fetch(form.action, { method: "POST", headers: HEADERS, credentials: "same-origin", body: new URLSearchParams(new FormData(form)) })
        .then(text).then(function (html) {
          var thread = form.closest(".thread");
          thread.innerHTML = "";
          thread.appendChild(toNodes(html));
        }).catch(function (x) { err.textContent = x.message; err.hidden = false; });
      return;
    }
    var li = form.closest(".row");
    if (form.hasAttribute("data-reply")) {
      // A reply is sent from its box (SPEC 13). On a phone Approve asks once more.
      var box = form.closest(".reply-box");
      if (phone.matches && !form.dataset.confirmed) {
        box.querySelector(".reply-acts").hidden = true;
        box.querySelector(".reply-confirm").hidden = false;
        box.querySelector(".js-send").focus();
        return;
      }
      delete form.dataset.confirmed;
      var err = box.querySelector(".reply-error");
      err.hidden = true;
      box.querySelectorAll("button").forEach(function (b) { b.disabled = true; });
      fetch(form.action, { method: "POST", headers: HEADERS, credentials: "same-origin", body: new URLSearchParams(new FormData(form)) })
        .then(text).then(function (html) { swapRow(li, html); refreshState(); })
        .catch(function (x) {
          box.querySelectorAll("button").forEach(function (b) { b.disabled = false; });
          box.querySelector(".reply-acts").hidden = false;
          box.querySelector(".reply-confirm").hidden = true;
          err.textContent = x.message;
          err.hidden = false;
        });
      return;
    }
    var quick = form.classList.contains("quick-act");
    // A nervous double tap on the one icon must not resolve and then reopen: after a tap the control does nothing more until the row has
    // been swapped, and the icon that takes its place ignores taps for a moment (QUICK_HOLD_MS) after it appears. Undo is the way back.
    if (quick && (form.dataset.busy || Date.now() < Number(form.dataset.holdUntil || 0))) return;
    if (quick) form.dataset.busy = "1";
    var was = li.getAttribute("data-stage"), num = (li.querySelector(".line-num") || {}).textContent || "";
    fetch(form.action, { method: "POST", headers: HEADERS, credentials: "same-origin", body: new URLSearchParams(new FormData(form)) })
      .then(text).then(function (html) {
        var fresh = swapRow(li, html); refreshState();
        if (quick) {
          var next = fresh.querySelector(".quick-act");
          if (next) next.dataset.holdUntil = String(Date.now() + QUICK_HOLD_MS);
          undoLine(fresh.getAttribute("data-id"), num, was, /\/reopen$/.test(form.getAttribute("action")));
        }
      }).catch(function () { delete form.dataset.busy; });
  });
  var QUICK_HOLD_MS = 600;
  // One tap on a phone resolves (or reopens) a closed row; a line at the bottom says so and offers Undo for six seconds (SPEC 5).
  var undoTimer = 0;
  function undoLine(id, num, was, reopened) {
    var bar = document.getElementById("undo-bar");
    if (!bar) {
      bar = document.createElement("div");
      bar.id = "undo-bar"; bar.className = "undo-bar"; bar.setAttribute("role", "status");
      bar.innerHTML = '<span class="undo-text"></span><button type="button" class="secondary js-undo">Undo</button>';
      document.body.appendChild(bar);
    }
    window.clearTimeout(undoTimer);
    bar.hidden = false;
    bar.querySelector(".undo-text").textContent = (reopened ? "Reopened " : "Resolved ") + num + ".";
    // back to where it came from: the server returns a resolved item to the stage it held before (New, In progress or Pending); resolve after a reopen
    var back = ["undo"]; // the server knows which move was the owner's last: a resolve goes back to where it came from, a reopen back to Resolved
    bar.querySelector(".js-undo").onclick = function () {
      window.clearTimeout(undoTimer);
      bar.hidden = true;
      var run = back.reduce(function (p, step) {
        return p.then(function () { return fetch("/items/" + id + "/" + step, { method: "POST", headers: HEADERS, credentials: "same-origin", body: new URLSearchParams() }).then(text); });
      }, Promise.resolve());
      run.then(function (html) {
          var cur = document.getElementById("item-" + id);
          if (cur) swapRow(cur, html);
          refreshState();
        }).catch(function () {});
    };
    undoTimer = window.setTimeout(function () { bar.hidden = true; }, 6000);
  }
  // Discuss opens the note box; on a phone Send / Cancel is the second question.
  page.addEventListener("click", function (e) {
    var b = e.target.closest && e.target.closest(".js-discuss, .js-send, .js-cancel");
    if (!b) return;
    var box = b.closest(".reply-box");
    if (b.classList.contains("js-discuss")) {
      var note = b.closest(".thread").querySelector(".say textarea");
      if (note) focusBox(note, true);
    } else if (b.classList.contains("js-send")) {
      var form = box.querySelector("form[data-reply]");
      b.disabled = true;
      form.dataset.confirmed = "1";
      form.requestSubmit();
    } else {
      box.querySelector(".reply-confirm").hidden = true;
      box.querySelector(".reply-acts").hidden = false;
    }
  });
  page.addEventListener("input", function (e) {
    var box = e.target;
    if (box.tagName !== "TEXTAREA") return;
    var err = box.form && box.form.querySelector(".say-error");
    if (err) err.hidden = true;
    box.style.height = "auto";
    box.style.height = Math.min(box.scrollHeight, 192) + "px";
  });

  // --- keys on a laptop: j and k move, Enter opens, Esc closes, c writes ---
  function visibleRows() {
    return Array.prototype.slice.call(page.querySelectorAll(".rows:not([hidden]) .row"));
  }
  function mark(li) {
    page.querySelectorAll(".row.kbd").forEach(function (r) { r.classList.remove("kbd"); });
    li.classList.add("kbd");
    var b = li.querySelector(".js-row-toggle");
    if (b) { if (lapPanes()) b.focus({ preventScroll: true }); else b.focus(); reveal(b, false); }
    updateRibbon();
  }
  document.addEventListener("keydown", function (e) {
    if (e.ctrlKey || e.metaKey || e.altKey) return;
    var tag = (e.target.tagName || "").toLowerCase();
    var typing = tag === "textarea" || tag === "input" || tag === "select";
    var cur = page.querySelector(".row.kbd");
    var rows = visibleRows();
    var i = cur ? rows.indexOf(cur) : -1;
    if (e.key === "Escape") {
      var li = (e.target.closest && e.target.closest(".row")) || cur;
      if (li && li.classList.contains("open")) { closeRow(li); e.preventDefault(); }
      return;
    }
    if (typing) return;
    var onRow = lapPanes() && e.target.closest && e.target.closest(".js-row-toggle");
    if ((e.key === "j" || (onRow && e.key === "ArrowDown")) && rows.length) { mark(rows[Math.min(i + 1, rows.length - 1)]); e.preventDefault(); }
    else if ((e.key === "k" || (onRow && e.key === "ArrowUp")) && rows.length) { mark(rows[Math.max(i - 1, 0)]); e.preventDefault(); }
    else if (e.key === "Enter" && cur && !(e.target.closest && e.target.closest(".js-row-toggle"))) { toggleRow(cur); e.preventDefault(); }
    else if (e.key === "c") {
      var open = cur && cur.classList.contains("open") ? cur : page.querySelector(".row.open");
      // c is for the note box under the thread, not the chat box (SPEC 2)
      var ta = open && open.querySelector(".thread .say textarea");
      if (ta) { focusBox(ta, false); e.preventDefault(); }
    }
  });

  // --- the three panes of a laptop (SPEC 1) ---
  function savedStage() {
    try { var v = localStorage.getItem(STAGE_KEY); return STAGES.indexOf(v) >= 0 ? v : "new"; } catch (e) { return "new"; }
  }
  function stageGroup(st) { return page.querySelector('.group[data-stage="' + st + '"]'); }
  function stageButton(st) { return document.querySelector('.js-stage[data-stage="' + st + '"]'); }
  // The heading of the middle pane: the stage and how many items it holds.
  function midHead() {
    var name = document.getElementById("mid-name"), cnt = document.getElementById("mid-count");
    if (!panes || !name || !cnt || !laptop.matches) return;
    var b = stageButton(stage);
    var label = b ? b.querySelector(".st-name").textContent : stage;
    var n = b ? parseInt(b.querySelector(".st-count").textContent, 10) || 0 : 0;
    // A row that the owner has moved on stays where it is until the page is read again: the sentence about an empty stage must not
    // push it down while it is still there (SPEC 2: nothing jumps).
    var g = stageGroup(stage), held = g ? g.querySelectorAll(".row").length : 0;
    var empty = n === 0 && held === 0;
    name.textContent = label;
    cnt.textContent = empty ? "Nothing in " + label + "." : (n === 1 ? "1 item" : n + " items, newest first");
    var hd = document.getElementById("mid-head");
    if (hd) hd.classList.toggle("is-empty", empty);
  }
  function anyEdited(list) {
    var rows = list.querySelectorAll(".row.open");
    for (var i = 0; i < rows.length; i++) if (rowIsBeingEdited(rows[i])) return true;
    return false;
  }
  function chooseStage(st, keep) {
    stage = st;
    if (!keep) { try { localStorage.setItem(STAGE_KEY, st); } catch (e) {} }
    document.querySelectorAll(".js-stage").forEach(function (b) { b.setAttribute("aria-pressed", b.getAttribute("data-stage") === st ? "true" : "false"); });
    page.querySelectorAll(".row.kbd").forEach(function (r) { r.classList.remove("kbd"); });
    STAGES.forEach(function (x) {
      var g = stageGroup(x);
      if (!g) return;
      var list = g.querySelector(".rows");
      // a stage chosen again is read afresh, unless something is being written in it
      if (x === st && !keep && list.getAttribute("data-loaded") && !anyEdited(list)) list.removeAttribute("data-loaded");
      showGroup(g.querySelector(".js-group"), x === st);
    });
    midHead();
    rowsLoaded();
  }
  // The chosen row is the one the keyboard cursor is on or the one last clicked;
  // when the stage is shown it is its first row.
  function chosenRow() { return page.querySelector(".rows:not([hidden]) .row.kbd"); }
  function chooseRow(li) {
    page.querySelectorAll(".row.kbd").forEach(function (r) { r.classList.remove("kbd"); });
    li.classList.add("kbd");
    updateRibbon();
  }
  function rowsLoaded() {
    if (!panes || !laptop.matches) return;
    if (!chosenRow()) {
      var rows = visibleRows();
      if (rows.length) rows[0].classList.add("kbd");
    }
    updateRibbon();
  }
  function rowAct(li, name) { return li.querySelector('.line-acts form[action$="/' + name + '"]'); }
  // The list is one stop for Tab: the chosen row (and the buttons of its proposal) can be reached, the other rows with the arrow
  // keys, j and k, so the agenda and the to-dos are a few Tabs away, not dozens (UX review 7).
  function rove() {
    if (!lapPanes()) return;
    var cur = chosenRow();
    page.querySelectorAll(".rows .row").forEach(function (r) {
      var on = r === cur;
      var t = r.querySelector(".js-row-toggle");
      if (t) t.tabIndex = on ? 0 : -1;
      r.querySelectorAll(".prop-chip button").forEach(function (b) { b.tabIndex = on ? 0 : -1; });
    });
  }
  function updateRibbon() {
    rove();
    var bar = document.getElementById("ribbon-tools");
    if (!bar) return;
    var li = chosenRow();
    var st = li ? li.getAttribute("data-stage") : "";
    function set(act, on, hide) {
      var b = bar.querySelector('[data-act="' + act + '"]');
      if (!b) return;
      b.setAttribute("aria-disabled", on ? "false" : "true");
      if (hide !== undefined) b.hidden = hide;
    }
    set("open", !!li);
    set("reply", !!li);
    set("pending", !!li && !!rowAct(li, "pending"));
    set("resolve", !!li && !!rowAct(li, "resolve"), st === "resolved");
    set("reopen", !!li && !!rowAct(li, "reopen"), !(st === "pending" || st === "resolved"));
    set("todo", !!li && !!rowAct(li, "todo"));
  }
  // Reply opens the row and puts the cursor in its reply box, or the chat box,
  // or the note box; it sends nothing.
  function focusReply(li) {
    function go(row) {
      var ta = row.querySelector(".reply-text") || row.querySelector(".chatbox textarea") || row.querySelector(".thread .say textarea");
      if (ta) focusBox(ta, true);
    }
    if (li.classList.contains("open")) { go(li); return; }
    openRow(li, false).then(function () {
      var row = document.getElementById("item-" + li.getAttribute("data-id"));
      if (row) go(row);
    });
  }
  var ribbon = document.getElementById("ribbon-tools");
  if (ribbon) ribbon.addEventListener("click", function (e) {
    var b = e.target.closest && e.target.closest("button.rb");
    if (!b || b.getAttribute("aria-disabled") === "true") return;
    var li = chosenRow();
    if (!li) return;
    var act = b.getAttribute("data-act");
    if (act === "open") {
      if (li.classList.contains("open")) reveal(li, false); else openRow(li, false);
    } else if (act === "reply") {
      focusReply(li);
    } else {
      // the very form the row's own button uses
      var f = rowAct(li, act);
      if (f) f.requestSubmit();
    }
  });
  document.addEventListener("click", function (e) {
    var b = e.target.closest && e.target.closest(".js-stage");
    if (b) chooseStage(b.getAttribute("data-stage"), false);
  });
  if (panes) {
    stage = savedStage();
    if (laptop.matches) chooseStage(stage, true);
    laptop.addEventListener("change", function () { if (laptop.matches) chooseStage(stage, true); });
  }
})();

// The agenda beside the list: hidden or shown (remembered), filters
// (remembered), done and add by form, refreshed when a row changes. The server
// renders everything, calendar text included; this script only swaps it in.
(function () {
  var aside = document.getElementById("agenda");
  var body = document.getElementById("ag-body");
  if (!aside || !body) return;
  var SHOW_KEY = "one.agenda";
  var FILTER_KEY = "one.agenda.filter";
  var HEADERS = { "X-Requested-With": "one" };
  var buttons = document.querySelectorAll(".js-agenda-toggle");
  var earlier = false;
  var scrolled = false;

  function store(k, v) { try { localStorage.setItem(k, v); } catch (e) {} }
  function load(k) { try { return localStorage.getItem(k); } catch (e) { return null; } }
  function isOn() { return !aside.hidden; }
  function show(on, remember) {
    aside.hidden = !on;
    document.documentElement.classList.toggle("agenda-on", on);
    buttons.forEach(function (b) { b.setAttribute("aria-expanded", on ? "true" : "false"); });
    if (remember) store(SHOW_KEY, on ? "1" : "0");
    if (on) toToday();
  }
  // The agenda opens at the line for the present moment (the Now line), and the
  // button Now brings it back. The line sits about a third of the way down.
  // On a laptop the agenda is a pane of its own and its events scroll by themselves;
  // on a phone the whole page of the agenda scrolls.
  var lap = window.matchMedia("(min-width: 68rem)");
  function scroller() { return (lap.matches && document.getElementById("ag-events")) || aside; }
  function toToday() {
    var line = document.getElementById("ag-now");
    if (!line || aside.hidden) return;
    var sc = scroller();
    var bar = sc === aside ? aside.querySelector(".ag-bar") : null;
    var barH = bar ? bar.offsetHeight : 0;
    var y = line.getBoundingClientRect().top - sc.getBoundingClientRect().top + sc.scrollTop;
    sc.scrollTop = Math.max(0, y - barH - (sc.clientHeight - barH) * 0.3);
    scrolled = true;
  }
  // The link "<n> to do" in the top line opens the agenda at the to-dos.
  function toTodos() {
    var h = document.getElementById("ag-todo-h");
    if (!h || aside.hidden) return;
    var bar = aside.querySelector(".ag-bar");
    var barH = bar ? bar.offsetHeight : 0;
    var y = h.getBoundingClientRect().top - aside.getBoundingClientRect().top + aside.scrollTop;
    aside.scrollTop = Math.max(0, y - barH - 8);
    scrolled = true;
    h.setAttribute("tabindex", "-1");
    h.focus({ preventScroll: true });
  }
  var todoLink = document.getElementById("todo-link");
  if (todoLink) todoLink.addEventListener("click", function (e) {
    e.preventDefault();
    // on a phone the to-dos are a group of the list, next to the stages (SPEC 19)
    var gh = !lap.matches && document.querySelector(".group-todo .js-group");
    if (gh) {
      if (gh.getAttribute("aria-expanded") !== "true") gh.click();
      gh.scrollIntoView({ block: "start" });
      gh.focus({ preventScroll: true });
      return;
    }
    show(true, true);
    toTodos();
  });
  var saved = load(SHOW_KEY);
  var wide = window.matchMedia("(min-width: 68rem)").matches;
  var nowBtn = aside.querySelector(".js-now");
  if (nowBtn) nowBtn.addEventListener("click", function () { toToday(); });
  buttons.forEach(function (b) {
    b.hidden = false;
    b.addEventListener("click", function () { show(!isOn(), true); });
  });
  // On a phone a tap on an entry opens it in place (one at a time, a second tap closes it);
  // on a laptop the entry shows its button on hover or focus by style alone.
  body.addEventListener("click", function (e) {
    if (lap.matches) return;
    var li = e.target.closest && e.target.closest(".ag-ev");
    if (!li || e.target.closest("a, button, form, input, select, textarea, .ag-props")) return;
    var was = li.classList.contains("open");
    body.querySelectorAll(".ag-ev.open").forEach(function (o) { o.classList.remove("open"); });
    if (!was) li.classList.add("open");
  });
  // At a laptop width the agenda is always there; the button and Close are not shown.
  function hint() {
    var f = body.querySelector("#ag-new");
    if (!f) return;
    if (lap.matches) f.setAttribute("placeholder", "Add a to-do"); else f.removeAttribute("placeholder");
  }
  function place() { show(lap.matches ? true : (saved === null ? wide : saved === "1"), false); hint(); }
  place();
  lap.addEventListener("change", function () { scrolled = false; saved = load(SHOW_KEY); place(); });

  // --- filters ---
  function filters() { try { return JSON.parse(load(FILTER_KEY) || "{}"); } catch (e) { return {}; } }
  function applyFilters() {
    var f = filters();
    var p = body.querySelector("#ag-person"), c = body.querySelector("#ag-cal");
    if (p) p.value = f.person || "";
    if (c) c.value = f.cal || "";
    if (p && p.value !== (f.person || "")) p.value = "";
    if (c && c.value !== (f.cal || "")) c.value = "";
    var pv = p ? p.value : "", cv = c ? c.value : "";
    var shown = 0, any = 0;
    body.querySelectorAll(".ag-ev").forEach(function (li) {
      any++;
      var ok = (!pv || li.getAttribute("data-person") === pv) &&
        (!cv || (" " + li.getAttribute("data-cals") + " ").indexOf(" " + cv + " ") >= 0);
      li.hidden = !ok;
      if (ok) shown++;
    });
    body.querySelectorAll(".ag-day").forEach(function (d) {
      d.hidden = !d.querySelector(".ag-ev:not([hidden]), .ag-now");
    });
    var none = body.querySelector("#ag-none");
    if (none) none.hidden = !(any > 0 && shown === 0);
  }
  body.addEventListener("change", function (e) {
    var sel = e.target;
    if (!sel.getAttribute || !sel.getAttribute("data-filter")) return;
    var f = filters();
    f[sel.getAttribute("data-filter")] = sel.value;
    store(FILTER_KEY, JSON.stringify(f));
    applyFilters();
  });

  // --- swapping the body ---
  // The phone's group To do on the list page shows the same to-dos as the agenda: it is rebuilt from every answer.
  var tbox = document.getElementById("g-todo");
  function syncTodoGroup(src) {
    if (!tbox) return;
    var list = tbox.querySelector(".td-list"), none = tbox.querySelector(".td-none");
    var items = src.querySelectorAll(".ag-td");
    var props = tbox.querySelector(".td-props");
    if (props) {
      props.textContent = "";
      var ul = src.querySelector(".ag-props");
      if (ul) props.appendChild(ul.cloneNode(true));
    }
    list.textContent = "";
    items.forEach(function (n) { list.appendChild(n.cloneNode(true)); });
    if (none) none.hidden = items.length > 0;
    document.querySelectorAll('[data-count="todo"]').forEach(function (el) { el.textContent = String(items.length); });
  }
  function put(html) {
    var t = document.createElement("template");
    t.innerHTML = html;
    var top = scroller().scrollTop;
    syncTodoGroup(t.content);
    body.innerHTML = "";
    body.appendChild(t.content);
    applyFilters();
    tick();
    hint();
    scroller().scrollTop = top;
    if (!scrolled) toToday();
  }
  function reload() {
    var box = body.querySelector("#ag-new"), tb = tbox && tbox.querySelector('input[name="text"]');
    if (box && (box.value !== "" || document.activeElement === box)) return Promise.resolve();
    if (tb && (tb.value !== "" || document.activeElement === tb)) return Promise.resolve();
    return fetch("/agenda" + (earlier ? "?earlier=1" : ""), { credentials: "same-origin", cache: "no-store" })
      .then(function (r) { return r.ok ? r.text() : Promise.reject(); }).then(put).catch(function () {});
  }
  // --- the Now line and the running mark stay current while the page is open ---
  function clockIn(zone, ms) {
    try {
      return new Intl.DateTimeFormat("en-GB", { timeZone: zone, hour: "2-digit", minute: "2-digit", hourCycle: "h23" }).format(new Date(ms));
    } catch (e) {
      return new Intl.DateTimeFormat("en-GB", { hour: "2-digit", minute: "2-digit", hourCycle: "h23" }).format(new Date(ms));
    }
  }
  function tick() {
    var line = document.getElementById("ag-now");
    var evs = document.getElementById("ag-events");
    if (!line || !evs) return;
    var nowS = Math.floor(Date.now() / 1000);
    var label = line.querySelector(".ag-now-text");
    if (label) label.textContent = "Now " + clockIn(evs.getAttribute("data-zone") || "Europe/Prague", Date.now());
    line.setAttribute("data-at", String(nowS));
    var list = line.parentNode;
    var before = null;
    var items = list.querySelectorAll(".ag-ev");
    for (var i = 0; i < items.length; i++) {
      if (items[i].parentNode !== list) continue;
      var st = items[i].getAttribute("data-start");
      if (st !== null && Number(st) > nowS) { before = items[i]; break; }
    }
    if (before) { if (line.nextElementSibling !== before) list.insertBefore(line, before); }
    else if (list.lastElementChild !== line) list.appendChild(line);
    body.querySelectorAll(".ag-ev[data-start]").forEach(function (li) {
      var on = Number(li.getAttribute("data-start")) <= nowS && nowS < Number(li.getAttribute("data-end")) && !li.classList.contains("off");
      li.classList.toggle("running", on);
      var mark = li.querySelector(".ag-run");
      if (mark) mark.hidden = !on;
    });
  }
  window.setInterval(function () { if (!document.hidden) tick(); }, 15000);
  document.addEventListener("visibilitychange", function () { if (!document.hidden) tick(); });
  document.addEventListener("one-changed", function () { reload(); });
  window.setInterval(function () { if (!document.hidden) reload(); }, 60000);

  function onSubmit(e) {
    var form = e.target.closest && e.target.closest("form[data-ajax]");
    if (!form) return;
    e.preventDefault();
    var kind = form.getAttribute("data-ajax");
    if (kind === "agenda-earlier") {
      earlier = true;
      var btn = form.querySelector("button");
      if (btn) btn.disabled = true;
      reload();
      return;
    }
    var box = form.querySelector('input[name="text"]');
    var err = form.querySelector(".say-error");
    if (box && box.value.trim() === "") { if (err) { err.textContent = "Write the to-do first."; err.hidden = false; } box.focus(); return; }
    var data = new URLSearchParams(new FormData(form));
    if (earlier) data.set("earlier", "1");
    fetch(form.action, { method: "POST", headers: HEADERS, credentials: "same-origin", body: data })
      .then(function (r) { return r.text().then(function (m) { if (!r.ok) throw new Error(m || "Something went wrong."); return m; }); })
      .then(function (m) { if (box && tbox && tbox.contains(form)) box.value = ""; put(m); })
      .catch(function (x) { if (err) { err.textContent = x.message; err.hidden = false; } });
  }
  body.addEventListener("submit", onSubmit);
  if (tbox) tbox.addEventListener("submit", onSubmit);
  body.addEventListener("input", function (e) {
    var err = body.querySelector("#ag-error");
    if (err && e.target.id === "ag-new") err.hidden = true;
  });
  if (tbox) tbox.addEventListener("input", function (e) {
    var err = tbox.querySelector(".say-error");
    if (err && e.target.name === "text") err.hidden = true;
  });
  document.addEventListener("keydown", function (e) {
    if (e.ctrlKey || e.metaKey || e.altKey || e.key !== "a") return;
    var tag = (e.target.tagName || "").toLowerCase();
    if (tag === "textarea" || tag === "input" || tag === "select") return;
    if (lap.matches) {
      // the agenda is always there: a puts the cursor in it, at the present moment
      toToday();
      aside.setAttribute("tabindex", "-1");
      aside.focus({ preventScroll: true });
      e.preventDefault();
      return;
    }
    show(!isOn(), true);
    e.preventDefault();
  });
  applyFilters();
})();

// Connections on the Connectors page: each is one closed row that opens on a
// click. A row the owner opened stays open when the page reloads itself while an
// import runs, and the one named in the address (#gmail) opens at once.
(function () {
  var rows = document.querySelectorAll(".crow > details");
  if (!rows.length) return;
  var KEY = "one.conn.open";
  function read() { try { return JSON.parse(sessionStorage.getItem(KEY) || "[]"); } catch (e) { return []; } }
  function write(set) { try { sessionStorage.setItem(KEY, JSON.stringify(set)); } catch (e) {} }
  var set = read();
  rows.forEach(function (d) {
    var id = d.parentNode.id;
    if (set.indexOf(id) >= 0 || ("#" + id) === window.location.hash) d.open = true;
  });
  rows.forEach(function (d) {
    d.addEventListener("toggle", function () {
      var id = d.parentNode.id;
      var now = read().filter(function (x) { return x !== id; });
      if (d.open) now.push(id);
      write(now);
    });
  });
})();

// Search while typing (SPEC 26). After each short pause in typing the field
// asks /search/live and shows the answer under itself, grouped. Everything
// from mail goes in with textContent and text nodes, never as markup. A newer
// keystroke cancels the request before it. The key / puts the cursor in the
// field when the owner is not typing elsewhere.
(function () {
  var input = document.getElementById("q");
  var box = document.getElementById("live");
  if (!input || !box) return;
  var form = input.form;
  var DELAY = 60;   // ms of quiet before asking
  var MIN = 2;      // letters before asking
  var timer = null, ctl = null, seq = 0, active = -1, rows = [];
  input.setAttribute("aria-expanded", "false");

  function el(tag, cls, txt) {
    var n = document.createElement(tag);
    if (cls) n.className = cls;
    if (txt) n.textContent = txt;
    return n;
  }
  function parts(into, list) {
    (list || []).forEach(function (p) {
      if (p.h) into.appendChild(el("mark", "", p.t));
      else into.appendChild(document.createTextNode(p.t));
    });
  }
  function close() {
    if (ctl) { ctl.abort(); ctl = null; }
    window.clearTimeout(timer);
    box.hidden = true;
    box.textContent = "";
    rows = [];
    active = -1;
    input.setAttribute("aria-expanded", "false");
    input.removeAttribute("aria-activedescendant");
  }
  function mark(i) {
    if (!rows.length) return;
    if (i < 0) i = rows.length - 1;
    if (i >= rows.length) i = 0;
    if (active >= 0) { rows[active].classList.remove("active"); rows[active].setAttribute("aria-selected", "false"); }
    active = i;
    rows[i].classList.add("active");
    rows[i].setAttribute("aria-selected", "true");
    input.setAttribute("aria-activedescendant", rows[i].id);
    rows[i].scrollIntoView({ block: "nearest" });
  }
  function show(ans, q) {
    box.textContent = "";
    rows = [];
    active = -1;
    if (!ans.groups.length) box.appendChild(el("p", "live-none", "No results."));
    ans.groups.forEach(function (g) {
      box.appendChild(el("div", "live-group", g.label));
      g.hits.forEach(function (h) {
        var a = el("a", "live-row");
        a.href = h.link;
        a.id = "live-" + rows.length;
        a.setAttribute("role", "option");
        a.setAttribute("aria-selected", "false");
        var l1 = el("span", "live-line");
        var title = el("span", "live-title");
        parts(title, h.title);
        l1.appendChild(title);
        var meta = [h.who, h.when].filter(Boolean).join(" · ");
        if (meta) l1.appendChild(el("span", "live-meta", meta));
        a.appendChild(l1);
        if (h.snippet && h.snippet.length) {
          var l2 = el("span", "live-snip");
          parts(l2, h.snippet);
          a.appendChild(l2);
        }
        rows.push(a);
        box.appendChild(a);
      });
    });
    var all = el("a", "live-all", "All results for this search");
    all.href = "/search?q=" + encodeURIComponent(q);
    box.appendChild(all);
    box.hidden = false;
    input.setAttribute("aria-expanded", "true");
  }
  function ask() {
    var q = input.value.trim();
    if (q.length < MIN) { close(); return; }
    if (ctl) ctl.abort();
    ctl = window.AbortController ? new AbortController() : null;
    var mine = ++seq;
    fetch("/search/live?q=" + encodeURIComponent(q), { credentials: "same-origin", cache: "no-store", signal: ctl ? ctl.signal : undefined })
      .then(function (r) { return r.ok ? r.json() : Promise.reject(); })
      .then(function (ans) { if (mine === seq && document.activeElement === input) show(ans, q); })
      .catch(function () {});
  }
  input.addEventListener("input", function () {
    window.clearTimeout(timer);
    if (input.value.trim().length < MIN) { close(); return; }
    timer = window.setTimeout(ask, DELAY);
  });
  input.addEventListener("focus", function () {
    if (input.value.trim().length >= MIN && box.hidden) ask();
  });
  input.addEventListener("keydown", function (e) {
    if (e.key === "ArrowDown" || e.key === "ArrowUp") {
      if (box.hidden) { if (input.value.trim().length >= MIN) ask(); e.preventDefault(); return; }
      mark(active + (e.key === "ArrowDown" ? 1 : -1));
      e.preventDefault();
    } else if (e.key === "Enter") {
      if (!box.hidden && active >= 0) { e.preventDefault(); window.location.assign(rows[active].href); }
    } else if (e.key === "Escape") {
      if (!box.hidden) { close(); e.preventDefault(); e.stopPropagation(); }
      else input.blur();
    }
  });
  box.addEventListener("mousedown", function (e) { e.preventDefault(); }); // keep the cursor in the field
  document.addEventListener("click", function (e) { if (!form.contains(e.target)) close(); });
  form.addEventListener("focusout", function (e) { if (!e.relatedTarget || !form.contains(e.relatedTarget)) close(); });
  document.addEventListener("keydown", function (e) {
    if (e.key !== "/" || e.ctrlKey || e.metaKey || e.altKey) return;
    var t = e.target;
    var tag = ((t && t.tagName) || "").toLowerCase();
    if (tag === "textarea" || tag === "input" || tag === "select" || (t && t.isContentEditable)) return;
    input.focus();
    input.select();
    e.preventDefault();
  });
})();

// The Refresh button in the top line (SPEC 15): fetches everything now. While a
// fetch runs it says so and pressing it again does nothing; when it has finished
// it says when, until the next reload.
(function () {
  var btn = document.getElementById("refresh");
  if (!btn) return;
  var HEADERS = { "X-Requested-With": "one" };
  var timer = null;
  var lab = btn.querySelector(".rb-label") || btn; // the icon stays; only the word changes
  // What happened (SPEC 15): when it finished, how many came, and each source
  // that did not answer, with its error. Text only; the errors are the app's own words.
  var box = document.getElementById("refresh-note");
  function note(st) {
    if (!box) return;
    box.textContent = "";
    function line(cls, text) {
      var p = document.createElement("p");
      p.className = cls;
      p.textContent = text;
      box.appendChild(p);
    }
    line("rn-main", "Refreshed " + st.finished + " " + (st["new"] > 0 ? st["new"] + " new" : "nothing new"));
    (st.failed || []).forEach(function (f) { line("rn-fail", f.source + " failed: " + f.error); });
    box.hidden = false;
    document.body.classList.add("rn-on"); // on a laptop the notice is a line at the foot of the middle pane that the list leaves room for
  }
  // On a phone the button keeps its word and its width while the notice shows (the top bar does not move); the notice line says when.
  var narrow = window.matchMedia("(max-width: 67.99rem)");
  function show(st) {
    if (st.running) {
      if (box) box.hidden = true;
      document.body.classList.remove("rn-on");
      lab.textContent = "Refreshing\u2026";
      btn.setAttribute("aria-disabled", "true");
      if (!timer) timer = window.setInterval(poll, 1000);
      return;
    }
    if (timer) { window.clearInterval(timer); timer = null; }
    btn.removeAttribute("aria-disabled");
    if (st.finished && btn.getAttribute("data-asked")) {
      lab.textContent = narrow.matches ? "Refresh" : "Refreshed " + st.finished;
      note(st);
      document.dispatchEvent(new Event("one-refreshed"));
    }
  }
  function poll() {
    fetch("/refresh", { credentials: "same-origin", cache: "no-store" }).then(function (r) { return r.json(); }).then(show).catch(function () {});
  }
  btn.addEventListener("click", function () {
    if (btn.getAttribute("aria-disabled") === "true") return;
    btn.setAttribute("data-asked", "1");
    btn.setAttribute("aria-disabled", "true");
    lab.textContent = "Refreshing\u2026";
    fetch("/refresh", { method: "POST", headers: HEADERS, credentials: "same-origin" })
      .then(function (r) { return r.json(); }).then(show)
      .catch(function () { btn.removeAttribute("aria-disabled"); lab.textContent = "Refresh"; });
  });
})();

// The strips under the heading share one line and never wrap. In order of
// importance (failed sources, review paused, proposals, night check, waiting
// for Claude), what does not fit is shortened with an ellipsis, the least
// important first, then left out; a link at the end says how many are left out
// (`+2 more`), with their full texts in its title and for screen readers.
(function () {
  var box = document.getElementById("strips");
  var more = document.getElementById("strip-more");
  if (!box || !more) return;
  var MIN = 96; // the least a shortened strip keeps, in pixels
  var lastWidth = -1;
  function textOf(s) {
    var a = s.querySelector("a");
    return ((a || s).textContent || "").replace(/\s+/g, " ").trim();
  }
  function fit() {
    var all = Array.prototype.slice.call(box.querySelectorAll(".strip"));
    all.forEach(function (s) { s.classList.remove("strip-drop"); s.style.maxWidth = ""; });
    more.hidden = true;
    var items = all.filter(function (s) { return !s.hidden; });
    if (!items.length) return;
    var gap = parseFloat(getComputedStyle(box).columnGap) || 0;
    var avail = box.clientWidth;
    items.forEach(function (s) { s.style.flex = "none"; });
    var w = items.map(function (s) { return s.getBoundingClientRect().width; });
    var fixed = items.map(function (s) { var f = s.querySelector("form"); return f ? f.getBoundingClientRect().width : 0; });
    items.forEach(function (s) { s.style.flex = ""; });
    var dropped = 0, n = items.length, cap = null;
    function moreWidth(k) {
      more.textContent = "+" + k + " more";
      more.hidden = false;
      var mw = more.getBoundingClientRect().width;
      more.hidden = true;
      return mw;
    }
    function sum(k) { var t = 0; for (var i = 0; i < k; i++) t += w[i]; return t; }
    for (;;) {
      var k = n - dropped;
      var extra = dropped ? gap + moreWidth(dropped) : 0;
      var need = sum(k) + gap * (k - 1) + extra;
      if (need <= avail + 0.5) break;
      var last = k - 1;
      var room = avail - extra - gap * (k - 1) - sum(k - 1); // what the last strip may take
      if (room >= Math.min(w[last], MIN + fixed[last]) || k === 1) {
        cap = { i: last, px: Math.max(room, 40) };
        break;
      }
      dropped++;
    }
    var left = [];
    items.forEach(function (s, i) {
      if (i >= n - dropped) { s.classList.add("strip-drop"); left.push(textOf(s)); }
      else if (cap && cap.i === i) s.style.maxWidth = Math.floor(cap.px) + "px";
    });
    if (dropped) {
      more.textContent = "+" + dropped + " more";
      more.title = left.join(" | ");
      more.setAttribute("aria-label", "+" + dropped + " more: " + left.join(". "));
      more.hidden = false;
    }
  }
  document.addEventListener("one:strips", fit);
  window.addEventListener("load", fit);
  window.addEventListener("resize", fit);
  if (document.fonts && document.fonts.ready) document.fonts.ready.then(fit);
  if (window.ResizeObserver) {
    new ResizeObserver(function () {
      var w = box.clientWidth;
      if (w !== lastWidth) { lastWidth = w; fit(); }
    }).observe(box);
  }
  fit();
})();

// A known model name beside a model field fills it; the field stays free text.
document.addEventListener("click", function (e) {
  var b = e.target.closest && e.target.closest(".js-model");
  if (!b) return;
  var input = document.getElementById(b.getAttribute("data-for"));
  if (!input) return;
  input.value = b.getAttribute("data-model");
  input.focus();
});

// Set picture: one button. Without a chosen file it opens the file chooser; once
// a file is chosen the picture is sent.
(function () {
  document.addEventListener("click", function (e) {
    var b = e.target.closest && e.target.closest(".js-pick");
    if (!b) return;
    var form = b.form;
    var input = form && form.querySelector("input[type=file]");
    if (input && !input.files.length) { e.preventDefault(); input.click(); }
  });
  document.addEventListener("change", function (e) {
    var input = e.target;
    if (!input.classList || !input.classList.contains("pic-input") || !input.files.length) return;
    var form = input.form;
    if (form && !form._sent) { form._sent = true; if (form.requestSubmit) form.requestSubmit(); else form.submit(); }
  });
  // The tokens are shown as dots until Show is pressed; Copy copies the real value.
  document.addEventListener("click", function (e) {
    var b = e.target.closest && e.target.closest(".js-reveal");
    if (!b) return;
    var input = document.getElementById(b.getAttribute("data-target"));
    if (!input) return;
    var show = input.classList.contains("masked");
    input.classList.toggle("masked", !show);
    b.textContent = show ? "Hide" : "Show";
    b.setAttribute("aria-pressed", show ? "true" : "false");
  });
})();

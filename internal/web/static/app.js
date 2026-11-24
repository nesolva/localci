// Live updates without polling: the page listens to the event stream and, when something concerns it, re-reads
// itself and swaps the live region. Nothing navigates, so scroll position and open tooltips survive.
(function () {
  "use strict";
  var live = document.querySelector("[data-live]");
  var badge = document.getElementById("live");
  var tip = document.getElementById("tip");

  function fmt(ms) {
    var s = Math.max(0, Math.floor(ms / 1000));
    if (s < 60) return s + " s";
    if (s < 3600) return Math.floor(s / 60) + " m " + ("0" + (s % 60)).slice(-2) + " s";
    return Math.floor(s / 3600) + " h " + ("0" + Math.floor(s / 60) % 60).slice(-2) + " m";
  }
  // Elapsed counters tick on the client from the start time the server rendered.
  function tick() {
    document.querySelectorAll("time[data-since]").forEach(function (el) {
      el.textContent = fmt(Date.now() - Number(el.dataset.since));
    });
  }
  setInterval(tick, 1000);
  tick();

  // Tooltips: one element, positioned next to the pointer.
  document.addEventListener("pointermove", function (e) {
    var t = e.target.closest ? e.target.closest("[data-tip]") : null;
    if (!t) { tip.hidden = true; return; }
    var inner = e.target.closest(".step[data-tip]");
    tip.textContent = (inner || t).dataset.tip;
    tip.hidden = false;
    var w = tip.offsetWidth;
    tip.style.left = Math.min(e.clientX + 12, window.innerWidth - w - 8) + "px";
    tip.style.top = e.clientY + 16 + "px";
  });
  document.addEventListener("click", function (e) {
    var b = e.target.closest("[data-copy]");
    if (!b || !navigator.clipboard) return;
    navigator.clipboard.writeText(b.dataset.copy).then(function () {
      b.textContent = "Copied";
      setTimeout(function () { b.textContent = "Copy"; }, 1500);
    });
  });

  function follow() {
    var el = document.querySelector("[data-follow-end]");
    if (el) el.scrollTop = el.scrollHeight;
  }
  follow();

  if (!live || !window.EventSource) return;
  var run = live.dataset.run;
  var seq = Number(live.dataset.seq) || 0;
  var follows = live.dataset.follow === "true";
  var pending = null, busy = false, again = false;

  async function refresh() {
    if (busy) { again = true; return; }
    busy = true;
    try {
      var res = await fetch(location.href, { headers: { Accept: "text/html" }, cache: "no-store" });
      if (!res.ok) return;
      var doc = new DOMParser().parseFromString(await res.text(), "text/html");
      var next = doc.querySelector("[data-live]");
      if (!next) return;
      var logs = document.querySelector("[data-follow-end]");
      var atEnd = logs && logs.scrollHeight - logs.scrollTop - logs.clientHeight < 40;
      live.innerHTML = next.innerHTML;
      live.dataset.follow = next.dataset.follow;
      follows = next.dataset.follow === "true";
      tick();
      if (atEnd) follow();
    } catch (_) { /* the next event retries */ } finally {
      busy = false;
      if (again) { again = false; refresh(); }
    }
  }

  // The stream names every event by its kind, so it is read as plain text: each "data:" line is an event.
  function handle(data) {
    if (run && data.run && data.run !== run) return;
    if (!follows && run) return; // a finished run's page no longer changes
    clearTimeout(pending);
    pending = setTimeout(refresh, 200);
  }

  async function connect() {
    try {
      var res = await fetch("/live?since=" + seq, { headers: { Accept: "text/event-stream" }, cache: "no-store" });
      if (!res.ok || !res.body) throw new Error("stream " + res.status);
      if (badge) badge.dataset.state = "on";
      var reader = res.body.getReader(), dec = new TextDecoder(), buf = "";
      for (;;) {
        var chunk = await reader.read();
        if (chunk.done) break;
        buf += dec.decode(chunk.value, { stream: true });
        var parts = buf.split("\n\n");
        buf = parts.pop();
        parts.forEach(function (block) {
          var data = null, id = null;
          block.split("\n").forEach(function (line) {
            if (line.indexOf("id: ") === 0) id = Number(line.slice(4));
            else if (line.indexOf("data: ") === 0) data = line.slice(6);
          });
          if (id) seq = id;
          if (data) { try { handle(JSON.parse(data)); } catch (_) {} }
        });
      }
    } catch (_) { /* reconnect below */ }
    if (badge) badge.dataset.state = "off";
    setTimeout(connect, 2000);
  }
  connect();
})();

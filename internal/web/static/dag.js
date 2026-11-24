// The DAG page: clicking a flow lights its steps in the timeline below and the edges that touch it. The page is
// re-rendered by the live script whenever an event arrives, so the choice is kept here and applied again after each swap.
(function () {
  "use strict";
  var live = document.querySelector("[data-live]");
  if (!live) return;
  var chosen = null;

  function has(list, key) { return (" " + list + " ").indexOf(" " + key + " ") >= 0; }

  function apply() {
    var nodes = live.querySelectorAll(".node[data-key]");
    if (chosen && !Array.prototype.some.call(nodes, function (n) { return n.dataset.key === chosen; })) chosen = null;
    nodes.forEach(function (n) {
      var on = n.dataset.key === chosen;
      n.classList.toggle("is-on", on);
      var b = n.querySelector(".n-pick");
      if (b) b.setAttribute("aria-pressed", on ? "true" : "false");
    });
    live.querySelectorAll(".dag-edges path").forEach(function (p) {
      p.classList.toggle("is-on", !!chosen && (p.dataset.from === chosen || p.dataset.to === chosen));
    });
    var tl = live.querySelector(".timeline:not(.empty)");
    if (tl) tl.classList.toggle("has-hl", !!chosen);
    live.querySelectorAll(".step[data-flows]").forEach(function (s) {
      s.classList.toggle("hit", !!chosen && has(s.dataset.flows, chosen));
    });
  }

  function pick(n) {
    chosen = chosen === n.dataset.key ? null : n.dataset.key;
    apply();
  }

  // The flow's button is a real <button>: Enter and Space arrive here as clicks. A test dot is a link and navigates.
  document.addEventListener("click", function (e) {
    var b = e.target.closest && e.target.closest(".n-pick");
    var n = b && b.closest(".node[data-key]");
    if (n) pick(n);
  });
  // Only children are watched, and apply() changes attributes only, so it cannot trigger itself.
  new MutationObserver(apply).observe(live, { childList: true });
  apply();
})();

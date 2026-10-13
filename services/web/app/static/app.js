// Narrow screens show tables as stacked rows: every cell gets the column header as its
// label (data-label), read from the header row. Tables without a header row stay as they are.
(function () {
  function label(root) {
    root.querySelectorAll("table").forEach(function (table) {
      if (table.classList.contains("stack") || table.closest(".prose")) return;
      var head = table.querySelector("tr");
      if (!head || !head.querySelector("th")) return;
      var names = Array.prototype.map.call(head.children, function (th) { return th.textContent.trim(); });
      head.classList.add("head");
      Array.prototype.forEach.call(table.querySelectorAll("tr"), function (tr) {
        if (tr === head) return;
        Array.prototype.forEach.call(tr.children, function (td, i) {
          if (td.colSpan > 1) { td.classList.add("span"); return; }
          if (names[i]) td.setAttribute("data-label", names[i]);
        });
      });
      table.classList.add("stack");
    });
  }
  document.addEventListener("DOMContentLoaded", function () { label(document); });
  // htmx replaces parts of pages (dashboard refresh): label the new tables.
  document.addEventListener("htmx:afterSettle", function () { label(document); });
})();

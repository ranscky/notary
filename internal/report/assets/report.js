/* Notary report filter: progressive enhancement, and nothing else.
 *
 * The renderer writes the index table complete, with every memory in the slice
 * and a working link on every row. This file only HIDES rows that do not match
 * what the reader typed, and it does that after the reader types: with this
 * file missing or blocked, no memory is lost, no link breaks, and the filter
 * control is not shown at all -- it is revealed here, because a control that
 * cannot work is worse than no control.
 *
 * It carries no index data and makes no request: the rows it filters are the
 * DOM the renderer wrote, and the query is compared against their own text.
 * That is also why this file is byte-identical in every report.
 */
(function () {
  "use strict";

  var box = document.getElementById("filter-box");
  var input = document.getElementById("filter-input");
  var table = document.getElementById("memories");
  var note = document.getElementById("filter-note");
  var count = document.getElementById("filter-count");
  if (!box || !input || !table || !note) {
    // No table to filter (an empty slice), or a page that is not the index.
    // Returning leaves the control hidden: nothing to reveal, nothing to break.
    return;
  }

  var tbody = table.tBodies[0];
  if (!tbody) {
    return;
  }
  var rows = [];
  for (var i = 0; i < tbody.rows.length; i++) {
    rows.push(tbody.rows[i]);
  }

  function apply() {
    var query = input.value.trim().toLowerCase();
    var shown = 0;
    for (var i = 0; i < rows.length; i++) {
      var match = query === "" || rows[i].textContent.toLowerCase().indexOf(query) !== -1;
      rows[i].hidden = !match;
      if (match) {
        shown++;
      }
    }
    note.hidden = shown !== 0;
    if (count) {
      count.textContent = shown + " of " + rows.length + " memories shown";
    }
  }

  input.addEventListener("input", apply);
  input.addEventListener("keydown", function (event) {
    if (event.key === "Escape") {
      input.value = "";
      apply();
    }
  });

  // Revealed here rather than by the markup, which is what makes "no script, no
  // dead widget" true by construction.
  box.hidden = false;
  apply();
})();

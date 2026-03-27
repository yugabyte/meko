(function () {
  document.addEventListener("DOMContentLoaded", function () {
    // Wrap HTML table into div element.
    document.querySelectorAll(".td-content table").forEach(table => {
      // skip if already wrapped
      if (table.parentElement.classList.contains("table-wrapper")) return;

      const wrapper = document.createElement("div");
      wrapper.className = "table-wrapper";

      table.parentNode.insertBefore(wrapper, table);
      wrapper.appendChild(table);
    });
  });
})();

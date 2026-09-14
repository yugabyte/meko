(() => {
  "use strict";

  const state = {
    entries: [],
    currentIndex: -1,
    slice: null,
    follow: true,
    sortKey: "total_ms",
    sortDirection: -1,
    search: "",
    page: 0,
    pageSize: 100,
    loadingId: null,
    inspectionRequest: 0,
    lastInspectionTrigger: null,
  };

  const $ = (id) => document.getElementById(id);
  const elements = {
    error: $("error-banner"),
    liveDot: $("live-dot"),
    connectionState: $("connection-state"),
    connectionTarget: $("connection-target"),
    first: $("first"),
    previous: $("previous"),
    next: $("next"),
    last: $("last"),
    follow: $("follow"),
    windowButton: $("window-button"),
    windowNumber: $("window-number"),
    windowTotal: $("window-total"),
    windowRange: $("window-range"),
    windowMeta: $("window-meta"),
    totalExecHeader: $("total-exec-header"),
    selectRate: $("metric-select-rate"),
    insertRate: $("metric-insert-rate"),
    updateRate: $("metric-update-rate"),
    deleteRate: $("metric-delete-rate"),
    rowRate: $("metric-row-rate"),
    totalRate: $("metric-total-rate"),
    queryRateChart: $("query-rate-chart"),
    operationLatencyChart: $("operation-latency-chart"),
    search: $("search"),
    statementRows: $("statement-rows"),
    rowCount: $("row-count"),
    pagePrevious: $("page-previous"),
    pageNext: $("page-next"),
    pageMeta: $("page-meta"),
    inspectionModal: $("inspection-modal"),
    inspectionCard: document.querySelector(".inspection-card"),
    inspectionClose: $("inspection-close"),
    inspectionContext: $("inspection-context"),
    inspectionQuery: $("inspection-query"),
    inspectionLatency: $("inspection-latency"),
    inspectionLatencyMeta: $("inspection-latency-meta"),
    latencyMax: $("latency-max"),
    latencyP99: $("latency-p99"),
    latencyP90: $("latency-p90"),
    latencyP50: $("latency-p50"),
    latencyAvg: $("latency-avg"),
    latencyMin: $("latency-min"),
    latencyHistogram: $("latency-histogram"),
    latencyAxisMin: $("latency-axis-min"),
    latencyAxisMax: $("latency-axis-max"),
    pgssLifetime: $("pgss-lifetime"),
    pgssStatsSince: $("pgss-stats-since"),
    pgssCalls: $("pgss-calls"),
    pgssMean: $("pgss-mean"),
    pgssMin: $("pgss-min"),
    pgssMax: $("pgss-max"),
    pgssStddev: $("pgss-stddev"),
    inspectionLoading: $("inspection-loading"),
    inspectionError: $("inspection-error"),
    inspectionResults: $("inspection-results"),
    inspectionParameterNote: $("inspection-parameter-note"),
    inspectionPlanMeta: $("inspection-plan-meta"),
    inspectionDdlMeta: $("inspection-ddl-meta"),
    inspectionStatisticsMeta: $("inspection-statistics-meta"),
    inspectionPlan: $("inspection-plan"),
    inspectionDdl: $("inspection-ddl"),
    inspectionStatistics: $("inspection-statistics"),
  };

  const number = new Intl.NumberFormat(undefined, { maximumFractionDigits: 0 });
  const decimal = new Intl.NumberFormat(undefined, { maximumFractionDigits: 2 });
  const statistic = new Intl.NumberFormat(undefined, { maximumFractionDigits: 6 });
  const operationSeries = [
    { key: "selects", label: "selects", rateMetric: "selects_per_sec", latencyMetric: "select_mean_exec_ms" },
    { key: "inserts", label: "inserts", rateMetric: "inserts_per_sec", latencyMetric: "insert_mean_exec_ms" },
    { key: "updates", label: "updates", rateMetric: "updates_per_sec", latencyMetric: "update_mean_exec_ms" },
    { key: "deletes", label: "deletes", rateMetric: "deletes_per_sec", latencyMetric: "delete_mean_exec_ms" },
  ];

  function formatNumber(value) {
    return number.format(Number(value || 0));
  }

  function formatDecimal(value) {
    return decimal.format(Number(value || 0));
  }

  function formatStatistic(value) {
    if (value == null) return "null";
    return statistic.format(Number(value));
  }

  function formatBytes(value) {
    let bytes = Number(value || 0);
    const units = ["B", "KiB", "MiB", "GiB", "TiB"];
    let index = 0;
    while (Math.abs(bytes) >= 1024 && index < units.length - 1) {
      bytes /= 1024;
      index += 1;
    }
    return `${formatDecimal(bytes)} ${units[index]}`;
  }

  function formatDurationMs(value) {
    const ms = Number(value || 0);
    if (ms >= 1000) return `${formatDecimal(ms / 1000)} s`;
    if (ms >= 1) return `${formatDecimal(ms)} ms`;
    return `${formatDecimal(ms * 1000)} µs`;
  }

  function formatLatency(value) {
    return value == null ? "—" : formatDurationMs(value);
  }

  function formatLatencyAxis(value, maximum) {
    const ms = Number(value || 0);
    if (maximum >= 1000) return `${formatDecimal(ms / 1000)} s`;
    if (maximum < 1) return `${formatDecimal(ms * 1000)} µs`;
    return `${formatDecimal(ms)} ms`;
  }

  function formatUtc(value, withDate = false) {
    if (!value) return "—";
    const date = new Date(value);
    const options = {
      hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false,
      timeZone: "UTC", timeZoneName: "short",
      ...(withDate ? { year: "numeric", month: "short", day: "2-digit" } : {}),
    };
    return new Intl.DateTimeFormat(undefined, options).format(date);
  }

  async function fetchJson(url, options = {}) {
    const response = await fetch(url, { cache: "no-store", ...options });
    let payload = null;
    try { payload = await response.json(); } catch (_) { /* handled below */ }
    if (!response.ok) {
      let message = `${response.status} ${response.statusText}`;
      if (payload) message = payload.detail || payload.error || message;
      throw new Error(message);
    }
    return payload;
  }

  function updateServiceStatus(status) {
    const source = status.source || {};
    elements.connectionTarget.textContent = `${source.user || "?"}@${source.host || "?"}:${source.port || "5432"}/${source.dbname || "?"}`;
    elements.liveDot.className = "live-dot";
    if (status.state === "live") {
      elements.liveDot.classList.add("live");
      elements.connectionState.textContent = `LIVE · ${formatDecimal(status.interval_seconds)} s`;
    } else if (status.state === "error") {
      elements.liveDot.classList.add("error");
      elements.connectionState.textContent = "ERROR";
    } else {
      elements.connectionState.textContent = "BASELINE";
    }
    if (status.last_error) {
      elements.error.textContent = status.last_error;
      elements.error.classList.remove("hidden");
    } else {
      elements.error.classList.add("hidden");
    }
    if (!state.entries.length && status.next_sample_at) {
      elements.windowMeta.textContent = `${formatUtc(status.next_sample_at)} · ${formatDecimal(status.interval_seconds)} s`;
    }
  }

  function sameEntryList(nextEntries) {
    if (nextEntries.length !== state.entries.length) return false;
    return nextEntries.every((entry, index) => state.entries[index] && state.entries[index].id === entry.id);
  }

  async function refreshManifest() {
    try {
      const currentId = state.entries[state.currentIndex] && state.entries[state.currentIndex].id;
      const lastId = state.entries.length ? state.entries[state.entries.length - 1].id : "";
      let manifest = await fetchJson(lastId ? `/api/manifest?after=${encodeURIComponent(lastId)}` : "/api/manifest");
      updateServiceStatus(manifest.status || {});
      const incoming = Array.isArray(manifest.entries) ? manifest.entries : [];
      const byId = new Map((lastId ? state.entries : []).map((entry) => [entry.id, entry]));
      incoming.forEach((entry) => byId.set(entry.id, entry));
      let nextEntries = Array.from(byId.values()).sort((left, right) => left.id.localeCompare(right.id));
      const entryState = manifest.entry_state || {};
      if (entryState.oldest_id) {
        nextEntries = nextEntries.filter((entry) => entry.id >= entryState.oldest_id);
      }
      if (Number.isFinite(Number(entryState.total)) && nextEntries.length !== Number(entryState.total)) {
        manifest = await fetchJson("/api/manifest");
        updateServiceStatus(manifest.status || {});
        nextEntries = Array.isArray(manifest.entries) ? manifest.entries : [];
      }
      const changed = !sameEntryList(nextEntries);
      state.entries = nextEntries;

      if (!nextEntries.length) {
        renderEmptyState();
        return;
      }
      if (state.follow || state.currentIndex < 0) {
        await loadSlice(nextEntries.length - 1);
      } else if (changed) {
        const retainedIndex = nextEntries.findIndex((entry) => entry.id === currentId);
        if (retainedIndex >= 0) {
          state.currentIndex = retainedIndex;
          renderNavigation();
          renderCharts();
        } else {
          await loadSlice(0);
        }
      } else {
        renderNavigation();
      }
    } catch (error) {
      elements.liveDot.className = "live-dot error";
      elements.connectionState.textContent = "DISCONNECTED";
      elements.error.textContent = `Could not refresh collector: ${error.message}`;
      elements.error.classList.remove("hidden");
    }
  }

  async function loadSlice(index) {
    if (index < 0 || index >= state.entries.length) return;
    const entry = state.entries[index];
    if (state.loadingId === entry.id) return;
    if (state.slice && state.slice.slice_id === entry.id) {
      state.currentIndex = index;
      renderAll();
      return;
    }
    state.loadingId = entry.id;
    try {
      const slice = await fetchJson(`/api/slices/${encodeURIComponent(entry.id)}`);
      if (state.loadingId !== entry.id) return;
      state.slice = slice;
      state.currentIndex = index;
      state.page = 0;
      renderAll();
    } catch (error) {
      elements.error.textContent = `Could not load timeslice: ${error.message}`;
      elements.error.classList.remove("hidden");
    } finally {
      if (state.loadingId === entry.id) state.loadingId = null;
    }
  }

  function renderEmptyState() {
    state.currentIndex = -1;
    state.slice = null;
    elements.windowNumber.textContent = "—";
    elements.windowTotal.textContent = "of —";
    elements.windowRange.textContent = "BASELINE";
    [elements.selectRate, elements.insertRate, elements.updateRate, elements.deleteRate, elements.rowRate, elements.totalRate]
      .forEach((element) => { element.textContent = "—"; });
    elements.statementRows.replaceChildren(emptyRow("—"));
    elements.queryRateChart.innerHTML = '<div class="chart-empty">—</div>';
    renderNavigation();
  }

  function renderAll() {
    renderNavigation();
    renderSummary();
    renderCharts();
    renderTable();
  }

  function renderNavigation() {
    const total = state.entries.length;
    const valid = state.currentIndex >= 0 && state.currentIndex < total;
    elements.windowNumber.textContent = valid ? String(state.currentIndex + 1) : "—";
    elements.windowTotal.textContent = `of ${total || "—"}`;
    elements.first.disabled = !valid || state.currentIndex === 0;
    elements.previous.disabled = !valid || state.currentIndex === 0;
    elements.next.disabled = !valid || state.currentIndex >= total - 1;
    elements.last.disabled = !valid || (state.currentIndex >= total - 1 && state.follow);
    elements.follow.classList.toggle("active", state.follow);
    elements.follow.innerHTML = "<span></span>LIVE";
    if (!state.slice) return;
    elements.windowRange.textContent = `${formatUtc(state.slice.start_utc, true)} → ${formatUtc(state.slice.end_utc)}`;
    const resetNote = state.slice.totals.counter_resets
      ? ` · resets=${formatNumber(state.slice.totals.counter_resets)}`
      : "";
    elements.windowMeta.textContent = `Δ pg_stat_statements · ${formatDecimal(state.slice.duration_seconds)} s${resetNote}`;
    // total_exec_time is a per-slice total while the two columns beside it are
    // per-second and per-call, so the window goes in the header: without it the
    // three read as one unit and total != queries/s * mean_exec_time.
    if (elements.totalExecHeader) {
      elements.totalExecHeader.textContent =
        `total_exec_time / ${formatDecimal(state.slice.duration_seconds)}s`;
    }
  }

  function renderSummary() {
    if (!state.slice) return;
    const totals = state.slice.totals || {};
    elements.selectRate.textContent = formatDecimal(totals.selects_per_sec);
    elements.insertRate.textContent = formatDecimal(totals.inserts_per_sec);
    elements.updateRate.textContent = formatDecimal(totals.updates_per_sec);
    elements.deleteRate.textContent = formatDecimal(totals.deletes_per_sec);
    elements.rowRate.textContent = formatDecimal(totals.rows_per_sec);
    elements.totalRate.textContent = formatDecimal(totals.calls_per_sec);
  }

  function svgElement(name, attributes = {}) {
    const element = document.createElementNS("http://www.w3.org/2000/svg", name);
    for (const [key, value] of Object.entries(attributes)) element.setAttribute(key, String(value));
    return element;
  }

  function niceAxis(values) {
    const observedMaximum = Math.max(0, ...values);
    if (observedMaximum === 0) return { maximum: 1, ticks: [0, 0.25, 0.5, 0.75, 1] };
    const roughStep = observedMaximum / 4;
    const magnitude = 10 ** Math.floor(Math.log10(roughStep));
    const normalized = roughStep / magnitude;
    const multiplier = normalized <= 1 ? 1 : normalized <= 2 ? 2 : normalized <= 5 ? 5 : 10;
    const step = multiplier * magnitude;
    const maximum = Math.max(step, Math.ceil(observedMaximum / step) * step);
    const ticks = [];
    for (let value = 0; value <= maximum + step / 2; value += step) ticks.push(value);
    return { maximum, ticks };
  }

  function visibleEntries() {
    const visible = 80;
    const selected = Math.max(0, state.currentIndex);
    const start = Math.max(0, Math.min(selected - Math.floor(visible / 2), state.entries.length - visible));
    const end = Math.min(state.entries.length, start + visible);
    return { start, end, entries: state.entries.slice(start, end) };
  }

  function renderOperationChart({
    container,
    metric,
    ariaLabel,
    markerLabel,
    axisFormatter,
    valueFormatter,
    valueSuffix,
  }) {
    container.replaceChildren();
    if (!state.entries.length) {
      container.innerHTML = '<div class="chart-empty">—</div>';
      return;
    }
    const { start, end, entries } = visibleEntries();
    const series = operationSeries.map((item) => ({
      ...item,
      values: entries.map((entry) => Number(entry.totals && entry.totals[item[metric]] || 0)),
    }));
    const { maximum, ticks } = niceAxis(series.flatMap((item) => item.values));
    const width = 700;
    const height = 160;
    const left = 59;
    const right = 9;
    const top = 8;
    const plotBottom = 118;
    const markerTop = 126;
    const markerHeight = 10;
    const plotWidth = width - left - right;
    const plotHeight = plotBottom - top;
    const x = (index) => entries.length === 1 ? left + plotWidth / 2 : left + (index / (entries.length - 1)) * plotWidth;
    const y = (value) => plotBottom - (Number(value || 0) / maximum) * plotHeight;
    const svg = svgElement("svg", {
      viewBox: `0 0 ${width} ${height}`,
      preserveAspectRatio: "none",
      role: "img",
      "aria-label": ariaLabel,
    });
    ticks.forEach((tick) => {
      const tickY = y(tick);
      svg.appendChild(svgElement("line", { x1: left, x2: width - right, y1: tickY, y2: tickY, class: "grid" }));
      const label = svgElement("text", { x: left - 8, y: tickY + 3, "text-anchor": "end", class: "axis-label y-axis-label" });
      label.textContent = axisFormatter(tick, maximum);
      svg.appendChild(label);
    });
    series.forEach((item) => {
      const points = item.values.map((value, index) => `${x(index)},${y(value)}`).join(" ");
      svg.appendChild(svgElement("polyline", { points, class: `rate-line ${item.key}` }));
      const selectedLocalIndex = state.currentIndex - start;
      if (selectedLocalIndex >= 0 && selectedLocalIndex < entries.length) {
        svg.appendChild(svgElement("circle", {
          cx: x(selectedLocalIndex), cy: y(item.values[selectedLocalIndex]), r: 2.75,
          class: `rate-point selected ${item.key}`,
        }));
      }
    });
    entries.forEach((entry, localIndex) => {
      const globalIndex = start + localIndex;
      const values = Object.fromEntries(series.map((item) => [item.key, item.values[localIndex]]));
      const slotWidth = plotWidth / Math.max(1, entries.length);
      const markerWidth = Math.max(4, Math.min(9, slotWidth * 0.62));
      const marker = svgElement("rect", {
        x: x(localIndex) - markerWidth / 2, y: markerTop,
        width: markerWidth, height: markerHeight, rx: 1,
        class: `slice-marker${globalIndex === state.currentIndex ? " selected" : ""}`,
        tabindex: 0,
        role: "button",
        "aria-label": `${markerLabel} timeslice ${globalIndex + 1}, ${series.map((item) => `${item.label} ${valueFormatter(values[item.key])}`).join(", ")}, ${valueSuffix}, ${formatUtc(entry.end_utc)}`,
      });
      const title = svgElement("title");
      title.textContent = `timeslice=${globalIndex + 1} · ${series.map((item) => `${item.label}=${valueFormatter(values[item.key])}`).join(" · ")} · ${valueSuffix} · ${formatUtc(entry.end_utc)}`;
      marker.appendChild(title);
      marker.addEventListener("click", () => {
        state.follow = globalIndex === state.entries.length - 1;
        loadSlice(globalIndex);
      });
      marker.addEventListener("keydown", (event) => {
        if (event.key === "Enter" || event.key === " ") {
          event.preventDefault();
          state.follow = globalIndex === state.entries.length - 1;
          loadSlice(globalIndex);
        }
      });
      svg.appendChild(marker);
    });
    const timeLabel = svgElement("text", { x: width - 4, y: height - 3, "text-anchor": "end", class: "axis-label" });
    timeLabel.textContent = end === state.entries.length ? "latest · UTC" : `${start + 1}–${end} · UTC`;
    svg.appendChild(timeLabel);
    container.appendChild(svg);
  }

  function renderCharts() {
    renderOperationChart({
      container: elements.queryRateChart,
      metric: "rateMetric",
      ariaLabel: "SELECT, INSERT, UPDATE, and DELETE queries per second",
      markerLabel: "Query rate",
      axisFormatter: (value) => formatDecimal(value),
      valueFormatter: (value) => formatDecimal(value),
      valueSuffix: "queries/s",
    });
    renderOperationChart({
      container: elements.operationLatencyChart,
      metric: "latencyMetric",
      ariaLabel: "Mean execution time for SELECT, INSERT, UPDATE, and DELETE",
      markerLabel: "Mean execution time",
      axisFormatter: formatLatencyAxis,
      valueFormatter: formatDurationMs,
      valueSuffix: "per execution",
    });
  }

  function emptyRow(message) {
    const row = document.createElement("tr");
    const cell = document.createElement("td");
    cell.colSpan = 9;
    cell.className = "empty";
    cell.textContent = message;
    row.appendChild(cell);
    return row;
  }

  function sortedFilteredRows() {
    if (!state.slice || !Array.isArray(state.slice.rows)) return [];
    const term = state.search.trim().toLowerCase();
    const rows = term
      ? state.slice.rows.filter((row) => [row.query, row.dbname, row.username, row.queryid].some((value) => String(value || "").toLowerCase().includes(term)))
      : [...state.slice.rows];
    const key = state.sortKey;
    rows.sort((left, right) => {
      const a = key === "database_role" ? `${left.dbname || ""}:${left.username || ""}` : left[key];
      const b = key === "database_role" ? `${right.dbname || ""}:${right.username || ""}` : right[key];
      if (typeof a === "number" || typeof b === "number") return (Number(a || 0) - Number(b || 0)) * state.sortDirection;
      return String(a || "").localeCompare(String(b || "")) * state.sortDirection;
    });
    return rows;
  }

  function numericCell(value, className = "") {
    const cell = document.createElement("td");
    cell.className = `numeric ${className}`.trim();
    cell.textContent = value;
    return cell;
  }

  function identityCell(database, role) {
    const cell = document.createElement("td");
    cell.className = "identity";
    cell.title = `${database || ""}:${role || ""}`;
    const databaseLine = document.createElement("span");
    databaseLine.className = "identity-database";
    databaseLine.textContent = database || "—";
    const roleLine = document.createElement("span");
    roleLine.className = "identity-role";
    roleLine.textContent = role || "—";
    cell.append(databaseLine, roleLine);
    return cell;
  }

  function closeInspection() {
    if (elements.inspectionModal.classList.contains("hidden")) return;
    state.inspectionRequest += 1;
    elements.inspectionModal.classList.add("hidden");
    document.body.classList.remove("modal-open");
    if (state.lastInspectionTrigger && document.contains(state.lastInspectionTrigger)) {
      state.lastInspectionTrigger.focus();
    }
  }

  function renderStatistics(result) {
    const relations = Array.isArray(result.statistics) ? result.statistics : [];
    elements.inspectionStatistics.replaceChildren();
    elements.inspectionStatisticsMeta.textContent = `${relations.length} relation${relations.length === 1 ? "" : "s"}`;
    if (!relations.length) {
      const empty = document.createElement("div");
      empty.className = "statistics-empty";
      empty.textContent = "—";
      elements.inspectionStatistics.appendChild(empty);
      return;
    }

    const fragment = document.createDocumentFragment();
    relations.forEach((relation) => {
      const section = document.createElement("section");
      section.className = "statistics-relation";

      const heading = document.createElement("div");
      heading.className = "statistics-relation-heading";
      const relationName = document.createElement("strong");
      relationName.textContent = relation.qualified_name || `${relation.schema}.${relation.name}`;
      const reltuples = document.createElement("span");
      reltuples.textContent = `reltuples_estimate=${formatStatistic(relation.reltuples)}`;
      heading.append(relationName, reltuples);

      const relationMeta = document.createElement("div");
      relationMeta.className = "statistics-relation-meta";
      [
        ["n_live_tup", relation.n_live_tup == null ? "—" : formatNumber(relation.n_live_tup)],
        ["n_dead_tup", relation.n_dead_tup == null ? "—" : formatNumber(relation.n_dead_tup)],
        ["n_mod_since_analyze", relation.n_mod_since_analyze == null ? "—" : formatNumber(relation.n_mod_since_analyze)],
        ["last_analyze", relation.last_analyze ? formatUtc(relation.last_analyze, true) : "—"],
        ["last_autoanalyze", relation.last_autoanalyze ? formatUtc(relation.last_autoanalyze, true) : "—"],
      ].forEach(([labelText, valueText]) => {
        const item = document.createElement("div");
        const label = document.createElement("label");
        const value = document.createElement("strong");
        label.textContent = labelText;
        value.textContent = valueText;
        item.append(label, value);
        relationMeta.appendChild(item);
      });

      const table = document.createElement("table");
      table.className = "statistics-table";
      const tableHead = document.createElement("thead");
      const headerRow = document.createElement("tr");
      ["attname", "null_frac", "avg_width", "n_distinct", "estimated_distinct"].forEach((name) => {
        const cell = document.createElement("th");
        cell.textContent = name;
        headerRow.appendChild(cell);
      });
      tableHead.appendChild(headerRow);
      const tableBody = document.createElement("tbody");
      const columns = Array.isArray(relation.columns) ? relation.columns : [];
      if (!columns.length) {
        const row = document.createElement("tr");
        const cell = document.createElement("td");
        cell.colSpan = 5;
        cell.className = "statistics-empty";
        cell.textContent = "—";
        row.appendChild(cell);
        tableBody.appendChild(row);
      } else {
        columns.forEach((column) => {
          const row = document.createElement("tr");
          const name = document.createElement("td");
          name.textContent = column.attname || "";
          row.appendChild(name);
          ["null_frac", "avg_width", "n_distinct"].forEach((key) => {
            const cell = document.createElement("td");
            cell.className = "numeric";
            cell.textContent = formatStatistic(column[key]);
            row.appendChild(cell);
          });
          const estimated = document.createElement("td");
          estimated.className = "numeric";
          const distinct = Number(column.n_distinct);
          estimated.textContent = Number.isFinite(distinct) && distinct < 0 && Number(relation.reltuples) >= 0
            ? formatNumber(Math.abs(distinct) * Number(relation.reltuples))
            : distinct >= 0 ? formatNumber(distinct) : "—";
          row.appendChild(estimated);
          tableBody.appendChild(row);
        });
      }
      table.append(tableHead, tableBody);
      section.append(heading, relationMeta, table);
      fragment.appendChild(section);
    });
    elements.inspectionStatistics.appendChild(fragment);
  }

  function renderLatency(result) {
    const latency = result.latency || {};
    const bins = Array.isArray(latency.bins) ? latency.bins : [];
    elements.inspectionLatency.classList.remove("hidden");
    elements.inspectionLatencyMeta.textContent = `${formatNumber(latency.samples)} timeslices · ${formatNumber(latency.calls)} calls`;
    elements.latencyMax.textContent = formatLatency(latency.max_ms);
    elements.latencyP99.textContent = formatLatency(latency.p99_ms);
    elements.latencyP90.textContent = formatLatency(latency.p90_ms);
    elements.latencyP50.textContent = formatLatency(latency.p50_ms);
    elements.latencyAvg.textContent = formatLatency(latency.avg_ms);
    elements.latencyMin.textContent = formatLatency(latency.min_ms);
    elements.latencyAxisMin.textContent = formatLatency(latency.min_ms);
    elements.latencyAxisMax.textContent = formatLatency(latency.max_ms);
    elements.latencyHistogram.replaceChildren();
    const maximum = Math.max(1, ...bins.map((bin) => Number(bin.calls || 0)));
    bins.forEach((bin) => {
      const column = document.createElement("div");
      column.className = "latency-bin";
      const timesliceLabel = Number(bin.timeslices) === 1 ? "timeslice" : "timeslices";
      column.title = `${formatLatency(bin.lower_ms)}–${formatLatency(bin.upper_ms)} · ${formatNumber(bin.timeslices)} ${timesliceLabel} · ${formatNumber(bin.calls)} calls`;
      column.tabIndex = 0;
      column.setAttribute("role", "img");
      column.setAttribute("aria-label", column.title);
      const bar = document.createElement("div");
      bar.className = "latency-bin-bar";
      bar.style.height = `${Math.max(2, Number(bin.calls || 0) / maximum * 100)}%`;
      column.appendChild(bar);
      elements.latencyHistogram.appendChild(column);
    });
    if (!bins.length) {
      const empty = document.createElement("div");
      empty.className = "chart-empty";
      empty.textContent = "—";
      elements.latencyHistogram.appendChild(empty);
    }
  }

  function renderPgssLifetime(result) {
    const lifetime = result.pgss_lifetime || {};
    elements.pgssLifetime.classList.remove("hidden");
    elements.pgssStatsSince.textContent = lifetime.stats_since
      ? `stats_since ${formatUtc(lifetime.stats_since, true)} · minmax_stats_since ${formatUtc(lifetime.minmax_stats_since || lifetime.stats_since, true)}`
      : "stats_since unavailable";
    elements.pgssCalls.textContent = lifetime.calls == null ? "—" : formatNumber(lifetime.calls);
    elements.pgssMean.textContent = formatLatency(lifetime.mean_ms);
    elements.pgssMin.textContent = formatLatency(lifetime.min_ms);
    elements.pgssMax.textContent = formatLatency(lifetime.max_ms);
    elements.pgssStddev.textContent = formatLatency(lifetime.stddev_ms);
  }

  function renderInspection(result) {
    const relationCount = Array.isArray(result.relations) ? result.relations.length : 0;
    elements.inspectionContext.textContent = `datname=${result.database} · queryid=${result.queryid} · recorded_role=${result.recorded_user || ""} · planning_role=${result.planned_as || ""} · ${formatDecimal(result.elapsed_ms)} ms`;
    elements.inspectionParameterNote.textContent = ` · generic plan as ${result.planned_as || "configured role"} · default_transaction_read_only=on · ${result.parameter_strategy} · search_path=${result.search_path || "default"}`;
    elements.inspectionPlanMeta.textContent = `${formatUtc(result.captured_at_utc, true)} · ${result.database}`;
    elements.inspectionDdlMeta.textContent = `${relationCount} relation${relationCount === 1 ? "" : "s"}`;
    elements.inspectionPlan.textContent = result.plan_text || "—";
    elements.inspectionDdl.textContent = result.ddl_text || "-- no user relations";
    renderLatency(result);
    renderPgssLifetime(result);
    renderStatistics(result);
    elements.inspectionLoading.classList.add("hidden");
    elements.inspectionResults.classList.remove("hidden");
  }

  async function openInspection(row, trigger) {
    if (!state.slice) return;
    const requestId = ++state.inspectionRequest;
    state.lastInspectionTrigger = trigger;
    elements.inspectionModal.classList.remove("hidden");
    document.body.classList.add("modal-open");
    elements.inspectionQuery.textContent = row.query || "—";
    elements.inspectionContext.textContent = `datname=${row.dbname} · queryid=${row.queryid} · rolname=${row.username || ""}`;
    elements.inspectionError.classList.add("hidden");
    elements.inspectionError.textContent = "";
    elements.inspectionResults.classList.add("hidden");
    elements.inspectionLatency.classList.add("hidden");
    elements.pgssLifetime.classList.add("hidden");
    elements.inspectionLoading.classList.remove("hidden");
    elements.inspectionCard.focus();
    try {
      const result = await fetchJson("/api/inspect", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          slice_id: state.slice.slice_id,
          dbid: row.dbid,
          userid: row.userid,
          queryid: row.queryid,
          toplevel: Boolean(row.toplevel),
          dbname: row.dbname,
          username: row.username,
        }),
      });
      if (requestId !== state.inspectionRequest) return;
      renderInspection(result);
    } catch (error) {
      if (requestId !== state.inspectionRequest) return;
      elements.inspectionLoading.classList.add("hidden");
      elements.inspectionError.textContent = `EXPLAIN: ${error.message}`;
      elements.inspectionError.classList.remove("hidden");
    }
  }

  function renderTable() {
    const filteredRows = sortedFilteredRows();
    const pageCount = Math.max(1, Math.ceil(filteredRows.length / state.pageSize));
    state.page = Math.max(0, Math.min(state.page, pageCount - 1));
    const start = state.page * state.pageSize;
    const rows = filteredRows.slice(start, start + state.pageSize);
    elements.statementRows.replaceChildren();
    if (!rows.length) {
      elements.statementRows.appendChild(emptyRow("0 rows"));
    } else {
      const fragment = document.createDocumentFragment();
      rows.forEach((row) => {
        const tr = document.createElement("tr");
        tr.appendChild(numericCell(`${formatDecimal(row.time_pct)}%`, "time-share"));
        tr.appendChild(numericCell(formatDurationMs(row.total_ms)));
        tr.appendChild(numericCell(formatDecimal(row.calls_per_sec)));
        tr.appendChild(numericCell(formatDurationMs(row.mean_ms)));
        tr.appendChild(numericCell(formatDecimal(row.rows_per_call)));
        tr.appendChild(numericCell(formatNumber(row.shared_blks_hit)));
        tr.appendChild(numericCell(formatBytes(row.wal_bytes)));
        tr.appendChild(identityCell(row.dbname, row.username));
        const queryCell = document.createElement("td");
        queryCell.className = "query";
        const queryButton = document.createElement("button");
        queryButton.type = "button";
        queryButton.className = "query-open";
        queryButton.title = "GENERIC LIVE PLAN · PLAN-REFERENCED DDL · STATISTICS";
        queryButton.setAttribute("aria-label", `Inspect query ${row.queryid}`);
        const queryText = document.createElement("div");
        queryText.className = "query-text";
        queryText.textContent = row.query || "—";
        const queryMeta = document.createElement("div");
        queryMeta.className = "query-meta";
        queryMeta.textContent = `queryid=${row.queryid} · toplevel=${Boolean(row.toplevel)}`;
        if (row.counter_reset) {
          const reset = document.createElement("span");
          reset.className = "reset-badge";
          reset.textContent = "reset";
          queryMeta.appendChild(reset);
        }
        queryButton.append(queryText, queryMeta);
        queryButton.addEventListener("click", () => openInspection(row, queryButton));
        queryCell.appendChild(queryButton);
        tr.appendChild(queryCell);
        fragment.appendChild(tr);
      });
      elements.statementRows.appendChild(fragment);
    }
    const total = state.slice && state.slice.rows ? state.slice.rows.length : 0;
    const end = Math.min(start + rows.length, filteredRows.length);
    elements.rowCount.textContent = filteredRows.length
      ? `${formatNumber(start + 1)}–${formatNumber(end)} of ${formatNumber(filteredRows.length)} · ${formatNumber(total)} total`
      : `0 of ${formatNumber(total)} statements`;
    elements.pageMeta.textContent = `${state.page + 1} / ${pageCount}`;
    elements.pagePrevious.disabled = state.page === 0;
    elements.pageNext.disabled = state.page >= pageCount - 1;
  }

  function updateSortIndicators() {
    document.querySelectorAll("th[data-key]").forEach((heading) => {
      const active = heading.dataset.key === state.sortKey;
      heading.classList.toggle("sorted", active);
      heading.setAttribute(
        "aria-sort",
        active ? (state.sortDirection < 0 ? "descending" : "ascending") : "none",
      );
    });
  }

  function activateSort(heading) {
    const key = heading.dataset.key;
    if (state.sortKey === key) state.sortDirection *= -1;
    else {
      state.sortKey = key;
      state.sortDirection = ["database_role", "query"].includes(key) ? 1 : -1;
    }
    state.page = 0;
    updateSortIndicators();
    renderTable();
  }

  async function copyFrom(button) {
    const source = $(button.dataset.copy);
    if (!source) return;
    try {
      await navigator.clipboard.writeText(source.textContent || "");
      button.textContent = "COPIED";
      button.classList.add("copied");
      window.setTimeout(() => {
        button.textContent = "COPY";
        button.classList.remove("copied");
      }, 1200);
    } catch (_) {
      button.textContent = "FAILED";
    }
  }

  function navigate(index, follow = false) {
    state.follow = follow;
    loadSlice(Math.max(0, Math.min(state.entries.length - 1, index)));
  }

  elements.first.addEventListener("click", () => navigate(0));
  elements.previous.addEventListener("click", () => navigate(state.currentIndex - 1));
  elements.next.addEventListener("click", () => navigate(state.currentIndex + 1, state.currentIndex + 1 === state.entries.length - 1));
  elements.last.addEventListener("click", () => navigate(state.entries.length - 1, true));
  elements.follow.addEventListener("click", () => navigate(state.entries.length - 1, true));
  elements.windowButton.addEventListener("click", () => {
    if (!state.entries.length) return;
    const requested = window.prompt(`timeslice 1–${state.entries.length}`, String(state.currentIndex + 1));
    if (requested == null) return;
    const index = Number.parseInt(requested, 10) - 1;
    if (Number.isInteger(index) && index >= 0 && index < state.entries.length) navigate(index, index === state.entries.length - 1);
  });
  elements.search.addEventListener("input", (event) => {
    state.search = event.target.value;
    state.page = 0;
    renderTable();
  });

  elements.pagePrevious.addEventListener("click", () => {
    state.page = Math.max(0, state.page - 1);
    renderTable();
  });
  elements.pageNext.addEventListener("click", () => {
    state.page += 1;
    renderTable();
  });

  document.querySelectorAll("th[data-key]").forEach((heading) => {
    heading.addEventListener("click", () => activateSort(heading));
    heading.addEventListener("keydown", (event) => {
      if (event.key === "Enter" || event.key === " ") {
        event.preventDefault();
        activateSort(heading);
      }
    });
  });

  document.querySelectorAll("[data-copy]").forEach((button) => {
    button.addEventListener("click", () => copyFrom(button));
  });

  elements.inspectionClose.addEventListener("click", closeInspection);
  elements.inspectionModal.addEventListener("click", (event) => {
    if (event.target === elements.inspectionModal) closeInspection();
  });

  document.addEventListener("keydown", (event) => {
    if (!elements.inspectionModal.classList.contains("hidden")) {
      if (event.key === "Escape") closeInspection();
      if (event.key === "Tab") {
        const focusable = Array.from(elements.inspectionCard.querySelectorAll("button, [tabindex]:not([tabindex='-1'])"))
          .filter((element) => !element.disabled && element.offsetParent !== null);
        if (focusable.length) {
          const first = focusable[0];
          const last = focusable[focusable.length - 1];
          if (event.shiftKey && document.activeElement === first) {
            event.preventDefault();
            last.focus();
          } else if (!event.shiftKey && document.activeElement === last) {
            event.preventDefault();
            first.focus();
          }
        }
      }
      return;
    }
    if (["INPUT", "TEXTAREA"].includes(document.activeElement.tagName)) return;
    if (event.key === "ArrowLeft" && state.currentIndex > 0) navigate(state.currentIndex - 1);
    if (event.key === "ArrowRight" && state.currentIndex < state.entries.length - 1) navigate(state.currentIndex + 1, state.currentIndex + 1 === state.entries.length - 1);
    if (event.key === "Home") navigate(0);
    if (event.key === "End") navigate(state.entries.length - 1, true);
    if (event.key === "/") { event.preventDefault(); elements.search.focus(); }
  });

  renderEmptyState();
  refreshManifest();
  window.setInterval(refreshManifest, 2000);
})();

(function () {
  "use strict";

  const csrfMeta = document.querySelector('meta[name="soma-csrf"]');
  const CSRF_TOKEN = csrfMeta ? csrfMeta.getAttribute("content") || "" : "";

  const params = new URLSearchParams(window.location.search);
  const initialTheme = params.get("theme") || localStorage.getItem("soma_theme") || "dark";
  document.documentElement.setAttribute("data-theme", initialTheme === "light" ? "light" : "dark");

  const ui = {
    view: ["tasks", "approvals", "memory"].includes(params.get("view")) ? params.get("view") : "tasks",
    selectedRunId: params.get("run") || "",
    selectedBranchByRun: new Map(),
    showDetails: params.get("details") === "1",
    expandedTools: new Set(),
    pageCache: new Map(),
    replayStatusByBranch: new Map(),
    memoryFilter: "",
    state: null,
  };

  function el(tag, attrs, ...children) {
    const node = document.createElement(tag);
    if (attrs) {
      for (const [k, v] of Object.entries(attrs)) {
        if (v === undefined || v === null || v === false) continue;
        if (k === "className") {
          node.className = v;
        } else if (k.startsWith("on") && typeof v === "function") {
          node.addEventListener(k.slice(2).toLowerCase(), v);
        } else {
          node.setAttribute(k, String(v));
        }
      }
    }
    for (const child of children) {
      if (child === null || child === undefined || child === false) continue;
      if (typeof child === "string" || typeof child === "number") {
        node.appendChild(document.createTextNode(String(child)));
      } else {
        node.appendChild(child);
      }
    }
    return node;
  }

  let toastTimer = null;
  function showToast(msg) {
    const t = document.getElementById("toast");
    t.textContent = msg;
    t.classList.remove("hidden");
    if (toastTimer) clearTimeout(toastTimer);
    toastTimer = setTimeout(() => t.classList.add("hidden"), 3200);
  }

  async function apiGet(url) {
    const resp = await fetch(url, { headers: { "Accept": "application/json" } });
    const data = await resp.json().catch(() => ({}));
    if (!resp.ok) {
      throw new Error(data.error || ("HTTP " + resp.status));
    }
    return data;
  }

  async function apiPost(url, bodyObj) {
    const resp = await fetch(url, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        "X-Soma-CSRF": CSRF_TOKEN,
      },
      body: JSON.stringify(bodyObj || {}),
    });
    const data = await resp.json().catch(() => ({}));
    if (!resp.ok) {
      throw new Error(data.error || ("HTTP " + resp.status));
    }
    return data;
  }

  function statusInfo(rawStatus) {
    const s = String(rawStatus || "").toLowerCase();
    switch (s) {
      case "completed":
        return { label: "Done", cls: "ok" };
      case "running":
      case "verifying":
        return { label: "Running", cls: "info" };
      case "suspended_approval":
      case "suspended_reconcile":
        return { label: "Needs Approval", cls: "warn" };
      case "suspended_quota":
      case "suspended_io":
        return { label: "Paused", cls: "warn" };
      case "failed":
      case "stalled":
      case "abandoned":
        return { label: "Failed", cls: "err" };
      case "queued":
        return { label: "Queued", cls: "muted" };
      default:
        return { label: s || "Unknown", cls: "muted" };
    }
  }

  function relativeTime(unixSec) {
    if (!unixSec) return "";
    const diff = Math.max(0, Math.floor(Date.now() / 1000) - unixSec);
    if (diff < 60) return "now";
    if (diff < 3600) return Math.floor(diff / 60) + "m";
    if (diff < 86400) return Math.floor(diff / 3600) + "h";
    return Math.floor(diff / 86400) + "d";
  }

  function activeRun() {
    const runs = (ui.state && ui.state.runs) || [];
    if (runs.length === 0) return null;
    if (ui.selectedRunId) {
      const found = runs.find((r) => r.id === ui.selectedRunId);
      if (found) return found;
    }
    ui.selectedRunId = runs[0].id;
    return runs[0];
  }

  function activeBranch(run) {
    if (!run || !run.branches || run.branches.length === 0) return null;
    const savedId = ui.selectedBranchByRun.get(run.id);
    if (savedId) {
      const found = run.branches.find((b) => b.id === savedId);
      if (found) return found;
    }
    return run.branches[0];
  }

  function switchView(name) {
    ui.view = name;
    document.querySelectorAll(".nav-item").forEach((btn) => {
      btn.classList.toggle("active", btn.dataset.view === name);
    });
    document.getElementById("viewTasks").classList.toggle("hidden", name !== "tasks");
    document.getElementById("viewApprovals").classList.toggle("hidden", name !== "approvals");
    document.getElementById("viewMemory").classList.toggle("hidden", name !== "memory");
    renderAll();
  }

  function renderSidebar() {
    const st = ui.state;
    const taskList = document.getElementById("taskList");
    taskList.replaceChildren();

    const runs = (st && st.runs) || [];
    const curRun = activeRun();
    if (runs.length === 0) {
      taskList.appendChild(
        el("div", { className: "sidebar-footer" }, "No tasks yet — type below to start one.")
      );
    } else {
      for (const r of runs) {
        const info = statusInfo(r.status);
        const btn = el(
          "button",
          {
            type: "button",
            className: "task-item" + (ui.view === "tasks" && curRun && curRun.id === r.id ? " active" : ""),
            onClick: () => {
              ui.selectedRunId = r.id;
              switchView("tasks");
            },
          },
          el("span", { className: "status-dot " + info.cls }),
          el("span", { className: "task-item-text" }, r.title || r.goal),
          el("span", { className: "task-item-time" }, relativeTime(r.created_at))
        );
        taskList.appendChild(btn);
      }
    }

    const badge = document.getElementById("approvalsBadge");
    const pending = (st && st.pending_approvals) || 0;
    badge.textContent = String(pending);
    badge.classList.toggle("hidden", pending === 0);

    const sbxDot = document.getElementById("sandboxDot");
    const sbxFoot = document.getElementById("sandboxFooter");
    if (st) {
      sbxDot.className = "status-dot " + (st.sandbox_ready ? "ok" : "warn");
      const modeLabel = st.provider_mode === "mock" ? "Mock mode" : st.provider_mode === "live" ? (st.model_id || "Live") : "Read-only";
      sbxFoot.textContent = (st.sandbox_ready ? st.sandbox_name + " sandbox" : "No sandbox") + " · " + modeLabel;

      const hint = document.getElementById("composerHint");
      const startBtn = document.getElementById("startBtn");
      if (st.provider_mode === "read-only") {
        hint.textContent = "Preview mode — start `soma web --mock` or set SOMA_API_BASE to run new tasks.";
        hint.classList.remove("hidden");
        startBtn.disabled = true;
      } else {
        hint.classList.add("hidden");
        startBtn.disabled = false;
      }
    }
  }

  function renderTasksView() {
    const st = ui.state;
    const run = activeRun();
    const br = activeBranch(run);

    const titleEl = document.getElementById("taskTitle");
    const pillEl = document.getElementById("taskStatusPill");
    const detailsBtn = document.getElementById("detailsBtn");
    const resumeBtn = document.getElementById("resumeBtn");
    const forkBtn = document.getElementById("forkBtn");
    const verifyBtn = document.getElementById("verifyBtn");
    const drawer = document.getElementById("detailsDrawer");
    const feed = document.getElementById("timeline");
    feed.replaceChildren();

    if (!run) {
      titleEl.textContent = "New Task";
      pillEl.classList.add("hidden");
      detailsBtn.classList.add("hidden");
      resumeBtn.classList.add("hidden");
      forkBtn.classList.add("hidden");
      verifyBtn.classList.add("hidden");
      drawer.classList.add("hidden");

      feed.appendChild(
        el(
          "div",
          { className: "empty-state" },
          el("h2", null, "Ready when you are"),
          el("p", null, "Describe a task below to run it inside an isolated sandbox with full step history and verification.")
        )
      );
      return;
    }

    const effStatus = br ? br.status : run.status;
    const info = statusInfo(effStatus);
    titleEl.textContent = run.title || "Task";
    pillEl.textContent = info.label;
    pillEl.className = "pill " + info.cls;
    detailsBtn.classList.remove("hidden");
    detailsBtn.textContent = ui.showDetails ? "Details ▴" : "Details ▾";
    forkBtn.classList.toggle("hidden", !br || !br.steps || br.steps.length === 0);
    verifyBtn.classList.toggle("hidden", !br || !br.steps || br.steps.length === 0);
    const canResume = br && ["queued", "suspended_quota", "suspended_approval"].includes((br.status || "").toLowerCase()) && st.provider_mode !== "read-only";
    resumeBtn.classList.toggle("hidden", !canResume);

    // Optional collapsible Details strip
    if (ui.showDetails && br) {
      drawer.classList.remove("hidden");
      drawer.replaceChildren();
      const ft = br.frame_tokens || {};
      const cacheStr = br.prompt_tokens > 0 ? br.cache_hit_pct.toFixed(1) + "%" : "—";
      const frameStr = ft.total > 0 ? (ft.total + " tok (L0 " + ft.l0 + " · L2 " + ft.l2 + ")") : "—";
      const checkStr = br.contracts_total > 0 ? (br.contracts_passed + "/" + br.contracts_total + " passed") : "None";
      const replayInfo = ui.replayStatusByBranch.get(br.id);

      const grid = el(
        "div",
        { className: "details-grid" },
        el("div", { className: "metric-box" }, el("div", { className: "metric-label" }, "KV Cache Hit"), el("div", { className: "metric-val" }, cacheStr)),
        el("div", { className: "metric-box" }, el("div", { className: "metric-label" }, "Context Frame"), el("div", { className: "metric-val" }, frameStr)),
        el("div", { className: "metric-box" }, el("div", { className: "metric-label" }, "Checks"), el("div", { className: "metric-val" }, checkStr)),
        el("div", { className: "metric-box" }, el("div", { className: "metric-label" }, "Epoch / Replay"), el("div", { className: "metric-val" }, "Epoch " + br.epoch + (replayInfo ? " · " + replayInfo : "")))
      );
      drawer.appendChild(grid);

      const metaRow = el("div", { className: "details-meta" });
      metaRow.appendChild(el("span", null, "Workspace: " + (br.workspace_dir || run.id) + " (" + (br.workspace_strategy || "isolated") + ")"));
      if (run.branches.length > 1) {
        const sel = el("select", {
          className: "select-input",
          "aria-label": "Select branch",
          onChange: (e) => {
            ui.selectedBranchByRun.set(run.id, e.target.value);
            renderTasksView();
          },
        });
        for (const b of run.branches) {
          sel.appendChild(el("option", { value: b.id, selected: b.id === br.id }, (b.label || b.id) + " · " + statusInfo(b.status).label));
        }
        metaRow.appendChild(sel);
      }
      drawer.appendChild(metaRow);
    } else {
      drawer.classList.add("hidden");
    }

    // Goal card
    feed.appendChild(
      el(
        "div",
        { className: "card goal-card" },
        el("div", { className: "card-kicker" }, "Goal"),
        el("div", null, run.goal)
      )
    );

    // Inline approval prompt if this run has pending inbox items
    const runPending = ((st && st.inbox) || []).filter((it) => it.status === "pending" && it.run_id === run.id);
    for (const item of runPending) {
      feed.appendChild(
        el(
          "div",
          { className: "card approval-banner" },
          el(
            "div",
            { className: "card-row" },
            el(
              "div",
              { className: "card-main" },
              el("div", { className: "card-kicker" }, "Action Required"),
              el("div", { className: "step-title" }, item.title),
              el("div", { className: "step-body" }, item.summary)
            ),
            el(
              "div",
              { className: "card-actions" },
              el(
                "button",
                {
                  type: "button",
                  className: "btn-approve",
                  onClick: async () => {
                    try {
                      await apiPost("/api/inbox/" + encodeURIComponent(item.id) + "/approve");
                      if (item.item_kind === "approval" && item.branch_id && st.provider_mode !== "read-only") {
                        await apiPost("/api/branches/" + encodeURIComponent(item.branch_id) + "/resume");
                        showToast("Approved & resumed task");
                      } else {
                        showToast("Approved");
                      }
                      await refreshState();
                    } catch (err) {
                      showToast(err.message);
                    }
                  },
                },
                item.item_kind === "approval" && st.provider_mode !== "read-only" ? "Approve & Resume" : "Approve"
              ),
              el(
                "button",
                {
                  type: "button",
                  className: "btn-ghost",
                  onClick: async () => {
                    try {
                      await apiPost("/api/inbox/" + encodeURIComponent(item.id) + "/dismiss");
                      showToast("Dismissed");
                      await refreshState();
                    } catch (err) {
                      showToast(err.message);
                    }
                  },
                },
                "Dismiss"
              )
            )
          )
        )
      );
    }

    // Steps timeline
    const steps = (br && br.steps) || [];
    let finalSummary = "";
    for (const s of steps) {
      if (s.done && s.final_summary) {
        finalSummary = s.final_summary;
      }
      const stepCard = el("div", { className: "card" });
      stepCard.appendChild(
        el(
          "div",
          { className: "step-head" },
          el("span", { className: "step-title" }, s.title || ("Step " + s.turn_index)),
          el("span", { className: "step-meta" }, s.latency_ms ? s.latency_ms + "ms" : "")
        )
      );
      if (s.reasoning) {
        stepCard.appendChild(el("div", { className: "step-body" }, s.reasoning));
      }

      if (s.tool_calls && s.tool_calls.length > 0) {
        const list = el("div", { className: "tool-list" });
        s.tool_calls.forEach((tc, idx) => {
          const toolKey = s.id + ":" + idx;
          const isOpen = ui.expandedTools.has(toolKey);
          const toggleBtn = el("button", {
            type: "button",
            className: "tool-toggle-btn",
            onClick: async () => {
              if (ui.expandedTools.has(toolKey)) {
                ui.expandedTools.delete(toolKey);
              } else {
                ui.expandedTools.add(toolKey);
                if (tc.page_id && !ui.pageCache.has(tc.page_id)) {
                  try {
                    const pg = await apiGet("/api/pages/" + encodeURIComponent(tc.page_id) + "?start=1&end=120");
                    ui.pageCache.set(tc.page_id, pg.content || "");
                  } catch (err) {
                    ui.pageCache.set(tc.page_id, "Error loading page: " + err.message);
                  }
                }
              }
              renderTasksView();
            },
          }, isOpen ? "Hide output ▴" : (tc.page_id ? "View spilled output ▾" : "Show output ▾"));

          const chip = el(
            "div",
            { className: "tool-chip" },
            el(
              "div",
              { className: "tool-chip-head" },
              el("span", { className: "tool-chip-label" }, tc.label || tc.name),
              toggleBtn
            )
          );

          if (isOpen) {
            const outText = (tc.page_id && ui.pageCache.has(tc.page_id))
              ? ui.pageCache.get(tc.page_id)
              : (tc.observation || "(no output)");
            chip.appendChild(el("pre", { className: "tool-output" }, outText));
          }
          list.appendChild(chip);
        });
        stepCard.appendChild(list);
      }

      feed.appendChild(stepCard);
    }

    if ((effStatus === "running" || effStatus === "verifying") && steps.length === 0) {
      feed.appendChild(el("div", { className: "card" }, el("div", { className: "step-body" }, "Starting sandboxed execution…")));
    }
    if (effStatus === "completed" && finalSummary) {
      feed.appendChild(
        el(
          "div",
          { className: "card summary-card" },
          el("div", { className: "card-kicker" }, "Completed"),
          el("div", null, finalSummary)
        )
      );
    }
  }

  function renderApprovalsView() {
    const st = ui.state;
    const feed = document.getElementById("approvalsFeed");
    feed.replaceChildren();

    const items = (st && st.inbox) || [];
    const pending = items.filter((it) => it.status === "pending");
    const resolved = items.filter((it) => it.status !== "pending");

    if (pending.length === 0) {
      feed.appendChild(
        el(
          "div",
          { className: "empty-state" },
          el("h2", null, "All clear"),
          el("p", null, "No actions, memory proposals, or reviews need your attention.")
        )
      );
    } else {
      feed.appendChild(el("div", { className: "section-heading" }, "Waiting for Review (" + pending.length + ")"));
      for (const it of pending) {
        const urgCls = it.urgency === "urgent" ? "warn" : it.urgency === "low" ? "muted" : "info";
        feed.appendChild(
          el(
            "div",
            { className: "card" },
            el(
              "div",
              { className: "card-row" },
              el(
                "div",
                { className: "card-main" },
                el(
                  "div",
                  { className: "card-title-row" },
                  el("span", { className: "step-title" }, it.title),
                  el("span", { className: "pill " + urgCls }, it.urgency || "normal")
                ),
                el("div", { className: "step-body" }, it.summary)
              ),
              el(
                "div",
                { className: "card-actions" },
                it.run_id ? el("button", {
                  type: "button",
                  className: "btn-ghost",
                  onClick: () => {
                    ui.selectedRunId = it.run_id;
                    switchView("tasks");
                  },
                }, "View Task") : null,
                el("button", {
                  type: "button",
                  className: "btn-approve",
                  onClick: async () => {
                    try {
                      const res = await apiPost("/api/inbox/" + encodeURIComponent(it.id) + "/approve");
                      showToast(res.status || "Approved");
                      await refreshState();
                    } catch (err) {
                      showToast(err.message);
                    }
                  },
                }, "Approve"),
                el("button", {
                  type: "button",
                  className: "btn-ghost",
                  onClick: async () => {
                    try {
                      await apiPost("/api/inbox/" + encodeURIComponent(it.id) + "/dismiss");
                      showToast("Dismissed");
                      await refreshState();
                    } catch (err) {
                      showToast(err.message);
                    }
                  },
                }, "Dismiss")
              )
            )
          )
        );
      }
    }

    if (resolved.length > 0) {
      feed.appendChild(el("div", { className: "section-heading" }, "Recent Decisions"));
      for (const it of resolved.slice(0, 8)) {
        const cls = it.status === "approved" ? "ok" : "muted";
        feed.appendChild(
          el(
            "div",
            { className: "card" },
            el(
              "div",
              { className: "card-row" },
              el(
                "div",
                { className: "card-main" },
                el("div", { className: "card-title-row" }, el("span", { className: "step-title" }, it.title), el("span", { className: "pill " + cls }, it.status)),
                el("div", { className: "step-body" }, it.summary)
              )
            )
          )
        );
      }
    }
  }

  function renderMemoryView() {
    const st = ui.state;
    const feed = document.getElementById("memoryFeed");
    feed.replaceChildren();
    const q = ui.memoryFilter.trim().toLowerCase();

    const mems = ((st && st.memory) || []).filter((m) =>
      !q || m.content.toLowerCase().includes(q) || m.scope.toLowerCase().includes(q)
    );
    feed.appendChild(el("div", { className: "section-heading" }, "Learned Rules (" + mems.length + ")"));

    if (mems.length === 0) {
      feed.appendChild(el("div", { className: "card" }, el("div", { className: "step-body" }, "No learned rules match.")));
    } else {
      for (const m of mems) {
        let stLabel = "Active";
        let stCls = "ok";
        if (m.status === "proposed") {
          stLabel = "Waiting Review";
          stCls = "warn";
        } else if (m.status === "superseded" || m.status === "archived") {
          stLabel = "Replaced";
          stCls = "muted";
        }
        const scopeLabel = m.scope === "global" ? "Global" : "Task-scoped";

        feed.appendChild(
          el(
            "div",
            { className: "card" },
            el(
              "div",
              { className: "card-row" },
              el(
                "div",
                { className: "card-main" },
                el(
                  "div",
                  { className: "card-title-row" },
                  el("span", { className: "pill " + stCls }, stLabel),
                  el("span", { className: "pill muted" }, scopeLabel),
                  m.replaces_id ? el("span", { className: "step-meta" }, "Replaces " + m.replaces_id) : null
                ),
                el("div", null, m.content)
              ),
              el(
                "div",
                { className: "card-actions" },
                m.status === "proposed" ? el("button", {
                  type: "button",
                  className: "btn-approve",
                  onClick: async () => {
                    try {
                      await apiPost("/api/memory/" + encodeURIComponent(m.id) + "/activate");
                      showToast("Rule approved");
                      await refreshState();
                    } catch (err) {
                      showToast(err.message);
                    }
                  },
                }, "Approve") : null,
                el("button", {
                  type: "button",
                  className: "switch-btn" + (m.enabled ? " on" : ""),
                  "aria-pressed": String(m.enabled),
                  onClick: async () => {
                    try {
                      await apiPost("/api/memory/" + encodeURIComponent(m.id) + "/toggle", { enabled: !m.enabled });
                      await refreshState();
                    } catch (err) {
                      showToast(err.message);
                    }
                  },
                }, m.enabled ? "On" : "Off")
              )
            )
          )
        );
      }
    }

    const skills = ((st && st.skills) || []).filter((sk) =>
      !q || sk.name.toLowerCase().includes(q) || sk.description.toLowerCase().includes(q)
    );
    if (skills.length > 0) {
      feed.appendChild(el("div", { className: "section-heading" }, "Installed Skills (" + skills.length + ")"));
      for (const sk of skills) {
        const cls = sk.badge === "CLEAN" ? "ok" : sk.badge === "WARN" ? "warn" : "err";
        const label = sk.badge === "CLEAN" ? "Clean" : sk.badge === "WARN" ? "Warning" : "Blocked";
        feed.appendChild(
          el(
            "div",
            { className: "card" },
            el(
              "div",
              { className: "card-title-row" },
              el("span", { className: "step-title" }, sk.name),
              el("span", { className: "pill " + cls }, label)
            ),
            el("div", { className: "step-body" }, sk.description)
          )
        );
      }
    }

    const accs = (st && st.accounts) || [];
    if (accs.length > 0) {
      feed.appendChild(el("div", { className: "section-heading" }, "Models & Quotas"));
      for (const a of accs) {
        const cool = a.cooldown_sec > 0;
        const badgeCls = cool ? "warn" : "ok";
        const badgeText = cool ? ("Cooling down " + a.cooldown_sec + "s") : "Ready";
        const spend = a.daily_budget_usd > 0 ? ("$" + a.daily_spent_usd.toFixed(2) + " / $" + a.daily_budget_usd.toFixed(2)) : a.billing_mode;
        feed.appendChild(
          el(
            "div",
            { className: "card" },
            el(
              "div",
              { className: "card-row" },
              el(
                "div",
                { className: "card-main" },
                el("div", { className: "step-title" }, a.id + " · " + a.model_id),
                el("div", { className: "step-body" }, a.provider + " · " + spend)
              ),
              el("span", { className: "pill " + badgeCls }, badgeText)
            )
          )
        );
      }
    }
  }

  function renderAll() {
    renderSidebar();
    if (ui.view === "tasks") renderTasksView();
    if (ui.view === "approvals") renderApprovalsView();
    if (ui.view === "memory") renderMemoryView();
  }

  async function refreshState() {
    try {
      ui.state = await apiGet("/api/state");
      renderAll();
    } catch (err) {
      showToast("Refresh failed: " + err.message);
    }
  }

  // Bind UI events
  document.getElementById("themeBtn").addEventListener("click", () => {
    const cur = document.documentElement.getAttribute("data-theme") === "light" ? "light" : "dark";
    const next = cur === "dark" ? "light" : "dark";
    document.documentElement.setAttribute("data-theme", next);
    localStorage.setItem("soma_theme", next);
  });

  document.getElementById("newTaskBtn").addEventListener("click", () => {
    switchView("tasks");
    const inp = document.getElementById("goalInput");
    inp.focus();
  });

  document.querySelectorAll(".nav-item").forEach((btn) => {
    btn.addEventListener("click", () => switchView(btn.dataset.view));
  });

  document.getElementById("detailsBtn").addEventListener("click", () => {
    ui.showDetails = !ui.showDetails;
    renderTasksView();
  });

  document.getElementById("resumeBtn").addEventListener("click", async () => {
    const br = activeBranch(activeRun());
    if (!br) return;
    try {
      await apiPost("/api/branches/" + encodeURIComponent(br.id) + "/resume");
      showToast("Task resumed");
      await refreshState();
    } catch (err) {
      showToast(err.message);
    }
  });

  document.getElementById("forkBtn").addEventListener("click", async () => {
    const run = activeRun();
    const br = activeBranch(run);
    if (!run || !br) return;
    try {
      const res = await apiPost("/api/branches/" + encodeURIComponent(br.id) + "/fork");
      ui.selectedBranchByRun.set(run.id, res.branch_id);
      ui.showDetails = true;
      showToast("Forked branch");
      await refreshState();
    } catch (err) {
      showToast(err.message);
    }
  });

  document.getElementById("verifyBtn").addEventListener("click", async () => {
    const br = activeBranch(activeRun());
    if (!br) return;
    try {
      const res = await apiPost("/api/branches/" + encodeURIComponent(br.id) + "/replay");
      const msg = res.diverged
        ? "Replay diverged"
        : ("Verified " + res.verified_count + "/" + res.steps_checked + " steps");
      ui.replayStatusByBranch.set(br.id, msg);
      ui.showDetails = true;
      showToast("SHA-256 " + msg);
      renderTasksView();
    } catch (err) {
      showToast(err.message);
    }
  });

  document.getElementById("goalForm").addEventListener("submit", async (e) => {
    e.preventDefault();
    const inp = document.getElementById("goalInput");
    const goal = inp.value.trim();
    if (!goal) return;
    inp.value = "";
    try {
      const res = await apiPost("/api/runs", { goal });
      ui.selectedRunId = res.run_id;
      switchView("tasks");
      await refreshState();
    } catch (err) {
      inp.value = goal;
      showToast(err.message);
    }
  });

  document.getElementById("memorySearch").addEventListener("input", (e) => {
    ui.memoryFilter = e.target.value || "";
    renderMemoryView();
  });

  // Live SSE stream with automatic refresh
  if (typeof EventSource !== "undefined") {
    const es = new EventSource("/api/events");
    es.addEventListener("state", () => {
      refreshState();
    });
  }

  switchView(ui.view);
  refreshState();
})();

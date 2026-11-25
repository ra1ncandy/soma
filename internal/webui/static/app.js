(() => {
  "use strict";

  const csrfToken = document.querySelector('meta[name="soma-csrf"]')?.content || "";

  // Streamed model text per branch, filled by SSE "delta" events.
  const liveText = new Map();

  const appState = {
    view: "tasks",
    connTab: "installed",
    harnessTab: "provider",
    approvalsTab: "pending",
    profile: "worker",
    selectedAccountId: "",
    selectedRunId: null,
    selectedBranchId: null,
    detailsOpen: false,
    splitDesktopOpen: false,
    desktopTakeOver: false,
    presetCategory: "All",
    memFilter: "all",
    openAccordions: new Set(),
    lastRenderedBranchId: null,
    lastStepCount: 0,
    authStatus: { auth_required: false, authenticated: true },
    state: null,
    harness: null,
    openAddModelOnRender: false,
  };

  let modalDirty = false;
  let modalProtected = false;
  let authModalOpen = false;
  let suppressHashEvent = false;

  // DOM helper (strictly avoids inline style attributes to satisfy CSP style-src 'self')
  function el(tag, attrs, ...children) {
    const node = document.createElement(tag);
    if (attrs) {
      for (const [k, v] of Object.entries(attrs)) {
        if (v === undefined || v === null) continue;
        if (k === "className") node.className = v;
        else if (k === "text") node.textContent = v;
        else if (k.startsWith("on") && typeof v === "function") {
          node.addEventListener(k.slice(2).toLowerCase(), v);
        } else if (k === "checked" || k === "disabled" || k === "selected" || k === "readOnly") {
          node[k] = Boolean(v);
        } else if (k === "value" && (tag === "input" || tag === "textarea" || tag === "select" || tag === "option")) {
          node.setAttribute("value", String(v));
          node.value = String(v);
        } else {
          node.setAttribute(k, String(v));
        }
      }
    }
    for (const c of children) {
      if (c === null || c === undefined || c === false) continue;
      if (typeof c === "string" || typeof c === "number") {
        node.appendChild(document.createTextNode(String(c)));
      } else {
        node.appendChild(c);
      }
    }
    return node;
  }

  let toastTimer = null;
  function showToast(msg, isError = false) {
    const t = document.getElementById("toast");
    const txt = document.getElementById("toastText");
    if (!t || !txt) return;
    txt.textContent = String(msg || "");
    t.classList.toggle("toast-err", Boolean(isError));
    t.classList.remove("hidden");
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => t.classList.add("hidden"), isError ? 8000 : 3800);
  }

  function showErrorToast(err) {
    showToast(err?.message || String(err || "Operation failed"), true);
  }

  async function copyToClipboard(text, label = "Copied") {
    try {
      await navigator.clipboard.writeText(String(text || ""));
      showToast(`${label} copied to clipboard`);
    } catch (_) {
      showToast("Copy failed — select and copy manually", true);
    }
  }

  function formatRelativeTime(unixSec) {
    if (!unixSec) return "";
    const diff = Math.max(0, Math.floor(Date.now() / 1000) - Number(unixSec));
    if (diff < 45) return "just now";
    if (diff < 3600) return `${Math.floor(diff / 60)}m ago`;
    if (diff < 86400) return `${Math.floor(diff / 3600)}h ago`;
    return `${Math.floor(diff / 86400)}d ago`;
  }

  function parseArgsQuoted(raw) {
    const matches = String(raw || "").match(/(?:[^\s"]+|"[^"]*")+/g);
    if (!matches) return [];
    return matches.map((tok) => (tok.startsWith('"') && tok.endsWith('"') ? tok.slice(1, -1) : tok));
  }

  async function apiGet(url) {
    const res = await fetch(url, { credentials: "same-origin" });
    if (!res.ok) {
      const err = await res.json().catch(() => ({}));
      const e = new Error(err.error || `HTTP ${res.status}`);
      e.status = res.status;
      throw e;
    }
    return res.json();
  }

  async function apiPost(url, body) {
    const res = await fetch(url, {
      method: "POST",
      credentials: "same-origin",
      headers: {
        "Content-Type": "application/json",
        "X-Soma-CSRF": csrfToken,
      },
      body: JSON.stringify(body || {}),
    });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) {
      const e = new Error(data.error || `HTTP ${res.status}`);
      e.status = res.status;
      throw e;
    }
    return data;
  }

  // URL Hash Routing
  function syncHash() {
    let next = `#${appState.view}`;
    if (appState.view === "tasks" && appState.selectedRunId) {
      next = `#tasks/${encodeURIComponent(appState.selectedRunId)}`;
      if (appState.selectedBranchId) {
        next += `/${encodeURIComponent(appState.selectedBranchId)}`;
      }
    } else if (appState.view === "connectors") {
      next = `#connectors/${appState.connTab}`;
    } else if (appState.view === "harness") {
      next = `#settings/${appState.harnessTab}`;
    } else if (appState.view === "approvals") {
      next = `#approvals/${appState.approvalsTab}`;
    }
    if (window.location.hash !== next) {
      suppressHashEvent = true;
      window.location.hash = next;
      setTimeout(() => { suppressHashEvent = false; }, 0);
    }
  }

  function restoreFromHash() {
    const raw = (window.location.hash || "").replace(/^#/, "").trim();
    if (!raw) return;
    const parts = raw.split("/").map((p) => decodeURIComponent(p));
    const root = parts[0];
    if (root === "tasks") {
      appState.view = "tasks";
      appState.selectedRunId = parts[1] || null;
      appState.selectedBranchId = parts[2] || null;
    } else if (root === "desktop") {
      appState.view = "desktop";
    } else if (root === "connectors") {
      appState.view = "connectors";
      if (["installed", "presets", "bridges"].includes(parts[1])) {
        appState.connTab = parts[1];
      }
    } else if (root === "settings" || root === "harness") {
      appState.view = "harness";
      if (["provider", "sandbox", "remote", "triggers"].includes(parts[1])) {
        appState.harnessTab = parts[1];
      }
    } else if (root === "approvals") {
      appState.view = "approvals";
      if (["pending", "resolved", "all"].includes(parts[1])) {
        appState.approvalsTab = parts[1];
      }
    } else if (root === "memory") {
      appState.view = "memory";
    }
  }

  // Theme & Shell controls
  function initTheme() {
    const saved = localStorage.getItem("soma_theme") || "dark";
    document.documentElement.setAttribute("data-theme", saved);
    document.getElementById("themeBtn").addEventListener("click", () => {
      const cur = document.documentElement.getAttribute("data-theme") || "dark";
      const next = cur === "dark" ? "light" : "dark";
      document.documentElement.setAttribute("data-theme", next);
      localStorage.setItem("soma_theme", next);
    });
  }

  function closeMobileDrawer() {
    document.getElementById("appShell").classList.remove("mobile-drawer-open");
    document.getElementById("sidebarBackdrop").classList.add("hidden");
  }

  function openMobileDrawer() {
    document.getElementById("appShell").classList.add("mobile-drawer-open");
    document.getElementById("sidebarBackdrop").classList.remove("hidden");
  }

  function syncViewDOM() {
    const view = appState.view;
    document.querySelectorAll(".nav-item").forEach((b) => {
      b.classList.toggle("active", b.dataset.view === view);
    });
    document.getElementById("viewTasks").classList.toggle("hidden", view !== "tasks");
    document.getElementById("viewDesktop").classList.toggle("hidden", view !== "desktop");
    document.getElementById("viewConnectors").classList.toggle("hidden", view !== "connectors");
    document.getElementById("viewHarness").classList.toggle("hidden", view !== "harness");
    document.getElementById("viewApprovals").classList.toggle("hidden", view !== "approvals");
    document.getElementById("viewMemory").classList.toggle("hidden", view !== "memory");

    document.querySelectorAll("[data-conn-tab]").forEach((x) => x.classList.toggle("active", x.dataset.connTab === appState.connTab));
    document.querySelectorAll("[data-harness-tab]").forEach((x) => x.classList.toggle("active", x.dataset.harnessTab === appState.harnessTab));
    document.querySelectorAll("[data-app-tab]").forEach((x) => x.classList.toggle("active", x.dataset.appTab === appState.approvalsTab));

    const mcpHeaderActions = document.getElementById("mcpHeaderActions");
    if (mcpHeaderActions) {
      mcpHeaderActions.classList.toggle("hidden", appState.connTab === "bridges");
    }
  }

  function switchView(view) {
    appState.view = view;
    closeMobileDrawer();
    syncViewDOM();
    syncHash();
    renderCurrentView();
  }

  // Modal System with Inline Errors, Escape Support & Dirty-Form Guard
  function openModal(title, bodyNodes, opts = {}) {
    const card = document.getElementById("modalCard");
    card.classList.toggle("modal-wide", Boolean(opts.wide));
    const errBox = el("div", { className: "modal-error hidden", id: "modalErrorBox" });
    card.replaceChildren(
      el("div", { className: "modal-head" },
        el("h2", { text: title }),
        el("button", { className: "icon-btn", type: "button", text: "✕", ariaLabel: "Close dialog", onClick: () => closeModal(true) })
      ),
      errBox,
      ...bodyNodes
    );
    modalDirty = false;
    modalProtected = Boolean(opts.protectAuthOverride);
    if (!opts.skipDirtyTracking) {
      card.addEventListener("input", () => { modalDirty = true; });
      card.addEventListener("change", () => { modalDirty = true; });
    }
    document.getElementById("modalBackdrop").classList.remove("hidden");
  }

  function showModalError(msg) {
    const box = document.getElementById("modalErrorBox");
    if (box) {
      box.textContent = String(msg || "Request failed");
      box.classList.remove("hidden");
    } else {
      showErrorToast(msg);
    }
  }

  function closeModal(force = true) {
    if (!force && modalDirty) {
      if (!window.confirm("Discard unsaved changes in this dialog?")) return;
    }
    modalDirty = false;
    modalProtected = false;
    authModalOpen = false;
    document.getElementById("modalBackdrop").classList.add("hidden");
  }

  function confirmModal(title, message, confirmLabel, onConfirm, danger = true) {
    openModal(title, [
      el("p", { className: "connector-desc", text: message }),
      el("div", { className: "connector-actions" },
        el("button", { className: "btn-secondary", type: "button", text: "Cancel", onClick: () => closeModal(true) }),
        el("button", {
          className: danger ? "btn-danger" : "btn-primary",
          type: "button",
          text: confirmLabel,
          onClick: async () => {
            try {
              await onConfirm();
              closeModal(true);
            } catch (e) {
              showModalError(e.message);
            }
          },
        })
      ),
    ], { skipDirtyTracking: true });
  }

  function openAuthModal() {
    if (authModalOpen || modalProtected) return;
    authModalOpen = true;
    const tokIn = el("input", { className: "field-input", type: "password", placeholder: "Enter soma dashboard access token" });
    const doLogin = async () => {
      try {
        await apiPost("/api/auth/login", { token: tokIn.value.trim() });
        authModalOpen = false;
        closeModal(true);
        await refreshAll();
        showToast("Signed in");
      } catch (e) {
        showModalError(e.message);
      }
    };
    tokIn.addEventListener("keydown", (ev) => {
      if (ev.key === "Enter") {
        ev.preventDefault();
        doLogin();
      }
    });
    openModal("Sign in to soma Dashboard", [
      el("p", { className: "connector-desc", text: "Authentication is enabled on this dashboard. Enter your access token to continue." }),
      el("div", { className: "field full" }, el("span", { className: "field-label", text: "Access Token" }), tokIn),
      el("div", { className: "connector-actions" },
        el("button", { className: "btn-primary", type: "button", text: "Sign In", onClick: doLogin })
      ),
    ], { skipDirtyTracking: true });
    authModalOpen = true;
    setTimeout(() => tokIn.focus(), 30);
  }

  // Every tool call and step emits an SSE event. Coalesce them: one refresh in flight at a time,
  // plus a single trailing refresh if more events arrived while it ran. This keeps responses in
  // order (an older state can never overwrite a newer one) and avoids rebuilding the whole view
  // once per event during a burst.
  let refreshInFlight = null;
  let refreshQueued = false;
  function refreshAll() {
    if (refreshInFlight) {
      refreshQueued = true;
      return refreshInFlight;
    }
    refreshInFlight = (async () => {
      try {
        await refreshAllNow();
      } finally {
        refreshInFlight = null;
        if (refreshQueued) {
          refreshQueued = false;
          refreshAll().catch(() => {});
        }
      }
    })();
    return refreshInFlight;
  }

  async function refreshAllNow() {
    try {
      const [st, har, authSt] = await Promise.all([
        apiGet("/api/state"),
        apiGet("/api/harness").catch(() => null),
        apiGet("/api/auth/status").catch(() => ({ auth_required: false, authenticated: true })),
      ]);
      appState.state = st;
      if (har) appState.harness = har;
      if (authSt) appState.authStatus = authSt;

      // Ensure selectedBranchId stays valid if selectedRunId exists
      if (appState.selectedRunId) {
        const run = (st.runs || []).find((r) => r.id === appState.selectedRunId);
        if (!run) {
          appState.selectedRunId = null;
          appState.selectedBranchId = null;
        } else if (!appState.selectedBranchId || !(run.branches || []).some((b) => b.id === appState.selectedBranchId)) {
          appState.selectedBranchId = run.branches?.[0]?.id || null;
        }
      }

      syncViewDOM();
      renderChrome();
      renderCurrentView();
    } catch (e) {
      if (e.status === 401 || String(e.message || "").includes("401") || String(e.message || "").toLowerCase().includes("unauthorized")) {
        openAuthModal();
        return;
      }
      throw e;
    }
  }

  function countActiveBridges(har) {
    const br = har?.bridges || {};
    let n = 0;
    for (const k of ["discord", "telegram", "slack", "matrix", "mattermost", "teams", "google_chat", "email", "whatsapp", "signal", "sms"]) {
      if (br[k]?.enabled) n++;
    }
    return n;
  }

  function resolveEffectiveModelLabel() {
    const st = appState.state;
    const har = appState.harness;
    const accs = har?.accounts || [];
    if (appState.selectedAccountId) {
      const explicit = accs.find((a) => a.id === appState.selectedAccountId);
      if (explicit) return explicit.model_id || explicit.id;
    }
    const wantRole = appState.profile === "frontier" ? "frontier" : "worker";
    const byRole = accs.find((a) => a.role === wantRole);
    if (byRole) return byRole.model_id || byRole.id;
    const def = accs.find((a) => a.is_default) || accs[0];
    return def?.model_id || st?.model_id || (st?.provider_mode === "mock" ? "mock" : "unconfigured");
  }

  function renderChrome() {
    const st = appState.state;
    const har = appState.harness;
    if (!st) return;

    const dot = document.getElementById("sandboxDot");
    dot.className = "status-dot " + (st.sandbox_ready ? "ok" : "warn");
    const activeModel = resolveEffectiveModelLabel();
    const tunOn = Boolean(har?.tunnel?.running);
    const netOn = Boolean(har?.provider?.allow_sandbox_network);
    const autoHigh = har?.provider?.autonomy_mode === "autonomous";
    const sbxLabel = st.sandbox_ready
      ? `Sandbox: ${st.sandbox_name}${netOn ? " (Net ON)" : ""}${autoHigh ? " · Auto" : ""}`
      : "No OS sandbox";
    document.getElementById("sandboxFooter").textContent = `${activeModel} · ${tunOn ? "Tunnel ON" : sbxLabel}`;

    const logoutBtn = document.getElementById("logoutBtn");
    if (logoutBtn) {
      logoutBtn.classList.toggle("hidden", !(appState.authStatus?.auth_required && appState.authStatus?.authenticated));
    }

    const accs = har?.accounts || [];
    const defaultAcc = accs.find((a) => a.is_default) || accs[0];
    const accSel = document.getElementById("composerAccountSel");
    if (accSel) {
      const prev = appState.selectedAccountId || "";
      const autoLabel = defaultAcc ? `Auto (${activeModel})` : "No model configured";
      accSel.replaceChildren(
        el("option", { value: "", text: autoLabel, selected: prev === "" }),
        ...accs.map((a) => el("option", {
          value: a.id,
          text: `${a.id} · ${a.model_id || a.provider}${a.role && a.role !== "default" ? " [" + a.role + "]" : ""}`,
          selected: a.id === prev,
        }))
      );
      accSel.value = accs.some((a) => a.id === prev) ? prev : "";
    }

    const run = getSelectedRun();
    const branch = getSelectedBranch(run);
    const isRunning = Boolean(branch && (branch.status === "running" || (branch.status === "queued" && run?.status === "running")));

    const anyRunning = (st.runs || []).some((r) => r.status === "running" || (r.branches || []).some((b) => b.status === "running"));
    const activeTaskDot = document.getElementById("activeTaskNavDot");
    if (activeTaskDot) {
      activeTaskDot.classList.toggle("hidden", !anyRunning);
      activeTaskDot.className = "status-dot nav-dot " + (anyRunning ? "running" : "hidden");
    }

    const goalInput = document.getElementById("goalInput");
    const sendBtn = document.getElementById("startBtn");
    const sendIcon = document.getElementById("sendBtnIcon");
    if (goalInput) {
      if (st.provider_mode === "read-only") {
        goalInput.placeholder = "Add an AI model in Settings to start…";
      } else if (isRunning) {
        goalInput.placeholder = "Working… click ■ to stop";
      } else if (appState.selectedRunId) {
        goalInput.placeholder = "Reply or follow up…";
      } else {
        goalInput.placeholder = "Ask soma to build, debug, or browse…";
      }
    }

    document.getElementById("mobileModelLabel").textContent = `${activeModel} · ${appState.profile === "frontier" ? "Frontier" : "Worker"}`;
    document.getElementById("composerModeLabel").textContent =
      appState.profile === "frontier" ? "Frontier" : "Worker";

    // Approvals badge
    const badge = document.getElementById("approvalsBadge");
    if (st.pending_approvals > 0) {
      badge.textContent = String(st.pending_approvals);
      badge.classList.remove("hidden");
    } else {
      badge.classList.add("hidden");
    }

    // Desktop status indicators
    const deskRunning = Boolean(har?.desktop?.running);
    document.getElementById("desktopNavDot").className = "status-dot nav-dot " + (deskRunning ? "ok" : "");
    document.getElementById("mobileDesktopDot").className = "status-dot mobile-corner-dot " + (deskRunning ? "ok" : "");
    document.getElementById("floatingDesktopDot").className = "status-dot " + (deskRunning ? "ok" : "");
    document.getElementById("splitDesktopDot").className = "status-dot " + (deskRunning ? "ok" : "");
    document.getElementById("quickDesktopState").textContent = deskRunning ? "Running" : "Stopped";

    // Connectors count (MCP + active Channel Bridges)
    const activeMcps = (har?.mcp_servers || []).filter((m) => m.enabled).length;
    const activeBridges = countActiveBridges(har);
    const totalActiveConnectors = activeMcps + activeBridges;
    document.getElementById("connectorsCountPill").textContent = String(totalActiveConnectors);
    document.getElementById("quickConnectorsCount").textContent = `${totalActiveConnectors} active`;

    // Composer hint & Send/Stop button state
    const hint = document.getElementById("composerHint");
    if (st.provider_mode === "read-only") {
      hint.textContent = "No AI model configured yet — click Settings → Models & Providers to connect a provider.";
      hint.classList.remove("hidden");
      sendBtn.disabled = true;
      sendBtn.classList.remove("stop-mode");
    } else if (isRunning) {
      hint.classList.add("hidden");
      sendBtn.disabled = false;
      sendBtn.classList.add("stop-mode");
      sendBtn.title = "Stop running task";
      if (sendIcon) {
        const rect = document.createElementNS("http://www.w3.org/2000/svg", "rect");
        rect.setAttribute("x", "6");
        rect.setAttribute("y", "6");
        rect.setAttribute("width", "12");
        rect.setAttribute("height", "12");
        rect.setAttribute("rx", "2");
        rect.setAttribute("fill", "currentColor");
        rect.setAttribute("stroke", "none");
        sendIcon.replaceChildren(rect);
      }
    } else {
      hint.classList.add("hidden");
      sendBtn.disabled = false;
      sendBtn.classList.remove("stop-mode");
      sendBtn.title = "Send (Enter)";
      if (sendIcon) {
        const path = document.createElementNS("http://www.w3.org/2000/svg", "path");
        path.setAttribute("d", "M5 12h14M13 5l7 7-7 7");
        sendIcon.replaceChildren(path);
      }
    }

    // Sidebar Recent Tasks list with Rename & Delete controls
    const list = document.getElementById("taskList");
    list.replaceChildren();
    if (!st.runs || st.runs.length === 0) {
      list.appendChild(el("div", { className: "muted caption-text", text: "No tasks yet." }));
    } else {
      for (const r of st.runs) {
        const isSelected = r.id === appState.selectedRunId;
        const dotCls =
          r.status === "completed" ? "ok" :
          r.status === "running" ? "running" :
          r.status === "failed" ? "err" : "warn";
        const brCount = r.branches?.length || 0;
        const relTime = formatRelativeTime(r.updated_at || r.created_at);
        const metaParts = [r.status];
        if (brCount > 1) metaParts.push(`${brCount} versions`);
        if (relTime) metaParts.push(relTime);

        const row = el("div", { className: "task-item-row" + (isSelected ? " active" : "") },
          el("button", {
            className: "task-item" + (isSelected ? " active" : ""),
            type: "button",
            onClick: () => {
              appState.selectedRunId = r.id;
              appState.selectedBranchId = r.branches?.[0]?.id || null;
              switchView("tasks");
            },
          },
            el("div", { className: "task-item-top" },
              el("span", { className: "status-dot " + dotCls }),
              el("span", { className: "task-item-title", text: r.title || r.goal || r.id })
            ),
            el("div", { className: "task-item-meta", text: metaParts.join(" · ") })
          ),
          el("div", { className: "task-row-actions" },
            el("button", {
              className: "task-row-btn",
              type: "button",
              title: "Rename task",
              text: "✎",
              onClick: (ev) => {
                ev.stopPropagation();
                openRenameTaskModal(r);
              },
            }),
            el("button", {
              className: "task-row-btn danger",
              type: "button",
              title: "Delete task",
              text: "✕",
              onClick: (ev) => {
                ev.stopPropagation();
                confirmDeleteTask(r);
              },
            })
          )
        );
        list.appendChild(row);
      }
    }
  }

  function openRenameTaskModal(run) {
    const titleIn = el("input", { className: "field-input", type: "text", value: run.title || run.goal || "" });
    openModal("Rename Task", [
      el("div", { className: "field full" }, el("span", { className: "field-label", text: "Task Title" }), titleIn),
      el("div", { className: "connector-actions" },
        el("button", { className: "btn-secondary", type: "button", text: "Cancel", onClick: () => closeModal(true) }),
        el("button", {
          className: "btn-primary",
          type: "button",
          text: "Save Title",
          onClick: async () => {
            const next = titleIn.value.trim();
            if (!next) { showModalError("Title cannot be empty"); return; }
            try {
              await apiPost(`/api/runs/${encodeURIComponent(run.id)}/rename`, { title: next });
              closeModal(true);
              await refreshAll();
              showToast("Task renamed");
            } catch (e) { showModalError(e.message); }
          },
        })
      ),
    ]);
    setTimeout(() => titleIn.select(), 30);
  }

  function confirmDeleteTask(run) {
    confirmModal(
      "Delete Task",
      `Permanently delete "${run.title || run.goal || run.id}" and clean up its isolated workspace? Project files in your main folder will not be touched.`,
      "Delete Task",
      async () => {
        await apiPost(`/api/runs/${encodeURIComponent(run.id)}/delete`, {});
        if (appState.selectedRunId === run.id) {
          appState.selectedRunId = null;
          appState.selectedBranchId = null;
          syncHash();
        }
        await refreshAll();
        showToast("Task deleted");
      }
    );
  }

  function renderCurrentView() {
    switch (appState.view) {
      case "tasks": renderTasksView(); break;
      case "desktop": renderDesktopView(); break;
      case "connectors": renderConnectorsView(); break;
      case "harness": renderHarnessView(); break;
      case "approvals": renderApprovalsView(); break;
      case "memory": renderMemoryView(); break;
    }
  }

  function getSelectedRun() {
    if (!appState.state || !appState.selectedRunId) return null;
    return (appState.state.runs || []).find((r) => r.id === appState.selectedRunId) || null;
  }

  function getSelectedBranch(run) {
    if (!run || !run.branches || run.branches.length === 0) return null;
    return run.branches.find((b) => b.id === appState.selectedBranchId) || run.branches[0];
  }

  function renderSplitDesktop() {
    const pane = document.getElementById("desktopSplitPane");
    if (!appState.splitDesktopOpen) {
      pane.classList.add("hidden");
      return;
    }
    pane.classList.remove("hidden");
    const desk = appState.harness?.desktop;
    const dot = document.getElementById("splitPaneDot");
    const ph = document.getElementById("splitPlaceholder");
    const frame = document.getElementById("splitDesktopFrame");
    const shield = document.getElementById("splitDesktopShield");
    const takeOverBtn = document.getElementById("splitTakeOverBtn");
    if (desk?.running) {
      dot.className = "status-dot ok";
      ph.classList.add("hidden");
      frame.classList.remove("hidden");
      takeOverBtn.classList.remove("hidden");
      takeOverBtn.textContent = appState.desktopTakeOver ? "Hand Back Control" : "Take Over";
      takeOverBtn.className = (appState.desktopTakeOver ? "btn-primary" : "btn-secondary") + " btn-sm";
      shield.classList.toggle("hidden", Boolean(appState.desktopTakeOver));
      const src = desk.proxy_url || desk.novnc_url;
      if (src && frame.getAttribute("data-src") !== src) {
        frame.setAttribute("data-src", src);
        frame.src = src;
      }
    } else {
      dot.className = "status-dot";
      ph.classList.remove("hidden");
      frame.classList.add("hidden");
      shield.classList.add("hidden");
      takeOverBtn.classList.add("hidden");
      if (frame.src) {
        frame.removeAttribute("src");
        frame.removeAttribute("data-src");
      }
    }
  }

  function appendInlineMarkdown(parent, line) {
    const re = /(`[^`]+`|\*\*[^*]+\*\*)/g;
    let last = 0;
    let m;
    while ((m = re.exec(line)) !== null) {
      if (m.index > last) {
        parent.appendChild(document.createTextNode(line.slice(last, m.index)));
      }
      const tok = m[0];
      if (tok.startsWith("`")) {
        parent.appendChild(el("code", { text: tok.slice(1, -1) }));
      } else {
        parent.appendChild(el("strong", { text: tok.slice(2, -2) }));
      }
      last = m.index + tok.length;
    }
    if (last < line.length) {
      parent.appendChild(document.createTextNode(line.slice(last)));
    }
  }

  function renderMarkdownEl(text) {
    const wrap = el("div", { className: "md-body" });
    const src = String(text || "");
    const fenceRe = /```([a-zA-Z0-9_-]*)\n([\s\S]*?)```/g;
    let last = 0;
    let m;
    const parseTableRow = (rowLine) =>
      rowLine.trim().replace(/^\|/, "").replace(/\|$/, "").split("|").map((c) => c.trim());
    const isTableSep = (rowLine) =>
      /^\s*\|?(\s*:?-{2,}:?\s*\|)+\s*:?-{2,}:?\s*\|?\s*$/.test(rowLine);

    const appendProse = (chunk) => {
      const lines = chunk.split("\n");
      let ul = null;
      for (let i = 0; i < lines.length; i++) {
        const line = lines[i].trimEnd();
        const trimmed = line.trim();
        if (!trimmed) { ul = null; continue; }

        // Markdown table
        if (trimmed.includes("|") && i + 1 < lines.length && isTableSep(lines[i + 1])) {
          ul = null;
          const headers = parseTableRow(trimmed);
          const thead = el("thead", null, el("tr", null, ...headers.map((h) => {
            const th = el("th");
            appendInlineMarkdown(th, h);
            return th;
          })));
          const tbody = el("tbody");
          i += 2;
          while (i < lines.length && lines[i].trim().includes("|") && !isTableSep(lines[i])) {
            const cells = parseTableRow(lines[i]);
            const tr = el("tr");
            for (let c = 0; c < headers.length; c++) {
              const td = el("td");
              appendInlineMarkdown(td, cells[c] || "");
              tr.appendChild(td);
            }
            tbody.appendChild(tr);
            i++;
          }
          i--;
          wrap.appendChild(el("div", { className: "md-table-wrap" }, el("table", { className: "md-table" }, thead, tbody)));
          continue;
        }

        // Headings (#, ##, ###)
        const hMatch = trimmed.match(/^(#{1,3})\s+(.+)$/);
        if (hMatch) {
          ul = null;
          const hTag = hMatch[1].length === 1 ? "h3" : hMatch[1].length === 2 ? "h4" : "h5";
          const hEl = el(hTag, { className: "md-heading" });
          appendInlineMarkdown(hEl, hMatch[2]);
          wrap.appendChild(hEl);
          continue;
        }

        // Bullet / numbered list
        if (/^\s*([-*]|\d+\.)\s+/.test(line)) {
          if (!ul) { ul = el("ul"); wrap.appendChild(ul); }
          const li = el("li");
          appendInlineMarkdown(li, line.replace(/^\s*([-*]|\d+\.)\s+/, ""));
          ul.appendChild(li);
          continue;
        }

        ul = null;
        const p = el("p");
        appendInlineMarkdown(p, line);
        wrap.appendChild(p);
      }
    };
    while ((m = fenceRe.exec(src)) !== null) {
      if (m.index > last) appendProse(src.slice(last, m.index));
      wrap.appendChild(el("pre", null, el("code", { text: m[2] })));
      last = m.index + m[0].length;
    }
    if (last < src.length) appendProse(src.slice(last));
    return wrap;
  }

  function formatStrategyLabel(strat) {
    switch (strat) {
      case "git_worktree":
      case "worktree":
        return "Isolated Git Worktree";
      case "copy_snapshot":
      case "copy":
        return "Isolated Snapshot";
      case "direct":
        return "Direct Project Folder";
      default:
        return strat || "Isolated Workspace";
    }
  }

  async function openBranchDiffModal(branch) {
    try {
      const diff = await apiGet(`/api/branches/${encodeURIComponent(branch.id)}/diff`);
      const files = diff.files || [];
      if (files.length === 0) {
        showToast("No unapplied file differences found");
        return;
      }
      const fileNodes = files.map((f) => {
        const pillCls = f.status === "added" ? "ok" : f.status === "deleted" ? "err" : "warn";
        return el("div", { className: "diff-file-card" },
          el("div", { className: "diff-file-head" },
            el("span", { text: f.path }),
            el("span", { className: "pill " + pillCls, text: f.status })
          ),
          f.unified_diff ? el("pre", { className: "diff-pre", text: f.unified_diff }) : null
        );
      });
      openModal(`Review Workspace Changes (${files.length} file${files.length === 1 ? "" : "s"})`, [
        el("p", { className: "connector-desc", text: "These edits were made inside soma's isolated sandbox workspace. Click Apply to Project to merge them into your main project folder." }),
        ...fileNodes,
        el("div", { className: "connector-actions" },
          el("button", { className: "btn-secondary", type: "button", text: "Close", onClick: () => closeModal(true) }),
          el("button", {
            className: "btn-primary",
            type: "button",
            text: "Apply to Project",
            onClick: async () => {
              try {
                await applyBranchToProject(branch);
                closeModal(true);
              } catch (e) { showModalError(e.message); }
            },
          })
        ),
      ], { wide: true, skipDirtyTracking: true });
    } catch (e) {
      showErrorToast(e);
    }
  }

  async function applyBranchToProject(branch) {
    const res = await apiPost(`/api/branches/${encodeURIComponent(branch.id)}/apply`, {});
    const rep = res.report || {};
    const total = (rep.added_files?.length || 0) + (rep.modified_files?.length || 0) + (rep.deleted_files?.length || 0);
    await refreshAll();
    showToast(total > 0 ? `Applied ${total} file(s) to project workspace` : "Workspace is already up to date");
  }

  function renderTasksView() {
    const run = getSelectedRun();
    const branch = getSelectedBranch(run);
    const stage = document.getElementById("tasksStage");
    const hero = document.getElementById("heroEmpty");
    const scroll = document.getElementById("timelineScroll");
    const appShell = document.getElementById("appShell");
    const topbar = document.getElementById("taskTopbar");
    const changesBar = document.getElementById("workspaceChangesBar");
    const inlineAppBar = document.getElementById("inlineApprovalsBar");

    const titleEl = document.getElementById("taskTitle");
    const statusPill = document.getElementById("taskStatusPill");
    const branchSel = document.getElementById("branchSelect");
    const stopBtn = document.getElementById("stopBtn");
    const resumeBtn = document.getElementById("resumeBtn");
    const detailsBtn = document.getElementById("detailsBtn");
    const forkBtn = document.getElementById("forkBtn");
    const verifyBtn = document.getElementById("verifyBtn");

    renderSplitDesktop();

    // Empty Home State
    if (!run) {
      appShell.classList.remove("has-active-task");
      topbar.classList.add("hidden");
      changesBar.classList.add("hidden");
      inlineAppBar.classList.add("hidden");
      stage.classList.add("empty-mode");
      hero.classList.remove("hidden");
      scroll.classList.add("hidden");
      statusPill.classList.add("hidden");
      branchSel.classList.add("hidden");
      [stopBtn, resumeBtn, detailsBtn, forkBtn, verifyBtn].forEach((b) => b.classList.add("hidden"));
      document.getElementById("detailsDrawer").classList.add("hidden");

      const setupCallout = document.getElementById("heroSetupCallout");
      if (setupCallout) {
        if (appState.state?.provider_mode === "read-only") {
          setupCallout.classList.remove("hidden");
          setupCallout.replaceChildren(
            el("span", { text: "Connect an AI provider (OpenRouter, Anthropic, OpenAI, Ollama, or Mock) to start running tasks." }),
            el("button", {
              className: "btn-primary btn-sm",
              type: "button",
              text: "+ Add AI Model",
              onClick: () => {
                appState.harnessTab = "provider";
                appState.openAddModelOnRender = true;
                switchView("harness");
              },
            })
          );
        } else {
          setupCallout.classList.add("hidden");
        }
      }
      return;
    }

    appShell.classList.add("has-active-task");
    topbar.classList.remove("hidden");
    stage.classList.remove("empty-mode");
    hero.classList.add("hidden");
    scroll.classList.remove("hidden");

    titleEl.textContent = run.title || run.goal;
    const activeStatus = branch?.status || run.status;
    statusPill.textContent = activeStatus;
    statusPill.className = "pill " + (
      activeStatus === "completed" ? "ok" :
      activeStatus === "running" ? "running" :
      activeStatus === "failed" ? "err" : "warn"
    );

    const branches = run.branches || [];
    if (branches.length > 1) {
      branchSel.classList.remove("hidden");
      branchSel.replaceChildren(
        ...branches.map((b) =>
          el("option", { value: b.id, text: `${b.label || b.id} (${b.status})`, selected: branch && b.id === branch.id })
        )
      );
    } else {
      branchSel.classList.add("hidden");
    }

    const isRunning = Boolean(branch && (branch.status === "running" || (branch.status === "queued" && run.status === "running")));
    const isCompleted = Boolean(branch && branch.status === "completed");
    const canResume = Boolean(branch && !isRunning && !isCompleted);

    stopBtn.classList.toggle("hidden", !isRunning);
    resumeBtn.classList.toggle("hidden", !canResume);
    detailsBtn.classList.toggle("hidden", !branch);
    forkBtn.classList.toggle("hidden", !branch || !(branch.steps?.length > 0));
    verifyBtn.classList.toggle("hidden", !branch || !(branch.steps?.length > 0));

    // Workspace Changes Diff & Apply Bar
    if (branch && branch.changed_files_count > 0) {
      changesBar.classList.remove("hidden");
      const cnt = branch.changed_files_count;
      changesBar.replaceChildren(
        el("div", { className: "task-banner-title" },
          el("span", { className: "status-dot ok" }),
          el("span", { text: `${cnt} changed file${cnt === 1 ? "" : "s"}` })
        ),
        el("div", { className: "connector-actions" },
          el("button", {
            className: "btn-secondary btn-sm",
            type: "button",
            text: "Review Diff",
            onClick: () => openBranchDiffModal(branch),
          }),
          el("button", {
            className: "btn-primary btn-sm",
            type: "button",
            text: "Apply to Project",
            onClick: async () => {
              try {
                await applyBranchToProject(branch);
              } catch (e) { showErrorToast(e); }
            },
          })
        )
      );
    } else {
      changesBar.classList.add("hidden");
    }

    // Inline Pending Approval Banner for this Run/Branch
    const pendingForRun = (appState.state?.inbox || []).filter(
      (it) => it.status === "pending" && (it.run_id === run.id || (branch && it.branch_id === branch.id))
    );
    if (pendingForRun.length > 0) {
      const firstApp = pendingForRun[0];
      inlineAppBar.classList.remove("hidden");
      inlineAppBar.replaceChildren(
        el("div", { className: "task-banner-title" },
          el("span", { className: "status-dot warn" }),
          el("span", { text: `Approval needed: ${firstApp.title}` })
        ),
        el("div", { className: "connector-actions" },
          el("button", {
            className: "btn-secondary btn-sm",
            type: "button",
            text: "Inspect",
            onClick: () => openApprovalInspectModal(firstApp),
          }),
          el("button", {
            className: "btn-primary btn-sm",
            type: "button",
            text: "Approve & Resume",
            onClick: async () => {
              try {
                const res = await apiPost(`/api/inbox/${encodeURIComponent(firstApp.id)}/approve`, {});
                await refreshAll();
                showToast(res.resumed ? "Approved — resuming task" : "Approved");
              } catch (e) { showErrorToast(e); }
            },
          }),
          el("button", {
            className: "btn-secondary btn-sm",
            type: "button",
            text: "Dismiss",
            onClick: async () => {
              try {
                await apiPost(`/api/inbox/${encodeURIComponent(firstApp.id)}/dismiss`, {});
                await refreshAll();
                showToast("Dismissed");
              } catch (e) { showErrorToast(e); }
            },
          })
        )
      );
    } else {
      inlineAppBar.classList.add("hidden");
    }

    // Human-readable Details drawer
    const drawer = document.getElementById("detailsDrawer");
    if (appState.detailsOpen && branch) {
      drawer.classList.remove("hidden");
      const ft = branch.frame_tokens || { l0: 0, l1: 0, l2: 0, l3: 0, total: 0 };
      const statsNodes = [
        el("div", { className: "detail-stat" },
          el("div", { className: "detail-stat-label", text: "Prompt Tokens (KV Cache Hit)" }),
          el("div", { className: "detail-stat-val", text: `${branch.prompt_tokens.toLocaleString()} (${branch.cache_hit_pct.toFixed(0)}% cached)` })
        ),
        el("div", { className: "detail-stat" },
          el("div", { className: "detail-stat-label", text: "Context Tokens (Sys / Mem / Hist / Turn)" }),
          el("div", { className: "detail-stat-val", text: `${ft.l0} / ${ft.l1} / ${ft.l2} / ${ft.l3}` })
        ),
        el("div", { className: "detail-stat" },
          el("div", { className: "detail-stat-label", text: "Workspace Isolation" }),
          el("div", { className: "detail-stat-val", text: formatStrategyLabel(branch.workspace_strategy) }),
          branch.workspace_dir ? el("div", { className: "connector-meta", text: branch.workspace_dir }) : null
        ),
      ];
      if (branch.contracts_total > 0) {
        statsNodes.push(
          el("div", { className: "detail-stat" },
            el("div", { className: "detail-stat-label", text: "Verification Contracts" }),
            el("div", { className: "detail-stat-val", text: `${branch.contracts_passed}/${branch.contracts_total} passed` })
          )
        );
      }
      drawer.replaceChildren(el("div", { className: "details-grid" }, ...statsNodes));
    } else {
      drawer.classList.add("hidden");
    }

    // Preserve scroll curiosity before rebuilding feed
    const wasNearBottom = (scroll.scrollHeight - scroll.scrollTop - scroll.clientHeight) < 120;
    const stepCount = branch?.steps?.length || 0;
    const branchChanged = appState.lastRenderedBranchId !== (branch?.id || null);

    const feed = document.getElementById("timeline");
    feed.replaceChildren();

    const pageLineByID = {};
    for (const p of run.pages || []) {
      pageLineByID[p.id] = p.line_count;
    }

    feed.appendChild(
      el("div", { className: "chat-msg chat-msg-user" },
        el("div", { className: "chat-msg-head" },
          el("span", { text: "You" }),
          branches.length > 1 ? el("span", { text: `Version: ${branch?.label || "main"}` }) : null
        ),
        renderMarkdownEl(run.goal)
      )
    );

    if (branch) {
      // Group steps into conversational turns delimited by user_message
      const turns = [];
      let currentTurn = { key: "turn-0", userStep: null, steps: [] };
      turns.push(currentTurn);
      for (const st of branch.steps || []) {
        if (st.step_type === "user_message") {
          currentTurn = { key: `turn-${st.id}`, userStep: st, steps: [] };
          turns.push(currentTurn);
        } else {
          currentTurn.steps.push(st);
        }
      }

      const renderCompactStepTrace = (stepsList, extraReasoning) => {
        const trace = el("div", { className: "thinking-trace" });
        for (const st of stepsList) {
          const tools = st.tool_calls || [];
          const secs = st.latency_ms ? `${(st.latency_ms / 1000).toFixed(1)}s` : "";
          const noteText = (st.final_summary || st.reasoning || "").trim();
          const stepRow = el("div", { className: "thinking-step-row" },
            el("div", { className: "thinking-step-head" },
              el("span", { text: `Step ${st.turn_index}${secs ? " · " + secs : ""}` }),
              el("button", {
                className: "task-row-btn",
                type: "button",
                title: `Branch from Step ${st.turn_index}`,
                text: "⑂",
                onClick: () => forkFromStep(branch.id, st.id, st.turn_index),
              })
            ),
            noteText ? el("div", { className: "thinking-step-note", text: noteText }) : null
          );
          for (const tc of tools) {
            const lineCnt = tc.page_id ? pageLineByID[tc.page_id] : 0;
            const pageBtnLabel = lineCnt ? `Output (${lineCnt}L)` : "Full output";
            stepRow.appendChild(
              el("div", { className: "tool-chip compact" },
                el("div", { className: "tool-chip-head" },
                  el("span", { text: tc.label || tc.name }),
                  tc.page_id
                    ? el("button", {
                        className: "btn-secondary btn-sm",
                        type: "button",
                        text: pageBtnLabel,
                        onClick: () => openPageModal(tc.page_id, 1),
                      })
                    : null
                ),
                tc.observation ? el("pre", { className: "tool-obs", text: tc.observation }) : null
              )
            );
          }
          trace.appendChild(stepRow);
        }
        if (extraReasoning) {
          trace.appendChild(
            el("div", { className: "thinking-step-row" },
              el("div", { className: "thinking-step-head" }, el("span", { text: "Reasoning" })),
              renderMarkdownEl(extraReasoning)
            )
          );
        }
        return trace;
      };

      turns.forEach((turn, idx) => {
        if (turn.userStep) {
          feed.appendChild(
            el("div", { className: "chat-msg chat-msg-user" },
              el("div", { className: "chat-msg-head" }, el("span", { text: "You" })),
              renderMarkdownEl(turn.userStep.reasoning || "")
            )
          );
        }
        const isLastTurn = idx === turns.length - 1;
        const turnSteps = turn.steps;
        const turnIterating = isLastTurn && isRunning;

        if (turnSteps.length === 0 && !turnIterating) return;

        let totalMs = 0;
        let totalTools = 0;
        for (const st of turnSteps) {
          totalMs += Number(st.latency_ms) || 0;
          totalTools += (st.tool_calls || []).length;
        }

        if (turnIterating) {
          const activeLabel = branch.active_tool
            ? `Working · ${branch.active_tool}…`
            : "Thinking…";
          const stats = [];
          if (turnSteps.length > 0) stats.push(`${turnSteps.length} step${turnSteps.length === 1 ? "" : "s"}`);
          if (totalTools > 0) stats.push(`${totalTools} tool${totalTools === 1 ? "" : "s"}`);
          if (totalMs > 0) stats.push(`${(totalMs / 1000).toFixed(1)}s`);
          const accKey = `${branch.id}:${turn.key}:thinking`;
          const det = el("details", { className: "thinking-bar live-thinking" },
            el("summary", null,
              el("span", { className: "thinking-summary-left" },
                el("span", { className: "pulse-dot" }),
                el("span", { text: activeLabel })
              ),
              stats.length ? el("span", { className: "thinking-summary-meta", text: stats.join(" · ") }) : null
            ),
            turnSteps.length > 0
              ? renderCompactStepTrace(turnSteps, "")
              : el("div", { className: "thinking-trace muted caption-text", text: "Preparing first step…" })
          );
          if (appState.openAccordions.has(accKey)) det.open = true;
          det.addEventListener("toggle", () => {
            if (det.open) appState.openAccordions.add(accKey);
            else appState.openAccordions.delete(accKey);
          });
          feed.appendChild(det);
          // Live model output streamed over SSE "delta" events (see connectSSE).
          const live = el("div", { className: "live-stream", text: liveText.get(branch.id) || "" });
          live.dataset.branch = branch.id;
          feed.appendChild(live);
          return;
        }

        const lastStep = turnSteps[turnSteps.length - 1];
        const lastHasReply = Boolean(lastStep && (lastStep.tool_calls || []).length === 0 && (lastStep.final_summary || lastStep.reasoning));
        const workSteps = lastHasReply ? turnSteps.slice(0, -1) : turnSteps;
        const replyStep = lastHasReply ? lastStep : null;
        const extraReasoning =
          replyStep && replyStep.final_summary && replyStep.reasoning && replyStep.reasoning !== replyStep.final_summary
            ? replyStep.reasoning
            : "";

        if (workSteps.length > 0 || extraReasoning) {
          const workParts = [];
          if (totalMs > 0) workParts.push(`Thought for ${(totalMs / 1000).toFixed(1)}s`);
          else workParts.push("Thought process");
          if (workSteps.length > 0) workParts.push(`${workSteps.length} step${workSteps.length === 1 ? "" : "s"}`);
          if (totalTools > 0) workParts.push(`${totalTools} tool${totalTools === 1 ? "" : "s"}`);

          const accKey = `${branch.id}:${turn.key}:thinking`;
          const det = el("details", { className: "thinking-bar" },
            el("summary", null,
              el("span", { className: "thinking-summary-left", text: workParts.join(" · ") })
            ),
            renderCompactStepTrace(workSteps, extraReasoning)
          );
          if (appState.openAccordions.has(accKey)) det.open = true;
          det.addEventListener("toggle", () => {
            if (det.open) appState.openAccordions.add(accKey);
            else appState.openAccordions.delete(accKey);
          });
          feed.appendChild(det);
        }

        if (replyStep) {
          const primaryText = replyStep.final_summary || replyStep.reasoning || "";
          const secs = replyStep.latency_ms ? `${(replyStep.latency_ms / 1000).toFixed(1)}s` : "";
          feed.appendChild(
            el("div", { className: "chat-msg chat-msg-assistant" },
              el("div", { className: "chat-msg-head" },
                el("span", { text: `soma${replyStep.model_id ? " · " + replyStep.model_id : ""}` }),
                el("div", { className: "inline-row" },
                  secs ? el("span", { text: secs }) : null,
                  el("button", {
                    className: "task-row-btn",
                    type: "button",
                    title: `Branch from Step ${replyStep.turn_index}`,
                    text: "⑂",
                    onClick: () => forkFromStep(branch.id, replyStep.id, replyStep.turn_index),
                  })
                )
              ),
              primaryText ? renderMarkdownEl(primaryText) : null
            )
          );
        }
      });
    }

    if (branchChanged || (wasNearBottom && stepCount !== appState.lastStepCount) || isRunning) {
      requestAnimationFrame(() => {
        scroll.scrollTop = scroll.scrollHeight;
      });
    }
    appState.lastRenderedBranchId = branch?.id || null;
    appState.lastStepCount = stepCount;
  }

  async function forkFromStep(branchID, stepID, turnIdx) {
    try {
      const res = await apiPost(`/api/branches/${encodeURIComponent(branchID)}/fork`, {
        step_id: stepID || "",
        label: turnIdx ? `from-step-${turnIdx}` : "",
      });
      appState.selectedBranchId = res.branch_id;
      syncHash();
      await refreshAll();
      showToast(turnIdx ? `Created branch from Step ${turnIdx}` : "Created new branch version");
    } catch (e) {
      showErrorToast(e);
    }
  }

  async function openPageModal(pageID, startLine = 1) {
    const pageSize = 200;
    const start = Math.max(1, Number(startLine) || 1);
    const end = start + pageSize - 1;
    try {
      const pg = await apiGet(`/api/pages/${encodeURIComponent(pageID)}?start=${start}&end=${end}`);
      const totalLines = Number(pg.line_count) || end;
      const shownEnd = Math.min(end, totalLines);
      const hasPrev = start > 1;
      const hasNext = shownEnd < totalLines;
      openModal(`Output Log (Lines ${start}–${shownEnd} of ${totalLines})`, [
        el("div", { className: "inline-row muted caption-text" },
          el("span", { text: `${pg.byte_size?.toLocaleString() || 0} bytes · source: ${pg.trust_tier || "workspace"}` }),
          el("button", { className: "btn-secondary btn-sm", type: "button", text: "Copy Slice", onClick: () => copyToClipboard(pg.content || "", "Output") })
        ),
        el("pre", { className: "code-block", text: pg.content || pg.preview || "" }),
        (hasPrev || hasNext)
          ? el("div", { className: "connector-actions" },
              el("button", {
                className: "btn-secondary btn-sm",
                type: "button",
                text: "← Previous 200 lines",
                disabled: !hasPrev,
                onClick: () => openPageModal(pageID, Math.max(1, start - pageSize)),
              }),
              el("button", {
                className: "btn-secondary btn-sm",
                type: "button",
                text: "Next 200 lines →",
                disabled: !hasNext,
                onClick: () => openPageModal(pageID, start + pageSize),
              })
            )
          : null,
      ], { wide: true, skipDirtyTracking: true });
    } catch (e) {
      showErrorToast(e);
    }
  }

  // Virtual Desktop view
  function renderDesktopView() {
    const desk = appState.harness?.desktop;
    const pill = document.getElementById("desktopStatusPill");
    const controlPill = document.getElementById("desktopControlPill");
    const takeOverBtn = document.getElementById("desktopTakeOverBtn");
    const startBtn = document.getElementById("desktopStartBtn");
    const stopBtn = document.getElementById("desktopStopBtn");
    const popout = document.getElementById("desktopPopoutLink");
    const resSel = document.getElementById("desktopResSelect");
    const netSel = document.getElementById("desktopNetSelect");
    const placeholder = document.getElementById("desktopPlaceholder");
    const frame = document.getElementById("desktopFrame");
    const shield = document.getElementById("desktopShield");
    const meta = document.getElementById("desktopMetaRow");

    if (desk?.running) {
      pill.textContent = `Running · ${desk.resolution || "1280x800"}`;
      pill.className = "pill ok";
      controlPill.classList.remove("hidden");
      controlPill.textContent = appState.desktopTakeOver ? "You Have Control" : "View-Only (Agent Control)";
      controlPill.className = "pill " + (appState.desktopTakeOver ? "warn" : "");
      takeOverBtn.classList.remove("hidden");
      takeOverBtn.textContent = appState.desktopTakeOver ? "Hand Back Control" : "Take Over";
      takeOverBtn.className = appState.desktopTakeOver ? "btn-primary" : "btn-secondary";
      startBtn.classList.add("hidden");
      stopBtn.classList.remove("hidden");
      popout.classList.remove("hidden");
      // Always prefer same-origin proxy_url so Pop Out works over Remote Access / Cloudflare Tunnel too.
      popout.href = desk.proxy_url || desk.novnc_url;
      resSel.disabled = true;
      netSel.disabled = true;
      resSel.title = "Stop desktop first to change resolution";
      netSel.title = "Stop desktop first to change network mode";
      placeholder.classList.add("hidden");
      frame.classList.remove("hidden");
      shield.classList.toggle("hidden", Boolean(appState.desktopTakeOver));
      const targetSrc = desk.proxy_url || desk.novnc_url;
      if (targetSrc && frame.getAttribute("data-src") !== targetSrc) {
        frame.setAttribute("data-src", targetSrc);
        frame.src = targetSrc;
      }
    } else {
      pill.textContent = "Stopped";
      pill.className = "pill";
      controlPill.classList.add("hidden");
      takeOverBtn.classList.add("hidden");
      startBtn.classList.remove("hidden");
      stopBtn.classList.add("hidden");
      popout.classList.add("hidden");
      resSel.disabled = false;
      netSel.disabled = false;
      resSel.title = "Screen resolution";
      netSel.title = "Container network access";
      placeholder.classList.remove("hidden");
      frame.classList.add("hidden");
      shield.classList.add("hidden");
      if (frame.src) {
        frame.removeAttribute("src");
        frame.removeAttribute("data-src");
      }
      meta.textContent = desk?.container_name ? `Container: ${desk.container_name} (${desk.image || "soma-desktop:latest"})` : "";
    }
  }

  // Connectors view (distinct Installed MCPs | Browse Catalog | Channel Bridges subtabs)
  function renderConnectorsView() {
    const har = appState.harness;
    const feed = document.getElementById("connectorsFeed");
    feed.replaceChildren();
    if (!har) {
      feed.appendChild(el("div", { className: "card", text: "Loading connectors…" }));
      return;
    }

    const mcpHeaderActions = document.getElementById("mcpHeaderActions");
    if (mcpHeaderActions) {
      mcpHeaderActions.classList.toggle("hidden", appState.connTab === "bridges");
    }

    const q = (document.getElementById("connectorSearch").value || "").trim().toLowerCase();
    const tab = appState.connTab;

    if (tab === "installed") {
      const installed = (har.mcp_servers || []).filter((m) =>
        !q || m.name.toLowerCase().includes(q) || (m.command || "").toLowerCase().includes(q) || (m.url || "").toLowerCase().includes(q)
      );
      feed.appendChild(
        el("div", { className: "section-header" },
          el("div", null,
            el("h2", { className: "section-title", text: "Installed MCP Servers" }),
            el("div", { className: "section-sub", text: "Click Test Connection to verify a server can start and advertise its tools." })
          ),
          el("span", { className: "section-sub", text: `${installed.length} configured` })
        )
      );
      if (installed.length === 0) {
        feed.appendChild(
          el("div", { className: "card" },
            el("p", { className: "connector-desc", text: "No MCP servers installed yet. Browse one-click presets or add a custom MCP server." }),
            el("div", { className: "connector-actions" },
              el("button", {
                className: "btn-primary btn-sm",
                type: "button",
                text: "Browse Preset Catalog →",
                onClick: () => {
                  appState.connTab = "presets";
                  syncViewDOM();
                  syncHash();
                  renderConnectorsView();
                },
              })
            )
          )
        );
      } else {
        const grid = el("div", { className: "card-grid-2" });
        for (const m of installed) {
          const endpointText = m.transport === "stdio" ? `${m.command} ${(m.args || []).join(" ")}`.trim() : m.url;
          grid.appendChild(
            el("div", { className: "card connector-card" },
              el("div", { className: "connector-info" },
                el("div", { className: "connector-title-row" },
                  el("span", { className: "status-dot " + (m.enabled ? "ok" : "") }),
                  el("span", { className: "connector-name", text: m.name }),
                  el("span", { className: "pill " + (m.enabled ? "ok" : ""), text: m.enabled ? "Enabled" : "Disabled" }),
                  el("span", { className: "pill", text: m.transport }),
                  m.oauth_enabled ? el("span", { className: "pill " + (m.oauth_connected ? "ok" : "warn"), text: m.oauth_connected ? "OAuth Authorized" : "OAuth Sign-In Needed" }) : null,
                  (m.include_tools?.length || m.exclude_tools?.length) ? el("span", { className: "pill", text: "Filtered" }) : null,
                  m.isolated_desktop ? el("span", { className: "pill ok", text: "Virtual Desktop" }) : null
                ),
                el("div", { className: "connector-meta", text: endpointText })
              ),
              el("div", { className: "connector-actions" },
                m.oauth_enabled ? el("button", {
                  className: "btn-primary btn-sm",
                  type: "button",
                  text: m.oauth_connected ? "Re-auth OAuth" : "Sign in (OAuth)",
                  onClick: async () => {
                    try {
                      const res = await apiPost(`/api/connectors/mcp/${encodeURIComponent(m.name)}/oauth/start`, {});
                      if (res.auth_url) {
                        window.open(res.auth_url, "_blank", "noopener");
                        showToast("Complete OAuth login in the opened tab");
                      }
                    } catch (e) { showErrorToast(e); }
                  },
                }) : null,
                (m.oauth_enabled && m.oauth_connected) ? el("button", {
                  className: "btn-secondary btn-sm",
                  type: "button",
                  text: "Revoke OAuth",
                  onClick: () => confirmModal(
                    `Revoke OAuth (${m.name})`,
                    `Disconnect saved OAuth credentials for ${m.name}?`,
                    "Revoke OAuth",
                    async () => {
                      appState.harness = await apiPost(`/api/connectors/mcp/${encodeURIComponent(m.name)}/oauth/logout`, {});
                      renderConnectorsView();
                      showToast(`Revoked OAuth token for ${m.name}`);
                    }
                  ),
                }) : null,
                el("button", {
                  className: "btn-secondary btn-sm",
                  type: "button",
                  text: "Test",
                  onClick: async (ev) => {
                    const btn = ev.currentTarget;
                    btn.disabled = true;
                    btn.textContent = "Testing…";
                    try {
                      const res = await apiPost(`/api/connectors/mcp/${encodeURIComponent(m.name)}/test`, {});
                      if (res.ok) {
                        const tools = res.tools || [];
                        const resources = res.resources || [];
                        const prompts = res.prompts || [];
                        openModal(`MCP Connection Verified: ${m.name}`, [
                          el("div", { className: "pill ok", text: `Connected (${tools.length} tools, ${resources.length} resources, ${prompts.length} prompts)` }),
                          el("pre", { className: "code-block", text: tools.join("\n") || "(no tools advertised)" }),
                          resources.length ? el("pre", { className: "code-block", text: "Resources:\n" + resources.join("\n") }) : null,
                          prompts.length ? el("pre", { className: "code-block", text: "Prompts:\n" + prompts.join("\n") }) : null,
                        ], { skipDirtyTracking: true });
                      } else {
                        openModal(`MCP Connection Failed: ${m.name}`, [
                          el("div", { className: "modal-error", text: res.error || "Unknown connection error" }),
                          el("p", { className: "connector-desc", text: "Verify the command/URL is reachable and required API tokens or packages (e.g. npx) are installed on the host." }),
                        ], { skipDirtyTracking: true });
                      }
                    } catch (e) {
                      showErrorToast(e);
                    } finally {
                      btn.disabled = false;
                      btn.textContent = "Test";
                    }
                  },
                }),
                el("button", {
                  className: "btn-secondary btn-sm",
                  type: "button",
                  text: "Edit",
                  onClick: () => openCustomMCPModal(m),
                }),
                el("button", {
                  className: "btn-secondary btn-sm",
                  type: "button",
                  text: m.enabled ? "Disable" : "Enable",
                  onClick: async () => {
                    try {
                      appState.harness = await apiPost(`/api/connectors/mcp/${encodeURIComponent(m.name)}/toggle`, { enabled: !m.enabled });
                      renderChrome();
                      renderConnectorsView();
                      showToast(`${m.name} ${!m.enabled ? "enabled" : "disabled"}`);
                    } catch (e) { showErrorToast(e); }
                  },
                }),
                el("button", {
                  className: "btn-danger btn-sm",
                  type: "button",
                  text: "Remove",
                  onClick: () => confirmModal(
                    `Remove ${m.name}`,
                    `Remove MCP connector "${m.name}" from your configuration?`,
                    "Remove Connector",
                    async () => {
                      appState.harness = await apiPost(`/api/connectors/mcp/${encodeURIComponent(m.name)}/delete`, {});
                      renderChrome();
                      renderConnectorsView();
                      showToast(`Removed ${m.name}`);
                    }
                  ),
                })
              )
            )
          );
        }
        feed.appendChild(grid);
      }
      return;
    }

    if (tab === "presets") {
      const allPresets = har.presets || [];
      const categories = ["All", ...Array.from(new Set(allPresets.map((p) => p.category || "Connectors"))).sort()];
      if (!categories.includes(appState.presetCategory)) appState.presetCategory = "All";
      const presets = allPresets.filter((p) =>
        (appState.presetCategory === "All" || (p.category || "Connectors") === appState.presetCategory) &&
        (!q || p.name.toLowerCase().includes(q) || p.description.toLowerCase().includes(q) || p.category.toLowerCase().includes(q))
      );
      feed.appendChild(
        el("div", { className: "section-header" },
          el("div", null,
            el("h2", { className: "section-title", text: "One-Click MCP Catalog" }),
            el("div", { className: "section-sub", text: "Pre-configured browser, developer, cloud, and productivity MCP connectors." })
          ),
          el("span", { className: "section-sub", text: `${presets.length} of ${allPresets.length} connectors` })
        )
      );
      const catBar = el("div", { className: "subtabs subtabs-wrap" });
      for (const cat of categories) {
        catBar.appendChild(el("button", {
          className: "subtab" + (appState.presetCategory === cat ? " active" : ""),
          type: "button",
          text: cat,
          onClick: () => { appState.presetCategory = cat; renderConnectorsView(); },
        }));
      }
      feed.appendChild(catBar);

      const pGrid = el("div", { className: "card-grid-2" });
      for (const p of presets) {
        pGrid.appendChild(
          el("div", { className: "card connector-card" },
            el("div", { className: "connector-info" },
              el("div", { className: "connector-title-row" },
                el("span", { className: "connector-name", text: p.name }),
                el("span", { className: "pill", text: p.category }),
                p.oauth_enabled ? el("span", { className: "pill ok", text: "OAuth 2.1" }) : null
              ),
              el("p", { className: "connector-desc", text: p.description + (p.required_env?.length ? ` (Requires: ${p.required_env.join(", ")})` : "") })
            ),
            el("div", { className: "connector-actions" },
              p.installed
                ? el("span", { className: "pill ok", text: "Installed" })
                : el("button", {
                    className: "btn-primary btn-sm",
                    type: "button",
                    text: "+ Install",
                    onClick: async () => {
                      if (p.required_env && p.required_env.length > 0) {
                        const inputs = {};
                        const fields = p.required_env.map((k) => {
                          const inp = el("input", { className: "field-input", type: "password", placeholder: `Paste ${k} (or leave blank to read host env)` });
                          inputs[k] = inp;
                          return el("div", { className: "field full" }, el("span", { className: "field-label", text: k }), inp);
                        });
                        openModal(`Install ${p.name}`, [
                          el("p", { className: "connector-desc", text: p.description }),
                          el("div", { className: "form-grid" }, ...fields),
                          el("div", { className: "connector-actions" },
                            el("button", { className: "btn-secondary", type: "button", text: "Cancel", onClick: () => closeModal(false) }),
                            el("button", {
                              className: "btn-primary",
                              type: "button",
                              text: "Install Connector",
                              onClick: async () => {
                                try {
                                  const env = {};
                                  for (const k of p.required_env) {
                                    const val = inputs[k].value.trim();
                                    env[k] = val || `\${${k}}`;
                                  }
                                  appState.harness = await apiPost("/api/connectors/mcp", { preset_id: p.id, env });
                                  closeModal(true);
                                  appState.connTab = "installed";
                                  syncViewDOM();
                                  renderChrome();
                                  renderConnectorsView();
                                  showToast(`Installed ${p.name}`);
                                } catch (e) { showModalError(e.message); }
                              },
                            })
                          ),
                        ]);
                        return;
                      }
                      try {
                        appState.harness = await apiPost("/api/connectors/mcp", { preset_id: p.id });
                        renderChrome();
                        renderConnectorsView();
                        showToast(`Installed ${p.name}`);
                      } catch (e) { showErrorToast(e); }
                    },
                  })
            )
          )
        );
      }
      feed.appendChild(pGrid);
      return;
    }

    // Channel Bridges & DM Pairing
    if (tab === "bridges") {
      const br = har.bridges || {};
      const brRunning = har.bridge_running || {};
      const brErrors = har.bridge_errors || {};
      const splitCSV = (v) => v.split(",").map((s) => s.trim()).filter(Boolean);
      const splitNums = (v) => v.split(",").map((s) => Number(s.trim())).filter((n) => !Number.isNaN(n) && n !== 0);

      const baseBridgesPayload = (curBr) => {
        const d0 = curBr.discord || {};
        const tg0 = curBr.telegram || {};
        const sl0 = curBr.slack || {};
        const mx0 = curBr.matrix || {};
        const mm0 = curBr.mattermost || {};
        const tm0 = curBr.teams || {};
        const gc0 = curBr.google_chat || {};
        const em0 = curBr.email || {};
        const mkSide = (sc = {}) => ({
          enabled: !!sc.enabled,
          outbound_url: sc.outbound_url || "",
          bearer_token: "",
          hmac_secret: "",
          allowed_senders: sc.allowed_senders || [],
        });
        return {
          discord: {
            enabled: !!d0.enabled,
            bot_token: "",
            public_key_hex: d0.public_key_hex || "",
            channel_id: d0.channel_id || "",
            allowed_user_ids: d0.allowed_user_ids || [],
            allowed_channel_ids: d0.allowed_channel_ids || [],
            listen_addr: d0.listen_addr || "",
          },
          telegram: {
            enabled: !!tg0.enabled,
            bot_token: "",
            alert_chat_id: Number(tg0.alert_chat_id) || 0,
            allowed_user_ids: tg0.allowed_user_ids || [],
            allowed_chat_ids: tg0.allowed_chat_ids || [],
          },
          slack: {
            enabled: !!sl0.enabled,
            bot_token: "",
            signing_secret: "",
            allowed_user_ids: sl0.allowed_user_ids || [],
            allowed_channel_ids: sl0.allowed_channel_ids || [],
          },
          matrix: {
            enabled: !!mx0.enabled,
            homeserver_url: mx0.homeserver_url || "",
            access_token: "",
            user_id: mx0.user_id || "",
            allowed_senders: mx0.allowed_senders || [],
            allowed_rooms: mx0.allowed_rooms || [],
          },
          mattermost: {
            enabled: !!mm0.enabled,
            server_url: mm0.server_url || "",
            bot_token: "",
            webhook_url: mm0.webhook_url || "",
            outgoing_token: "",
            allowed_user_ids: mm0.allowed_user_ids || [],
          },
          teams: {
            enabled: !!tm0.enabled,
            webhook_url: tm0.webhook_url || "",
            allowed_user_ids: tm0.allowed_user_ids || [],
          },
          google_chat: {
            enabled: !!gc0.enabled,
            webhook_url: gc0.webhook_url || "",
            allowed_user_ids: gc0.allowed_user_ids || [],
          },
          email: {
            enabled: !!em0.enabled,
            smtp_host: em0.smtp_host || "",
            smtp_port: Number(em0.smtp_port) || 587,
            from: em0.from || "",
            username: em0.username || "",
            password: "",
          },
          whatsapp: mkSide(curBr.whatsapp),
          signal: mkSide(curBr.signal),
          sms: mkSide(curBr.sms),
        };
      };

      const saveSingleBridge = async (key, sectionPayload, toastText) => {
        const payload = baseBridgesPayload(appState.harness?.bridges || {});
        payload[key] = sectionPayload;
        appState.harness = await apiPost("/api/connectors/bridges", payload);
        renderChrome();
        renderConnectorsView();
        showToast(toastText || "Bridge settings saved");
      };

      const webhookBase = window.location.origin;

      const BRIDGE_CHANNELS = [
        {
          key: "telegram",
          name: "Telegram",
          desc: "Long-polling bot bridge — works locally with zero port forwarding.",
          isConfigured: (c) => !!(c.enabled || c.has_bot_token || c.alert_chat_id),
          summary: (c) => [c.has_bot_token ? `Token ${c.bot_token_masked || "saved"}` : "No token", c.alert_chat_id ? `Alert chat ${c.alert_chat_id}` : ""].filter(Boolean).join(" · "),
          buildForm: (c) => {
            const firstSetup = !(c.enabled || c.has_bot_token || c.alert_chat_id);
            const enabled = el("input", { type: "checkbox", checked: firstSetup ? true : !!c.enabled });
            const token = el("input", { className: "field-input", type: "password", placeholder: c.has_bot_token ? `Saved (${c.bot_token_masked})` : "Paste token from @BotFather" });
            const alertId = el("input", { className: "field-input", type: "number", value: c.alert_chat_id || "", placeholder: "Optional chat ID for approval alerts" });
            const users = el("input", { className: "field-input", type: "text", value: (c.allowed_user_ids || []).join(", "), placeholder: "Optional (or pair via DM code)" });
            return {
              nodes: [
                el("label", { className: "checkbox-row" }, enabled, el("span", { text: "Enable Telegram bridge in soma web" })),
                el("div", { className: "form-grid" },
                  el("div", { className: "field" }, el("span", { className: "field-label", text: "Bot Token" }), token),
                  el("div", { className: "field" }, el("span", { className: "field-label", text: "Alert Chat ID (optional)" }), alertId),
                  el("div", { className: "field full" }, el("span", { className: "field-label", text: "Allowed Numeric User IDs (optional if using DM Pairing)" }), users)
                ),
              ],
              collect: () => ({
                enabled: enabled.checked,
                bot_token: token.value.trim(),
                alert_chat_id: Number(alertId.value) || 0,
                allowed_user_ids: splitNums(users.value),
                allowed_chat_ids: c.allowed_chat_ids || [],
              }),
            };
          },
        },
        {
          key: "discord",
          name: "Discord",
          webhookPath: "/webhooks/bridges/discord",
          desc: "Slash interactions webhook & inbox channel alerts.",
          isConfigured: (c) => !!(c.enabled || c.has_bot_token || c.channel_id || c.public_key_hex),
          summary: (c) => [c.has_bot_token ? `Token ${c.bot_token_masked || "saved"}` : "No token", c.channel_id ? `Channel ${c.channel_id}` : ""].filter(Boolean).join(" · "),
          buildForm: (c) => {
            const enabled = el("input", { type: "checkbox", checked: c.enabled !== undefined && c.has_bot_token ? !!c.enabled : !!(c.enabled ?? true) });
            const token = el("input", { className: "field-input", type: "password", placeholder: c.has_bot_token ? `Saved (${c.bot_token_masked})` : "Discord Bot Token" });
            const pubKey = el("input", { className: "field-input", type: "text", value: c.public_key_hex || "", placeholder: "Ed25519 Public Key (hex)" });
            const chId = el("input", { className: "field-input", type: "text", value: c.channel_id || "", placeholder: "Alert Channel ID" });
            const users = el("input", { className: "field-input", type: "text", value: (c.allowed_user_ids || []).join(", "), placeholder: "Comma-separated Discord user IDs" });
            return {
              nodes: [
                el("label", { className: "checkbox-row" }, enabled, el("span", { text: "Enable Discord bridge" })),
                el("div", { className: "form-grid" },
                  el("div", { className: "field" }, el("span", { className: "field-label", text: "Bot Token" }), token),
                  el("div", { className: "field" }, el("span", { className: "field-label", text: "Public Key (Ed25519 Hex)" }), pubKey),
                  el("div", { className: "field" }, el("span", { className: "field-label", text: "Alert Channel ID" }), chId),
                  el("div", { className: "field" }, el("span", { className: "field-label", text: "Allowed User IDs" }), users)
                ),
              ],
              collect: () => ({
                enabled: enabled.checked,
                bot_token: token.value.trim(),
                public_key_hex: pubKey.value.trim(),
                channel_id: chId.value.trim(),
                allowed_user_ids: splitCSV(users.value),
                allowed_channel_ids: c.allowed_channel_ids || [],
                listen_addr: c.listen_addr || "",
              }),
            };
          },
        },
        {
          key: "slack",
          name: "Slack",
          webhookPath: "/webhooks/bridges/slack",
          desc: "Events API webhook & bot replies with DM pairing.",
          isConfigured: (c) => !!(c.enabled || c.has_bot_token || c.has_signing_secret || (c.allowed_channel_ids || []).length),
          summary: (c) => [c.has_bot_token ? `Token ${c.bot_token_masked || "saved"}` : "No token", (c.allowed_channel_ids || []).length ? `${c.allowed_channel_ids.length} channel(s)` : ""].filter(Boolean).join(" · "),
          buildForm: (c) => {
            const firstSetup = !(c.enabled || c.has_bot_token || c.has_signing_secret);
            const enabled = el("input", { type: "checkbox", checked: firstSetup ? true : !!c.enabled });
            const token = el("input", { className: "field-input", type: "password", placeholder: c.has_bot_token ? `Saved (${c.bot_token_masked})` : "xoxb-..." });
            const secret = el("input", { className: "field-input", type: "password", placeholder: c.has_signing_secret ? "Saved (leave blank to keep)" : "Signing Secret" });
            const users = el("input", { className: "field-input", type: "text", value: (c.allowed_user_ids || []).join(", "), placeholder: "U123, U456 (optional if using DM Pairing)" });
            const channels = el("input", { className: "field-input", type: "text", value: (c.allowed_channel_ids || []).join(", "), placeholder: "C123, D456" });
            return {
              nodes: [
                el("label", { className: "checkbox-row" }, enabled, el("span", { text: "Enable Slack bridge" })),
                el("div", { className: "form-grid" },
                  el("div", { className: "field" }, el("span", { className: "field-label", text: "Bot Token" }), token),
                  el("div", { className: "field" }, el("span", { className: "field-label", text: "Signing Secret" }), secret),
                  el("div", { className: "field" }, el("span", { className: "field-label", text: "Allowed User IDs" }), users),
                  el("div", { className: "field" }, el("span", { className: "field-label", text: "Allowed Channel IDs" }), channels)
                ),
              ],
              collect: () => ({
                enabled: enabled.checked,
                bot_token: token.value.trim(),
                signing_secret: secret.value.trim(),
                allowed_user_ids: splitCSV(users.value),
                allowed_channel_ids: splitCSV(channels.value),
              }),
            };
          },
        },
        {
          key: "matrix",
          name: "Matrix",
          desc: "Homeserver /sync listener — works locally with zero port forwarding.",
          isConfigured: (c) => !!(c.enabled || c.has_access_token || c.homeserver_url),
          summary: (c) => [c.homeserver_url, c.user_id].filter(Boolean).join(" · ") || "Access token saved",
          buildForm: (c) => {
            const firstSetup = !(c.enabled || c.has_access_token || c.homeserver_url);
            const enabled = el("input", { type: "checkbox", checked: firstSetup ? true : !!c.enabled });
            const hs = el("input", { className: "field-input", type: "text", value: c.homeserver_url || "", placeholder: "https://matrix.org" });
            const tok = el("input", { className: "field-input", type: "password", placeholder: c.has_access_token ? "Saved (leave blank to keep)" : "Access Token" });
            const uid = el("input", { className: "field-input", type: "text", value: c.user_id || "", placeholder: "@soma:matrix.org" });
            const senders = el("input", { className: "field-input", type: "text", value: (c.allowed_senders || []).join(", "), placeholder: "@alice:matrix.org" });
            return {
              nodes: [
                el("label", { className: "checkbox-row" }, enabled, el("span", { text: "Enable Matrix bridge" })),
                el("div", { className: "form-grid" },
                  el("div", { className: "field" }, el("span", { className: "field-label", text: "Homeserver URL" }), hs),
                  el("div", { className: "field" }, el("span", { className: "field-label", text: "Access Token" }), tok),
                  el("div", { className: "field" }, el("span", { className: "field-label", text: "Bot User ID" }), uid),
                  el("div", { className: "field" }, el("span", { className: "field-label", text: "Allowed Senders" }), senders)
                ),
              ],
              collect: () => ({
                enabled: enabled.checked,
                homeserver_url: hs.value.trim(),
                access_token: tok.value.trim(),
                user_id: uid.value.trim(),
                allowed_senders: splitCSV(senders.value),
                allowed_rooms: c.allowed_rooms || [],
              }),
            };
          },
        },
        {
          key: "mattermost",
          name: "Mattermost",
          webhookPath: "/webhooks/bridges/mattermost",
          desc: "Self-hosted server URL, bot token & webhook integration.",
          isConfigured: (c) => !!(c.enabled || c.has_bot_token || c.server_url || c.webhook_url),
          summary: (c) => [c.server_url, c.webhook_url ? "Webhook configured" : ""].filter(Boolean).join(" · ") || "Token saved",
          buildForm: (c) => {
            const firstSetup = !(c.enabled || c.has_bot_token || c.server_url || c.webhook_url);
            const enabled = el("input", { type: "checkbox", checked: firstSetup ? true : !!c.enabled });
            const srv = el("input", { className: "field-input", type: "text", value: c.server_url || "", placeholder: "https://mattermost.example.com" });
            const tok = el("input", { className: "field-input", type: "password", placeholder: c.has_bot_token ? "Saved (leave blank to keep)" : "Bot Token" });
            const hook = el("input", { className: "field-input", type: "text", value: c.webhook_url || "", placeholder: "Incoming Webhook URL (optional)" });
            const users = el("input", { className: "field-input", type: "text", value: (c.allowed_user_ids || []).join(", "), placeholder: "Allowed user IDs" });
            return {
              nodes: [
                el("label", { className: "checkbox-row" }, enabled, el("span", { text: "Enable Mattermost bridge" })),
                el("div", { className: "form-grid" },
                  el("div", { className: "field" }, el("span", { className: "field-label", text: "Server URL" }), srv),
                  el("div", { className: "field" }, el("span", { className: "field-label", text: "Bot Token" }), tok),
                  el("div", { className: "field" }, el("span", { className: "field-label", text: "Webhook URL" }), hook),
                  el("div", { className: "field" }, el("span", { className: "field-label", text: "Allowed User IDs" }), users)
                ),
              ],
              collect: () => ({
                enabled: enabled.checked,
                server_url: srv.value.trim(),
                bot_token: tok.value.trim(),
                webhook_url: hook.value.trim(),
                allowed_user_ids: splitCSV(users.value),
              }),
            };
          },
        },
        {
          key: "teams",
          name: "Microsoft Teams",
          desc: "Outbound Teams webhook notifications for automations & alerts.",
          isConfigured: (c) => !!(c.enabled || c.webhook_url),
          summary: (c) => c.webhook_url ? "Webhook configured" : "Enabled",
          buildForm: (c) => {
            const firstSetup = !(c.enabled || c.webhook_url);
            const enabled = el("input", { type: "checkbox", checked: firstSetup ? true : !!c.enabled });
            const hook = el("input", { className: "field-input", type: "text", value: c.webhook_url || "", placeholder: "Teams Incoming Webhook URL" });
            const users = el("input", { className: "field-input", type: "text", value: (c.allowed_user_ids || []).join(", "), placeholder: "Allowed user IDs" });
            return {
              nodes: [
                el("label", { className: "checkbox-row" }, enabled, el("span", { text: "Enable Teams notifications" })),
                el("div", { className: "form-grid" },
                  el("div", { className: "field full" }, el("span", { className: "field-label", text: "Incoming Webhook URL" }), hook),
                  el("div", { className: "field full" }, el("span", { className: "field-label", text: "Allowed User IDs" }), users)
                ),
              ],
              collect: () => ({ enabled: enabled.checked, webhook_url: hook.value.trim(), allowed_user_ids: splitCSV(users.value) }),
            };
          },
        },
        {
          key: "google_chat",
          name: "Google Chat",
          webhookPath: "/webhooks/bridges/google-chat",
          desc: "Space webhook & synchronous event replies.",
          isConfigured: (c) => !!(c.enabled || c.webhook_url),
          summary: (c) => c.webhook_url ? "Webhook configured" : "Enabled",
          buildForm: (c) => {
            const firstSetup = !(c.enabled || c.webhook_url);
            const enabled = el("input", { type: "checkbox", checked: firstSetup ? true : !!c.enabled });
            const hook = el("input", { className: "field-input", type: "text", value: c.webhook_url || "", placeholder: "Google Chat Space Webhook URL" });
            const users = el("input", { className: "field-input", type: "text", value: (c.allowed_user_ids || []).join(", "), placeholder: "users/123..." });
            return {
              nodes: [
                el("label", { className: "checkbox-row" }, enabled, el("span", { text: "Enable Google Chat" })),
                el("div", { className: "form-grid" },
                  el("div", { className: "field full" }, el("span", { className: "field-label", text: "Space Webhook URL" }), hook),
                  el("div", { className: "field full" }, el("span", { className: "field-label", text: "Allowed User IDs" }), users)
                ),
              ],
              collect: () => ({ enabled: enabled.checked, webhook_url: hook.value.trim(), allowed_user_ids: splitCSV(users.value) }),
            };
          },
        },
        {
          key: "email",
          name: "Email (SMTP)",
          desc: "Outbound SMTP notifications for automations & approvals.",
          isConfigured: (c) => !!(c.enabled || c.smtp_host || c.from || c.has_password),
          summary: (c) => [c.smtp_host ? `${c.smtp_host}:${c.smtp_port || 587}` : "", c.from].filter(Boolean).join(" · ") || "SMTP configured",
          buildForm: (c) => {
            const firstSetup = !(c.enabled || c.smtp_host || c.from || c.has_password);
            const enabled = el("input", { type: "checkbox", checked: firstSetup ? true : !!c.enabled });
            const host = el("input", { className: "field-input", type: "text", value: c.smtp_host || "", placeholder: "smtp.example.com" });
            const port = el("input", { className: "field-input", type: "number", value: c.smtp_port || 587, placeholder: "587" });
            const from = el("input", { className: "field-input", type: "text", value: c.from || "", placeholder: "soma@example.com" });
            const user = el("input", { className: "field-input", type: "text", value: c.username || "", placeholder: "SMTP Username" });
            const pass = el("input", { className: "field-input", type: "password", placeholder: c.has_password ? "Saved (leave blank to keep)" : "SMTP Password" });
            return {
              nodes: [
                el("label", { className: "checkbox-row" }, enabled, el("span", { text: "Enable Email notifications" })),
                el("div", { className: "form-grid" },
                  el("div", { className: "field" }, el("span", { className: "field-label", text: "SMTP Host" }), host),
                  el("div", { className: "field" }, el("span", { className: "field-label", text: "SMTP Port" }), port),
                  el("div", { className: "field" }, el("span", { className: "field-label", text: "From Address" }), from),
                  el("div", { className: "field" }, el("span", { className: "field-label", text: "Username" }), user),
                  el("div", { className: "field full" }, el("span", { className: "field-label", text: "Password" }), pass)
                ),
              ],
              collect: () => ({
                enabled: enabled.checked,
                smtp_host: host.value.trim(),
                smtp_port: Number(port.value) || 587,
                from: from.value.trim(),
                username: user.value.trim(),
                password: pass.value.trim(),
              }),
            };
          },
        },
        ...["whatsapp", "signal", "sms"].map((k) => {
          const label = k === "whatsapp" ? "WhatsApp Sidecar" : k === "signal" ? "Signal Sidecar" : "SMS Sidecar";
          return {
            key: k,
            name: label,
            webhookPath: `/webhooks/bridges/${k}`,
            desc: `${label.replace(" Sidecar", "")} webhook relay with HMAC verification & DM pairing.`,
            isConfigured: (c) => !!(c.enabled || c.outbound_url || c.has_bearer_token),
            summary: (c) => c.outbound_url || "Sidecar configured",
            buildForm: (c) => {
              const firstSetup = !(c.enabled || c.outbound_url || c.has_bearer_token);
              const enabled = el("input", { type: "checkbox", checked: firstSetup ? true : !!c.enabled });
              const url = el("input", { className: "field-input", type: "text", value: c.outbound_url || "", placeholder: "http://127.0.0.1:9001/send" });
              const bearer = el("input", { className: "field-input", type: "password", placeholder: c.has_bearer_token ? "Saved (leave blank to keep)" : "Bearer Token (optional)" });
              const hmac = el("input", { className: "field-input", type: "password", placeholder: c.has_hmac_secret ? "Saved (leave blank to keep)" : "HMAC Secret (optional)" });
              const senders = el("input", { className: "field-input", type: "text", value: (c.allowed_senders || []).join(", "), placeholder: "+15551234567" });
              return {
                nodes: [
                  el("label", { className: "checkbox-row" }, enabled, el("span", { text: `Enable ${label}` })),
                  el("div", { className: "form-grid" },
                    el("div", { className: "field full" }, el("span", { className: "field-label", text: "Sidecar Outbound URL" }), url),
                    el("div", { className: "field" }, el("span", { className: "field-label", text: "Bearer Token" }), bearer),
                    el("div", { className: "field" }, el("span", { className: "field-label", text: "HMAC Secret" }), hmac),
                    el("div", { className: "field full" }, el("span", { className: "field-label", text: "Allowed Senders" }), senders)
                  ),
                ],
                collect: () => ({
                  enabled: enabled.checked,
                  outbound_url: url.value.trim(),
                  bearer_token: bearer.value.trim(),
                  hmac_secret: hmac.value.trim(),
                  allowed_senders: splitCSV(senders.value),
                }),
              };
            },
          };
        }),
      ];

      const openBridgeConfigModal = (ch) => {
        const cur = (appState.harness?.bridges || {})[ch.key] || {};
        const form = ch.buildForm(cur);
        const hookUrl = ch.webhookPath ? `${har.tunnel?.public_url || webhookBase}${ch.webhookPath}` : "";
        openModal(`${ch.isConfigured(cur) ? "Configure" : "Connect"} ${ch.name}`, [
          el("p", { className: "connector-desc", text: ch.desc }),
          hookUrl
            ? el("div", { className: "info-banner inline-row" },
                el("span", { text: `Inbound Webhook URL: ${hookUrl}` }),
                el("button", { className: "btn-secondary btn-sm", type: "button", text: "Copy URL", onClick: () => copyToClipboard(hookUrl, "Webhook URL") })
              )
            : null,
          ...form.nodes,
          el("div", { className: "connector-actions" },
            el("button", { className: "btn-secondary", type: "button", text: "Cancel", onClick: () => closeModal(false) }),
            el("button", {
              className: "btn-primary",
              type: "button",
              text: `Save ${ch.name}`,
              onClick: async () => {
                try {
                  await saveSingleBridge(ch.key, form.collect(), `${ch.name} bridge saved`);
                  closeModal(true);
                } catch (e) { showModalError(e.message); }
              },
            })
          ),
        ]);
      };

      const configuredChannels = BRIDGE_CHANNELS.filter((ch) => ch.isConfigured(br[ch.key] || {}));
      const availableChannels = BRIDGE_CHANNELS.filter((ch) =>
        !ch.isConfigured(br[ch.key] || {}) &&
        (!q || ch.name.toLowerCase().includes(q) || ch.desc.toLowerCase().includes(q))
      );

      feed.appendChild(
        el("div", { className: "section-header" },
          el("div", null,
            el("h2", { className: "section-title", text: "Configured Channel Bridges" }),
            el("div", { className: "section-sub", text: "Enabled bridges are supervised directly inside soma web (and shared with `soma bridge start`)." })
          ),
          el("span", { className: "section-sub", text: `${configuredChannels.filter((ch) => (br[ch.key] || {}).enabled).length} active` })
        )
      );

      if (configuredChannels.length === 0) {
        feed.appendChild(el("div", { className: "card muted", text: "No channel bridges configured yet. Choose Telegram, Discord, Slack, Matrix, Email, or another channel below." }));
      } else {
        const cfgGrid = el("div", { className: "card-grid-2" });
        for (const ch of configuredChannels) {
          const cur = br[ch.key] || {};
          const isRun = Boolean(brRunning[ch.key]);
          const runErr = brErrors[ch.key] || "";
          cfgGrid.appendChild(
            el("div", { className: "card connector-card" },
              el("div", { className: "connector-info" },
                el("div", { className: "connector-title-row" },
                  el("span", { className: "status-dot " + (cur.enabled ? (runErr ? "err" : "ok") : "") }),
                  el("span", { className: "connector-name", text: ch.name }),
                  el("span", { className: "pill " + (cur.enabled ? "ok" : ""), text: cur.enabled ? (isRun ? "Running" : "Enabled") : "Disabled" }),
                  runErr ? el("span", { className: "pill err", text: "Error" }) : null
                ),
                el("p", { className: "connector-desc", text: ch.summary(cur) || ch.desc }),
                runErr ? el("div", { className: "connector-meta", text: runErr }) : null
              ),
              el("div", { className: "connector-actions" },
                el("button", {
                  className: "btn-secondary btn-sm",
                  type: "button",
                  text: cur.enabled ? "Disable" : "Enable",
                  onClick: async () => {
                    try {
                      const base = baseBridgesPayload(appState.harness?.bridges || {});
                      await saveSingleBridge(ch.key, { ...base[ch.key], enabled: !cur.enabled }, `${ch.name} ${!cur.enabled ? "enabled" : "disabled"}`);
                    } catch (e) { showErrorToast(e); }
                  },
                }),
                el("button", {
                  className: "btn-primary btn-sm",
                  type: "button",
                  text: "Edit",
                  onClick: () => openBridgeConfigModal(ch),
                }),
                el("button", {
                  className: "btn-danger btn-sm",
                  type: "button",
                  text: "Remove",
                  onClick: () => confirmModal(
                    `Remove ${ch.name} Bridge`,
                    `Clear all saved credentials and settings for ${ch.name}?`,
                    "Remove Bridge",
                    async () => {
                      appState.harness = await apiPost("/api/connectors/bridges", { reset_channel: ch.key });
                      renderChrome();
                      renderConnectorsView();
                      showToast(`Removed ${ch.name} bridge`);
                    }
                  ),
                })
              )
            )
          );
        }
        feed.appendChild(cfgGrid);
      }

      // DM Pairing Section (placed clearly with 3-step onboarding explanation)
      feed.appendChild(
        el("div", { className: "section-header card-spaced" },
          el("div", null,
            el("h2", { className: "section-title", text: "DM Pairing & Sender Access" }),
            el("div", { className: "section-sub", text: "How pairing works: 1. Enable a bridge · 2. Send a DM to your bot to receive a one-time code · 3. Approve the code below." })
          )
        )
      );

      const pairing = har.pairing || { approved: [], pending: [] };
      const pairCodeIn = el("input", { className: "field-input", type: "text", placeholder: "Enter pairing code from bot DM (e.g. ABCD-1234)" });
      const pairingCard = el("div", { className: "card" },
        el("div", { className: "connector-title-row" },
          el("span", { className: "connector-name", text: "Paired Senders" }),
          el("span", { className: "pill " + ((pairing.pending || []).length ? "warn" : ""), text: `${(pairing.pending || []).length} pending` }),
          el("span", { className: "pill ok", text: `${(pairing.approved || []).length} approved` })
        ),
        el("div", { className: "form-grid" },
          el("div", { className: "field" }, pairCodeIn),
          el("div", { className: "connector-actions" },
            el("button", {
              className: "btn-primary btn-sm",
              type: "button",
              text: "Approve Code",
              onClick: async () => {
                if (!pairCodeIn.value.trim()) return;
                try {
                  appState.harness = await apiPost("/api/connectors/bridges/pair", { code: pairCodeIn.value.trim() });
                  renderConnectorsView();
                  showToast("Pairing code approved");
                } catch (e) { showErrorToast(e); }
              },
            })
          )
        )
      );
      for (const p of (pairing.pending || [])) {
        pairingCard.appendChild(
          el("div", { className: "connector-title-row" },
            el("span", { className: "pill warn", text: p.code }),
            el("span", { text: `${p.channel}: ${p.sender_id} ${p.name ? "(" + p.name + ")" : ""}` }),
            el("button", {
              className: "btn-primary btn-sm",
              type: "button",
              text: "Approve",
              onClick: async () => {
                try {
                  appState.harness = await apiPost("/api/connectors/bridges/pair", { code: p.code });
                  renderConnectorsView();
                  showToast(`Approved ${p.channel}:${p.sender_id}`);
                } catch (e) { showErrorToast(e); }
              },
            })
          )
        );
      }
      for (const a of (pairing.approved || [])) {
        pairingCard.appendChild(
          el("div", { className: "connector-title-row" },
            el("span", { className: "pill ok", text: a.channel }),
            el("span", { text: `${a.sender_id} ${a.name ? "(" + a.name + ")" : ""}` }),
            el("button", {
              className: "btn-danger btn-sm",
              type: "button",
              text: "Revoke",
              onClick: () => confirmModal(
                "Revoke Paired Sender",
                `Revoke chat access for ${a.sender_id} on ${a.channel}?`,
                "Revoke Access",
                async () => {
                  appState.harness = await apiPost("/api/connectors/bridges/unpair", { channel: a.channel, sender_id: a.sender_id });
                  renderConnectorsView();
                  showToast(`Revoked ${a.channel}:${a.sender_id}`);
                }
              ),
            })
          )
        );
      }
      feed.appendChild(pairingCard);

      if (availableChannels.length > 0) {
        feed.appendChild(
          el("div", { className: "section-header card-spaced" },
            el("h2", { className: "section-title", text: "Available Channel Bridges" }),
            el("span", { className: "section-sub", text: `${availableChannels.length} available` })
          )
        );
        const availGrid = el("div", { className: "card-grid-2" });
        for (const ch of availableChannels) {
          availGrid.appendChild(
            el("div", { className: "card connector-card" },
              el("div", { className: "connector-info" },
                el("div", { className: "connector-title-row" },
                  el("span", { className: "connector-name", text: ch.name })
                ),
                el("p", { className: "connector-desc", text: ch.desc })
              ),
              el("div", { className: "connector-actions" },
                el("button", {
                  className: "btn-primary btn-sm",
                  type: "button",
                  text: "+ Connect",
                  onClick: () => openBridgeConfigModal(ch),
                })
              )
            )
          );
        }
        feed.appendChild(availGrid);
      }
    }
  }

  function parseKVLines(text) {
    const out = {};
    for (const raw of (text || "").split("\n")) {
      const line = raw.trim();
      if (!line) continue;
      const idx = line.indexOf("=");
      if (idx > 0) out[line.slice(0, idx).trim()] = line.slice(idx + 1).trim();
    }
    return out;
  }

  function openImportMCPModal() {
    const srcSel = el("select", { className: "select-input" },
      el("option", { value: "claude-code", text: "Claude Code (~/.claude.json / .mcp.json)" }),
      el("option", { value: "cursor", text: "Cursor (~/.cursor/mcp.json)" }),
      el("option", { value: "codex", text: "Codex (~/.codex/config.toml)" }),
      el("option", { value: "hermes", text: "Hermes (~/.hermes/config.yaml)" }),
      el("option", { value: "auto", text: "Auto-detect from pasted config or path" })
    );
    const pathIn = el("input", { className: "field-input", type: "text", placeholder: "Optional file path override (leave blank for default)" });
    const contentIn = el("textarea", { className: "field-textarea mono-box", rows: 6, placeholder: "Or paste mcpServers JSON / TOML / YAML directly…" });

    openModal("Import MCP Configuration", [
      el("div", { className: "form-grid" },
        el("div", { className: "field full" }, el("span", { className: "field-label", text: "Source Agent" }), srcSel),
        el("div", { className: "field full" }, el("span", { className: "field-label", text: "File Path (optional)" }), pathIn),
        el("div", { className: "field full" }, el("span", { className: "field-label", text: "Paste Config Content (optional)" }), contentIn)
      ),
      el("div", { className: "connector-actions" },
        el("button", { className: "btn-secondary", type: "button", text: "Cancel", onClick: () => closeModal(false) }),
        el("button", {
          className: "btn-primary",
          type: "button",
          text: "Import Servers",
          onClick: async () => {
            try {
              const res = await apiPost("/api/connectors/mcp/import", {
                source: srcSel.value,
                path: pathIn.value.trim(),
                content: contentIn.value,
              });
              appState.harness = res.harness;
              closeModal(true);
              appState.connTab = "installed";
              syncViewDOM();
              renderChrome();
              renderConnectorsView();
              showToast(`Imported ${res.imported || 0} MCP server(s)`);
            } catch (e) { showModalError(e.message); }
          },
        })
      ),
    ]);
  }

  function openCustomMCPModal(existing = null) {
    const isEdit = !!(existing && existing.name);
    const nameIn = el("input", { className: "field-input", type: "text", value: isEdit ? existing.name : "", disabled: isEdit, placeholder: "e.g. github-mcp" });
    const transSel = el("select", { className: "select-input" },
      el("option", { value: "stdio", text: "Local Command (stdio)" }),
      el("option", { value: "streamable_http", text: "Remote MCP (Streamable HTTP)" }),
      el("option", { value: "sse", text: "Remote SSE (Legacy)" }),
      el("option", { value: "http", text: "Stateless HTTP JSON-RPC" })
    );
    if (isEdit && existing.transport) transSel.value = existing.transport;

    const cmdIn = el("input", { className: "field-input", type: "text", value: isEdit ? (existing.command || "") : "", placeholder: "npx" });
    const argsIn = el("input", { className: "field-input", type: "text", value: isEdit ? (existing.args || []).map((a) => (/\s/.test(a) ? `"${a}"` : a)).join(" ") : "", placeholder: '-y @modelcontextprotocol/server-github' });
    const urlIn = el("input", { className: "field-input", type: "text", value: isEdit ? (existing.url || "") : "", placeholder: "https://mcp.example.com/mcp" });
    const tokenIn = el("input", { className: "field-input", type: "password", placeholder: isEdit && existing.has_auth_token ? "Saved (leave blank to keep)" : "Bearer token (optional)" });

    const removedEnvKeys = new Set();
    const removedHdrKeys = new Set();

    const makeSavedKeyChips = (keys, removedSet) => {
      const wrap = el("div", { className: "chip-list" });
      const render = () => {
        wrap.replaceChildren();
        for (const k of keys || []) {
          if (removedSet.has(k)) continue;
          wrap.appendChild(
            el("span", { className: "key-chip" },
              el("span", { text: `${k} (saved)` }),
              el("button", {
                type: "button",
                title: `Remove ${k}`,
                text: "×",
                onClick: () => { removedSet.add(k); modalDirty = true; render(); },
              })
            )
          );
        }
      };
      render();
      return wrap;
    };

    const envChips = isEdit && existing.env_keys?.length ? makeSavedKeyChips(existing.env_keys, removedEnvKeys) : null;
    const hdrChips = isEdit && existing.header_keys?.length ? makeSavedKeyChips(existing.header_keys, removedHdrKeys) : null;

    const envIn = el("textarea", { className: "field-textarea mono-box", rows: 2, placeholder: isEdit ? "Add or override KEY=VAL (existing saved keys are preserved)" : "KEY=VAL (one per line)" });
    const hdrIn = el("textarea", { className: "field-textarea mono-box", rows: 2, placeholder: isEdit ? "Add or override X-Header=val (existing headers are preserved)" : "X-Custom-Header=val (one per line)" });
    const incIn = el("input", { className: "field-input", type: "text", value: isEdit ? (existing.include_tools || []).join(", ") : "", placeholder: "Optional allowlist globs: read_*, list_*" });
    const excIn = el("input", { className: "field-input", type: "text", value: isEdit ? (existing.exclude_tools || []).join(", ") : "", placeholder: "Optional blocklist globs: delete_*" });
    const oauthChk = el("input", { type: "checkbox", checked: isEdit ? !!existing.oauth_enabled : false });
    const cidIn = el("input", { className: "field-input", type: "text", placeholder: isEdit && existing.oauth_enabled ? "Leave blank to keep existing Client ID" : "OAuth Client ID (optional)" });
    const isoChk = el("input", { type: "checkbox", checked: isEdit ? !!existing.isolated_desktop : false });

    const cmdField = el("div", { className: "field" }, el("span", { className: "field-label", text: "Command" }), cmdIn);
    const argsField = el("div", { className: "field" }, el("span", { className: "field-label", text: "Arguments (quote paths with spaces)" }), argsIn);
    const envField = el("div", { className: "field full" }, el("span", { className: "field-label", text: "Environment Variables (KEY=VAL)" }), envChips, envIn);
    const isoField = el("div", { className: "field full" }, el("label", { className: "checkbox-row" }, isoChk, el("span", { text: "Run inside Virtual Desktop container" })));

    const urlField = el("div", { className: "field full" }, el("span", { className: "field-label", text: "Remote Endpoint URL" }), urlIn);
    const tokenField = el("div", { className: "field" }, el("span", { className: "field-label", text: "Bearer Token (optional)" }), tokenIn);
    const oauthToggleField = el("div", { className: "field" }, el("label", { className: "checkbox-row" }, oauthChk, el("span", { text: "Use OAuth 2.1 PKCE" })));
    const cidField = el("div", { className: "field full" }, el("span", { className: "field-label", text: "OAuth Client ID (optional)" }), cidIn);
    const hdrField = el("div", { className: "field full" }, el("span", { className: "field-label", text: "Custom HTTP Headers (KEY=VAL)" }), hdrChips, hdrIn);

    const syncTransportFields = () => {
      const isStdio = transSel.value === "stdio";
      cmdField.classList.toggle("hidden", !isStdio);
      argsField.classList.toggle("hidden", !isStdio);
      envField.classList.toggle("hidden", !isStdio);
      isoField.classList.toggle("hidden", !isStdio);

      urlField.classList.toggle("hidden", isStdio);
      tokenField.classList.toggle("hidden", isStdio);
      oauthToggleField.classList.toggle("hidden", isStdio);
      hdrField.classList.toggle("hidden", isStdio);
      cidField.classList.toggle("hidden", isStdio || !oauthChk.checked);
    };
    syncTransportFields();
    transSel.addEventListener("change", syncTransportFields);
    oauthChk.addEventListener("change", syncTransportFields);

    openModal(isEdit ? `Edit MCP Connector: ${existing.name}` : "Add Custom MCP Connector", [
      el("div", { className: "form-grid" },
        el("div", { className: "field" }, el("span", { className: "field-label", text: "Connector Name" }), nameIn),
        el("div", { className: "field" }, el("span", { className: "field-label", text: "Transport Type" }), transSel),
        cmdField,
        argsField,
        envField,
        isoField,
        urlField,
        tokenField,
        oauthToggleField,
        cidField,
        hdrField,
        el("div", { className: "field" }, el("span", { className: "field-label", text: "Include Tools (optional)" }), incIn),
        el("div", { className: "field" }, el("span", { className: "field-label", text: "Exclude Tools (optional)" }), excIn)
      ),
      el("div", { className: "connector-actions" },
        el("button", { className: "btn-secondary", type: "button", text: "Cancel", onClick: () => closeModal(false) }),
        el("button", {
          className: "btn-primary",
          type: "button",
          text: "Save Connector",
          onClick: async () => {
            const nm = nameIn.value.trim();
            if (!nm) { showModalError("Connector name is required"); return; }
            if (transSel.value === "stdio" && !cmdIn.value.trim()) {
              showModalError("Command is required for Local Command (stdio) connectors");
              return;
            }
            if (transSel.value !== "stdio" && !urlIn.value.trim()) {
              showModalError("Remote Endpoint URL is required");
              return;
            }
            try {
              const splitGlobs = (v) => v.split(",").map((s) => s.trim()).filter(Boolean);
              const envObj = parseKVLines(envIn.value);
              for (const rk of removedEnvKeys) envObj[rk] = "";
              const hdrObj = parseKVLines(hdrIn.value);
              for (const rk of removedHdrKeys) hdrObj[rk] = "";

              appState.harness = await apiPost("/api/connectors/mcp", {
                name: nm,
                preset_id: isEdit ? (existing.preset_id || "") : "",
                transport: transSel.value,
                command: cmdIn.value.trim(),
                args: parseArgsQuoted(argsIn.value),
                url: urlIn.value.trim(),
                auth_token: tokenIn.value.trim(),
                env: Object.keys(envObj).length ? envObj : undefined,
                headers: Object.keys(hdrObj).length ? hdrObj : undefined,
                include_tools: splitGlobs(incIn.value),
                exclude_tools: splitGlobs(excIn.value),
                oauth: oauthChk.checked ? { enabled: true, client_id: cidIn.value.trim() } : undefined,
                isolated_desktop: isoChk.checked,
                enabled: isEdit ? !!existing.enabled : true,
              });
              closeModal(true);
              appState.connTab = "installed";
              syncViewDOM();
              renderChrome();
              renderConnectorsView();
              showToast("MCP connector saved");
            } catch (e) { showModalError(e.message); }
          },
        })
      ),
    ]);
  }

  // Settings (Harness Control) View
  function renderHarnessView() {
    const har = appState.harness;
    const feed = document.getElementById("harnessFeed");
    feed.replaceChildren();
    if (!har) {
      feed.appendChild(el("div", { className: "card", text: "Loading settings…" }));
      return;
    }

    const tab = appState.harnessTab;
    if (tab === "provider") {
      const provPresets = har.provider_presets || [];
      const presetByID = Object.fromEntries(provPresets.map((pp) => [pp.id, pp]));
      const accs = har.accounts || [];

      const openModelModal = (existing) => {
        const isEdit = Boolean(existing);
        const initProv = existing ? existing.provider : (provPresets[0]?.id || "openrouter");
        const initPreset = presetByID[initProv];

        const provSel = el("select", { className: "select-input" },
          ...provPresets.map((pp) =>
            el("option", { value: pp.id, text: `${pp.name} (${pp.category})`, selected: initProv === pp.id })
          ),
          el("option", { value: "openai_compat", text: "Custom OpenAI-Compatible API", selected: initProv === "openai_compat" }),
          el("option", { value: "mock", text: "Local Mock Simulator (Offline Testing)", selected: initProv === "mock" })
        );

        let idTouched = isEdit;
        const defaultNameFor = (pv) => {
          const base = (pv || "model").replace(/[^a-z0-9-_]/gi, "-").toLowerCase();
          if (!accs.some((a) => a.id === base)) return base;
          let n = 2;
          while (accs.some((a) => a.id === `${base}-${n}`)) n++;
          return `${base}-${n}`;
        };
        const idIn = el("input", {
          className: "field-input",
          type: "text",
          value: existing ? existing.id : defaultNameFor(initProv),
          disabled: isEdit,
          placeholder: "e.g. openrouter-frontier",
        });
        idIn.addEventListener("input", () => { idTouched = true; });

        const apiBaseIn = el("input", {
          className: "field-input",
          type: "text",
          value: existing?.api_base || initPreset?.base_url || "",
          placeholder: "https://api.openai.com/v1",
        });
        const apiKeyIn = el("input", {
          className: "field-input",
          type: "password",
          placeholder: existing?.has_api_key ? `Saved (${existing.api_key_masked}) — leave blank to keep` : "Paste API Key",
        });

        const modelSel = el("select", { className: "select-input" });
        const modelIn = el("input", {
          className: "field-input",
          type: "text",
          value: existing?.model_id || initPreset?.default_model || "",
          placeholder: "Model ID (e.g. anthropic/claude-sonnet-4.5)",
        });
        const modelStatus = el("div", { className: "muted caption-text", text: "Loading available models…" });

        const populateModelSelect = (models, source, warn) => {
          const cur = modelIn.value.trim();
          const list = Array.from(new Set([...(models || []), cur].filter(Boolean)));
          modelSel.replaceChildren(
            el("option", { value: "", text: list.length ? "— Quick-pick from provider list —" : "— Type custom Model ID below —" }),
            ...list.map((m) => el("option", { value: m, text: m, selected: m === cur }))
          );
          if (!cur && models && models.length > 0) {
            modelIn.value = models[0];
            modelSel.value = models[0];
          } else if (cur && list.includes(cur)) {
            modelSel.value = cur;
          }
          if (source === "live") {
            modelStatus.textContent = `Loaded ${models.length} model(s) live from provider API.`;
          } else {
            modelStatus.textContent = warn
              ? `Showing suggested models (${warn}). Paste your API key to load live models automatically.`
              : `Showing ${list.length} suggested model(s).`;
          }
        };

        modelSel.addEventListener("change", () => {
          if (modelSel.value) modelIn.value = modelSel.value;
        });
        modelIn.addEventListener("input", () => {
          const v = modelIn.value.trim();
          modelSel.value = Array.from(modelSel.options).some((o) => o.value === v) ? v : "";
        });

        const refreshModels = async () => {
          modelStatus.textContent = "Querying models from provider…";
          try {
            const res = await apiPost("/api/harness/models", {
              provider: provSel.value,
              api_base: apiBaseIn.value.trim(),
              api_key: apiKeyIn.value.trim(),
              account_id: isEdit ? existing.id : "",
            });
            populateModelSelect(res.models || [], res.source, res.warning);
          } catch (e) {
            modelStatus.textContent = `Could not query models: ${e.message}`;
          }
        };

        let keyDebounce = null;
        apiKeyIn.addEventListener("input", () => {
          clearTimeout(keyDebounce);
          if (apiKeyIn.value.trim().length >= 8) {
            keyDebounce = setTimeout(refreshModels, 500);
          }
        });

        const roleSel = el("select", { className: "select-input" },
          el("option", { value: "default", text: "Default (Primary model)", selected: (existing?.role || (accs.length === 0 ? "default" : "worker")) === "default" }),
          el("option", { value: "worker", text: "Worker (Used when Worker depth is selected)", selected: existing?.role === "worker" }),
          el("option", { value: "frontier", text: "Frontier (Used when Frontier depth is selected)", selected: existing?.role === "frontier" }),
          el("option", { value: "gui", text: "GUI / Vision (Used for Virtual Desktop tasks)", selected: existing?.role === "gui" }),
          el("option", { value: "fallback", text: "Fallback Only (Backup when primary hits rate limits)", selected: existing?.role === "fallback" })
        );

        const initBill = existing?.billing_mode || initPreset?.suggested_billing_mode || (initProv === "mock" ? "local" : "metered");
        const billSel = el("select", { className: "select-input" },
          el("option", { value: "local", text: "Local / Free (no spend cap)", selected: initBill === "local" }),
          el("option", { value: "subscription", text: "Subscription / Fixed Plan (no spend cap)", selected: initBill === "subscription" }),
          el("option", { value: "metered", text: "Metered API (optional daily USD cap)", selected: initBill === "metered" })
        );
        const concIn = el("input", { className: "field-input", type: "number", value: existing?.max_concurrent || 2, min: 1 });
        const budIn = el("input", { className: "field-input", type: "number", value: existing ? existing.daily_budget_usd : 10, step: "0.5", min: 0 });

        const apiBaseField = el("div", { className: "field" }, el("span", { className: "field-label", text: "API Base URL" }), apiBaseIn);
        const apiKeyField = el("div", { className: "field" }, el("span", { className: "field-label", text: "API Key / Token" }), apiKeyIn);
        const budField = el("div", { className: "field" }, el("span", { className: "field-label", text: "Daily Spend Cap (USD, 0 = unlimited)" }), budIn);

        const oauthBtn = el("button", {
          className: "btn-secondary",
          type: "button",
          text: "Sign in with OAuth",
          onClick: async () => {
            try {
              const res = await apiPost("/api/harness/provider/oauth/start", {
                provider: provSel.value,
                account_id: idIn.value.trim(),
              });
              if (res.auth_url) {
                window.open(res.auth_url, "_blank", "noopener");
                showToast("Complete OAuth login in the opened tab");
              }
            } catch (e) { showModalError(e.message); }
          },
        });

        const syncVisibility = () => {
          const pv = provSel.value;
          const isMock = pv === "mock";
          const chosen = presetByID[pv];
          const isLocal = isMock || (chosen && chosen.category === "local");
          apiBaseField.classList.toggle("hidden", isMock);
          apiKeyField.classList.toggle("hidden", isMock);
          if (isLocal && !existing?.has_api_key) {
            apiKeyIn.placeholder = "Optional (not required for local)";
          } else if (chosen?.env_var && !existing?.has_api_key) {
            apiKeyIn.placeholder = `Paste key or set ${chosen.env_var}`;
          }
          oauthBtn.classList.toggle("hidden", !(pv === "openrouter" || pv === "openai-codex"));
          budField.classList.toggle("hidden", billSel.value !== "metered");
        };
        syncVisibility();

        provSel.addEventListener("change", () => {
          const pv = provSel.value;
          const chosen = presetByID[pv];
          if (!idTouched) idIn.value = defaultNameFor(pv);
          if (chosen) {
            apiBaseIn.value = chosen.base_url || "";
            modelIn.value = chosen.default_model || "";
            if (chosen.suggested_billing_mode) billSel.value = chosen.suggested_billing_mode;
          } else if (pv === "mock") {
            apiBaseIn.value = "";
            modelIn.value = "mock-worker-v1";
            billSel.value = "local";
          } else {
            billSel.value = "metered";
          }
          syncVisibility();
          refreshModels();
        });
        billSel.addEventListener("change", syncVisibility);

        openModal(isEdit ? `Edit Model: ${existing.id}` : "Add AI Model / Provider", [
          el("div", { className: "form-grid" },
            el("div", { className: "field" }, el("span", { className: "field-label", text: "Provider" }), provSel),
            el("div", { className: "field" }, el("span", { className: "field-label", text: "Profile ID" }), idIn),
            apiBaseField,
            apiKeyField,
            el("div", { className: "field full" },
              el("span", { className: "field-label", text: "Model Selection" }),
              el("div", { className: "inline-row" },
                modelSel,
                el("button", { className: "btn-secondary btn-sm", type: "button", text: "Refresh List", onClick: refreshModels })
              ),
              modelIn,
              modelStatus
            ),
            el("div", { className: "field" }, el("span", { className: "field-label", text: "Assigned Role" }), roleSel),
            el("div", { className: "field" }, el("span", { className: "field-label", text: "Billing Type" }), billSel),
            el("div", { className: "field" }, el("span", { className: "field-label", text: "Max Parallel Tasks" }), concIn),
            budField
          ),
          el("div", { className: "connector-actions" },
            oauthBtn,
            el("button", { className: "btn-secondary", type: "button", text: "Cancel", onClick: () => closeModal(false) }),
            el("button", {
              className: "btn-primary",
              type: "button",
              text: isEdit ? "Save Changes" : "Add Model",
              onClick: async () => {
                try {
                  appState.harness = await apiPost("/api/harness/accounts", {
                    ID: idIn.value.trim(),
                    Provider: provSel.value,
                    ModelID: modelIn.value.trim(),
                    BillingMode: billSel.value,
                    MaxConcurrent: Number(concIn.value) || 2,
                    DailyBudgetUSD: billSel.value === "metered" ? (Number(budIn.value) || 0) : 0,
                    api_base: apiBaseIn.value.trim(),
                    api_key: apiKeyIn.value.trim(),
                    role: roleSel.value,
                    set_default: roleSel.value === "default" || Boolean(existing?.is_default),
                  });
                  closeModal(true);
                  await refreshAll();
                  showToast(isEdit ? `Updated ${idIn.value.trim()}` : `Added ${idIn.value.trim()}`);
                } catch (e) { showModalError(e.message); }
              },
            })
          ),
        ]);
        refreshModels();
      };

      feed.appendChild(
        el("div", { className: "section-header" },
          el("div", null,
            el("h2", { className: "section-title", text: "Models & Providers" }),
            el("div", { className: "section-sub", text: "Assign models to roles (Default, Worker, Frontier, GUI, Fallback). Settings persist automatically across restarts." })
          ),
          el("button", { className: "btn-primary", type: "button", text: "+ Add Model", onClick: () => openModelModal(null) })
        )
      );

      if (accs.length === 0) {
        feed.appendChild(el("div", { className: "card muted", text: "No models configured yet. Click + Add Model to connect OpenRouter, Anthropic, OpenAI, Ollama, or the offline Mock simulator." }));
      } else {
        const grid = el("div", { className: "card-grid-2" });
        for (const a of accs) {
          const isMetered = a.billing_mode === "metered";
          const capLabel = a.daily_budget_usd > 0 ? `$${a.daily_budget_usd.toFixed(2)} cap` : "no cap";
          const spendText = isMetered ? ` · Today: $${a.daily_spent_usd.toFixed(2)} (${capLabel})` : "";
          grid.appendChild(
            el("div", { className: "card connector-card" },
              el("div", { className: "connector-info" },
                el("div", { className: "connector-title-row" },
                  el("span", { className: "status-dot " + (a.is_default ? "ok" : "") }),
                  el("span", { className: "connector-name", text: a.id }),
                  a.is_default ? el("span", { className: "pill ok", text: "Default" }) : null,
                  a.role && a.role !== "default" ? el("span", { className: "pill", text: `Role: ${a.role}` }) : null,
                  el("span", { className: "pill", text: a.provider }),
                  a.has_api_key ? el("span", { className: "pill ok", text: "Key Saved" }) : null,
                  a.cooldown_sec > 0 ? el("span", { className: "pill warn", text: `Rate-limit cooldown ${a.cooldown_sec}s` }) : null
                ),
                el("div", { className: "connector-meta", text: `Model: ${a.model_id || "(unset)"}${a.api_base ? " · " + a.api_base : ""}` }),
                el("div", { className: "muted caption-text", text: `Active Runs: ${a.active_leases} / ${a.max_concurrent}${spendText}` })
              ),
              el("div", { className: "connector-actions" },
                !a.is_default
                  ? el("button", {
                      className: "btn-primary btn-sm",
                      type: "button",
                      text: "Set Default",
                      onClick: async () => {
                        try {
                          appState.harness = await apiPost(`/api/harness/accounts/${encodeURIComponent(a.id)}/activate`, {});
                          await refreshAll();
                          showToast(`${a.id} is now default`);
                        } catch (e) { showErrorToast(e); }
                      },
                    })
                  : null,
                el("button", {
                  className: "btn-secondary btn-sm",
                  type: "button",
                  text: "Edit",
                  onClick: () => openModelModal(a),
                }),
                el("button", {
                  className: "btn-danger btn-sm",
                  type: "button",
                  text: "Remove",
                  onClick: () => confirmModal(
                    `Remove Model (${a.id})`,
                    `Remove model profile "${a.id}" and its saved credentials?`,
                    "Remove Model",
                    async () => {
                      appState.harness = await apiPost(`/api/harness/accounts/${encodeURIComponent(a.id)}/delete`, {});
                      await refreshAll();
                      showToast(`Removed ${a.id}`);
                    }
                  ),
                })
              )
            )
          );
        }
        feed.appendChild(grid);
      }

      if (appState.openAddModelOnRender) {
        appState.openAddModelOnRender = false;
        setTimeout(() => openModelModal(null), 20);
      }
    } else if (tab === "sandbox") {
      const p = har.provider || {};
      const st = appState.state;
      const embedUrl = el("input", { className: "field-input", type: "text", value: p.embed_url || "", placeholder: "http://127.0.0.1:11434/v1" });
      const embedModel = el("input", { className: "field-input", type: "text", value: p.embed_model || "", placeholder: "qwen3-embedding:0.6b" });
      const maxTurns = el("input", { className: "field-input", type: "number", value: p.max_turns || 15, min: 1, max: 200 });
      const curStrat = p.workspace_strategy || "worktree";
      const wsStrat = el("select", { className: "select-input" },
        el("option", { value: "worktree", text: "Isolated Git Worktree (Recommended — review diff before applying)", selected: curStrat === "worktree" || curStrat === "git_worktree" }),
        el("option", { value: "copy", text: "Isolated Copy Snapshot (for non-Git folders)", selected: curStrat === "copy" || curStrat === "copy_snapshot" }),
        el("option", { value: "direct", text: "Direct Project Folder (edit files in place immediately)", selected: curStrat === "direct" })
      );
      const curSbx = (p.sandbox_mode === "bwrap" || p.sandbox_mode === "seatbelt") ? p.sandbox_mode : "auto";
      const sbxMode = el("select", { className: "select-input" },
        el("option", { value: "auto", text: "Auto-detect Host Sandbox (Recommended)", selected: curSbx === "auto" }),
        el("option", { value: "bwrap", text: "Bubblewrap (Linux bwrap)", selected: curSbx === "bwrap" }),
        el("option", { value: "seatbelt", text: "Seatbelt (macOS sandbox-exec)", selected: curSbx === "seatbelt" })
      );
      const curAuto = p.autonomy_mode === "autonomous" ? "autonomous" : "standard";
      const autonomySel = el("select", { className: "select-input" },
        el("option", { value: "standard", text: "Standard — Ask before external actions after web reads", selected: curAuto === "standard" }),
        el("option", { value: "autonomous", text: "High Autonomy — Auto-approve external/MCP actions (ask only on irreversible ops)", selected: curAuto === "autonomous" })
      );
      const netChk = el("input", { type: "checkbox", checked: Boolean(p.allow_sandbox_network) });

      const saveSandboxSettings = async () => {
        try {
          appState.harness = await apiPost("/api/harness/provider", {
            provider_type: p.provider_type,
            api_base: p.api_base,
            model_id: p.model_id,
            embed_url: embedUrl.value.trim(),
            embed_model: embedModel.value.trim(),
            max_turns: Number(maxTurns.value) || 15,
            workspace_strategy: wsStrat.value,
            sandbox_mode: sbxMode.value,
            allow_sandbox_network: netChk.checked,
            autonomy_mode: autonomySel.value,
          });
          await refreshAll();
          showToast("Autonomy, sandbox & embedding settings applied");
        } catch (e) { showErrorToast(e); }
      };

      feed.appendChild(
        el("div", { className: "card" },
          el("div", { className: "connector-title-row" },
            el("span", { className: "connector-name", text: "Autonomy, Access & OS Sandbox" }),
            el("span", {
              className: "pill " + (st?.sandbox_ready ? "ok" : "warn"),
              text: st?.sandbox_ready ? `Host Sandbox Ready (${st.sandbox_name})` : "Host Sandbox Unavailable (shell commands blocked)",
            }),
            p.allow_sandbox_network ? el("span", { className: "pill warn", text: "Internet Access ON" }) : el("span", { className: "pill", text: "Offline Sandbox" }),
            curAuto === "autonomous" ? el("span", { className: "pill warn", text: "High Autonomy" }) : null
          ),
          el("p", { className: "connector-desc", text: "Configure how much autonomy and network/filesystem access soma has while keeping shell commands fail-closed inside the OS sandbox (with host secrets scrubbed)." }),
          el("div", { className: "form-grid" },
            el("div", { className: "field" }, el("span", { className: "field-label", text: "Autonomy Level" }), autonomySel),
            el("div", { className: "field" }, el("span", { className: "field-label", text: "Workspace File Mode" }), wsStrat),
            el("div", { className: "field" }, el("span", { className: "field-label", text: "OS Sandbox Backend" }), sbxMode),
            el("div", { className: "field" }, el("span", { className: "field-label", text: "Default Max Steps per Task" }), maxTurns)
          ),
          el("label", { className: "inline-check-row" },
            netChk,
            el("span", { text: "Allow outbound internet access inside sandboxed commands (enables curl, pip, npm, git fetch while keeping workspace & secret isolation)" })
          ),
          el("div", { className: "connector-actions" },
            el("button", { className: "btn-primary", type: "button", text: "Save Access & Sandbox Settings", onClick: saveSandboxSettings })
          )
        )
      );

      feed.appendChild(
        el("div", { className: "card card-spaced" },
          el("div", { className: "connector-title-row" },
            el("span", { className: "connector-name", text: "Memory & Output Vector Embeddings" }),
            p.embed_url || p.embed_model ? el("span", { className: "pill ok", text: "Configured" }) : el("span", { className: "pill", text: "BM25 Keyword Default" })
          ),
          el("p", { className: "connector-desc", text: "Optional local (Ollama) or OpenAI-compatible embedding endpoint for hybrid BM25 + semantic vector retrieval across Memory and spilled logs." }),
          el("div", { className: "form-grid" },
            el("div", { className: "field" }, el("span", { className: "field-label", text: "Embedder API Base URL" }), embedUrl),
            el("div", { className: "field" }, el("span", { className: "field-label", text: "Embedding Model ID" }), embedModel)
          ),
          el("div", { className: "connector-actions" },
            el("button", { className: "btn-primary", type: "button", text: "Save Embedding Settings", onClick: saveSandboxSettings })
          )
        )
      );
    } else if (tab === "remote") {
      const tun = har.tunnel || {};
      const tunMode = el("select", { className: "select-input" },
        el("option", { value: "quick", text: "Quick Tunnel (instant random *.trycloudflare.com URL)", selected: (tun.mode || "quick") === "quick" }),
        el("option", { value: "token", text: "Named Tunnel (stable custom domain)", selected: tun.mode === "token" })
      );
      const tunToken = el("input", { className: "field-input", type: "password", placeholder: tun.has_tunnel_token ? "Saved (leave blank to keep)" : "Cloudflare Zero Trust Tunnel Token" });
      const tunHost = el("input", { className: "field-input", type: "text", value: tun.hostname || "", placeholder: "soma.yourdomain.com" });
      const tunAuth = el("input", { className: "field-input", type: "password", placeholder: tun.has_auth_token ? `Saved (${tun.auth_token_masked}) — leave blank to keep` : "Set custom password or click Generate Token" });
      const tunAutoStart = el("input", { type: "checkbox", checked: Boolean(tun.auto_start) });

      const tunTokenField = el("div", { className: "field" }, el("span", { className: "field-label", text: "Cloudflare Tunnel Token" }), tunToken);
      const tunHostField = el("div", { className: "field" }, el("span", { className: "field-label", text: "Custom Hostname" }), tunHost);
      const syncTunnelFields = () => {
        const isNamed = tunMode.value === "token";
        tunTokenField.classList.toggle("hidden", !isNamed);
        tunHostField.classList.toggle("hidden", !isNamed);
      };
      syncTunnelFields();
      tunMode.addEventListener("change", syncTunnelFields);

      const showGeneratedToken = (tok) => {
        if (!tok) return;
        openModal("Dashboard Access Token Generated", [
          el("p", { className: "connector-desc", text: "Save this token now — you will need it to sign in from remote devices. Your current browser session is already signed in." }),
          el("pre", { className: "code-block", text: tok }),
          el("div", { className: "connector-actions" },
            el("button", { className: "btn-secondary", type: "button", text: "Close", onClick: () => closeModal(true) }),
            el("button", { className: "btn-primary", type: "button", text: "Copy Token", onClick: () => copyToClipboard(tok, "Access token") })
          ),
        ], { protectAuthOverride: true, skipDirtyTracking: true });
      };

      feed.appendChild(
        el("div", { className: "card" },
          el("div", { className: "connector-title-row" },
            el("span", { className: "connector-name", text: "Remote Access (Cloudflare Tunnel)" }),
            el("span", { className: "pill " + (tun.installed ? "ok" : "warn"), text: tun.installed ? "cloudflared installed" : "cloudflared missing" }),
            el("span", { className: "pill " + (tun.running ? "ok" : ""), text: tun.running ? "Tunnel Running" : "Stopped" }),
            tun.has_auth_token ? el("span", { className: "pill ok", text: "Password Protected" }) : el("span", { className: "pill warn", text: "Password Generated on Start" })
          ),
          !tun.installed
            ? el("div", { className: "info-banner", text: "Install `cloudflared` on this host to expose the dashboard securely (e.g. `brew install cloudflare/cloudflare/cloudflared` or download from developers.cloudflare.com)." })
            : null,
          el("p", { className: "connector-desc", text: "Access soma from your phone or laptop over HTTPS. An access token is always enforced before opening any external tunnel." }),
          tun.public_url
            ? el("div", { className: "info-banner inline-row" },
                el("span", { text: "Public URL: " }),
                el("a", { href: tun.public_url, target: "_blank", rel: "noopener", text: tun.public_url }),
                el("button", { className: "btn-secondary btn-sm", type: "button", text: "Copy URL", onClick: () => copyToClipboard(tun.public_url, "Public URL") })
              )
            : null,
          tun.last_error ? el("div", { className: "modal-error", text: `Tunnel error: ${tun.last_error}` }) : null,
          el("div", { className: "form-grid" },
            el("div", { className: "field" }, el("span", { className: "field-label", text: "Tunnel Mode" }), tunMode),
            el("div", { className: "field" }, el("span", { className: "field-label", text: "Dashboard Access Token" }), tunAuth),
            tunTokenField,
            tunHostField,
            el("div", { className: "field full" }, el("label", { className: "checkbox-row" }, tunAutoStart, el("span", { text: "Auto-start tunnel when soma web boots (pair with `soma web install-service`)" })))
          ),
          el("div", { className: "connector-actions" },
            el("button", {
              className: "btn-secondary",
              type: "button",
              text: "Generate New Token",
              onClick: async () => {
                try {
                  const res = await apiPost("/api/tunnel/config", {
                    mode: tunMode.value,
                    tunnel_token: tunToken.value.trim(),
                    hostname: tunHost.value.trim(),
                    generate_auth_token: true,
                    auto_start: tunAutoStart.checked,
                  });
                  appState.harness = res.harness;
                  await refreshAll();
                  showGeneratedToken(res.generated_auth_token);
                } catch (e) { showErrorToast(e); }
              },
            }),
            el("button", {
              className: "btn-secondary",
              type: "button",
              text: "Save Config",
              onClick: async () => {
                try {
                  const res = await apiPost("/api/tunnel/config", {
                    mode: tunMode.value,
                    tunnel_token: tunToken.value.trim(),
                    hostname: tunHost.value.trim(),
                    auth_token: tunAuth.value.trim(),
                    auto_start: tunAutoStart.checked,
                  });
                  appState.harness = res.harness;
                  tunAuth.value = "";
                  await refreshAll();
                  showToast("Tunnel settings saved");
                } catch (e) { showErrorToast(e); }
              },
            }),
            tun.running
              ? el("button", {
                  className: "btn-danger",
                  type: "button",
                  text: "Stop Tunnel",
                  onClick: async () => {
                    try {
                      appState.harness = await apiPost("/api/tunnel/stop", {});
                      renderChrome();
                      renderHarnessView();
                      showToast("Cloudflared tunnel stopped");
                    } catch (e) { showErrorToast(e); }
                  },
                })
              : el("button", {
                  className: "btn-primary",
                  type: "button",
                  text: "Start Tunnel",
                  onClick: async (ev) => {
                    const btn = ev.currentTarget;
                    btn.disabled = true;
                    btn.textContent = "Starting…";
                    try {
                      const res = await apiPost("/api/tunnel/start", {});
                      appState.harness = res.harness;
                      await refreshAll();
                      if (res.generated_auth_token) showGeneratedToken(res.generated_auth_token);
                      showToast("Cloudflared tunnel started");
                    } catch (e) {
                      showErrorToast(e);
                    } finally {
                      btn.disabled = false;
                      btn.textContent = "Start Tunnel";
                    }
                  },
                })
          )
        )
      );
    } else if (tab === "triggers") {
      const tr = har.triggers || {};
      const triggersList = (tr.triggers || []).slice();
      const lastRuns = har.trigger_last_run || {};

      const quietIn = el("input", { className: "field-input", type: "text", value: tr.quiet_hours || "", placeholder: "23:00-07:00 (optional)" });
      const maxInbIn = el("input", { className: "field-input", type: "number", value: tr.max_pending_inbox || 3, min: 1 });
      const budIn = el("input", { className: "field-input", type: "number", value: tr.daily_metered_budget_usd || 0, step: "0.5", min: 0 });
      const hookIn = el("input", { className: "field-input", type: "text", value: tr.webhook_addr || "127.0.0.1:8787" });
      const gcIn = el("input", { className: "field-input", type: "text", value: tr.gc_interval || "5m" });

      const saveTriggersPayload = async (nextList, msg = "Automation settings saved") => {
        appState.harness = await apiPost("/api/harness/triggers", {
          quiet_hours: quietIn.value.trim(),
          max_pending_inbox: Number(maxInbIn.value) || 3,
          daily_metered_budget_usd: Number(budIn.value) || 0,
          webhook_addr: hookIn.value.trim(),
          gc_interval: gcIn.value.trim(),
          triggers: nextList,
        });
        renderHarnessView();
        showToast(msg);
      };

      const isEnabled = (t) => t.Enabled !== false && t.enabled !== false;
      const tVal = (t, pascal, camel) => t[pascal] ?? t[camel];

      const openTriggerModal = (existing, editIdx) => {
        const isEdit = Boolean(existing);
        const curType = existing ? (tVal(existing, "Type", "type") || "cron") : "cron";

        const idIn = el("input", {
          className: "field-input",
          type: "text",
          value: existing ? (tVal(existing, "ID", "id") || "") : "",
          disabled: isEdit,
          placeholder: "e.g. morning-health-check",
        });
        const typeSel = el("select", { className: "select-input" },
          el("option", { value: "cron", text: "Schedule (Interval or 5-field Cron)", selected: curType === "cron" }),
          el("option", { value: "fsnotify", text: "File Watcher (on file edits)", selected: curType === "fsnotify" }),
          el("option", { value: "watch", text: "Command Watcher (run check periodically)", selected: curType === "watch" }),
          el("option", { value: "sentinel", text: "Command Sentinel (wake only when output changes)", selected: curType === "sentinel" }),
          el("option", { value: "webhook", text: "Incoming HTTP Webhook", selected: curType === "webhook" })
        );

        const schedPresetSel = el("select", { className: "select-input" },
          el("option", { value: "", text: "— Quick schedule presets —" }),
          el("option", { value: "15m", text: "Every 15 minutes (15m)" }),
          el("option", { value: "1h", text: "Every hour (1h)" }),
          el("option", { value: "24h", text: "Once a day (24h)" }),
          el("option", { value: "0 9 * * *", text: "Daily at 9:00 AM (0 9 * * *)" }),
          el("option", { value: "0 9 * * 1-5", text: "Weekdays at 9:00 AM (0 9 * * 1-5)" })
        );
        const intIn = el("input", {
          className: "field-input",
          type: "text",
          value: existing ? (tVal(existing, "Interval", "interval") || "1h") : "1h",
          placeholder: "e.g. 15m, 6h, 7d, or 0 9 * * *",
        });
        schedPresetSel.addEventListener("change", () => {
          if (schedPresetSel.value) intIn.value = schedPresetSel.value;
        });

        const existingPaths = existing ? (tVal(existing, "Paths", "paths") || []) : [];
        const pathsIn = el("input", {
          className: "field-input",
          type: "text",
          value: existingPaths.join(", "),
          placeholder: "e.g. ./src, ./docs (comma-separated)",
        });
        const debounceIn = el("input", {
          className: "field-input",
          type: "text",
          value: existing ? (tVal(existing, "Debounce", "debounce") || "2s") : "2s",
          placeholder: "2s",
        });

        const cmdIn = el("input", {
          className: "field-input",
          type: "text",
          value: existing ? (tVal(existing, "Command", "command") || "") : "",
          placeholder: "e.g. git status --porcelain",
        });
        const wakeSel = el("select", { className: "select-input" },
          el("option", { value: "output_hash_changed", text: "When command output changes", selected: (existing && tVal(existing, "WakeOn", "wake_on")) === "output_hash_changed" }),
          el("option", { value: "nonzero", text: "When command exits non-zero", selected: (existing && tVal(existing, "WakeOn", "wake_on")) === "nonzero" }),
          el("option", { value: "always", text: "Every interval", selected: (existing && tVal(existing, "WakeOn", "wake_on")) === "always" })
        );

        const srcIn = el("input", {
          className: "field-input",
          type: "text",
          value: existing ? (tVal(existing, "Source", "source") || "") : "",
          placeholder: "e.g. github, stripe, custom",
        });
        const filterIn = el("input", {
          className: "field-input",
          type: "text",
          value: existing ? (tVal(existing, "Filter", "filter") || "") : "",
          placeholder: 'e.g. action == "opened" or pull_request.opened',
        });

        const curCap = existing ? (tVal(existing, "Capability", "capability") || "notify_only") : "notify_only";
        const capSel = el("select", { className: "select-input" },
          el("option", { value: "notify_only", text: "AI Check & Report (runs prompt and posts summary to Approvals)", selected: curCap === "notify_only" || curCap === "read_only" }),
          el("option", { value: "draft_only", text: "Draft Changes (runs in isolated workspace for review)", selected: curCap === "draft_only" }),
          el("option", { value: "alert_only", text: "Alert Only (zero-token notification, no AI call)", selected: curCap === "alert_only" })
        );
        const curUrg = existing ? (tVal(existing, "Urgency", "urgency") || "normal") : "normal";
        const urgSel = el("select", { className: "select-input" },
          el("option", { value: "low", text: "Low", selected: curUrg === "low" }),
          el("option", { value: "normal", text: "Normal", selected: curUrg === "normal" }),
          el("option", { value: "urgent", text: "Urgent (bypasses quiet hours)", selected: curUrg === "urgent" })
        );

        const curNotifyCh = existing ? (tVal(existing, "NotifyChannel", "notify_channel") || "") : "";
        const notifyChSel = el("select", { className: "select-input" },
          el("option", { value: "", text: "Approvals Inbox only", selected: curNotifyCh === "" }),
          ...["telegram", "discord", "slack", "email", "matrix", "mattermost", "teams", "google-chat", "whatsapp", "signal"].map((ch) =>
            el("option", { value: ch, text: ch, selected: curNotifyCh === ch })
          )
        );
        const notifyTargetIn = el("input", {
          className: "field-input",
          type: "text",
          value: existing ? (tVal(existing, "NotifyTarget", "notify_target") || "") : "",
          placeholder: "Channel ID, chat ID, or email address",
        });

        const promptIn = el("textarea", {
          className: "field-textarea",
          rows: 4,
          value: existing ? (tVal(existing, "Prompt", "prompt") || "") : "",
          placeholder: "What should soma check or do when this automation triggers?",
        });

        const schedPresetField = el("div", { className: "field" }, el("span", { className: "field-label", text: "Schedule Preset" }), schedPresetSel);
        const intField = el("div", { className: "field" }, el("span", { className: "field-label", text: "Interval or Cron Expression" }), intIn);
        const pathsField = el("div", { className: "field" }, el("span", { className: "field-label", text: "Watch Paths" }), pathsIn);
        const debounceField = el("div", { className: "field" }, el("span", { className: "field-label", text: "Debounce Window" }), debounceIn);
        const cmdField = el("div", { className: "field" }, el("span", { className: "field-label", text: "Check Command" }), cmdIn);
        const wakeField = el("div", { className: "field" }, el("span", { className: "field-label", text: "Wake Condition" }), wakeSel);
        const srcField = el("div", { className: "field" }, el("span", { className: "field-label", text: "Webhook Source Label" }), srcIn);
        const filterField = el("div", { className: "field" }, el("span", { className: "field-label", text: "Event Filter (optional)" }), filterIn);
        const notifyTargetField = el("div", { className: "field" }, el("span", { className: "field-label", text: "Delivery Target" }), notifyTargetIn);

        const syncTriggerFields = () => {
          const tp = typeSel.value;
          const isSched = tp === "cron" || tp === "watch" || tp === "sentinel";
          const isCmd = tp === "watch" || tp === "sentinel";
          const isFS = tp === "fsnotify";
          const isHook = tp === "webhook";
          schedPresetField.classList.toggle("hidden", !isSched);
          intField.classList.toggle("hidden", !isSched);
          pathsField.classList.toggle("hidden", !isFS);
          debounceField.classList.toggle("hidden", !isFS);
          cmdField.classList.toggle("hidden", !isCmd);
          wakeField.classList.toggle("hidden", !isCmd);
          srcField.classList.toggle("hidden", !isHook);
          filterField.classList.toggle("hidden", !isHook);
          notifyTargetField.classList.toggle("hidden", !notifyChSel.value);
        };
        syncTriggerFields();
        typeSel.addEventListener("change", syncTriggerFields);
        notifyChSel.addEventListener("change", syncTriggerFields);

        openModal(isEdit ? `Edit Automation: ${idIn.value}` : "New Automation", [
          el("div", { className: "form-grid" },
            el("div", { className: "field" }, el("span", { className: "field-label", text: "Automation ID" }), idIn),
            el("div", { className: "field" }, el("span", { className: "field-label", text: "Trigger Type" }), typeSel),
            schedPresetField,
            intField,
            pathsField,
            debounceField,
            cmdField,
            wakeField,
            srcField,
            filterField,
            el("div", { className: "field" }, el("span", { className: "field-label", text: "Action Mode" }), capSel),
            el("div", { className: "field" }, el("span", { className: "field-label", text: "Urgency" }), urgSel),
            el("div", { className: "field" }, el("span", { className: "field-label", text: "Deliver Result To" }), notifyChSel),
            notifyTargetField,
            el("div", { className: "field full" }, el("span", { className: "field-label", text: "Instructions / Prompt" }), promptIn)
          ),
          el("div", { className: "connector-actions" },
            el("button", { className: "btn-secondary", type: "button", text: "Cancel", onClick: () => closeModal(false) }),
            el("button", {
              className: "btn-primary",
              type: "button",
              text: isEdit ? "Save Changes" : "Create Automation",
              onClick: async () => {
                const id = idIn.value.trim();
                if (!id) { showModalError("Automation ID is required"); return; }
                const tp = typeSel.value;
                const spec = {
                  ...(existing || {}),
                  ID: id,
                  Type: tp,
                  Interval: (tp === "cron" || tp === "watch" || tp === "sentinel") ? intIn.value.trim() : "",
                  Paths: tp === "fsnotify" ? pathsIn.value.split(",").map((s) => s.trim()).filter(Boolean) : undefined,
                  Debounce: tp === "fsnotify" ? debounceIn.value.trim() : "",
                  Command: (tp === "watch" || tp === "sentinel") ? cmdIn.value.trim() : "",
                  WakeOn: (tp === "watch" || tp === "sentinel") ? wakeSel.value : "",
                  Source: tp === "webhook" ? srcIn.value.trim() : "",
                  Filter: tp === "webhook" ? filterIn.value.trim() : "",
                  Capability: capSel.value,
                  Urgency: urgSel.value,
                  NotifyChannel: notifyChSel.value,
                  NotifyTarget: notifyChSel.value ? notifyTargetIn.value.trim() : "",
                  Prompt: promptIn.value.trim(),
                  Enabled: existing ? isEnabled(existing) : true,
                };
                const next = isEdit
                  ? triggersList.map((item, idx) => (idx === editIdx ? spec : item))
                  : [...triggersList, spec];
                try {
                  await saveTriggersPayload(next, isEdit ? `Updated ${id}` : `Created ${id}`);
                  closeModal(true);
                } catch (e) {
                  showModalError(e.message);
                }
              },
            })
          ),
        ]);
      };

      feed.appendChild(
        el("div", { className: "section-header" },
          el("div", null,
            el("div", { className: "connector-title-row" },
              el("h2", { className: "section-title", text: "Automations" }),
              el("span", {
                className: "pill " + (har.daemon_running ? "ok" : "warn"),
                text: har.daemon_running ? "Background Scheduler Active" : "Scheduler Standby",
              })
            ),
            el("div", { className: "section-sub", text: "Run scheduled tasks (intervals or 5-field cron), watch files, or receive webhooks at /webhooks/triggers/<id>." })
          ),
          el("button", { className: "btn-primary", type: "button", text: "+ New Automation", onClick: () => openTriggerModal(null, -1) })
        )
      );

      const typeLabel = {
        cron: "Schedule",
        fsnotify: "File Watch",
        watch: "Command Watch",
        sentinel: "Sentinel",
        webhook: "Webhook",
      };

      const capLabel = {
        notify_only: "AI Check",
        read_only: "AI Check",
        draft_only: "Draft Changes",
        alert_only: "Alert Only",
      };

      if (triggersList.length === 0) {
        feed.appendChild(el("div", { className: "card muted", text: "No automations configured yet. Click + New Automation to schedule a recurring check or watch files/webhooks." }));
      } else {
        const grid = el("div", { className: "card-grid-2" });
        for (let i = 0; i < triggersList.length; i++) {
          const t = triggersList[i];
          const id = tVal(t, "ID", "id") || `trigger-${i + 1}`;
          const tp = tVal(t, "Type", "type") || "cron";
          const active = isEnabled(t);
          const interval = tVal(t, "Interval", "interval");
          const paths = tVal(t, "Paths", "paths") || [];
          const cmd = tVal(t, "Command", "command");
          const cap = tVal(t, "Capability", "capability") || "notify_only";
          const nCh = tVal(t, "NotifyChannel", "notify_channel");
          const nTgt = tVal(t, "NotifyTarget", "notify_target");
          const prompt = tVal(t, "Prompt", "prompt") || cmd || "";
          const lr = lastRuns[id];

          const whenParts = [];
          if (interval) whenParts.push(`Schedule: ${interval}`);
          if (paths.length) whenParts.push(`Paths: ${paths.join(", ")}`);
          if (tp === "webhook") whenParts.push(`POST /webhooks/triggers/${id}`);
          if (nCh) whenParts.push(`Deliver → ${nCh}${nTgt ? ":" + nTgt : ""}`);
          if (lr?.status) whenParts.push(`Last: ${lr.status}`);

          grid.appendChild(
            el("div", { className: "card connector-card" },
              el("div", { className: "connector-info" },
                el("div", { className: "connector-title-row" },
                  el("span", { className: "status-dot " + (active ? "ok" : "") }),
                  el("span", { className: "connector-name", text: id }),
                  el("span", { className: "pill", text: typeLabel[tp] || tp }),
                  el("span", { className: "pill " + (active ? "ok" : "warn"), text: active ? "Enabled" : "Paused" }),
                  el("span", { className: "pill", text: capLabel[cap] || "AI Check" })
                ),
                whenParts.length ? el("div", { className: "connector-meta", text: whenParts.join(" · ") }) : null,
                prompt ? el("div", { className: "connector-desc", text: prompt }) : null
              ),
              el("div", { className: "connector-actions" },
                el("button", {
                  className: "btn-primary btn-sm",
                  type: "button",
                  text: "Run Now",
                  onClick: async () => {
                    try {
                      const res = await apiPost(`/api/harness/triggers/${encodeURIComponent(id)}/run`, {});
                      appState.selectedRunId = res.run_id;
                      appState.selectedBranchId = res.branch_id;
                      await refreshAll();
                      switchView("tasks");
                      showToast(`Started automation "${id}"`);
                    } catch (e) { showErrorToast(e); }
                  },
                }),
                el("button", {
                  className: "btn-secondary btn-sm",
                  type: "button",
                  text: active ? "Pause" : "Resume",
                  onClick: async () => {
                    try {
                      const next = triggersList.map((item, idx) => idx === i ? { ...item, Enabled: !active } : item);
                      await saveTriggersPayload(next, `${id} ${!active ? "resumed" : "paused"}`);
                    } catch (e) { showErrorToast(e); }
                  },
                }),
                el("button", {
                  className: "btn-secondary btn-sm",
                  type: "button",
                  text: "Edit",
                  onClick: () => openTriggerModal(t, i),
                }),
                el("button", {
                  className: "btn-danger btn-sm",
                  type: "button",
                  text: "Remove",
                  onClick: () => confirmModal(
                    `Remove Automation (${id})`,
                    `Delete automation "${id}"?`,
                    "Remove Automation",
                    async () => {
                      const next = triggersList.filter((_, idx) => idx !== i);
                      await saveTriggersPayload(next, `Removed ${id}`);
                    }
                  ),
                })
              )
            )
          );
        }
        feed.appendChild(grid);
      }

      feed.appendChild(
        el("div", { className: "card card-spaced" },
          el("div", { className: "connector-title-row" },
            el("span", { className: "connector-name", text: "Safety Guardrails" }),
            el("span", { className: "pill", text: ".soma/triggers.yaml" })
          ),
          el("p", { className: "connector-desc", text: "Automatic brakes that pause background automations overnight, when approvals queue up, or when daily automated spend hits your limit." }),
          el("div", { className: "form-grid" },
            el("div", { className: "field" }, el("span", { className: "field-label", text: "Quiet Hours (HH:MM-HH:MM)" }), quietIn),
            el("div", { className: "field" }, el("span", { className: "field-label", text: "Max Queued Approvals" }), maxInbIn),
            el("div", { className: "field" }, el("span", { className: "field-label", text: "Daily Automated Spend Limit (USD, 0 = off)" }), budIn),
            el("div", { className: "field" }, el("span", { className: "field-label", text: "Standalone Daemon Webhook Port" }), hookIn),
            el("div", { className: "field" }, el("span", { className: "field-label", text: "Cleanup / GC Interval" }), gcIn)
          ),
          el("div", { className: "connector-actions" },
            el("button", {
              className: "btn-secondary",
              type: "button",
              text: "Save Guardrails",
              onClick: async () => {
                try {
                  await saveTriggersPayload(triggersList, "Safety guardrails saved");
                } catch (e) { showErrorToast(e); }
              },
            })
          )
        )
      );
    }
  }

  function openApprovalInspectModal(item) {
    const ref = String(item.artifact_ref || "").trim();
    if (ref.startsWith("@page:")) {
      const pageId = ref.slice(6).split("#")[0].trim();
      openPageModal(pageId, 1);
      return;
    }
    const detailBlocks = [
      el("div", { className: "inline-row muted caption-text" },
        el("span", { className: "pill", text: item.item_kind }),
        el("span", { className: "pill", text: item.status }),
        item.run_title ? el("span", { text: `Task: ${item.run_title}` }) : null
      ),
      item.summary ? el("p", { className: "connector-desc", text: item.summary }) : null,
    ];
    if (item.action_tool) {
      let formattedArgs = item.action_args || "{}";
      try {
        formattedArgs = JSON.stringify(JSON.parse(formattedArgs), null, 2);
      } catch (_) {}
      detailBlocks.push(
        el("div", { className: "field full" },
          el("span", { className: "field-label", text: `Tool Action: ${item.action_tool}` }),
          el("pre", { className: "code-block", text: formattedArgs })
        )
      );
    } else if (item.proposed_content) {
      detailBlocks.push(
        el("div", { className: "field full" },
          el("span", { className: "field-label", text: "Proposed Memory Rule / Fact" }),
          el("pre", { className: "code-block", text: item.proposed_content })
        )
      );
    } else if (ref) {
      detailBlocks.push(el("pre", { className: "code-block", text: ref }));
    }
    openModal(item.title || "Approval Details", detailBlocks, { wide: true, skipDirtyTracking: true });
  }

  // Approvals View
  function renderApprovalsView() {
    const feed = document.getElementById("approvalsFeed");
    feed.replaceChildren();
    document.querySelectorAll("[data-app-tab]").forEach((b) => {
      b.classList.toggle("active", b.dataset.appTab === appState.approvalsTab);
    });
    const allItems = appState.state?.inbox || [];
    const items = allItems.filter((item) => {
      if (appState.approvalsTab === "resolved") return item.status !== "pending";
      if (appState.approvalsTab === "all") return true;
      return item.status === "pending";
    });
    if (items.length === 0) {
      const msg = appState.approvalsTab === "resolved"
        ? "No resolved approvals yet."
        : appState.approvalsTab === "all"
          ? "No approvals or proposals recorded yet."
          : "No pending approvals — all tasks are clear.";
      feed.appendChild(el("div", { className: "card muted", text: msg }));
      return;
    }

    const kindNames = {
      approval: "Action Approval",
      memory_proposal: "Memory Proposal",
      reconcile: "Crash Recovery",
      draft_pr: "Draft Proposal",
      report: "Automation Report",
      alert: "Alert",
      skill_patch: "Skill Patch",
    };

    for (const item of items) {
      const statusCls = item.status === "pending" ? "warn" : (item.status === "approved" ? "ok" : "");
      const canInspect = !!(item.action_tool || item.proposed_content || item.artifact_ref);
      feed.appendChild(
        el("div", { className: "card" },
          el("div", { className: "step-head" },
            el("div", { className: "connector-title-row" },
              el("span", { className: "connector-name", text: item.title }),
              el("span", { className: "pill " + statusCls, text: `${kindNames[item.item_kind] || item.item_kind} · ${item.status}` }),
              item.urgency === "urgent" ? el("span", { className: "pill err", text: "Urgent" }) : null
            ),
            item.run_id
              ? el("button", {
                  className: "btn-secondary btn-sm",
                  type: "button",
                  text: item.run_title ? `Open "${item.run_title}" →` : "Open Task →",
                  onClick: () => {
                    appState.selectedRunId = item.run_id;
                    if (item.branch_id) appState.selectedBranchId = item.branch_id;
                    switchView("tasks");
                  },
                })
              : null
          ),
          el("div", { className: "step-body", text: item.proposed_content || item.summary }),
          item.action_tool
            ? el("div", { className: "connector-meta", text: `Tool: ${item.action_tool} ${item.action_args || ""}` })
            : null,
          (item.status === "pending" || canInspect)
            ? el("div", { className: "connector-actions" },
                canInspect
                  ? el("button", {
                      className: "btn-secondary btn-sm",
                      type: "button",
                      text: "Inspect Details",
                      onClick: () => openApprovalInspectModal(item),
                    })
                  : null,
                item.status === "pending"
                  ? el("button", {
                      className: "btn-primary btn-sm",
                      type: "button",
                      text: (item.item_kind === "approval" || item.item_kind === "reconcile") ? "Approve & Resume" : "Approve",
                      onClick: async () => {
                        try {
                          const res = await apiPost(`/api/inbox/${encodeURIComponent(item.id)}/approve`, {});
                          await refreshAll();
                          showToast(res.resumed ? "Approved — task resumed automatically" : "Approved");
                        } catch (e) { showErrorToast(e); }
                      },
                    })
                  : null,
                item.status === "pending"
                  ? el("button", {
                      className: "btn-secondary btn-sm",
                      type: "button",
                      text: "Dismiss",
                      onClick: async () => {
                        try {
                          await apiPost(`/api/inbox/${encodeURIComponent(item.id)}/dismiss`, {});
                          await refreshAll();
                          showToast("Dismissed");
                        } catch (e) { showErrorToast(e); }
                      },
                    })
                  : null
              )
            : null
        )
      );
    }
  }

  // Memory & Skills View
  function renderMemoryView() {
    const feed = document.getElementById("memoryFeed");
    feed.replaceChildren();
    const q = (document.getElementById("memorySearch").value || "").trim().toLowerCase();
    if (!appState.memFilter) appState.memFilter = "all";

    const allMems = appState.state?.memory || [];
    const mems = allMems.filter((m) => {
      const matchQ = !q || m.content.toLowerCase().includes(q) || m.kind.toLowerCase().includes(q) || (m.scope || "").toLowerCase().includes(q);
      if (!matchQ) return false;
      switch (appState.memFilter) {
        case "active": return m.status === "active" && m.enabled;
        case "proposed": return m.status !== "active";
        case "rule":
        case "fact":
        case "preference": return m.kind === appState.memFilter;
        default: return true;
      }
    });
    const skills = (appState.state?.skills || []).filter((s) =>
      !q || s.name.toLowerCase().includes(q) || (s.description || "").toLowerCase().includes(q)
    );

    feed.appendChild(
      el("div", { className: "section-header" },
        el("div", null,
          el("h2", { className: "section-title", text: "Persistent Memory Rules & Facts" }),
          el("div", { className: "section-sub", text: "Instructions, preferences, and facts automatically injected into soma's context." })
        ),
        el("span", { className: "section-sub", text: `${mems.length} of ${allMems.length} entries` })
      )
    );
    const filterBar = el("div", { className: "subtabs subtabs-wrap" });
    for (const [id, label] of [["all", "All"], ["active", "Active"], ["proposed", "Pending Review"], ["rule", "Rules"], ["fact", "Facts"], ["preference", "Preferences"]]) {
      filterBar.appendChild(el("button", {
        className: "subtab" + (appState.memFilter === id ? " active" : ""),
        type: "button",
        text: label,
        onClick: () => { appState.memFilter = id; renderMemoryView(); },
      }));
    }
    feed.appendChild(filterBar);

    if (mems.length === 0) {
      feed.appendChild(el("div", { className: "card muted", text: "No memory entries match your filter. Click + Add Memory to save a persistent rule, fact, or preference." }));
    } else {
      for (const m of mems) {
        const effectiveStatusText = !m.enabled ? "Disabled" : (m.status === "active" ? "Active" : m.status);
        const effectiveStatusCls = !m.enabled ? "" : (m.status === "active" ? "ok" : "warn");
        const scopeLabel = m.scope === "global" ? "Global" : (m.scope === "workspace" || (m.scope || "").startsWith("workspace:") ? "This Workspace" : m.scope);
        feed.appendChild(
          el("div", { className: "card connector-card" },
            el("div", { className: "connector-info" },
              el("div", { className: "connector-title-row" },
                el("span", { className: "pill", text: m.kind }),
                el("span", { className: "pill " + effectiveStatusCls, text: effectiveStatusText }),
                scopeLabel ? el("span", { className: "pill", text: scopeLabel }) : null,
                m.replaces_id ? el("span", { className: "pill", text: `Replaces ${m.replaces_id}` }) : null
              ),
              el("div", { className: "step-body", text: m.content })
            ),
            el("div", { className: "connector-actions" },
              m.status !== "active"
                ? el("button", {
                    className: "btn-primary btn-sm",
                    type: "button",
                    text: "Approve & Activate",
                    onClick: async () => {
                      try {
                        await apiPost(`/api/memory/${encodeURIComponent(m.id)}/activate`, {});
                        await refreshAll();
                        showToast("Memory entry activated");
                      } catch (e) { showErrorToast(e); }
                    },
                  })
                : null,
              el("button", {
                className: "btn-secondary btn-sm",
                type: "button",
                text: m.enabled ? "Disable" : "Enable",
                onClick: async () => {
                  try {
                    await apiPost(`/api/memory/${encodeURIComponent(m.id)}/toggle`, { enabled: !m.enabled });
                    await refreshAll();
                    showToast(`Memory entry ${!m.enabled ? "enabled" : "disabled"}`);
                  } catch (e) { showErrorToast(e); }
                },
              }),
              el("button", {
                className: "btn-secondary btn-sm",
                type: "button",
                text: "Edit",
                onClick: () => openEditMemoryModal(m),
              }),
              el("button", {
                className: "btn-danger btn-sm",
                type: "button",
                text: "Delete",
                onClick: () => confirmModal(
                  "Delete Memory Entry",
                  `Delete this ${m.kind} permanently?`,
                  "Delete Memory",
                  async () => {
                    await apiPost(`/api/memory/${encodeURIComponent(m.id)}/delete`, {});
                    await refreshAll();
                    showToast("Memory entry deleted");
                  }
                ),
              })
            )
          )
        );
      }
    }

    feed.appendChild(
      el("div", { className: "section-header card-spaced" },
        el("div", null,
          el("h2", { className: "section-title", text: "Agent Skills (SKILL.md)" }),
          el("div", { className: "section-sub", text: "Reusable workflow instructions discovered in .soma/skills, .agents/skills, or .skills." })
        ),
        el("span", { className: "section-sub", text: `${skills.length} skill(s)` })
      )
    );
    if (skills.length === 0) {
      feed.appendChild(el("div", { className: "card muted", text: "No skills installed yet. Click + New Skill to author a reusable SKILL.md playbook." }));
    } else {
      const grid = el("div", { className: "card-grid-2" });
      for (const sk of skills) {
        const bCls = sk.badge === "CLEAN" ? "ok" : sk.badge === "WARN" ? "warn" : "err";
        const bText = sk.badge === "CLEAN" ? "Verified" : sk.badge === "WARN" ? `${sk.lint_count} Warning(s)` : "Blocked by Security Lint";
        grid.appendChild(
          el("div", { className: "card connector-card" },
            el("div", { className: "connector-info" },
              el("div", { className: "connector-title-row" },
                el("span", { className: "connector-name", text: sk.name }),
                el("span", { className: "pill " + bCls, text: bText })
              ),
              el("p", { className: "connector-desc", text: sk.description || "No description" })
            ),
            el("div", { className: "connector-actions" },
              el("button", {
                className: "btn-secondary btn-sm",
                type: "button",
                text: "Edit",
                onClick: () => openInspectSkillModal(sk.name),
              }),
              el("button", {
                className: "btn-danger btn-sm",
                type: "button",
                text: "Delete",
                onClick: () => confirmModal(
                  `Delete Skill (${sk.name})`,
                  `Delete skill "${sk.name}" and its directory from the workspace?`,
                  "Delete Skill",
                  async () => {
                    await apiPost(`/api/skills/${encodeURIComponent(sk.name)}/delete`, {});
                    await refreshAll();
                    showToast(`Deleted skill ${sk.name}`);
                  }
                ),
              })
            )
          )
        );
      }
      feed.appendChild(grid);
    }
  }

  function openEditMemoryModal(m) {
    const contentIn = el("textarea", { className: "field-textarea", rows: 5, value: m.content || "" });
    openModal(`Edit Memory (${m.kind || "rule"})`, [
      el("div", { className: "connector-title-row" },
        el("span", { className: "pill", text: m.kind || "fact" }),
        m.scope ? el("span", { className: "pill", text: m.scope }) : null
      ),
      el("div", { className: "field full" }, el("span", { className: "field-label", text: "Content" }), contentIn),
      el("div", { className: "connector-actions" },
        el("button", { className: "btn-secondary", type: "button", text: "Cancel", onClick: () => closeModal(false) }),
        el("button", {
          className: "btn-primary",
          type: "button",
          text: "Save Changes",
          onClick: async () => {
            try {
              await apiPost(`/api/memory/${encodeURIComponent(m.id)}/update`, { content: contentIn.value.trim() });
              closeModal(true);
              await refreshAll();
              showToast("Memory updated");
            } catch (e) { showModalError(e.message); }
          },
        })
      ),
    ]);
  }

  function openAddMemoryModal() {
    const scopeSel = el("select", { className: "select-input" },
      el("option", { value: "global", text: "Global (all tasks in this store)" }),
      el("option", { value: "workspace", text: "This Workspace Only" })
    );
    const kindSel = el("select", { className: "select-input" },
      el("option", { value: "rule", text: "Rule (must follow)" }),
      el("option", { value: "preference", text: "Preference (style / workflow)" }),
      el("option", { value: "fact", text: "Fact (project context)" })
    );
    const contentIn = el("textarea", { className: "field-textarea", rows: 4, placeholder: "e.g. Always run `go test -race ./...` before finishing code changes." });

    openModal("Add Memory Rule / Fact", [
      el("div", { className: "form-grid" },
        el("div", { className: "field" }, el("span", { className: "field-label", text: "Scope" }), scopeSel),
        el("div", { className: "field" }, el("span", { className: "field-label", text: "Type" }), kindSel),
        el("div", { className: "field full" }, el("span", { className: "field-label", text: "Instruction or Fact" }), contentIn)
      ),
      el("div", { className: "connector-actions" },
        el("button", { className: "btn-secondary", type: "button", text: "Cancel", onClick: () => closeModal(false) }),
        el("button", {
          className: "btn-primary",
          type: "button",
          text: "Save Memory",
          onClick: async () => {
            if (!contentIn.value.trim()) { showModalError("Memory content is required"); return; }
            try {
              await apiPost("/api/memory", {
                scope: scopeSel.value,
                kind: kindSel.value,
                content: contentIn.value.trim(),
              });
              closeModal(true);
              await refreshAll();
              showToast("Memory saved");
            } catch (e) { showModalError(e.message); }
          },
        })
      ),
    ]);
  }

  function openNewSkillModal() {
    const nameIn = el("input", { className: "field-input", type: "text", placeholder: "e.g. deploy-check (lowercase letters, digits, hyphens)" });
    const descIn = el("input", { className: "field-input", type: "text", placeholder: "When should soma activate this skill?" });
    const bodyIn = el("textarea", {
      className: "field-textarea mono-box",
      rows: 10,
      value: "---\nname: deploy-check\ndescription: Describe when to use this skill.\n---\n\n# Instructions\n\n1. Step one\n",
    });

    const syncFrontmatter = () => {
      const cleanName = nameIn.value.trim().toLowerCase().replace(/[^a-z0-9-]+/g, "-").replace(/^-+|-+$/g, "") || "my-skill";
      const cleanDesc = descIn.value.trim();
      let cur = bodyIn.value;
      cur = cur.replace(/^(\s*---\r?\n[\s\S]*?^name:\s*)([^\r\n]+)/m, `$1${cleanName}`);
      if (cleanDesc) {
        cur = cur.replace(/^(\s*---\r?\n[\s\S]*?^description:\s*)([^\r\n]+)/m, `$1${cleanDesc}`);
      }
      bodyIn.value = cur;
    };
    nameIn.addEventListener("input", syncFrontmatter);
    descIn.addEventListener("input", syncFrontmatter);

    openModal("Create Agent Skill (SKILL.md)", [
      el("div", { className: "form-grid" },
        el("div", { className: "field" }, el("span", { className: "field-label", text: "Skill Name" }), nameIn),
        el("div", { className: "field" }, el("span", { className: "field-label", text: "Short Description" }), descIn),
        el("div", { className: "field full" }, el("span", { className: "field-label", text: "SKILL.md Content" }), bodyIn)
      ),
      el("div", { className: "connector-actions" },
        el("button", { className: "btn-secondary", type: "button", text: "Cancel", onClick: () => closeModal(false) }),
        el("button", {
          className: "btn-primary",
          type: "button",
          text: "Create Skill",
          onClick: async () => {
            const cleanName = nameIn.value.trim().toLowerCase().replace(/[^a-z0-9-]+/g, "-").replace(/^-+|-+$/g, "");
            if (!cleanName) { showModalError("Skill name is required (e.g. deploy-check)"); return; }
            syncFrontmatter();
            try {
              await apiPost("/api/skills", {
                name: cleanName,
                content: bodyIn.value,
              });
              closeModal(true);
              await refreshAll();
              showToast(`Created skill ${cleanName}`);
            } catch (e) { showModalError(e.message); }
          },
        })
      ),
    ], { wide: true });
  }

  async function openInspectSkillModal(name) {
    try {
      const data = await apiGet(`/api/skills/${encodeURIComponent(name)}`);
      const findings = data.lint_findings || [];
      const fallbackRaw = `---\nname: ${name}\ndescription: ${data.skill?.Description || name}\n---\n\n${data.skill?.Body || data.skill?.body || ""}`;
      const editorIn = el("textarea", {
        className: "field-textarea mono-box",
        rows: 14,
        value: data.raw_content || fallbackRaw,
      });
      openModal(`Edit Skill: ${name}`, [
        el("div", { className: "connector-title-row" },
          el("span", { className: "pill ok", text: data.path || name }),
          el("span", { className: "pill " + (findings.length ? "warn" : "ok"), text: findings.length ? `${findings.length} security lint warning(s)` : "Security lint clean" })
        ),
        findings.length ? el("pre", { className: "code-block", text: JSON.stringify(findings, null, 2) }) : null,
        el("div", { className: "field full" }, el("span", { className: "field-label", text: "SKILL.md" }), editorIn),
        el("div", { className: "connector-actions" },
          el("button", { className: "btn-secondary", type: "button", text: "Close", onClick: () => closeModal(false) }),
          el("button", {
            className: "btn-primary",
            type: "button",
            text: "Save SKILL.md",
            onClick: async () => {
              try {
                await apiPost("/api/skills", { name, content: editorIn.value });
                closeModal(true);
                await refreshAll();
                showToast(`Saved skill ${name}`);
              } catch (e) { showModalError(e.message); }
            },
          })
        ),
      ], { wide: true });
    } catch (e) { showErrorToast(e); }
  }

  // Mobile Model & Execution Profile Selector Modal
  function openMobileModelModal() {
    const accs = appState.harness?.accounts || [];
    const modSel = el("select", { className: "select-input" },
      el("option", { value: "", text: `Auto (${resolveEffectiveModelLabel()})`, selected: appState.selectedAccountId === "" }),
      ...accs.map((a) => el("option", {
        value: a.id,
        text: `${a.id} · ${a.model_id || a.provider}`,
        selected: a.id === appState.selectedAccountId,
      }))
    );
    const profSel = el("select", { className: "select-input" },
      el("option", { value: "worker", text: "Worker — Fast, scoped execution", selected: appState.profile === "worker" }),
      el("option", { value: "frontier", text: "Frontier — Deep multi-step reasoning", selected: appState.profile === "frontier" })
    );

    openModal("Model & Execution Depth", [
      el("div", { className: "form-grid" },
        el("div", { className: "field full" }, el("span", { className: "field-label", text: "AI Model Profile" }), modSel),
        el("div", { className: "field full" }, el("span", { className: "field-label", text: "Execution Depth" }), profSel)
      ),
      el("div", { className: "connector-actions" },
        el("button", {
          className: "btn-secondary",
          type: "button",
          text: "Manage Models…",
          onClick: () => {
            closeModal(true);
            appState.harnessTab = "provider";
            switchView("harness");
          },
        }),
        el("button", {
          className: "btn-primary",
          type: "button",
          text: "Apply",
          onClick: () => {
            appState.selectedAccountId = modSel.value || "";
            appState.profile = profSel.value;
            closeModal(true);
            renderChrome();
          },
        })
      ),
    ], { skipDirtyTracking: true });
  }

  function autoResizeGoalInput() {
    const goalInput = document.getElementById("goalInput");
    if (!goalInput) return;
    goalInput.style.height = "auto";
    goalInput.style.height = `${Math.min(168, goalInput.scrollHeight)}px`;
  }

  // Event Wiring
  function wireEvents() {
    document.getElementById("toastCloseBtn").addEventListener("click", () => {
      clearTimeout(toastTimer);
      document.getElementById("toast").classList.add("hidden");
    });

    document.getElementById("collapseSidebarBtn").addEventListener("click", () => {
      document.getElementById("appShell").classList.toggle("sidebar-collapsed");
    });

    document.getElementById("mobileMenuBtn").addEventListener("click", openMobileDrawer);
    document.getElementById("mobileCloseBtn").addEventListener("click", closeMobileDrawer);
    document.getElementById("sidebarBackdrop").addEventListener("click", closeMobileDrawer);
    document.getElementById("mobileDesktopBtn").addEventListener("click", () => switchView("desktop"));
    document.getElementById("mobileModelPill").addEventListener("click", openMobileModelModal);

    document.getElementById("logoutBtn").addEventListener("click", async () => {
      try {
        await apiPost("/api/auth/logout", {});
        showToast("Signed out");
        openAuthModal();
      } catch (e) { showErrorToast(e); }
    });

    const startNewTask = () => {
      appState.selectedRunId = null;
      appState.selectedBranchId = null;
      renderChrome();
      switchView("tasks");
      document.getElementById("goalInput").focus();
    };
    document.getElementById("newTaskBtn").addEventListener("click", startNewTask);
    document.getElementById("brandHomeBtn").addEventListener("click", startNewTask);
    document.getElementById("mobileNewTaskBtn").addEventListener("click", startNewTask);

    document.querySelectorAll(".nav-item").forEach((btn) => {
      btn.addEventListener("click", () => {
        if (btn.dataset.view === "tasks" && !appState.selectedRunId && appState.state?.runs?.length) {
          // Keep current selectedRunId if set; if none selected and user clicks Tasks while on another tab, show latest or home cleanly.
        }
        switchView(btn.dataset.view);
      });
    });

    // Starter prompt chips
    document.querySelectorAll(".starter-chip").forEach((chip) => {
      chip.addEventListener("click", () => {
        const goalInput = document.getElementById("goalInput");
        goalInput.value = chip.dataset.prompt || "";
        document.getElementById("goalForm").classList.toggle("has-text", goalInput.value.trim().length > 0);
        autoResizeGoalInput();
        goalInput.focus();
      });
    });

    // Composer popovers
    const toolsMenu = document.getElementById("composerToolsMenu");
    const modeMenu = document.getElementById("composerModeMenu");
    document.getElementById("composerPlusBtn").addEventListener("click", (e) => {
      e.stopPropagation();
      modeMenu.classList.add("hidden");
      toolsMenu.classList.toggle("hidden");
    });
    document.getElementById("composerModeBtn").addEventListener("click", (e) => {
      e.stopPropagation();
      toolsMenu.classList.add("hidden");
      modeMenu.classList.toggle("hidden");
    });
    document.addEventListener("click", () => {
      toolsMenu.classList.add("hidden");
      modeMenu.classList.add("hidden");
    });

    modeMenu.querySelectorAll("[data-profile]").forEach((btn) => {
      btn.addEventListener("click", () => {
        appState.profile = btn.dataset.profile;
        renderChrome();
        modeMenu.classList.add("hidden");
      });
    });

    document.getElementById("quickDesktopToggle").addEventListener("click", () => {
      appState.splitDesktopOpen = !appState.splitDesktopOpen;
      if (appState.view !== "tasks") switchView("tasks");
      else renderSplitDesktop();
    });
    document.getElementById("quickOpenConnectors").addEventListener("click", () => switchView("connectors"));
    document.getElementById("quickOpenHarness").addEventListener("click", () => switchView("harness"));

    // Multi-line Composer Form & Stop/Send Handling
    const goalInput = document.getElementById("goalInput");
    const goalForm = document.getElementById("goalForm");
    goalInput.addEventListener("input", () => {
      goalForm.classList.toggle("has-text", goalInput.value.trim().length > 0);
      autoResizeGoalInput();
    });
    goalInput.addEventListener("keydown", (e) => {
      if (e.key === "Enter" && !e.shiftKey && !e.isComposing) {
        e.preventDefault();
        goalForm.requestSubmit();
      }
    });

    goalForm.addEventListener("submit", async (e) => {
      e.preventDefault();
      const activeBranch = getSelectedBranch(getSelectedRun());
      if (activeBranch && activeBranch.status === "running") {
        try {
          await apiPost(`/api/branches/${encodeURIComponent(activeBranch.id)}/stop`, {});
          await refreshAll();
          showToast("Task stopped");
        } catch (err) {
          showErrorToast(err);
        }
        return;
      }

      const goal = goalInput.value.trim();
      if (!goal) return;
      try {
        if (activeBranch) {
          const res = await apiPost(`/api/branches/${encodeURIComponent(activeBranch.id)}/continue`, {
            goal,
            profile: appState.profile,
            account_id: appState.selectedAccountId || "",
          });
          appState.selectedRunId = res.run_id || appState.selectedRunId;
          appState.selectedBranchId = res.branch_id || activeBranch.id;
        } else {
          const res = await apiPost("/api/runs", {
            goal,
            profile: appState.profile,
            account_id: appState.selectedAccountId || "",
          });
          appState.selectedRunId = res.run_id;
          appState.selectedBranchId = res.branch_id;
        }
        goalInput.value = "";
        goalInput.style.height = "auto";
        goalForm.classList.remove("has-text");
        switchView("tasks");
        await refreshAll();
      } catch (err) {
        showErrorToast(err);
      }
    });

    const composerAccSel = document.getElementById("composerAccountSel");
    if (composerAccSel) {
      composerAccSel.addEventListener("change", (e) => {
        appState.selectedAccountId = e.target.value || "";
        renderChrome();
      });
    }

    // Live Desktop Split Pane Controls
    const toggleSplit = () => {
      appState.splitDesktopOpen = !appState.splitDesktopOpen;
      renderSplitDesktop();
    };
    document.getElementById("floatingDesktopBtn").addEventListener("click", toggleSplit);
    document.getElementById("splitDesktopBtn").addEventListener("click", toggleSplit);
    document.getElementById("splitCloseBtn").addEventListener("click", () => {
      appState.splitDesktopOpen = false;
      renderSplitDesktop();
    });
    document.getElementById("splitExpandDesktopBtn").addEventListener("click", () => {
      appState.splitDesktopOpen = false;
      switchView("desktop");
    });
    document.getElementById("splitLaunchCta").addEventListener("click", async (ev) => {
      const btn = ev.currentTarget;
      btn.disabled = true;
      btn.textContent = "Launching…";
      try {
        const desk = await apiPost("/api/desktop/start", {});
        if (appState.harness) appState.harness.desktop = desk;
        renderChrome();
        renderSplitDesktop();
        showToast("Virtual Desktop running");
      } catch (e) {
        showErrorToast(e);
      } finally {
        btn.disabled = false;
        btn.textContent = "Launch Desktop";
      }
    });

    // Task Header Actions
    document.getElementById("renameTaskBtn").addEventListener("click", () => {
      const r = getSelectedRun();
      if (r) openRenameTaskModal(r);
    });
    const branchSel = document.getElementById("branchSelect");
    if (branchSel) {
      branchSel.addEventListener("change", (e) => {
        if (e.target.value) {
          appState.selectedBranchId = e.target.value;
          syncHash();
          renderChrome();
          renderTasksView();
        }
      });
    }
    document.getElementById("stopBtn").addEventListener("click", async () => {
      const br = getSelectedBranch(getSelectedRun());
      if (!br) return;
      try {
        await apiPost(`/api/branches/${encodeURIComponent(br.id)}/stop`, {});
        await refreshAll();
        showToast("Task stopped");
      } catch (e) { showErrorToast(e); }
    });
    document.getElementById("detailsBtn").addEventListener("click", () => {
      appState.detailsOpen = !appState.detailsOpen;
      renderTasksView();
    });
    document.getElementById("resumeBtn").addEventListener("click", async () => {
      const br = getSelectedBranch(getSelectedRun());
      if (!br) return;
      try {
        await apiPost(`/api/branches/${encodeURIComponent(br.id)}/resume`, {
          profile: appState.profile,
          account_id: appState.selectedAccountId || "",
        });
        await refreshAll();
        showToast("Resumed task");
      } catch (e) { showErrorToast(e); }
    });
    document.getElementById("forkBtn").addEventListener("click", () => {
      const br = getSelectedBranch(getSelectedRun());
      if (br) forkFromStep(br.id, "", 0);
    });
    document.getElementById("verifyBtn").addEventListener("click", async () => {
      const br = getSelectedBranch(getSelectedRun());
      if (!br) return;
      try {
        const res = await apiPost(`/api/branches/${encodeURIComponent(br.id)}/replay`, {});
        if (res.diverged) {
          showToast("Audit warning: step history hash mismatch detected!", true);
        } else {
          showToast(`Audit passed: all ${res.verified_count} step(s) cryptographically verified`);
        }
      } catch (e) { showErrorToast(e); }
    });

    // Virtual Desktop View Controls
    document.getElementById("desktopStartBtn").addEventListener("click", async () => {
      const btn = document.getElementById("desktopStartBtn");
      btn.disabled = true;
      btn.textContent = "Launching…";
      try {
        const desk = await apiPost("/api/desktop/start", {
          resolution: document.getElementById("desktopResSelect").value,
          network_mode: document.getElementById("desktopNetSelect").value,
        });
        if (appState.harness) appState.harness.desktop = desk;
        renderChrome();
        renderDesktopView();
        showToast("Virtual Desktop running");
      } catch (e) {
        showErrorToast(e);
      } finally {
        btn.disabled = false;
        btn.textContent = "Launch Desktop";
      }
    });
    document.getElementById("desktopStopBtn").addEventListener("click", async () => {
      try {
        const desk = await apiPost("/api/desktop/stop", {});
        if (appState.harness) appState.harness.desktop = desk;
        appState.desktopTakeOver = false;
        renderChrome();
        renderDesktopView();
        renderSplitDesktop();
        showToast("Virtual Desktop stopped");
      } catch (e) { showErrorToast(e); }
    });
    const toggleTakeOver = async () => {
      appState.desktopTakeOver = !appState.desktopTakeOver;
      renderDesktopView();
      renderSplitDesktop();
      try {
        await apiPost("/api/desktop/takeover", { active: appState.desktopTakeOver });
      } catch (_) {}
      showToast(
        appState.desktopTakeOver
          ? "Manual control enabled — click Hand Back Control when done"
          : "Returned desktop to View-Only (Agent Control)"
      );
    };
    document.getElementById("desktopTakeOverBtn").addEventListener("click", toggleTakeOver);
    document.getElementById("splitTakeOverBtn").addEventListener("click", toggleTakeOver);
    [document.getElementById("desktopShield"), document.getElementById("splitDesktopShield")].forEach((sh) => {
      sh?.addEventListener("click", () => {
        showToast("View-only mode protects agent work — click Take Over to control");
      });
    });

    // Subtabs & Search
    document.querySelectorAll("[data-conn-tab]").forEach((b) => {
      b.addEventListener("click", () => {
        appState.connTab = b.dataset.connTab;
        syncViewDOM();
        syncHash();
        renderConnectorsView();
      });
    });
    document.getElementById("connectorSearch").addEventListener("input", renderConnectorsView);
    document.getElementById("importMcpBtn").addEventListener("click", openImportMCPModal);
    document.getElementById("addCustomMcpBtn").addEventListener("click", () => openCustomMCPModal(null));

    document.querySelectorAll("[data-app-tab]").forEach((b) => {
      b.addEventListener("click", () => {
        appState.approvalsTab = b.dataset.appTab;
        syncViewDOM();
        syncHash();
        renderApprovalsView();
      });
    });

    document.querySelectorAll("[data-harness-tab]").forEach((b) => {
      b.addEventListener("click", () => {
        appState.harnessTab = b.dataset.harnessTab;
        syncViewDOM();
        syncHash();
        renderHarnessView();
      });
    });

    document.getElementById("memorySearch").addEventListener("input", renderMemoryView);
    document.getElementById("addMemoryBtn").addEventListener("click", openAddMemoryModal);
    document.getElementById("newSkillBtn").addEventListener("click", openNewSkillModal);

    // Modal Escape & Backdrop Guard
    document.getElementById("modalBackdrop").addEventListener("click", (e) => {
      if (e.target.id === "modalBackdrop") closeModal(false);
    });
    document.addEventListener("keydown", (e) => {
      if (e.key === "Escape" && !document.getElementById("modalBackdrop").classList.contains("hidden")) {
        closeModal(false);
      }
    });

    // Browser Back/Forward hash sync
    window.addEventListener("hashchange", () => {
      if (suppressHashEvent) return;
      restoreFromHash();
      syncViewDOM();
      renderChrome();
      renderCurrentView();
    });
  }

  function connectSSE() {
    const es = new EventSource("/api/events");
    es.addEventListener("state", () => {
      // A refresh means a step was committed or a tool started: the streamed text is now part of
      // the recorded trace, so the live buffer starts over.
      liveText.clear();
      refreshAll().catch(() => {});
    });
    es.addEventListener("delta", (ev) => {
      let d;
      try {
        d = JSON.parse(ev.data);
      } catch (_) {
        return;
      }
      if (!d || !d.branch_id || typeof d.text !== "string") return;
      const next = ((liveText.get(d.branch_id) || "") + d.text).slice(-20000);
      liveText.set(d.branch_id, next);
      for (const node of document.querySelectorAll(".live-stream")) {
        if (node.dataset.branch === d.branch_id) {
          node.textContent = next;
          node.scrollIntoView({ block: "nearest" });
        }
      }
    });
    es.onerror = () => {
      // EventSource automatically reconnects.
    };
  }

  initTheme();
  restoreFromHash();
  wireEvents();
  refreshAll().catch((e) => showErrorToast(e));
  connectSSE();
})();

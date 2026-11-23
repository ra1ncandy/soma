(() => {
  "use strict";

  const csrfToken = document.querySelector('meta[name="soma-csrf"]')?.content || "";

  const appState = {
    view: "tasks",
    connTab: "all",
    harnessTab: "provider",
    profile: "worker",
    selectedRunId: null,
    selectedBranchId: null,
    detailsOpen: false,
    state: null,
    harness: null,
  };

  // DOM helpers
  function el(tag, attrs, ...children) {
    const node = document.createElement(tag);
    if (attrs) {
      for (const [k, v] of Object.entries(attrs)) {
        if (v === undefined || v === null) continue;
        if (k === "className") node.className = v;
        else if (k === "text") node.textContent = v;
        else if (k.startsWith("on") && typeof v === "function") {
          node.addEventListener(k.slice(2).toLowerCase(), v);
        } else if (k === "checked" || k === "disabled" || k === "selected") {
          node[k] = Boolean(v);
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
  function showToast(msg) {
    const t = document.getElementById("toast");
    t.textContent = msg;
    t.classList.remove("hidden");
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => t.classList.add("hidden"), 3200);
  }

  async function apiGet(url) {
    const res = await fetch(url, { credentials: "same-origin" });
    if (!res.ok) {
      const err = await res.json().catch(() => ({}));
      throw new Error(err.error || `HTTP ${res.status}`);
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
      throw new Error(data.error || `HTTP ${res.status}`);
    }
    return data;
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

  function switchView(view) {
    appState.view = view;
    document.querySelectorAll(".nav-item").forEach((b) => {
      b.classList.toggle("active", b.dataset.view === view);
    });
    document.getElementById("viewTasks").classList.toggle("hidden", view !== "tasks");
    document.getElementById("viewDesktop").classList.toggle("hidden", view !== "desktop");
    document.getElementById("viewConnectors").classList.toggle("hidden", view !== "connectors");
    document.getElementById("viewHarness").classList.toggle("hidden", view !== "harness");
    document.getElementById("viewApprovals").classList.toggle("hidden", view !== "approvals");
    document.getElementById("viewMemory").classList.toggle("hidden", view !== "memory");
    closeMobileDrawer();
    renderCurrentView();
  }

  function openModal(title, bodyNodes) {
    const card = document.getElementById("modalCard");
    card.replaceChildren(
      el("div", { className: "modal-head" },
        el("h2", { text: title }),
        el("button", { className: "icon-btn", type: "button", text: "✕", onClick: closeModal })
      ),
      ...bodyNodes
    );
    document.getElementById("modalBackdrop").classList.remove("hidden");
  }

  function closeModal() {
    document.getElementById("modalBackdrop").classList.add("hidden");
  }

  async function refreshAll() {
    const [st, har] = await Promise.all([
      apiGet("/api/state"),
      apiGet("/api/harness").catch(() => null),
    ]);
    appState.state = st;
    if (har) appState.harness = har;
    renderChrome();
    renderCurrentView();
  }

  function renderChrome() {
    const st = appState.state;
    const har = appState.harness;
    if (!st) return;

    const dot = document.getElementById("sandboxDot");
    dot.className = "status-dot " + (st.sandbox_ready ? "ok" : "warn");
    const modeLabel = st.provider_mode === "live" ? (st.model_id || "live") : st.provider_mode;
    document.getElementById("sandboxFooter").textContent = `${st.sandbox_name} · ${modeLabel}`;

    const modelText = st.model_id || modeLabel || "soma";
    document.getElementById("mobileModelLabel").textContent = `${modelText} · ${appState.profile}`;
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
    document.getElementById("desktopNavDot").className =
      "status-dot nav-dot " + (deskRunning ? "ok" : "");
    document.getElementById("mobileDesktopDot").className =
      "status-dot " + (deskRunning ? "ok" : "");
    document.getElementById("quickDesktopState").textContent = deskRunning ? "Running" : "Stopped";

    // Connectors count
    const activeMcps = (har?.mcp_servers || []).filter((m) => m.enabled).length;
    document.getElementById("connectorsCountPill").textContent = String(activeMcps);
    document.getElementById("quickConnectorsCount").textContent = `${activeMcps} active`;

    // Composer hint
    const hint = document.getElementById("composerHint");
    const sendBtn = document.getElementById("startBtn");
    if (st.provider_mode === "read-only") {
      hint.textContent = "Read-only mode: configure a provider in Harness Setup or start `soma web --mock`.";
      hint.classList.remove("hidden");
      sendBtn.disabled = true;
    } else {
      hint.classList.add("hidden");
      sendBtn.disabled = false;
    }

    // Sidebar task list
    const list = document.getElementById("taskList");
    list.replaceChildren();
    for (const run of st.runs || []) {
      const isSelected = run.id === appState.selectedRunId;
      const dotCls =
        run.status === "completed" ? "ok" :
        run.status === "running" ? "running" :
        run.status === "failed" ? "err" : "warn";
      list.appendChild(
        el("button", {
          className: "task-item" + (isSelected ? " active" : ""),
          type: "button",
          onClick: () => {
            appState.selectedRunId = run.id;
            appState.selectedBranchId = run.branches?.[0]?.id || null;
            switchView("tasks");
          },
        },
          el("div", { className: "task-item-top" },
            el("span", { className: "status-dot " + dotCls }),
            el("span", { className: "task-item-title", text: run.title || run.goal || run.id })
          ),
          el("div", { className: "task-item-meta", text: `${run.status} · ${run.branches?.length || 0} branch` })
        )
      );
    }
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

  function renderTasksView() {
    const run = getSelectedRun();
    const branch = getSelectedBranch(run);
    const stage = document.getElementById("tasksStage");
    const hero = document.getElementById("heroEmpty");
    const scroll = document.getElementById("timelineScroll");
    const appShell = document.getElementById("appShell");

    const titleEl = document.getElementById("taskTitle");
    const statusPill = document.getElementById("taskStatusPill");
    const resumeBtn = document.getElementById("resumeBtn");
    const detailsBtn = document.getElementById("detailsBtn");
    const forkBtn = document.getElementById("forkBtn");
    const verifyBtn = document.getElementById("verifyBtn");

    if (!run) {
      appShell.classList.remove("has-active-task");
      stage.classList.add("empty-mode");
      hero.classList.remove("hidden");
      scroll.classList.add("hidden");
      titleEl.textContent = "New Task";
      statusPill.classList.add("hidden");
      [resumeBtn, detailsBtn, forkBtn, verifyBtn].forEach((b) => b.classList.add("hidden"));
      document.getElementById("detailsDrawer").classList.add("hidden");
      return;
    }

    appShell.classList.add("has-active-task");
    stage.classList.remove("empty-mode");
    hero.classList.add("hidden");
    scroll.classList.remove("hidden");

    titleEl.textContent = run.title || run.goal;
    statusPill.textContent = run.status;
    statusPill.className = "pill " + (
      run.status === "completed" ? "ok" :
      run.status === "running" ? "running" :
      run.status === "failed" ? "err" : "warn"
    );

    const canResume = branch && branch.status !== "running";
    resumeBtn.classList.toggle("hidden", !canResume);
    detailsBtn.classList.toggle("hidden", !branch);
    forkBtn.classList.toggle("hidden", !branch);
    verifyBtn.classList.toggle("hidden", !branch);

    // Details drawer
    const drawer = document.getElementById("detailsDrawer");
    if (appState.detailsOpen && branch) {
      drawer.classList.remove("hidden");
      const ft = branch.frame_tokens || { l0: 0, l1: 0, l2: 0, l3: 0, total: 0 };
      drawer.replaceChildren(
        el("div", { className: "details-grid" },
          el("div", { className: "detail-stat" },
            el("div", { className: "detail-stat-label", text: "Prompt / Cached" }),
            el("div", { className: "detail-stat-val", text: `${branch.prompt_tokens} / ${branch.cached_tokens} (${branch.cache_hit_pct.toFixed(0)}%)` })
          ),
          el("div", { className: "detail-stat" },
            el("div", { className: "detail-stat-label", text: "Frame L0–L3" }),
            el("div", { className: "detail-stat-val", text: `${ft.l0}/${ft.l1}/${ft.l2}/${ft.l3}` })
          ),
          el("div", { className: "detail-stat" },
            el("div", { className: "detail-stat-label", text: "Contracts" }),
            el("div", { className: "detail-stat-val", text: `${branch.contracts_passed}/${branch.contracts_total}` })
          ),
          el("div", { className: "detail-stat" },
            el("div", { className: "detail-stat-label", text: "Workspace" }),
            el("div", { className: "detail-stat-val", text: branch.workspace_strategy || "worktree" })
          )
        )
      );
    } else {
      drawer.classList.add("hidden");
    }

    // Timeline feed
    const feed = document.getElementById("timeline");
    feed.replaceChildren();

    feed.appendChild(
      el("div", { className: "step-card" },
        el("div", { className: "step-head" },
          el("span", { className: "step-title", text: "Goal" }),
          el("span", { className: "step-meta", text: `Branch: ${branch?.label || "main"}` })
        ),
        el("div", { className: "step-body", text: run.goal })
      )
    );

    if (branch) {
      for (const st of branch.steps || []) {
        const toolChips = (st.tool_calls || []).map((tc) =>
          el("div", { className: "tool-chip" },
            el("div", { className: "tool-chip-head" },
              el("span", { text: `${tc.name}${tc.label ? " · " + tc.label : ""}` }),
              tc.page_id
                ? el("button", {
                    className: "btn-secondary btn-sm",
                    type: "button",
                    text: `@page:${tc.page_id.slice(0, 8)}`,
                    onClick: () => openPageModal(tc.page_id),
                  })
                : null
            ),
            tc.observation ? el("pre", { className: "tool-obs", text: tc.observation }) : null
          )
        );

        feed.appendChild(
          el("div", { className: "step-card" },
            el("div", { className: "step-head" },
              el("span", { className: "step-title", text: `Turn ${st.turn_index} · ${st.title || "Step"}` }),
              el("span", { className: "step-meta", text: `${st.model_id || ""} · ${st.latency_ms}ms` })
            ),
            st.reasoning ? el("div", { className: "step-body", text: st.reasoning }) : null,
            ...toolChips,
            st.final_summary
              ? el("div", { className: "tool-chip" },
                  el("div", { className: "tool-chip-head" }, el("span", { text: "Final Summary" })),
                  el("div", { className: "step-body", text: st.final_summary })
                )
              : null
          )
        );
      }
    }
  }

  async function openPageModal(pageID) {
    try {
      const pg = await apiGet(`/api/pages/${encodeURIComponent(pageID)}?start=1&end=200`);
      openModal(`@page ${pageID}`, [
        el("div", { className: "muted", text: `${pg.content_type || "text"} · ${pg.line_count} lines · ${pg.byte_size} bytes · trust=${pg.trust_tier}` }),
        el("pre", { className: "code-block", text: pg.content || pg.preview || "" }),
      ]);
    } catch (e) {
      showToast(e.message);
    }
  }

  // Virtual Desktop view
  function renderDesktopView() {
    const desk = appState.harness?.desktop;
    const pill = document.getElementById("desktopStatusPill");
    const startBtn = document.getElementById("desktopStartBtn");
    const stopBtn = document.getElementById("desktopStopBtn");
    const popout = document.getElementById("desktopPopoutLink");
    const placeholder = document.getElementById("desktopPlaceholder");
    const frame = document.getElementById("desktopFrame");
    const meta = document.getElementById("desktopMetaRow");

    if (desk?.running) {
      pill.textContent = `Running · ${desk.resolution || "1280x800"}`;
      pill.className = "pill ok";
      startBtn.classList.add("hidden");
      stopBtn.classList.remove("hidden");
      popout.classList.remove("hidden");
      popout.href = desk.novnc_url || desk.proxy_url;
      placeholder.classList.add("hidden");
      frame.classList.remove("hidden");
      const targetSrc = desk.proxy_url || desk.novnc_url;
      if (targetSrc && frame.getAttribute("data-src") !== targetSrc) {
        frame.setAttribute("data-src", targetSrc);
        frame.src = targetSrc;
      }
    } else {
      pill.textContent = "Stopped";
      pill.className = "pill";
      startBtn.classList.remove("hidden");
      stopBtn.classList.add("hidden");
      popout.classList.add("hidden");
      placeholder.classList.remove("hidden");
      frame.classList.add("hidden");
      if (frame.src) {
        frame.removeAttribute("src");
        frame.removeAttribute("data-src");
      }
      meta.textContent = desk?.container_name ? `Container: ${desk.container_name} (${desk.image || "soma-desktop:latest"})` : "";
    }
  }

  // Connectors view (Gemini /customize/connectors inspired)
  function renderConnectorsView() {
    const har = appState.harness;
    const feed = document.getElementById("connectorsFeed");
    feed.replaceChildren();
    if (!har) {
      feed.appendChild(el("div", { className: "card", text: "Loading connectors…" }));
      return;
    }

    const q = (document.getElementById("connectorSearch").value || "").trim().toLowerCase();
    const tab = appState.connTab;

    // 1. Installed / Configured MCP Servers
    if (tab === "all" || tab === "mcp") {
      const installed = (har.mcp_servers || []).filter((m) =>
        !q || m.name.toLowerCase().includes(q) || (m.command || "").toLowerCase().includes(q) || (m.url || "").toLowerCase().includes(q)
      );
      feed.appendChild(
        el("div", { className: "section-header" },
          el("h2", { className: "section-title", text: "Installed MCP Connectors" }),
          el("span", { className: "section-sub", text: `${installed.length} configured` })
        )
      );
      if (installed.length === 0) {
        feed.appendChild(el("div", { className: "card muted", text: "No MCP connectors installed yet. Install a preset below or add a custom MCP server." }));
      } else {
        const grid = el("div", { className: "card-grid-2" });
        for (const m of installed) {
          const endpointText = m.transport === "http" ? m.url : `${m.command} ${(m.args || []).join(" ")}`.trim();
          grid.appendChild(
            el("div", { className: "card connector-card" },
              el("div", { className: "connector-info" },
                el("div", { className: "connector-title-row" },
                  el("span", { className: "status-dot " + (m.enabled ? "ok" : "") }),
                  el("span", { className: "connector-name", text: m.name }),
                  el("span", { className: "pill", text: m.transport }),
                  m.isolated_desktop ? el("span", { className: "pill ok", text: "Isolated Desktop" }) : null
                ),
                el("div", { className: "connector-meta", text: endpointText })
              ),
              el("div", { className: "connector-actions" },
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
                    } catch (e) { showToast(e.message); }
                  },
                }),
                el("button", {
                  className: "btn-secondary btn-sm",
                  type: "button",
                  text: "Test",
                  onClick: async () => {
                    showToast(`Testing ${m.name}…`);
                    try {
                      const res = await apiPost(`/api/connectors/mcp/${encodeURIComponent(m.name)}/test`, {});
                      if (res.ok) {
                        openModal(`MCP Test: ${m.name}`, [
                          el("div", { className: "pill ok", text: `Connected (${(res.tools || []).length} tools)` }),
                          el("pre", { className: "code-block", text: (res.tools || []).join("\n") || "(no tools advertised)" }),
                        ]);
                      } else {
                        showToast(`Test failed: ${res.error}`);
                      }
                    } catch (e) { showToast(e.message); }
                  },
                }),
                el("button", {
                  className: "btn-danger btn-sm",
                  type: "button",
                  text: "Remove",
                  onClick: async () => {
                    try {
                      appState.harness = await apiPost(`/api/connectors/mcp/${encodeURIComponent(m.name)}/delete`, {});
                      renderChrome();
                      renderConnectorsView();
                      showToast(`Removed ${m.name}`);
                    } catch (e) { showToast(e.message); }
                  },
                })
              )
            )
          );
        }
        feed.appendChild(grid);
      }

      // Presets grid
      const presets = (har.presets || []).filter((p) =>
        !q || p.name.toLowerCase().includes(q) || p.description.toLowerCase().includes(q) || p.category.toLowerCase().includes(q)
      );
      feed.appendChild(
        el("div", { className: "section-header" },
          el("h2", { className: "section-title", text: "Available Presets" }),
          el("span", { className: "section-sub", text: "One-click soma MCP integrations" })
        )
      );
      const pGrid = el("div", { className: "card-grid-2" });
      for (const p of presets) {
        pGrid.appendChild(
          el("div", { className: "card connector-card" },
            el("div", { className: "connector-info" },
              el("div", { className: "connector-title-row" },
                el("span", { className: "connector-name", text: p.name }),
                el("span", { className: "pill", text: p.category })
              ),
              el("p", { className: "connector-desc", text: p.description })
            ),
            el("div", { className: "connector-actions" },
              p.installed
                ? el("span", { className: "pill ok", text: "Installed" })
                : el("button", {
                    className: "btn-primary btn-sm",
                    type: "button",
                    text: "+ Install",
                    onClick: async () => {
                      try {
                        appState.harness = await apiPost("/api/connectors/mcp", { preset_id: p.id });
                        renderChrome();
                        renderConnectorsView();
                        showToast(`Installed ${p.name}`);
                      } catch (e) { showToast(e.message); }
                    },
                  })
            )
          )
        );
      }
      feed.appendChild(pGrid);
    }

    // 2. Channel Bridges (Discord & Telegram)
    if (tab === "all" || tab === "bridges") {
      feed.appendChild(
        el("div", { className: "section-header" },
          el("h2", { className: "section-title", text: "Channel Bridges" }),
          el("span", { className: "section-sub", text: "Connect soma to Discord or Telegram via .soma/kernel.sock" })
        )
      );

      const d = har.bridges?.discord || {};
      const tg = har.bridges?.telegram || {};

      const dEnabled = el("input", { type: "checkbox", checked: d.enabled });
      const dToken = el("input", { className: "field-input", type: "password", placeholder: d.has_bot_token ? `Saved (${d.bot_token_masked})` : "Discord Bot Token" });
      const dPubKey = el("input", { className: "field-input", type: "text", value: d.public_key_hex || "", placeholder: "Ed25519 Public Key (hex)" });
      const dChannel = el("input", { className: "field-input", type: "text", value: d.channel_id || "", placeholder: "Default Channel ID" });
      const dUsers = el("input", { className: "field-input", type: "text", value: (d.allowed_user_ids || []).join(", "), placeholder: "Comma-separated allowed user IDs" });

      const tgEnabled = el("input", { type: "checkbox", checked: tg.enabled });
      const tgToken = el("input", { className: "field-input", type: "password", placeholder: tg.has_bot_token ? `Saved (${tg.bot_token_masked})` : "Telegram Bot Token" });
      const tgAlert = el("input", { className: "field-input", type: "number", value: tg.alert_chat_id || "", placeholder: "Alert Chat ID" });
      const tgUsers = el("input", { className: "field-input", type: "text", value: (tg.allowed_user_ids || []).join(", "), placeholder: "Comma-separated numeric user IDs" });

      const saveBridges = async () => {
        try {
          const payload = {
            discord: {
              enabled: dEnabled.checked,
              bot_token: dToken.value.trim(),
              public_key_hex: dPubKey.value.trim(),
              channel_id: dChannel.value.trim(),
              allowed_user_ids: dUsers.value.split(",").map((s) => s.trim()).filter(Boolean),
            },
            telegram: {
              enabled: tgEnabled.checked,
              bot_token: tgToken.value.trim(),
              alert_chat_id: Number(tgAlert.value) || 0,
              allowed_user_ids: tgUsers.value.split(",").map((s) => Number(s.trim())).filter((n) => !Number.isNaN(n) && n !== 0),
            },
          };
          appState.harness = await apiPost("/api/connectors/bridges", payload);
          renderConnectorsView();
          showToast("Bridge settings saved");
        } catch (e) { showToast(e.message); }
      };

      feed.appendChild(
        el("div", { className: "card-grid-2" },
          el("div", { className: "card" },
            el("div", { className: "connector-title-row" },
              el("span", { className: "connector-name", text: "Discord Bridge" }),
              el("span", { className: "pill " + (d.enabled ? "ok" : ""), text: d.enabled ? "Enabled" : "Disabled" })
            ),
            el("label", { className: "checkbox-row" }, dEnabled, el("span", { text: "Enable Discord bridge" })),
            el("div", { className: "field" }, el("span", { className: "field-label", text: "Bot Token" }), dToken),
            el("div", { className: "field" }, el("span", { className: "field-label", text: "Public Key (Ed25519 Hex)" }), dPubKey),
            el("div", { className: "field" }, el("span", { className: "field-label", text: "Alert Channel ID" }), dChannel),
            el("div", { className: "field" }, el("span", { className: "field-label", text: "Allowed User IDs" }), dUsers),
            el("div", { className: "connector-actions" },
              el("button", { className: "btn-primary btn-sm", type: "button", text: "Save Discord", onClick: saveBridges })
            )
          ),
          el("div", { className: "card" },
            el("div", { className: "connector-title-row" },
              el("span", { className: "connector-name", text: "Telegram Bridge" }),
              el("span", { className: "pill " + (tg.enabled ? "ok" : ""), text: tg.enabled ? "Enabled" : "Disabled" })
            ),
            el("label", { className: "checkbox-row" }, tgEnabled, el("span", { text: "Enable Telegram bridge" })),
            el("div", { className: "field" }, el("span", { className: "field-label", text: "Bot Token" }), tgToken),
            el("div", { className: "field" }, el("span", { className: "field-label", text: "Alert Chat ID" }), tgAlert),
            el("div", { className: "field" }, el("span", { className: "field-label", text: "Allowed User IDs" }), tgUsers),
            el("div", { className: "connector-actions" },
              el("button", { className: "btn-primary btn-sm", type: "button", text: "Save Telegram", onClick: saveBridges })
            )
          )
        )
      );
    }
  }

  function openCustomMCPModal() {
    const nameIn = el("input", { className: "field-input", type: "text", placeholder: "e.g. github-mcp" });
    const transSel = el("select", { className: "select-input" },
      el("option", { value: "stdio", text: "stdio (local command)" }),
      el("option", { value: "http", text: "http (remote endpoint)" })
    );
    const cmdIn = el("input", { className: "field-input", type: "text", placeholder: "npx" });
    const argsIn = el("input", { className: "field-input", type: "text", placeholder: "-y @modelcontextprotocol/server-github" });
    const urlIn = el("input", { className: "field-input", type: "text", placeholder: "https://example.com/mcp" });
    const isoChk = el("input", { type: "checkbox" });

    openModal("Add Custom MCP Connector", [
      el("div", { className: "form-grid" },
        el("div", { className: "field" }, el("span", { className: "field-label", text: "Name" }), nameIn),
        el("div", { className: "field" }, el("span", { className: "field-label", text: "Transport" }), transSel),
        el("div", { className: "field" }, el("span", { className: "field-label", text: "Command (stdio)" }), cmdIn),
        el("div", { className: "field" }, el("span", { className: "field-label", text: "Args (space-separated)" }), argsIn),
        el("div", { className: "field full" }, el("span", { className: "field-label", text: "URL (http)" }), urlIn),
        el("div", { className: "field full" }, el("label", { className: "checkbox-row" }, isoChk, el("span", { text: "Runs inside isolated desktop sandbox" })))
      ),
      el("div", { className: "connector-actions" },
        el("button", { className: "btn-secondary", type: "button", text: "Cancel", onClick: closeModal }),
        el("button", {
          className: "btn-primary",
          type: "button",
          text: "Save Connector",
          onClick: async () => {
            try {
              const args = argsIn.value.trim() ? argsIn.value.trim().split(/\s+/) : [];
              appState.harness = await apiPost("/api/connectors/mcp", {
                name: nameIn.value.trim(),
                transport: transSel.value,
                command: cmdIn.value.trim(),
                args,
                url: urlIn.value.trim(),
                isolated_desktop: isoChk.checked,
                enabled: true,
              });
              closeModal();
              renderChrome();
              renderConnectorsView();
              showToast("MCP connector saved");
            } catch (e) { showToast(e.message); }
          },
        })
      ),
    ]);
  }

  // Harness Control view
  function renderHarnessView() {
    const har = appState.harness;
    const feed = document.getElementById("harnessFeed");
    feed.replaceChildren();
    if (!har) {
      feed.appendChild(el("div", { className: "card", text: "Loading harness configuration…" }));
      return;
    }

    const tab = appState.harnessTab;
    if (tab === "provider") {
      const p = har.provider || {};
      const typeSel = el("select", { className: "select-input" },
        el("option", { value: "auto", text: "Auto (CLI / Env)", selected: p.provider_type === "auto" }),
        el("option", { value: "openai_compat", text: "OpenAI-Compatible API", selected: p.provider_type === "openai_compat" }),
        el("option", { value: "mock", text: "Local Mock Simulator", selected: p.provider_type === "mock" })
      );
      const apiBase = el("input", { className: "field-input", type: "text", value: p.api_base || "", placeholder: "https://api.openai.com/v1 or http://127.0.0.1:11434/v1" });
      const apiKey = el("input", { className: "field-input", type: "password", placeholder: p.has_api_key ? `Saved (${p.api_key_masked})` : "API Key (leave blank to keep)" });
      const modelId = el("input", { className: "field-input", type: "text", value: p.model_id || "", placeholder: "e.g. gpt-4o, qwen3:32b, mock-worker-v1" });
      const embedUrl = el("input", { className: "field-input", type: "text", value: p.embed_url || "", placeholder: "http://127.0.0.1:11434/v1" });
      const embedModel = el("input", { className: "field-input", type: "text", value: p.embed_model || "", placeholder: "qwen3-embedding:0.6b" });
      const maxTurns = el("input", { className: "field-input", type: "number", value: p.max_turns || 15 });
      const wsStrat = el("select", { className: "select-input" },
        el("option", { value: "worktree", text: "Git Worktree (isolated)", selected: p.workspace_strategy === "worktree" }),
        el("option", { value: "copy", text: "Copy Workspace", selected: p.workspace_strategy === "copy" }),
        el("option", { value: "direct", text: "Direct Workspace", selected: p.workspace_strategy === "direct" })
      );
      const sbxMode = el("select", { className: "select-input" },
        el("option", { value: "auto", text: "Auto (bwrap / landlock / seatbelt)", selected: p.sandbox_mode === "auto" }),
        el("option", { value: "bwrap", text: "Bubblewrap (bwrap)", selected: p.sandbox_mode === "bwrap" }),
        el("option", { value: "landlock", text: "Landlock", selected: p.sandbox_mode === "landlock" }),
        el("option", { value: "none", text: "Disabled (dev only)", selected: p.sandbox_mode === "none" })
      );

      feed.appendChild(
        el("div", { className: "card" },
          el("div", { className: "connector-title-row" },
            el("span", { className: "connector-name", text: "Model Provider & Execution Harness" }),
            el("span", { className: "pill ok", text: `Mode: ${p.mode || "auto"}` })
          ),
          el("div", { className: "form-grid" },
            el("div", { className: "field" }, el("span", { className: "field-label", text: "Provider Type" }), typeSel),
            el("div", { className: "field" }, el("span", { className: "field-label", text: "Model ID" }), modelId),
            el("div", { className: "field" }, el("span", { className: "field-label", text: "API Base URL" }), apiBase),
            el("div", { className: "field" }, el("span", { className: "field-label", text: "API Key" }), apiKey),
            el("div", { className: "field" }, el("span", { className: "field-label", text: "Embedder URL" }), embedUrl),
            el("div", { className: "field" }, el("span", { className: "field-label", text: "Embedder Model" }), embedModel),
            el("div", { className: "field" }, el("span", { className: "field-label", text: "Default Max Turns" }), maxTurns),
            el("div", { className: "field" }, el("span", { className: "field-label", text: "Workspace Strategy" }), wsStrat),
            el("div", { className: "field" }, el("span", { className: "field-label", text: "OS Sandbox Backend" }), sbxMode)
          ),
          el("div", { className: "connector-actions" },
            el("button", {
              className: "btn-primary",
              type: "button",
              text: "Save Harness Settings",
              onClick: async () => {
                try {
                  appState.harness = await apiPost("/api/harness/provider", {
                    provider_type: typeSel.value,
                    api_base: apiBase.value.trim(),
                    api_key: apiKey.value.trim(),
                    model_id: modelId.value.trim(),
                    embed_url: embedUrl.value.trim(),
                    embed_model: embedModel.value.trim(),
                    max_turns: Number(maxTurns.value) || 15,
                    workspace_strategy: wsStrat.value,
                    sandbox_mode: sbxMode.value,
                  });
                  await refreshAll();
                  showToast("Harness settings saved");
                } catch (e) { showToast(e.message); }
              },
            })
          )
        )
      );
    } else if (tab === "accounts") {
      const accs = har.accounts || [];
      const accGrid = el("div", { className: "card-grid-2" });
      for (const a of accs) {
        accGrid.appendChild(
          el("div", { className: "card" },
            el("div", { className: "connector-title-row" },
              el("span", { className: "connector-name", text: a.id }),
              el("span", { className: "pill", text: a.billing_mode })
            ),
            el("div", { className: "muted", text: `${a.provider} · ${a.model_id}` }),
            el("div", { className: "connector-meta", text: `Concurrent: ${a.active_leases}/${a.max_concurrent} · Spent: $${a.daily_spent_usd.toFixed(2)} / $${a.daily_budget_usd.toFixed(2)}` })
          )
        );
      }
      feed.appendChild(accGrid);

      const idIn = el("input", { className: "field-input", type: "text", placeholder: "primary-openai" });
      const provIn = el("input", { className: "field-input", type: "text", placeholder: "openai" });
      const modIn = el("input", { className: "field-input", type: "text", placeholder: "gpt-4o" });
      const billSel = el("select", { className: "select-input" },
        el("option", { value: "subscription", text: "subscription" }),
        el("option", { value: "metered", text: "metered" }),
        el("option", { value: "local", text: "local" })
      );
      const concIn = el("input", { className: "field-input", type: "number", value: 2 });
      const budIn = el("input", { className: "field-input", type: "number", value: 10, step: "0.5" });

      feed.appendChild(
        el("div", { className: "card" },
          el("div", { className: "connector-name", text: "Add / Update Model Account" }),
          el("div", { className: "form-grid" },
            el("div", { className: "field" }, el("span", { className: "field-label", text: "Account ID" }), idIn),
            el("div", { className: "field" }, el("span", { className: "field-label", text: "Provider" }), provIn),
            el("div", { className: "field" }, el("span", { className: "field-label", text: "Model ID" }), modIn),
            el("div", { className: "field" }, el("span", { className: "field-label", text: "Billing Mode" }), billSel),
            el("div", { className: "field" }, el("span", { className: "field-label", text: "Max Concurrent" }), concIn),
            el("div", { className: "field" }, el("span", { className: "field-label", text: "Daily Budget (USD)" }), budIn)
          ),
          el("div", { className: "connector-actions" },
            el("button", {
              className: "btn-primary",
              type: "button",
              text: "Save Account",
              onClick: async () => {
                try {
                  appState.harness = await apiPost("/api/harness/accounts", {
                    ID: idIn.value.trim(),
                    Provider: provIn.value.trim(),
                    ModelID: modIn.value.trim(),
                    BillingMode: billSel.value,
                    MaxConcurrent: Number(concIn.value) || 2,
                    DailyBudgetUSD: Number(budIn.value) || 0,
                  });
                  renderHarnessView();
                  showToast("Account saved");
                } catch (e) { showToast(e.message); }
              },
            })
          )
        )
      );
    } else if (tab === "triggers") {
      const tr = har.triggers || {};
      const quietIn = el("input", { className: "field-input", type: "text", value: tr.quiet_hours || "", placeholder: "23:00-07:00" });
      const maxInbIn = el("input", { className: "field-input", type: "number", value: tr.max_pending_inbox || 3 });
      const budIn = el("input", { className: "field-input", type: "number", value: tr.daily_metered_budget_usd || 0, step: "0.5" });
      const hookIn = el("input", { className: "field-input", type: "text", value: tr.webhook_addr || "127.0.0.1:8787" });
      const gcIn = el("input", { className: "field-input", type: "text", value: tr.gc_interval || "5m" });

      const triggersList = (tr.triggers || []).slice();

      const saveTriggersPayload = async (nextList) => {
        try {
          appState.harness = await apiPost("/api/harness/triggers", {
            quiet_hours: quietIn.value.trim(),
            max_pending_inbox: Number(maxInbIn.value) || 3,
            daily_metered_budget_usd: Number(budIn.value) || 0,
            webhook_addr: hookIn.value.trim(),
            gc_interval: gcIn.value.trim(),
            triggers: nextList,
          });
          renderHarnessView();
          showToast("Daemon & triggers saved to .soma/triggers.yaml");
        } catch (e) { showToast(e.message); }
      };

      feed.appendChild(
        el("div", { className: "card" },
          el("div", { className: "connector-name", text: "Tier-0 Daemon & Brakes (.soma/triggers.yaml)" }),
          el("div", { className: "form-grid" },
            el("div", { className: "field" }, el("span", { className: "field-label", text: "Quiet Hours" }), quietIn),
            el("div", { className: "field" }, el("span", { className: "field-label", text: "Max Pending Inbox" }), maxInbIn),
            el("div", { className: "field" }, el("span", { className: "field-label", text: "Daily Metered Budget (USD)" }), budIn),
            el("div", { className: "field" }, el("span", { className: "field-label", text: "Webhook Addr" }), hookIn),
            el("div", { className: "field" }, el("span", { className: "field-label", text: "GC Interval" }), gcIn)
          ),
          el("div", { className: "connector-actions" },
            el("button", { className: "btn-primary btn-sm", type: "button", text: "Save Daemon Config", onClick: () => saveTriggersPayload(triggersList) })
          )
        )
      );

      feed.appendChild(
        el("div", { className: "section-header" },
          el("h2", { className: "section-title", text: "Configured Triggers" }),
          el("span", { className: "section-sub", text: `${triggersList.length} triggers` })
        )
      );

      for (let i = 0; i < triggersList.length; i++) {
        const t = triggersList[i];
        feed.appendChild(
          el("div", { className: "card connector-card" },
            el("div", { className: "connector-info" },
              el("div", { className: "connector-title-row" },
                el("span", { className: "connector-name", text: t.ID || t.id }),
                el("span", { className: "pill", text: t.Type || t.type }),
                t.Interval || t.interval ? el("span", { className: "pill", text: t.Interval || t.interval }) : null
              ),
              el("div", { className: "connector-desc", text: t.Prompt || t.prompt || t.Command || t.command || "" })
            ),
            el("div", { className: "connector-actions" },
              el("button", {
                className: "btn-danger btn-sm",
                type: "button",
                text: "Remove",
                onClick: () => {
                  const next = triggersList.filter((_, idx) => idx !== i);
                  saveTriggersPayload(next);
                },
              })
            )
          )
        );
      }

      // Add Trigger form
      const tId = el("input", { className: "field-input", type: "text", placeholder: "nightly-audit" });
      const tType = el("select", { className: "select-input" },
        el("option", { value: "cron", text: "cron" }),
        el("option", { value: "watch", text: "watch" }),
        el("option", { value: "webhook", text: "webhook" }),
        el("option", { value: "sentinel", text: "sentinel" })
      );
      const tInt = el("input", { className: "field-input", type: "text", placeholder: "1h or 0 * * * *" });
      const tPrompt = el("textarea", { className: "field-textarea", placeholder: "Prompt to execute when trigger fires…" });

      feed.appendChild(
        el("div", { className: "card" },
          el("div", { className: "connector-name", text: "Add Trigger" }),
          el("div", { className: "form-grid" },
            el("div", { className: "field" }, el("span", { className: "field-label", text: "Trigger ID" }), tId),
            el("div", { className: "field" }, el("span", { className: "field-label", text: "Type" }), tType),
            el("div", { className: "field full" }, el("span", { className: "field-label", text: "Interval / Schedule" }), tInt),
            el("div", { className: "field full" }, el("span", { className: "field-label", text: "Prompt" }), tPrompt)
          ),
          el("div", { className: "connector-actions" },
            el("button", {
              className: "btn-primary btn-sm",
              type: "button",
              text: "+ Add Trigger",
              onClick: () => {
                if (!tId.value.trim()) { showToast("Trigger ID is required"); return; }
                const next = [...triggersList, {
                  ID: tId.value.trim(),
                  Type: tType.value,
                  Interval: tInt.value.trim(),
                  Prompt: tPrompt.value.trim(),
                }];
                saveTriggersPayload(next);
              },
            })
          )
        )
      );
    }
  }

  // Approvals view
  function renderApprovalsView() {
    const feed = document.getElementById("approvalsFeed");
    feed.replaceChildren();
    const items = appState.state?.inbox || [];
    if (items.length === 0) {
      feed.appendChild(el("div", { className: "card muted", text: "No pending approvals or proposals." }));
      return;
    }
    for (const item of items) {
      feed.appendChild(
        el("div", { className: "card" },
          el("div", { className: "step-head" },
            el("span", { className: "connector-name", text: item.title }),
            el("span", { className: "pill " + (item.status === "pending" ? "warn" : "ok"), text: `${item.item_kind} · ${item.status}` })
          ),
          el("div", { className: "step-body", text: item.summary }),
          item.status === "pending"
            ? el("div", { className: "connector-actions" },
                el("button", {
                  className: "btn-primary btn-sm",
                  type: "button",
                  text: "Approve",
                  onClick: async () => {
                    try {
                      await apiPost(`/api/inbox/${encodeURIComponent(item.id)}/approve`, {});
                      await refreshAll();
                      showToast("Approved");
                    } catch (e) { showToast(e.message); }
                  },
                }),
                el("button", {
                  className: "btn-secondary btn-sm",
                  type: "button",
                  text: "Dismiss",
                  onClick: async () => {
                    try {
                      await apiPost(`/api/inbox/${encodeURIComponent(item.id)}/dismiss`, {});
                      await refreshAll();
                      showToast("Dismissed");
                    } catch (e) { showToast(e.message); }
                  },
                })
              )
            : null
        )
      );
    }
  }

  // Memory & Skills view
  function renderMemoryView() {
    const feed = document.getElementById("memoryFeed");
    feed.replaceChildren();
    const q = (document.getElementById("memorySearch").value || "").trim().toLowerCase();
    const mems = (appState.state?.memory || []).filter((m) =>
      !q || m.content.toLowerCase().includes(q) || m.kind.toLowerCase().includes(q)
    );
    const skills = (appState.state?.skills || []).filter((s) =>
      !q || s.name.toLowerCase().includes(q) || s.description.toLowerCase().includes(q)
    );

    feed.appendChild(el("div", { className: "section-header" }, el("h2", { className: "section-title", text: "Memory Rules & Facts" })));
    if (mems.length === 0) {
      feed.appendChild(el("div", { className: "card muted", text: "No memory nodes match your filter." }));
    } else {
      for (const m of mems) {
        feed.appendChild(
          el("div", { className: "card connector-card" },
            el("div", { className: "connector-info" },
              el("div", { className: "connector-title-row" },
                el("span", { className: "pill", text: m.kind }),
                el("span", { className: "pill " + (m.status === "active" ? "ok" : "warn"), text: m.status }),
                el("span", { className: "muted", text: m.trust_tier })
              ),
              el("div", { className: "step-body", text: m.content })
            ),
            el("div", { className: "connector-actions" },
              m.status !== "active"
                ? el("button", {
                    className: "btn-primary btn-sm",
                    type: "button",
                    text: "Activate",
                    onClick: async () => {
                      await apiPost(`/api/memory/${encodeURIComponent(m.id)}/activate`, {});
                      await refreshAll();
                    },
                  })
                : null,
              el("button", {
                className: "btn-secondary btn-sm",
                type: "button",
                text: m.enabled ? "Disable" : "Enable",
                onClick: async () => {
                  await apiPost(`/api/memory/${encodeURIComponent(m.id)}/toggle`, { enabled: !m.enabled });
                  await refreshAll();
                },
              })
            )
          )
        );
      }
    }

    feed.appendChild(el("div", { className: "section-header" }, el("h2", { className: "section-title", text: "Installed AAIF Skills" })));
    if (skills.length === 0) {
      feed.appendChild(el("div", { className: "card muted", text: "No skills installed in .soma/skills or .skills." }));
    } else {
      const grid = el("div", { className: "card-grid-2" });
      for (const sk of skills) {
        const bCls = sk.badge === "CLEAN" ? "ok" : sk.badge === "WARN" ? "warn" : "err";
        grid.appendChild(
          el("div", { className: "card" },
            el("div", { className: "connector-title-row" },
              el("span", { className: "connector-name", text: sk.name }),
              el("span", { className: "pill " + bCls, text: sk.badge })
            ),
            el("p", { className: "connector-desc", text: sk.description || "No description" })
          )
        );
      }
      feed.appendChild(grid);
    }
  }

  // Event wiring
  function wireEvents() {
    document.getElementById("collapseSidebarBtn").addEventListener("click", () => {
      document.getElementById("appShell").classList.toggle("sidebar-collapsed");
    });

    document.getElementById("mobileMenuBtn").addEventListener("click", openMobileDrawer);
    document.getElementById("mobileCloseBtn").addEventListener("click", closeMobileDrawer);
    document.getElementById("sidebarBackdrop").addEventListener("click", closeMobileDrawer);
    document.getElementById("mobileDesktopBtn").addEventListener("click", () => switchView("desktop"));

    const startNewTask = () => {
      appState.selectedRunId = null;
      appState.selectedBranchId = null;
      switchView("tasks");
      document.getElementById("goalInput").focus();
    };
    document.getElementById("newTaskBtn").addEventListener("click", startNewTask);
    document.getElementById("brandHomeBtn").addEventListener("click", startNewTask);
    document.getElementById("mobileNewTaskBtn").addEventListener("click", startNewTask);

    document.querySelectorAll(".nav-item").forEach((btn) => {
      btn.addEventListener("click", () => switchView(btn.dataset.view));
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
    document.getElementById("mobileModelPill").addEventListener("click", () => {
      appState.profile = appState.profile === "worker" ? "frontier" : "worker";
      renderChrome();
      showToast(`Profile: ${appState.profile}`);
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

    document.getElementById("quickDesktopToggle").addEventListener("click", () => switchView("desktop"));
    document.getElementById("quickOpenConnectors").addEventListener("click", () => switchView("connectors"));
    document.getElementById("quickOpenHarness").addEventListener("click", () => switchView("harness"));

    // Start task form
    document.getElementById("goalForm").addEventListener("submit", async (e) => {
      e.preventDefault();
      const input = document.getElementById("goalInput");
      const goal = input.value.trim();
      if (!goal) return;
      try {
        const res = await apiPost("/api/runs", { goal, profile: appState.profile });
        input.value = "";
        appState.selectedRunId = res.run_id;
        appState.selectedBranchId = res.branch_id;
        await refreshAll();
      } catch (err) {
        showToast(err.message);
      }
    });

    // Task actions
    document.getElementById("detailsBtn").addEventListener("click", () => {
      appState.detailsOpen = !appState.detailsOpen;
      renderTasksView();
    });
    document.getElementById("resumeBtn").addEventListener("click", async () => {
      const br = getSelectedBranch(getSelectedRun());
      if (!br) return;
      try {
        await apiPost(`/api/branches/${encodeURIComponent(br.id)}/resume`, {});
        await refreshAll();
      } catch (e) { showToast(e.message); }
    });
    document.getElementById("forkBtn").addEventListener("click", async () => {
      const br = getSelectedBranch(getSelectedRun());
      if (!br) return;
      try {
        const res = await apiPost(`/api/branches/${encodeURIComponent(br.id)}/fork`, { label: "fork" });
        appState.selectedBranchId = res.branch_id;
        await refreshAll();
        showToast("Branch forked");
      } catch (e) { showToast(e.message); }
    });
    document.getElementById("verifyBtn").addEventListener("click", async () => {
      const br = getSelectedBranch(getSelectedRun());
      if (!br) return;
      try {
        const res = await apiPost(`/api/branches/${encodeURIComponent(br.id)}/replay`, {});
        showToast(res.diverged ? "Replay diverged!" : `Verified ${res.verified_count} steps (ModeExact)`);
      } catch (e) { showToast(e.message); }
    });

    // Virtual Desktop controls
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
        showToast(e.message);
      } finally {
        btn.disabled = false;
        btn.textContent = "Launch Desktop";
      }
    });
    document.getElementById("desktopStopBtn").addEventListener("click", async () => {
      try {
        const desk = await apiPost("/api/desktop/stop", {});
        if (appState.harness) appState.harness.desktop = desk;
        renderChrome();
        renderDesktopView();
        showToast("Virtual Desktop stopped");
      } catch (e) { showToast(e.message); }
    });

    // Connectors subtabs & search
    document.querySelectorAll("[data-conn-tab]").forEach((b) => {
      b.addEventListener("click", () => {
        appState.connTab = b.dataset.connTab;
        document.querySelectorAll("[data-conn-tab]").forEach((x) => x.classList.toggle("active", x === b));
        renderConnectorsView();
      });
    });
    document.getElementById("connectorSearch").addEventListener("input", renderConnectorsView);
    document.getElementById("addCustomMcpBtn").addEventListener("click", openCustomMCPModal);

    // Harness subtabs
    document.querySelectorAll("[data-harness-tab]").forEach((b) => {
      b.addEventListener("click", () => {
        appState.harnessTab = b.dataset.harnessTab;
        document.querySelectorAll("[data-harness-tab]").forEach((x) => x.classList.toggle("active", x === b));
        renderHarnessView();
      });
    });

    // Memory search
    document.getElementById("memorySearch").addEventListener("input", renderMemoryView);

    // Modal backdrop close
    document.getElementById("modalBackdrop").addEventListener("click", (e) => {
      if (e.target.id === "modalBackdrop") closeModal();
    });
  }

  function connectSSE() {
    const es = new EventSource("/api/events");
    es.addEventListener("state", () => {
      refreshAll().catch(() => {});
    });
    es.onerror = () => {
      // EventSource automatically reconnects.
    };
  }

  initTheme();
  wireEvents();
  refreshAll().catch((e) => showToast(e.message));
  connectSSE();
})();

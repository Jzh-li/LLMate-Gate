// LLMate Gate 调试面板前端（vanilla JS，无构建链；UI设计 §1.1）。
//
// 数据源：
//   - /_api/traffic  GET  环形缓冲快照
//   - /_api/detect   POST Playground 检测
//   - /_api/replace  POST Playground 检测+替换
//   - /_api/rules    GET/PUT 规则读取/热加载
//   - /ws/events     WebSocket 实时事件流

(function () {
  "use strict";

  // ----- 状态 -----
  const state = {
    ws: null,
    records: [], // 按时间顺序（旧 → 新）
    selectedID: null,
  };

  // ----- 工具 -----
  const $ = (s) => document.querySelector(s);
  const $$ = (s) => Array.from(document.querySelectorAll(s));
  function escape(text) {
    if (text == null) return "";
    return String(text)
      .replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;")
      .replace(/"/g, "&quot;").replace(/'/g, "&#39;");
  }
  function maskValue(v, type) {
    if (!v) return "";
    // 仅在审计页/规则页默认打码（UI设计 §3.3）
    return "•".repeat(Math.min(v.length, 12));
  }
  function fmtTime(iso) {
    try { return new Date(iso).toLocaleTimeString("zh-CN"); } catch { return iso; }
  }
  function fmtDuration(ms) {
    if (!ms && ms !== 0) return "—";
    return ms + " ms";
  }

  // ----- Tabs -----
  $$(".tab").forEach((btn) => {
    btn.addEventListener("click", () => {
      $$(".tab").forEach((t) => t.classList.remove("active"));
      $$(".tab-pane").forEach((p) => p.classList.remove("active"));
      btn.classList.add("active");
      $("#tab-" + btn.dataset.tab).classList.add("active");
    });
  });

  // ----- WebSocket -----
  function setStatus(text, cls) {
    $("#wsStatusText").textContent = text;
    $("#wsStatus").className = "dot " + cls;
  }
  function connectWS() {
    if (state.ws && state.ws.readyState === WebSocket.OPEN) return;
    const proto = location.protocol === "https:" ? "wss:" : "ws:";
    const url = proto + "//" + location.host + "/ws/events";
    let ws;
    try { ws = new WebSocket(url); } catch (e) { setStatus("连接失败", "err"); scheduleReconnect(); return; }
    state.ws = ws;
    setStatus("连接中…", "off");
    ws.onopen = () => setStatus("已连接", "on");
    ws.onclose = () => { setStatus("已断开（重连中）", "off"); scheduleReconnect(); };
    ws.onerror = () => setStatus("错误", "err");
    ws.onmessage = (ev) => {
      try {
        const msg = JSON.parse(ev.data);
        if (msg.type === "request.received" || msg.type === "upstream.response" || msg.type === "restore.done") {
          // 仅在收到完整 5 段或最后一段时刷新（简化：每次都拉取一次 traffic 快照）
          refreshTraffic();
        }
      } catch (e) { /* ignore */ }
    };
  }
  function scheduleReconnect() {
    if (state._reconnect) return;
    state._reconnect = setTimeout(() => { state._reconnect = null; connectWS(); }, 1500);
  }

  // ----- 流量 -----
  async function refreshTraffic() {
    try {
      const resp = await fetch("/_api/traffic", { headers: { "Accept": "application/json" } });
      if (!resp.ok) return;
      const data = await resp.json();
      state.records = (data.records || []).filter((r) => r && r.request_id);
      renderList();
      if (state.selectedID) {
        const rec = state.records.find((r) => r.request_id === state.selectedID);
        if (rec) renderDetail(rec); else clearDetail();
      }
    } catch (e) { /* ignore */ }
  }
  $("#clearTraffic").addEventListener("click", async () => {
    if (!confirm("清空所有流量记录？")) return;
    await fetch("/_api/traffic", { method: "DELETE" });
    state.records = []; state.selectedID = null; renderList(); clearDetail();
  });

  function renderList() {
    const ul = $("#trafficList");
    $("#trafficCount").textContent = state.records.length;
    if (state.records.length === 0) {
      ul.innerHTML = '<li style="color: var(--text-dim); cursor: default;">暂无记录</li>';
      return;
    }
    // 最新在上
    const items = state.records.slice().reverse().map((r) => {
      const entN = (r.detected || []).length;
      const cls = r.outcome === "blocked" ? "blocked" : (r.outcome === "error" ? "error" : "passed");
      const active = r.request_id === state.selectedID ? "active" : "";
      const started = r.started_at || r.timestamp || "";
      return `<li class="${active}" data-id="${escape(r.request_id)}">
        <div class="time">${escape(fmtTime(started))}</div>
        <div><span class="endpoint">${escape(r.endpoint || "")}</span></div>
        <div class="meta-row">
          <span class="badge ${cls}">${escape(r.outcome || "?")}</span>
          <span class="pii-count">PII: ${entN}</span>
          <span>${escape(fmtDuration(r.duration_ms))}</span>
        </div>
      </li>`;
    }).join("");
    ul.innerHTML = items;
    $$("#trafficList li[data-id]").forEach((li) => {
      li.addEventListener("click", () => {
        state.selectedID = li.dataset.id;
        const rec = state.records.find((r) => r.request_id === state.selectedID);
        if (rec) renderDetail(rec);
        $$("#trafficList li").forEach((x) => x.classList.remove("active"));
        li.classList.add("active");
      });
    });
  }
  function clearDetail() {
    $("#emptyState").style.display = "";
    $("#detailBody").classList.add("hidden");
  }
  function renderDetail(r) {
    $("#emptyState").style.display = "none";
    $("#detailBody").classList.remove("hidden");
    $("#dMethod").textContent = r.method || "POST";
    $("#dEndpoint").textContent = r.endpoint || "";
    $("#dOutcome").textContent = r.outcome || "—";
    $("#dOutcome").className = "badge " + (r.outcome === "blocked" ? "blocked" : (r.outcome === "error" ? "error" : "passed"));
    $("#dDuration").textContent = fmtDuration(r.duration_ms);
    $("#dReqID").textContent = r.request_id || "—";
    $("#dStrategy").textContent = r.strategy || "—";

    $("#dRaw").textContent = r.raw_request || "(空)";
    $("#dReplaced").textContent = r.replaced || "(无替换)";

    const ents = r.detected || [];
    $("#dEntityCount").textContent = ents.length;
    $("#dEntities").innerHTML = ents.length === 0
      ? '<li style="color: var(--text-dim);">未检测到 PII</li>'
      : ents.map((e) => {
          const pii = '<span class="pii-marker">[敏感]</span>';
          return `<li><span class="type">${escape(e.type)}</span> ${pii}<span>${escape(maskValue(e.value))}</span><span class="range">[${e.start}, ${e.end}]</span><span class="conf">conf=${escape((e.score || 0).toFixed(2))}</span></li>`;
        }).join("");

    const map = r.mapping || [];
    $("#dMapping tbody").innerHTML = map.length === 0
      ? '<tr><td colspan="3" style="color: var(--text-dim); text-align: center;">空</td></tr>'
      : map.map((m) => {
          const isMask = m.value && m.value.startsWith("[REDACTED]");
          return `<tr>
            <td class="placeholder">${escape(m.placeholder)}</td>
            <td>${escape(m.type)}</td>
            <td class="value ${isMask ? "mask" : ""}">${escape(isMask ? m.value : maskValue(m.value))}</td>
          </tr>`;
        }).join("");

    $("#dUpstream").textContent = r.upstream_response || "(空)";
    $("#dRestored").textContent = r.restored || "(空)";

    if (r.error) {
      $("#dErrorBlock").classList.remove("hidden");
      $("#dError").textContent = r.error;
    } else {
      $("#dErrorBlock").classList.add("hidden");
    }
  }

  // ----- Playground -----
  $("#pgDetect").addEventListener("click", async () => {
    const text = $("#pgInput").value.trim();
    if (!text) return alert("请输入文本");
    $("#pgEntities").innerHTML = '<li style="color: var(--text-dim);">检测中…</li>';
    $("#pgReplaced").textContent = "";
    $("#pgMapping tbody").innerHTML = "";
    try {
      const resp = await fetch("/_api/detect", {
        method: "POST", headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ text }),
      });
      const data = await resp.json();
      const ents = data.entities || [];
      $("#pgEntities").innerHTML = ents.length === 0
        ? '<li style="color: var(--text-dim);">未检测到实体</li>'
        : ents.map((e) => {
            return `<li><span class="type">${escape(e.type)}</span> <span>${escape(maskValue(e.value))}</span><span class="range">[${e.start}, ${e.end}]</span><span class="conf">conf=${escape((e.confidence || 0).toFixed(2))}</span></li>`;
          }).join("");
      if (data.error) $("#pgEntities").innerHTML += `<li style="color: var(--err);">${escape(data.error)}</li>`;
    } catch (e) {
      $("#pgEntities").innerHTML = `<li style="color: var(--err);">请求失败：${escape(e.message)}</li>`;
    }
  });
  $("#pgReplace").addEventListener("click", async () => {
    const text = $("#pgInput").value.trim();
    const strategy = $("#pgStrategy").value;
    if (!text) return alert("请输入文本");
    $("#pgReplaced").textContent = "替换中…";
    $("#pgMapping tbody").innerHTML = "";
    try {
      const resp = await fetch("/_api/replace", {
        method: "POST", headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ text, strategy }),
      });
      if (!resp.ok) {
        const err = await resp.json().catch(() => ({}));
        $("#pgReplaced").textContent = "失败：" + (err.error && err.error.message || resp.status);
        return;
      }
      const data = await resp.json();
      $("#pgReplaced").textContent = data.replaced || "(空)";
      const map = data.mapping || [];
      $("#pgMapping tbody").innerHTML = map.length === 0
        ? '<tr><td colspan="3" style="color: var(--text-dim); text-align: center;">无映射</td></tr>'
        : map.map((m) => `<tr>
            <td class="placeholder">${escape(m.placeholder)}</td>
            <td>${escape(m.type)}</td>
            <td class="value">${escape(maskValue(m.value))}</td>
          </tr>`).join("");
    } catch (e) {
      $("#pgReplaced").textContent = "请求失败：" + e.message;
    }
  });

  // ----- 规则 -----
  async function loadRules() {
    try {
      const resp = await fetch("/_api/rules");
      const data = await resp.json();
      $("#ruleStrategy").textContent = data.strategy || "—";
      $("#ruleIrreversible").textContent = (data.irreversible || []).join(", ") || "—";
      $("#ruleStrategySelect").value = data.strategy || "placeholder";
    } catch (e) { /* ignore */ }
  }
  $("#ruleApply").addEventListener("click", async () => {
    const strategy = $("#ruleStrategySelect").value;
    $("#ruleStatus").textContent = "应用…";
    try {
      const resp = await fetch("/_api/rules", {
        method: "PUT", headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ strategy }),
      });
      if (!resp.ok) {
        const err = await resp.json().catch(() => ({}));
        $("#ruleStatus").textContent = "失败：" + (err.error && err.error.message || resp.status);
        return;
      }
      $("#ruleStatus").textContent = "已应用：" + strategy;
      loadRules();
    } catch (e) {
      $("#ruleStatus").textContent = "失败：" + e.message;
    }
  });

  // ----- 启动 -----
  connectWS();
  refreshTraffic();
  loadRules();
  // 周期性拉取兜底（WS 断线期间不丢记录）
  setInterval(refreshTraffic, 3000);
})();
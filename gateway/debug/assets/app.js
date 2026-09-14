// LLMate Gate 调试面板前端（vanilla JS，无构建链；UI设计 §1.1）。
//
// 数据源：
//   - /_api/traffic  GET  环形缓冲快照
//   - /_api/detect   POST Playground 检测
//   - /_api/replace  POST Playground 检测+替换
//   - /_api/rules    GET/PUT 规则读取/热加载
//   - /_api/dictionary GET/PUT 仿真词典（运行时）
//   - /_api/registry GET/PUT 登记表（明文 PII，PUT 同时落盘）
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
      if (btn.dataset.tab === "audit" && typeof auditRefresh === "function") auditRefresh();
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

  // ----- 规则：策略开关 -----
  let currentStrategy = "placeholder";

  const STRATEGY_LABEL = {
    bypass: "bypass（透传，不脱敏）",
    placeholder: "placeholder（占位符）",
    simulate: "simulate（仿真）",
  };

  function setHint(el, text, kind) {
    if (!el) return;
    el.textContent = text;
    el.className = "hint" + (kind ? " " + kind : "");
  }

  function renderSeg() {
    $$("#ruleSeg .seg-item").forEach((b) => {
      b.classList.toggle("active", b.dataset.value === currentStrategy);
    });
  }

  async function loadRules() {
    try {
      const resp = await fetch("/_api/rules");
      const data = await resp.json();
      currentStrategy = data.strategy || "placeholder";
      $("#ruleStrategy").textContent = STRATEGY_LABEL[currentStrategy] || currentStrategy;
      $("#ruleIrreversible").textContent = (data.irreversible || []).join(", ") || "—";
      renderSeg();
    } catch (e) { /* ignore */ }
  }

  // 点一下即切换。乐观更新 + 失败回滚，避免用户以为切过去了。
  $$("#ruleSeg .seg-item").forEach((btn) => {
    btn.addEventListener("click", async () => {
      const strategy = btn.dataset.value;
      if (strategy === currentStrategy) return;
      const prev = currentStrategy;
      currentStrategy = strategy;
      renderSeg();
      setHint($("#ruleStatus"), "切换中…");
      try {
        const resp = await fetch("/_api/rules", {
          method: "PUT", headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ strategy }),
        });
        if (!resp.ok) {
          const err = await resp.json().catch(() => ({}));
          currentStrategy = prev;
          renderSeg();
          setHint($("#ruleStatus"), "失败：" + ((err.error && err.error.message) || resp.status), "err");
          return;
        }
        setHint($("#ruleStatus"), "已切到 " + (STRATEGY_LABEL[strategy] || strategy), "ok");
        loadRules();
      } catch (e) {
        currentStrategy = prev;
        renderSeg();
        setHint($("#ruleStatus"), "失败：" + e.message, "err");
      }
    });
  });

  // ----- 规则：仿真词典 -----
  let dictTypes = [];  // 可仿真的实体类型（服务端给出，顺序固定）
  let dictRows = [];   // 编辑中的行：[{type, real, fake}]

  function markDictDirty() {
    setHint($("#dictStatus"), "有未保存的改动", "warn");
  }

  function renderDict() {
    const tb = $("#dictBody");
    if (dictRows.length === 0) {
      tb.innerHTML = '<tr><td colspan="5" class="dict-empty">还没有自定义词条——所有实体都走内置词表 + 确定性派生。</td></tr>';
      return;
    }
    tb.innerHTML = dictRows.map((row, i) => {
      const opts = dictTypes.map((t) =>
        `<option value="${escape(t)}"${t === row.type ? " selected" : ""}>${escape(t)}</option>`).join("");
      return `<tr>
        <td><select data-i="${i}" data-f="type">${opts}</select></td>
        <td><input data-i="${i}" data-f="real" value="${escape(row.real)}" placeholder="真实值，如 张三" /></td>
        <td class="dict-arrow">→</td>
        <td><input data-i="${i}" data-f="fake" value="${escape(row.fake)}" placeholder="仿真值，如 王五" /></td>
        <td><button type="button" class="btn tiny danger" data-del="${i}" title="删除这一行">✕</button></td>
      </tr>`;
    }).join("");

    // input 事件只改数据、不重渲染——重渲染会丢焦点。
    $$("#dictBody [data-f]").forEach((el) => {
      const handler = () => {
        dictRows[Number(el.dataset.i)][el.dataset.f] = el.value;
        markDictDirty();
      };
      el.addEventListener("input", handler);
      el.addEventListener("change", handler);
    });
    // 删除才重渲染，索引与渲染时一致（input 期间行序没变）。
    $$("#dictBody [data-del]").forEach((el) => {
      el.addEventListener("click", () => {
        dictRows.splice(Number(el.dataset.del), 1);
        renderDict();
        markDictDirty();
      });
    });
  }

  async function loadDictionary() {
    try {
      const resp = await fetch("/_api/dictionary");
      const data = await resp.json();
      dictTypes = data.types || [];
      const dict = data.dictionary || {};
      dictRows = [];
      Object.keys(dict).forEach((t) => {
        Object.keys(dict[t] || {}).forEach((real) => {
          dictRows.push({ type: t, real: real, fake: dict[t][real] });
        });
      });
      dictRows.sort((a, b) => cmp(a.type, b.type) || cmp(a.real, b.real));
      renderDict();
      setHint($("#dictStatus"), dictRows.length === 0 ? "" : "共 " + dictRows.length + " 条", "ok");
    } catch (e) { /* ignore */ }
  }

  function cmp(a, b) { return a < b ? -1 : a > b ? 1 : 0; }

  // 收集编辑中的行 → PUT 的 dictionary 结构；顺带在客户端先拦一遍
  // 「仿真值重复」——服务端也会拦，但本地报错能定位到具体哪两行。
  // 注意是全局查重：还原表是一张平表，跨类型撞车一样会串。
  function collectDict() {
    const out = {};
    const seenFake = {}; // 仿真值 → "类型/真实值"
    for (const row of dictRows) {
      const real = (row.real || "").trim();
      const fake = (row.fake || "").trim();
      if (!real && !fake) continue; // 整行空白 → 视作没填，忽略
      if (!real) return { error: "有一行只填了仿真值，真实值为空" };
      if (!fake) return { error: "「" + real + "」的仿真值为空" };
      if (seenFake[fake]) {
        return { error: "仿真值「" + fake + "」被 " + seenFake[fake] + " 和 " + row.type + "/" + real + " 共用，还原会串" };
      }
      seenFake[fake] = row.type + "/" + real;
      if (!out[row.type]) out[row.type] = {};
      out[row.type][real] = fake;
    }
    return { dict: out };
  }

  function dictCount(dict) {
    return Object.keys(dict).reduce((n, t) => n + Object.keys(dict[t]).length, 0);
  }

  $("#dictAdd").addEventListener("click", () => {
    dictRows.push({ type: dictTypes[0] || "zh_person_name", real: "", fake: "" });
    renderDict();
    markDictDirty();
    const reals = $$("#dictBody input[data-f='real']");
    if (reals.length) reals[reals.length - 1].focus();
  });

  $("#dictSave").addEventListener("click", async () => {
    const res = collectDict();
    if (res.error) { setHint($("#dictStatus"), res.error, "err"); return; }
    setHint($("#dictStatus"), "保存中…");
    try {
      const resp = await fetch("/_api/dictionary", {
        method: "PUT", headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ dictionary: res.dict }),
      });
      if (!resp.ok) {
        const err = await resp.json().catch(() => ({}));
        setHint($("#dictStatus"), "失败：" + ((err.error && err.error.message) || resp.status), "err");
        return;
      }
      setHint($("#dictStatus"), "已生效（" + dictCount(res.dict) + " 条）", "ok");
      loadDictionary();
    } catch (e) {
      setHint($("#dictStatus"), "失败：" + e.message, "err");
    }
  });

  // 词典只存在运行时。导出成 YAML 片段，粘进 config 才能跨重启保留。
  $("#dictExport").addEventListener("click", () => {
    const res = collectDict();
    if (res.error) { setHint($("#dictStatus"), res.error, "err"); return; }
    const pre = $("#dictYAML");
    const types = Object.keys(res.dict).sort();
    if (types.length === 0) {
      pre.textContent = "# 词典为空，没有可导出的内容";
    } else {
      const lines = ["replacement:", "  simulate_zh:", "    dictionary:"];
      types.forEach((t) => {
        lines.push("      " + t + ":");
        Object.keys(res.dict[t]).sort().forEach((real) => {
          lines.push("        " + JSON.stringify(real) + ": " + JSON.stringify(res.dict[t][real]));
        });
      });
      pre.textContent = lines.join("\n");
    }
    pre.classList.remove("hidden");
    pre.scrollIntoView({ behavior: "smooth", block: "nearest" });
  });

  // ----- 规则：登记表（用户自报真实 PII 值，检测层补召回） -----
  let regTypes = [];    // 可登记的实体类型（服务端给出）
  let regRows = [];     // 编辑中的行：[{type, value}]
  let regEnabled = false;

  // 只折 ASCII A-Z，与 Go 侧 registry.foldASCII 一致。
  // 不能用 toLowerCase()——那是 Unicode 折叠，会和服务端的判重规则分叉，
  // 本地报重复而服务端放行（或反之）都很难解释。
  function foldASCII(s) {
    return s.replace(/[A-Z]/g, (c) => c.toLowerCase());
  }

  function markRegDirty() {
    setHint($("#regStatus"), "有未保存的改动", "warn");
  }

  function setRegEnabled(on, path) {
    regEnabled = on;
    $("#regPath").textContent = path || "—";
    const notice = $("#regNotice");
    if (!on) {
      notice.classList.add("off");
      $("#regAdd").disabled = true;
      $("#regSave").disabled = true;
    } else {
      notice.classList.remove("off");
      $("#regAdd").disabled = false;
      $("#regSave").disabled = false;
    }
  }

  function renderReg() {
    const tb = $("#regBody");
    if (regRows.length === 0) {
      tb.innerHTML = '<tr><td colspan="3" class="dict-empty">还没有登记项——'
        + (regEnabled
          ? "把模型漏检的真实值填进来，出现即脱敏。"
          : "登记表未启用。") + "</td></tr>";
      return;
    }
    tb.innerHTML = regRows.map((row, i) => {
      const opts = regTypes.map((t) =>
        `<option value="${escape(t)}"${t === row.type ? " selected" : ""}>${escape(t)}</option>`).join("");
      return `<tr>
        <td><select data-ri="${i}" data-rf="type">${opts}</select></td>
        <td><input data-ri="${i}" data-rf="value" value="${escape(row.value)}" placeholder="真实值，如 张三" /></td>
        <td><button type="button" class="btn tiny danger" data-rdel="${i}" title="删除这一行">✕</button></td>
      </tr>`;
    }).join("");

    // input 只改数据、不重渲染（重渲染会丢焦点）；删除才重渲染，索引与渲染时一致。
    $$("#regBody [data-rf]").forEach((el) => {
      const handler = () => {
        regRows[Number(el.dataset.ri)][el.dataset.rf] = el.value;
        markRegDirty();
      };
      el.addEventListener("input", handler);
      el.addEventListener("change", handler);
    });
    $$("#regBody [data-rdel]").forEach((el) => {
      el.addEventListener("click", () => {
        regRows.splice(Number(el.dataset.rdel), 1);
        renderReg();
        markRegDirty();
      });
    });
  }

  async function loadRegistry() {
    try {
      const resp = await fetch("/_api/registry");
      const data = await resp.json();
      regTypes = data.types || [];
      setRegEnabled(!!data.enabled, data.path);
      const reg = data.registry || {};
      regRows = [];
      Object.keys(reg).forEach((t) => {
        (reg[t] || []).forEach((v) => regRows.push({ type: t, value: v }));
      });
      regRows.sort((a, b) => cmp(a.type, b.type) || cmp(a.value, b.value));
      renderReg();
      if (!data.enabled) {
        setHint($("#regStatus"),
          "未启用：配置里打开 detection.registry.enabled 并设好 detection.registry.path，重启后可用", "warn");
      } else {
        setHint($("#regStatus"), regRows.length === 0 ? "" : "共 " + regRows.length + " 条", "ok");
      }
    } catch (e) { /* ignore */ }
  }

  // 收集编辑中的行 → PUT 结构。本地先拦一遍，报错能定位到具体是哪儿的问题；
  // 服务端规则相同（registry.Validate），且服务端消息不回显值。
  function collectReg() {
    const out = {};
    const seen = {}; // 折叠值 → "类型/值"
    for (const row of regRows) {
      const v = (row.value || "").trim();
      if (!v) continue; // 整行空白 → 视作没填
      if (v !== row.value) return { error: "「" + v + "」首尾有空白，去掉再保存" };
      if (Array.from(v).length < 2) {
        return { error: "「" + v + "」只有 1 个字符，会把正文里每次出现都当成 PII，太短不能登记" };
      }
      const f = foldASCII(v);
      if (seen[f]) {
        return { error: "「" + v + "」同时登记在 " + seen[f].split("/")[0] + " 和 " + row.type + "，同一个值只能属于一个类型" };
      }
      seen[f] = row.type + "/" + v;
      (out[row.type] = out[row.type] || []).push(v);
    }
    return { reg: out };
  }

  function regCount(reg) {
    return Object.keys(reg).reduce((n, t) => n + reg[t].length, 0);
  }

  $("#regAdd").addEventListener("click", () => {
    regRows.push({ type: regTypes[0] || "zh_person_name", value: "" });
    renderReg();
    markRegDirty();
    const vals = $$("#regBody input[data-rf='value']");
    if (vals.length) vals[vals.length - 1].focus();
  });

  $("#regSave").addEventListener("click", async () => {
    const res = collectReg();
    if (res.error) { setHint($("#regStatus"), res.error, "err"); return; }
    setHint($("#regStatus"), "保存中…");
    try {
      const resp = await fetch("/_api/registry", {
        method: "PUT", headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ registry: res.reg }),
      });
      if (!resp.ok) {
        const err = await resp.json().catch(() => ({}));
        setHint($("#regStatus"), "失败：" + ((err.error && err.error.message) || resp.status), "err");
        return;
      }
      setHint($("#regStatus"), "已生效（" + regCount(res.reg) + " 条，已落盘）", "ok");
      loadRegistry();
    } catch (e) {
      setHint($("#regStatus"), "失败：" + e.message, "err");
    }
  });

  // ----- 审计 -----
  async function auditRefresh() {
    const limit = $("#auditLimit") ? $("#auditLimit").value : 50;
    const status = $("#auditStatus");
    try {
      const resp = await fetch("/_api/audit?limit=" + encodeURIComponent(limit), { headers: { "Accept": "application/json" } });
      if (!resp.ok) { if (status) status.textContent = "查询失败 " + resp.status; return; }
      const data = await resp.json();
      const events = data.events || [];
      renderAudit(events);
      if (status) status.textContent = "共 " + (data.count != null ? data.count : events.length) + " 条";
    } catch (e) {
      if (status) status.textContent = "请求失败：" + e.message;
    }
  }
  function renderAudit(events) {
    const tb = $("#auditBody");
    if (!tb) return;
    if (events.length === 0) {
      tb.innerHTML = '<tr><td colspan="9" style="color: var(--text-dim); text-align:center;">暂无审计事件（需开启 audit.enabled）</td></tr>';
      return;
    }
    tb.innerHTML = events.map((e) => {
      const ents = (e.detected_entities || []).map((x) => x.type).join(", ") || "—";
      const cls = e.outcome === "blocked" ? "blocked" : (e.outcome === "error" ? "error" : "passed");
      return `<tr data-detail='${escape(JSON.stringify(e))}'>
        <td>${escape(fmtTime(e.timestamp))}</td>
        <td>${escape(e.model || "—")}</td>
        <td>${escape(e.upstream || "—")}</td>
        <td>${escape(ents)}</td>
        <td>${escape(e.replaced_count)}</td>
        <td>${escape(e.strategy || "—")}</td>
        <td>${e.restored ? "✓" : "✗"}</td>
        <td><span class="badge ${cls}">${escape(e.outcome || "?")}</span></td>
        <td>${escape(fmtDuration(e.latency_ms))}</td>
      </tr>`;
    }).join("");
    $$("#auditBody tr[data-detail]").forEach((tr) => {
      tr.addEventListener("click", () => {
        const raw = tr.getAttribute("data-detail");
        let obj; try { obj = JSON.parse(raw); } catch { obj = null; }
        const block = $("#auditDetailBlock");
        const pre = $("#auditDetail");
        if (!obj) { block.classList.add("hidden"); return; }
        block.classList.remove("hidden");
        pre.textContent = JSON.stringify(obj, null, 2);
        pre.scrollIntoView({ behavior: "smooth", block: "nearest" });
      });
    });
  }
  if ($("#auditRefresh")) $("#auditRefresh").addEventListener("click", auditRefresh);
  if ($("#auditLimit")) $("#auditLimit").addEventListener("change", auditRefresh);

  // ----- 启动 -----
  connectWS();
  refreshTraffic();
  loadRules();
  loadDictionary();
  loadRegistry();
  // 周期性拉取兜底（WS 断线期间不丢记录）
  setInterval(refreshTraffic, 3000);
  // 审计页自动刷新（仅当审计 Tab 激活且开启自动刷新）
  setInterval(() => {
    const active = $(".tab.active");
    if (active && active.dataset.tab === "audit" && $("#auditAuto") && $("#auditAuto").checked) {
      if (typeof auditRefresh === "function") auditRefresh();
    }
  }, 3000);
})();
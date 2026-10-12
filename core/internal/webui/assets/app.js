/* 值班台：對話看板 + 設定抽屜。無建置，Alpine 管狀態，資料來自 daemon（api.js）。
   規則：任何來自 agent 或使用者的文字一律用 x-text 顯示，從不當成標記塞進頁面。 */
(function () {
  const S = window.SCHEMA, API = window.API;

  // ── 索引：鍵 → 欄位（列表項欄位用 "mcp.servers[].level" 這種形式）──────────
  const byKey = {}, secOf = {};
  S.sections.forEach((sec) => sec.blocks.forEach((blk) => blk.fields.forEach((f) => {
    byKey[f.key] = f; f._sec = sec; f._blk = blk; secOf[f.key] = sec.id;
    if (f.type === "list") f.item.fields.forEach((c) => { byKey[f.key + "[]." + c.key] = c; });
  })));
  S.byKey = byKey;

  const clone = (v) => (v === undefined ? undefined : JSON.parse(JSON.stringify(v)));
  const eq = (a, b) => JSON.stringify(a) === JSON.stringify(b);
  const baseKey = (k) => k.replace(/\[\d+\].*$/, "");
  const at = (ts) => (ts ? Date.parse(ts) : null);

  // ── 格式 ─────────────────────────────────────────────────────────
  function fmtBytes(n) {
    if (!(n > 0)) return "";
    const u = ["B", "KiB", "MiB", "GiB"]; let i = 0;
    while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
    return (Number.isInteger(n) ? n : n.toFixed(1)) + " " + u[i];
  }
  function rel(ts) {
    if (!ts) return "—";
    const s = (Date.now() - ts) / 1000;
    if (s < 60) return "剛剛";
    if (s < 3600) return Math.floor(s / 60) + " 分鐘前";
    if (s < 86400) return Math.floor(s / 3600) + " 小時前";
    if (s < 172800) return "昨天";
    return Math.floor(s / 86400) + " 天前";
  }
  function until(ts) {
    if (!ts) return "—";
    const m = Math.round((ts - Date.now()) / 60000);
    if (m < 1) return "不到 1 分鐘後";
    if (m < 60) return m + " 分鐘後";
    if (m < 1440) return Math.round(m / 60) + " 小時後";
    return Math.round(m / 1440) + " 天後";
  }
  function dur(ms) {
    const m = Math.round(ms / 60000);
    return m < 1 ? "不到 1 分鐘" : m < 60 ? m + " 分鐘" : Math.floor(m / 60) + " 小時 " + (m % 60) + " 分";
  }
  const WEEK = ["日", "一", "二", "三", "四", "五", "六"];
  function cron(x) {
    const m = String(x).match(/^(\d+) (\d+) \* \* (\*|\d)$/);
    if (!m) return x;
    const t = String(m[2]).padStart(2, "0") + ":" + String(m[1]).padStart(2, "0");
    return m[3] === "*" ? "每天 " + t : "每週" + WEEK[+m[3]] + " " + t;
  }

  const STATUS = { run: "進行中", approval: "等你核准", input: "等你回答", done: "已完成", failed: "失敗", cancelled: "已中斷", cancelling: "中斷中", idle: "尚無執行" };
  const RUN = {
    RUN_STATUS_QUEUED: "run", RUN_STATUS_RUNNING: "run", RUN_STATUS_AWAITING_APPROVAL: "approval", RUN_STATUS_AWAITING_INPUT: "input",
    RUN_STATUS_CANCELLING: "cancelling", RUN_STATUS_COMPLETED: "done", RUN_STATUS_CANCELLED: "cancelled", RUN_STATUS_FAILED: "failed",
  };
  const LIVE = new Set(["run", "approval", "input", "cancelling"]);
  const PLATFORM = { discord: "Discord", telegram: "Telegram" };

  function originLabel(o) {
    if (!o) return "未記錄";
    if (o.kind === "RUN_ORIGIN_KIND_GATEWAY") {
      const p = o.principal || {}, name = PLATFORM[p.platform] || p.platform || "聊天";
      return p.displayName ? name + " · " + p.displayName : name;
    }
    if (o.kind === "RUN_ORIGIN_KIND_LOCAL_CLIENT") return "本機 · " + (o.clientId || "client");
    return "未記錄";
  }

  const mapRun = (r) => ({
    id: r.id, sessionId: r.sessionId, ui: RUN[r.status] || "idle", kind: r.kind || "", parent: r.parentRunId || "",
    start: at(r.createdAt), end: at(r.finishedAt), origin: originLabel(r.origin), originRaw: r.origin,
  });

  document.addEventListener("alpine:init", () => {
    Alpine.data("app", () => ({
      // ── 介面狀態 ──
      ready: false, settled: false, paired: false, pairFailed: false, online: true,
      view: "board", sid: null, filter: "all",
      drawer: false, tab: "section", section: "provider", fileId: "persona", menu: false, q: "", helpAll: false,
      dialog: null, newTitle: "", newModel: "", target: null, toast: "", folds: {}, open: {}, busy: false,
      // ── 對話 ──
      sessions: [], runsBy: {}, approvals: [], questions: [], schedules: [], models: [], modelDefault: "",
      // ── 設定 ──
      cfg: { path: "", hash: "" }, file: {}, running: {}, lock: {}, edits: {},
      check: { problems: [], diff: [], sig: "", busy: false },
      docs: { persona: { saved: "", draft: "", hash: "", loaded: false }, agents: { saved: "", draft: "", hash: "", loaded: false } },
      S, daemonAddr: location.host,

      // ═══════════ 啟動 ═══════════
      async init() {
        const p = new URLSearchParams(location.search);
        if (p.get("section")) this.section = p.get("section");
        if (p.get("drawer")) this.drawer = true;
        if (p.get("q")) { this.q = p.get("q"); this.drawer = true; }

        try { await API.pairFromAddress(); } catch (_) { this.pairFailed = true; }
        this.paired = API.paired;
        this.settled = true;

        window.addEventListener("keydown", (e) => { if ((e.metaKey || e.ctrlKey) && e.key === ",") { e.preventDefault(); this.drawer = !this.drawer; } });
        this.$watch("editSig", () => this.scheduleCheck());
        if (this.paired) await this.start();
      },
      async start() {
        // 哪一個失敗都不該讓整頁起不來：憑證失效回到連線畫面，其餘告訴使用者。
        const guard = (p) => p.catch((e) => this.fail(e));
        await Promise.all([guard(this.loadConfig()), guard(this.loadModels()), this.refresh()]);
        if (!this.paired) return;
        const sid = new URLSearchParams(location.search).get("session");
        if (sid && this.sess(sid)) this.openSession(sid);
        this.ready = true;
        clearInterval(this._poll);
        // 看著才更新：分頁在背景時不打擾 daemon。
        this._poll = setInterval(() => { if (!document.hidden) this.refresh(); }, 4000);
      },
      esc() { if (this.dialog) this.dialog = null; else if (this.menu) this.menu = false; else if (this.drawer) this.drawer = false; },
      say(t) { this.toast = t; clearTimeout(this._t); this._t = setTimeout(() => (this.toast = ""), 4200); },

      // 呼叫失敗：憑證失效就回到連線畫面，其餘告訴使用者。
      fail(e) {
        if (e instanceof API.Unauthorized) { clearInterval(this._poll); this.paired = false; this.ready = false; this.settled = true; return; }
        if (e.code === "unavailable") { this.online = false; return; }
        this.say(e.message || "失敗了");
      },
      async act(call, ok) {
        try {
          await call(); if (ok) this.say(ok); await this.refresh();
          // 做完一件事，下一個狀態常常一兩秒內就到，不必等下一輪輪詢。
          setTimeout(() => this.refresh(), 1200);
        } catch (e) { this.fail(e); }
      },

      // ═══════════ 對話：讀 ═══════════
      statusZh: (s) => STATUS[s] || s,
      rel, until, dur, cron, fmtBytes,
      get defaultModel() {
        if (this.modelDefault) return this.modelDefault;
        const b = this.runningVal("provider.backend");
        return this.runningVal(b === "fake" ? "provider.fake_model" : "provider." + b + ".model") || "（未指定）";
      },
      async loadModels() {
        try {
          const r = await API.call("SessionService", "ListModels", {});
          this.models = (r.models || []).map((m) => ({ id: m.id, name: m.displayName || m.id }));
          this.modelDefault = r.default || r.current || "";
        } catch (e) { if (e instanceof API.Unauthorized) throw e; this.models = []; }
      },
      async refresh() {
        if (this.busy || !this.paired) return;
        this.busy = true;
        try {
          const [ss, sc] = await Promise.all([
            API.call("SessionService", "ListSessions", {}), API.call("SessionService", "ListSchedules", {}),
          ]);

          const sessions = (ss.sessions || []).map((s) => ({
            id: s.id, title: s.title || "（沒有標題）", model: s.model || "", created: at(s.createdAt), updated: at(s.updatedAt),
          })).sort((a, b) => b.updated - a.updated);

          // 執行紀錄一次一段對話，所以只抓最近更新的，加上正開著的那一段；
          // 沒變、也沒在跑的就不重抓。
          const want = sessions.slice(0, 30).map((s) => s.id);
          if (this.sid && !want.includes(this.sid)) want.push(this.sid);
          await Promise.all(want.map((id) => {
            const s = sessions.find((x) => x.id === id), cached = this.runsBy[id];
            if (cached && cached.at === (s && s.updated) && !cached.runs.some((r) => LIVE.has(r.ui))) return null;
            return API.call("SessionService", "ListRuns", { sessionId: id })
              .then((r) => { this.runsBy[id] = { at: s && s.updated, runs: (r.runs || []).map(mapRun).sort((a, b) => b.start - a.start) }; })
              .catch((e) => { if (e instanceof API.Unauthorized) throw e; });
          }));

          // 核准與提問只能按對話查。執行紀錄已經說了哪些對話在等，只問那些。
          const approvals = [], questions = [];
          await Promise.all(want.flatMap((id) => {
            const runs = (this.runsBy[id] || { runs: [] }).runs, calls = [];
            if (runs.some((r) => r.ui === "approval")) calls.push(API.call("SessionService", "ListApprovals", { sessionId: id }).then((r) => {
              (r.approvals || []).forEach((a) => approvals.push({
                id: a.id, sessionId: a.sessionId, runId: a.runId, tool: a.toolName, summary: a.summary || a.toolName,
                effects: a.effects || [], preview: a.preview || "", readForeign: !!a.readForeign, created: at(a.createdAt),
              }));
            }));
            if (runs.some((r) => r.ui === "input")) calls.push(API.call("SessionService", "ListQuestions", { sessionId: id }).then((r) => {
              (r.questions || []).forEach((q) => questions.push({
                id: q.id, sessionId: q.sessionId, runId: q.runId, prompt: q.prompt, created: at(q.createdAt),
                choice: q.kind === "QUESTION_KIND_CHOICE", options: (q.options || []).map((o) => ({ id: o.id, label: o.label, detail: o.detail || "" })),
              }));
            }));
            return calls;
          }));
          this.approvals = approvals;
          this.questions = questions;
          this.schedules = (sc.schedules || []).map((x) => ({
            id: x.id, expression: x.expression, zone: x.zone, prompt: x.prompt, sessionId: x.sessionId,
            paused: !!x.paused, next: at(x.nextAt),
          }));

          // 一段對話現在的樣子，由「等你的事」與最近一次執行決定。
          sessions.forEach((s) => {
            const runs = (this.runsBy[s.id] || { runs: [] }).runs.filter((r) => !r.kind), last = runs[0];
            s.origin = { label: last ? last.origin : "未記錄" };
            // 「等你」只在真的有東西等的時候說：剛被你答覆的執行還沒起來時，
            // 它的紀錄還寫著在等，但那時已經沒有東西等你了。
            s.status = this.approvals.some((a) => a.sessionId === s.id) ? "approval"
              : this.questions.some((q) => q.sessionId === s.id) ? "input"
              : last ? (last.ui === "approval" || last.ui === "input" ? "run" : last.ui) : "idle";
          });
          this.sessions = sessions;
          this.online = true;
        } catch (e) { this.fail(e); } finally { this.busy = false; }
      },
      sess(id) { return this.sessions.find((s) => s.id === id); },
      titleOf(id) { const s = this.sess(id); return s ? s.title : id; },
      get pending() {
        const a = this.approvals.map((x) => ({ type: "approval", id: x.id, sid: x.sessionId, x, created: x.created }));
        const q = this.questions.map((x) => ({ type: "question", id: x.id, sid: x.sessionId, x, created: x.created }));
        return a.concat(q).sort((m, n) => n.created - m.created);
      },
      pendingOf(sid) { return this.pending.filter((p) => p.sid === sid); },
      get cards() { return this.view === "session" ? this.pendingOf(this.sid) : this.pending; },
      get counts() {
        const c = { all: this.sessions.length, run: 0, wait: 0, ended: 0 };
        this.sessions.forEach((s) => {
          if (s.status === "run" || s.status === "cancelling") c.run++;
          else if (s.status === "approval" || s.status === "input") c.wait++;
          else if (["done", "failed", "cancelled"].includes(s.status)) c.ended++;
        });
        return c;
      },
      get list() {
        const f = this.filter;
        return this.sessions.filter((s) => f === "all" || (f === "run" && (s.status === "run" || s.status === "cancelling")) ||
          (f === "wait" && (s.status === "approval" || s.status === "input")) || (f === "ended" && ["done", "failed", "cancelled"].includes(s.status)));
      },
      runsOf(sid) { return (this.runsBy[sid] || { runs: [] }).runs; },
      timeline(sid) {
        const W = 24 * 3600e3, t0 = Date.now() - W;
        return this.runsOf(sid).filter((r) => !r.kind && r.start).map((r) => {
          const end = r.end || Date.now(), l = Math.max(0, (r.start - t0) / W * 100), w = Math.max(2.2, (end - Math.max(r.start, t0)) / W * 100);
          return { l: Math.min(l, 97.8), w, c: r.ui === "run" ? "run" : r.ui === "approval" || r.ui === "input" ? "wait" : r.ui === "failed" ? "fail" : "", over: end < t0 };
        }).filter((x) => !x.over);
      },
      get detail() { return this.sess(this.sid); },
      openSession(id) { this.view = "session"; this.sid = id; this.refresh(); },
      back() { this.view = "board"; this.sid = null; },
      schedulesOf(sid) { return this.schedules.filter((s) => s.sessionId === sid); },

      // ═══════════ 對話：做 ═══════════
      setModel(s, id) {
        return this.act(() => API.call("SessionService", "SetSessionModel", { meta: API.meta(), sessionId: s.id, model: id }),
          "這段對話之後改用 " + (id || this.defaultModel));
      },
      interrupt(s) {
        const run = this.runsOf(s.id).find((r) => !r.kind && LIVE.has(r.ui));
        if (!run) return null;
        return this.act(() => API.call("SessionService", "InterruptRun", { meta: API.meta(), runId: run.id }), "已送出中斷「" + s.title + "」");
      },
      decide(p, ok) {
        return this.act(() => API.call("SessionService", "DecideApproval", {
          meta: API.meta(), approvalId: p.id, remember: "REMEMBER_SCOPE_ONCE",
          decision: ok ? "APPROVAL_DECISION_ALLOW" : "APPROVAL_DECISION_DENY",
        }), (ok ? "已核准" : "已拒絕") + "：" + p.x.summary);
      },
      answer(p, text) {
        if (!String(text || "").trim()) return null;
        return this.act(() => API.call("SessionService", "AnswerQuestion", { meta: API.meta(), questionId: p.id, answer: String(text).trim() }),
          "已回答，這段對話接著執行");
      },
      openNew() { this.newTitle = ""; this.newModel = ""; this.dialog = "new"; },
      async createSession() {
        const title = this.newTitle.trim() || "未命名的對話";
        try {
          const made = await API.call("SessionService", "CreateSession", { meta: API.meta(), title });
          const id = made.session.id;
          if (this.newModel) await API.call("SessionService", "SetSessionModel", { meta: API.meta(), sessionId: id, model: this.newModel });
          this.dialog = null; await this.refresh(); this.openSession(id); this.say("已建立「" + title + "」");
        } catch (e) { this.fail(e); }
      },
      pause(sc) {
        return this.act(() => API.call("SessionService", "SetSchedulePaused", { meta: API.meta(), scheduleId: sc.id, paused: !sc.paused }),
          sc.paused ? "已恢復排程" : "已暫停排程");
      },
      askDelete(sc) { this.target = sc; this.dialog = "delsch"; },
      async doDelete() {
        const id = this.target.id; this.dialog = null;
        await this.act(() => API.call("SessionService", "DeleteSchedule", { meta: API.meta(), scheduleId: id }), "已刪除排程");
      },

      // ═══════════ 設定：值、來源、暫存 ═══════════
      async loadConfig() {
        const c = await API.call("ConfigService", "GetConfig", {});
        this.cfg = { path: c.path, hash: c.hash || "" };
        this.file = c.file || {}; this.running = c.running || {};
        this.lock = {}; (c.overrides || []).forEach((o) => { this.lock[o.key] = o; });
      },
      def(key) { return byKey[key] ? clone(byKey[key].def) : undefined; },
      // 檔案裡的列表項可能少了空白欄位；補齊，這樣沒動過就是沒動過。
      filled(key, v) {
        const f = byKey[key];
        if (!f || f.type !== "list" || !Array.isArray(v)) return v;
        return v.map((item) => { const o = clone(item); f.item.fields.forEach((c) => { if (o[c.key] === undefined) o[c.key] = clone(c.def); }); return o; });
      },
      base(key) { return key in this.lock ? this.running[key] : key in this.file ? this.filled(key, clone(this.file[key])) : this.def(key); },
      runningVal(key) { return key in this.running ? this.running[key] : this.def(key); },
      val(key) { return key in this.edits ? this.edits[key] : this.base(key); },
      src(key) { return key in this.edits ? "edit" : key in this.lock ? "env" : key in this.file ? "file" : "default"; },
      locked(key) { return key in this.lock; },
      lockNote(key) {
        const o = this.lock[key]; if (!o) return "";
        return (o.source === "flag" ? "被旗標 " : "被環境變數 ") + o.name;
      },
      pendingKey(key) { return !!byKey[key] && !(key in this.lock) && !eq(key in this.file ? this.file[key] : this.def(key), key in this.running ? this.running[key] : this.def(key)); },
      setv(key, v) { if (this.locked(key)) return; if (eq(v, this.base(key))) delete this.edits[key]; else this.edits[key] = v; },
      revert(key) { this.setv(key, this.base(key)); },
      toDefault(key) { this.setv(key, this.def(key)); },
      isDefault(key) { return eq(this.val(key), this.def(key)); },

      // 列表項
      itemGet(lk, i, c) { const l = this.val(lk); return l[i] ? l[i][c] : undefined; },
      itemSet(lk, i, c, v) { const l = clone(this.val(lk)); l[i][c] = v; this.setv(lk, l); },
      blank(f) { const o = {}; f.item.fields.forEach((c) => (o[c.key] = clone(c.def))); return o; },
      addItem(f) { const l = clone(this.val(f.key)); l.push(this.blank(f)); this.setv(f.key, l); this.open[f.key + "[" + (l.length - 1) + "]"] = true; },
      delItem(f, i) { const l = clone(this.val(f.key)); l.splice(i, 1); this.setv(f.key, l); },
      itemTitle(f, i) {
        const v = this.itemGet(f.key, i, f.item.title);
        const t = Array.isArray(v) ? v.join("、") : v;
        return t || "（還沒有" + (f.item.title === "name" ? "名字" : "內容") + "）";
      },

      // ── 驗證：由 daemon 用它啟動時同一套檢查來回答，這裡不另寫一份 ──
      get editKeys() { return Object.keys(this.edits); },
      get editSig() { return JSON.stringify(this.edits); },
      editsPayload() {
        // 改回預設就是把那一行還原成註解，不是寫一行跟預設一樣的值。
        return this.editKeys.map((k) => (eq(this.edits[k], this.def(k)) ? { key: k, remove: true } : { key: k, value: this.edits[k] }));
      },
      scheduleCheck() {
        clearTimeout(this._ct);
        if (!this.editKeys.length) { this.check = { problems: [], diff: [], sig: this.editSig, busy: false }; return; }
        this.check.busy = true;
        this._ct = setTimeout(() => this.runCheck(), 250);
      },
      async runCheck() {
        const sig = this.editSig;
        try {
          const r = await API.call("ConfigService", "CheckConfig", { meta: API.meta(), edits: this.editsPayload() });
          if (sig !== this.editSig) return; // 這中間又改了，等下一次
          this.check = { problems: r.problems || [], diff: r.diff || [], sig, busy: false };
        } catch (e) { this.check.busy = false; this.fail(e); }
      },
      get problems() { return this.check.problems; },
      get checked() { return !this.check.busy && this.check.sig === this.editSig; },
      probs(path) { return this.problems.filter((p) => p.key === path); },
      secEdits(id) { return this.editKeys.filter((k) => secOf[baseKey(k)] === id).length; },
      secErrs(id) { return this.problems.filter((p) => secOf[baseKey(p.key)] === id).length; },
      get restartKeys() { return Object.keys(Object.assign({}, this.file, this.running)).filter((k) => this.pendingKey(k)); },
      goProblem() {
        const p = this.problems[0]; if (!p) return;
        this.q = ""; this.tab = "section"; this.section = secOf[baseKey(p.key)] || this.section; this.drawer = true;
        this.$nextTick(() => { const el = document.querySelector('[data-path="' + CSS.escape(p.key) + '"]'); if (el) { el.scrollIntoView({ block: "center" }); const i = el.querySelector("input,select,textarea,button"); if (i) i.focus(); } });
      },

      // ═══════════ 設定：畫面用的列 ═══════════
      get sectionNow() { return S.sections.find((s) => s.id === this.section); },
      whenOk(blk) { return !blk.when || this.val(blk.when.key) === blk.when.eq; },
      fieldHas(f) { return this.editKeys.some((k) => baseKey(k) === f.key) || this.problems.some((p) => baseKey(p.key) === f.key); },
      foldOpen(id, auto) { return id in this.folds ? this.folds[id] : auto; },
      flip(id, auto) { this.folds[id] = !this.foldOpen(id, auto); },
      fieldRows(f, out, hide) {
        const wide = ["strings", "map", "list"].includes(f.type) || ["url", "path", "secretfile", "addr"].includes(f.kind) || (f.type === "enum" && f.options.some((o) => o.note)) || (f.type === "bool" && f.help.length > 50);
        out.push({ id: f.key, kind: f.type === "list" ? "list" : "field", path: f.key, f, depth: 0, wide: wide || this.probs(f.key).length > 0, hide });
        if (f.type === "list") {
          const items = this.val(f.key) || [];
          items.forEach((it, i) => {
            const ik = f.key + "[" + i + "]", expanded = !!this.open[ik];
            out.push({ id: ik, kind: "item", f, i, path: ik, hide, expanded });
            if (expanded) f.item.fields.forEach((c) => out.push({ id: ik + "." + c.key, kind: "field", path: ik + "." + c.key, f: c, lk: f.key, i, depth: 1, hide, wide: ["strings", "map"].includes(c.type) || ["url", "path"].includes(c.kind) }));
          });
          out.push({ id: f.key + ":add", kind: "add", f, hide });
        }
      },
      get rows() {
        const out = [], q = this.q.trim().toLowerCase();
        if (q) {
          let n = 0;
          S.sections.forEach((sec) => {
            const hits = [];
            sec.blocks.forEach((blk) => blk.fields.forEach((f) => { if ((f.key + " " + f.zh + " " + f.help).toLowerCase().includes(q) && n < 60) { hits.push(f); n++; } }));
            if (!hits.length) return;
            out.push({ id: "s:" + sec.id, kind: "sec", sec });
            hits.forEach((f) => this.fieldRows(f, out, false));
          });
          if (!out.length) out.push({ id: "none", kind: "none" });
          return out;
        }
        const sec = this.sectionNow; if (!sec) return out;
        const on = sec.blocks.filter((b) => this.whenOk(b)), off = sec.blocks.filter((b) => !this.whenOk(b));
        const emit = (blk, hide) => {
          out.push({ id: "b:" + sec.id + ":" + sec.blocks.indexOf(blk), kind: "blk", blk, hide });
          const norm = blk.fields.filter((f) => !f.adv), adv = blk.fields.filter((f) => f.adv);
          norm.forEach((f) => this.fieldRows(f, out, hide));
          if (adv.length) {
            const fid = "adv:" + sec.id + ":" + sec.blocks.indexOf(blk), auto = adv.some((f) => this.fieldHas(f)), open = this.foldOpen(fid, auto);
            out.push({ id: fid, kind: "fold", fid, auto, open, label: "進階", n: adv.length, hide });
            adv.forEach((f) => this.fieldRows(f, out, hide || !open));
          }
        };
        on.forEach((b) => emit(b, false));
        if (off.length) {
          const fid = "oth:" + sec.id, auto = off.some((b) => b.fields.some((f) => this.fieldHas(f))), open = this.foldOpen(fid, auto);
          out.push({ id: fid, kind: "fold", fid, auto, open, label: sec.id === "provider" ? "其他後端的設定（都還在，切換後端不用重填）" : "另一個平台的設定", n: off.length, hide: false, strong: true });
          off.forEach((b) => emit(b, !open));
        }
        return out;
      },
      // 單一列的值
      ev(r) { return r.lk ? this.itemGet(r.lk, r.i, r.f.key) : this.val(r.path); },
      es(r, v) { if (r.lk) this.itemSet(r.lk, r.i, r.f.key, v); else this.setv(r.path, v); },
      shortKey(r) { return r.lk ? r.f.key : r.f.key.split(".").pop(); },
      rowLocked(r) { return !r.lk && this.locked(r.path); },
      rowSrc(r) { return r.lk ? "" : this.src(r.path); },
      hint(r) {
        const v = this.ev(r), f = r.f;
        if (f.unit === "bytes" && v > 0) return "＝ " + fmtBytes(v);
        if (f.unit === "ratio" && typeof v === "number") return "＝ " + Math.round(v * 100) + "%";
        return "";
      },
      num(r, raw) { const f = r.f; if (raw === "" || raw === null) return this.es(r, ""); this.es(r, f.type === "int" ? parseInt(raw, 10) : parseFloat(raw)); },
      // 字串清單
      addChip(r, text) { const t = String(text).trim().replace(/,$/, ""); if (!t) return; const v = (this.ev(r) || []).slice(); if (!v.includes(t)) v.push(t); this.es(r, v); },
      delChip(r, i) { const v = (this.ev(r) || []).slice(); v.splice(i, 1); this.es(r, v); },
      // map：一行一組 KEY=值
      mapText(r) { return Object.entries(this.ev(r) || {}).map(([k, v]) => k + "=" + v).join("\n"); },
      mapSet(r, text) { const o = {}; text.split("\n").forEach((l) => { const i = l.indexOf("="); if (i > 0) o[l.slice(0, i).trim()] = l.slice(i + 1).trim(); }); this.es(r, o); },

      // ═══════════ 寫入 ═══════════
      // daemon 回 kind/text；樣式用的是 ctx/del/add。
      get diff() { return this.check.diff.map((l) => ({ c: { context: "ctx", removed: "del", added: "add" }[l.kind] || "ctx", t: l.text })); },
      askWrite() { if (this.problems.length) return this.goProblem(); if (this.editKeys.length && this.checked) this.dialog = "write"; },
      async write() {
        const n = this.editKeys.length;
        try {
          const r = await API.call("ConfigService", "WriteConfig", { meta: API.meta(), hash: this.cfg.hash, edits: this.editsPayload() });
          this.dialog = null;
          if (!r.written) { this.check = { problems: r.problems || [], diff: this.check.diff, sig: this.editSig, busy: false }; this.goProblem(); return; }
          this.edits = {};
          await this.loadConfig();
          this.say("已寫入 " + n + " 項，重啟後生效");
        } catch (e) {
          if (e.code === "failed_precondition") {
            // 有人在你編輯的時候改了檔案：讀進新的，你的修改留著，確認後再寫。
            this.dialog = null; await this.loadConfig().catch(() => {});
            this.say("設定檔在你編輯時被改過，已重新載入。你的修改還在，確認後再寫入");
            return;
          }
          this.fail(e);
        }
      },
      discard() { this.edits = {}; this.dialog = null; this.say("已捨棄未寫入的修改"); },

      // ═══════════ 人格與指示 ═══════════
      docName: (id) => (id === "persona" ? "INSTRUCTION_FILE_PERSONA" : "INSTRUCTION_FILE_AGENTS"),
      async openDoc(id) {
        try {
          const r = await API.call("ConfigService", "GetInstructionFile", { file: this.docName(id) });
          this.docs[id] = { saved: r.text || "", draft: r.text || "", hash: r.hash || "", loaded: true };
        } catch (e) { this.fail(e); }
      },
      async saveDoc(id) {
        try {
          const r = await API.call("ConfigService", "PutInstructionFile", { meta: API.meta(), file: this.docName(id), text: this.docs[id].draft, hash: this.docs[id].hash });
          this.docs[id].saved = this.docs[id].draft; this.docs[id].hash = r.hash || "";
          this.say(id === "persona" ? "已儲存 PERSONA.md，下一次執行就會用到" : "已儲存 AGENTS.md，下一次執行就會用到");
        } catch (e) {
          if (e.code === "failed_precondition") { this.say("這個檔案在你編輯時被改過；重新載入後再改"); return; }
          this.fail(e);
        }
      },
      top() { this.$nextTick(() => { const el = document.querySelector(".drawer .db"); if (el) el.scrollTop = 0; }); },
      pick(id) { this.tab = "section"; this.section = id; this.menu = false; this.q = ""; this.top(); },
      pickFile(id) { this.tab = "file"; this.fileId = id; this.menu = false; this.q = ""; this.top(); if (!this.docs[id].loaded) this.openDoc(id); },
      // 把 `x` 轉成 <code>。只用在設定架構裡寫死的說明，先把 < > & 轉掉。
      md(t) { return String(t || "").replace(/[&<>]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;" }[c])).replace(/`([^`]+)`/g, "<code>$1</code>"); },
    }));
  });
})();

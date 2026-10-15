/* 值班台：對話看板 + 設定整頁。無建置，Alpine 管狀態，資料來自 daemon（api.js）。
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
  // 物件的鍵順序不算差別（儲存的設定與執行中的值排法不同）。
  const canon = (v) => JSON.stringify(v, (k, x) => (x && typeof x === "object" && !Array.isArray(x) ? Object.fromEntries(Object.entries(x).sort(([a], [b]) => (a < b ? -1 : 1))) : x));
  const eq = (a, b) => canon(a) === canon(b);
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
    const m = String(x).match(/^([0-5]?\d) ([01]?\d|2[0-3]) \* \* (\*|[0-7])$/);
    if (!m) return x;
    const t = String(m[2]).padStart(2, "0") + ":" + String(m[1]).padStart(2, "0");
    return m[3] === "*" ? "每天 " + t : "每週" + WEEK[+m[3] % 7] + " " + t;
  }
  const browserZone = () => { try { return Intl.DateTimeFormat().resolvedOptions().timeZone || ""; } catch (_) { return ""; } };

  const STATUS = { run: "進行中", approval: "等你核准", input: "等你回答", done: "已完成", failed: "失敗", cancelled: "已中斷", cancelling: "中斷中", idle: "尚無執行" };
  const RUN = {
    RUN_STATUS_QUEUED: "run", RUN_STATUS_RUNNING: "run", RUN_STATUS_AWAITING_APPROVAL: "approval", RUN_STATUS_AWAITING_INPUT: "input",
    RUN_STATUS_CANCELLING: "cancelling", RUN_STATUS_COMPLETED: "done", RUN_STATUS_CANCELLED: "cancelled", RUN_STATUS_FAILED: "failed",
  };
  const LIVE = new Set(["run", "approval", "input", "cancelling"]);
  const PLATFORM = { discord: "Discord", telegram: "Telegram" };
  // 設定裡 channels / consoles 兩份清單各自對應的權限設定，和 daemon 啟動時套用的一致。
  const PROFILE = { gateway: "對話頻道", console: "私人控制頻道" };
  const TRUST = {
    TRUST_LEVEL_UNTRUSTED: "不可信", TRUST_LEVEL_USER: "使用者", TRUST_LEVEL_WORKSPACE: "工作區", TRUST_LEVEL_SYSTEM: "系統",
  };
  const MEM_LIMIT = 200;
  const ymd = (d) => d.getFullYear() + "-" + String(d.getMonth() + 1).padStart(2, "0") + "-" + String(d.getDate()).padStart(2, "0");
  const words = (t) => String(t || "").split(/[\s,，、]+/).filter(Boolean);

  // 三份存在 daemon 資料庫裡的文件。file / enum 是 ConfigService 對它們的稱呼。
  const DOCS = {
    config: { zh: "設定", name: "config", enum: "SETTINGS_DOCUMENT_CONFIG",
      lead: "儲存的設定全文。註解、順序和這個版本不認得的設定都原樣在裡面；儲存時先做 daemon 啟動時對設定做的同一套檢查，通不過就不會存；金鑰、模型、沙盒這類要到啟動才看得到的問題不在其中。",
      note: "全文不遮蔽任何東西，填在 mcp.servers 的標頭與環境變數裡的金鑰也在其中，舊版本裡也可能還有（見版本紀錄）。daemon 只在啟動時讀設定，儲存後要重啟才會生效。" },
    persona: { zh: "人格", name: "persona", enum: "SETTINGS_DOCUMENT_PERSONA", file: "INSTRUCTION_FILE_PERSONA",
      lead: "它是誰、怎麼說話。",
      note: "存在 daemon 的資料庫裡，不是 workspace 裡的檔案。daemon 只在啟動時讀，儲存後要重啟才會用上；有改動的儲存都會留一個版本，可以在版本紀錄還原。" },
    agents: { zh: "指示", name: "agents", enum: "SETTINGS_DOCUMENT_AGENTS", file: "INSTRUCTION_FILE_AGENTS",
      lead: "做事的方式與專案慣例。通用的 AGENTS.md 格式，其他工具也會讀。",
      note: "存在 daemon 的資料庫裡，不是 workspace 裡的檔案。daemon 只在啟動時讀，儲存後要重啟才會用上；有改動的儲存都會留一個版本，可以在版本紀錄還原。" },
  };
  // 金鑰檔的狀態，照 CredentialService 的列舉說成人話。這裡的字串都是寫死的；
  // 檔名、環境變數名稱和 fileDetail 是設定或系統給的，只用 x-text 放進頁面。
  const CRED_NEED = {
    CREDENTIAL_NEED_REQUIRED: "必要",
    CREDENTIAL_NEED_OPTIONAL: "選用，有就用，沒有也不會出錯",
    CREDENTIAL_NEED_UNUSED: "用不到，目前的設定沒有選到會讀它的功能",
  };
  const CRED_FILE = {
    CREDENTIAL_FILE_STATE_ABSENT: "還沒有這個檔案",
    CREDENTIAL_FILE_STATE_EMPTY: "是空的，讀起來等於沒有",
    CREDENTIAL_FILE_STATE_PRESENT: "有內容，只有擁有者讀得到",
    CREDENTIAL_FILE_STATE_WRONG_MODE: "有內容，但別人也讀得到，權限不對",
    CREDENTIAL_FILE_STATE_NOT_REGULAR: "那個名字上不是一般檔案（目錄、連結或裝置）",
    CREDENTIAL_FILE_STATE_UNREADABLE: "讀不了",
  };
  const CRED_WHY = {
    CREDENTIAL_FILE_STATE_INVALID_NAME: "檔名不能用",
    CREDENTIAL_FILE_STATE_WRONG_MODE: "檔案權限不對，別人讀得到",
    CREDENTIAL_FILE_STATE_NOT_REGULAR: "那個名字上不是一般檔案",
    CREDENTIAL_FILE_STATE_UNREADABLE: "檔案讀不了",
  };
  const stamp = (ts) => new Date(ts).toLocaleString("zh-TW", { hour12: false });

  // ── 技能：儲存庫、技能自己的檔案和 git 的錯誤訊息都是別人寫的字 ──
  // 只用 x-text 放進頁面。另外，看不見的字元（控制字元、零寬、tag 字元）和會改變顯示順序的字元（雙向控制）
  // 會讓人看到的和模型讀到的不是同一段話，所以顯示時換成看得見的 ‹U+XXXX›。這只影響畫面，
  // 送給 daemon 的名字、摘要和路徑一律是原字串。
  // 異體字選擇符（FE00–FE0F）跟在表情符號、文字後面是正常的，只有接在 ASCII 後面或連著出現時才標：
  // 那不是在修飾一個字，而是在夾帶看不見的東西。
  const HIDDEN = /[\u0000-\u0008\u000B\u000C\u000E-\u001F\u007F-\u009F\u00AD\u034F\u061C\u115F\u1160\u17B4\u17B5\u180B-\u180E\u200B-\u200F\u2028-\u202E\u2060-\u2064\u2066-\u206F\u2800\u3164\uFEFF\uFFA0\uFFF9-\uFFFB]|\r(?!\n)|[\u{E0000}-\u{E007F}\u{E0100}-\u{E01EF}]|(?<![^\x00-\x7F])[\uFE00-\uFE0F]|(?<=[\uFE00-\uFE0F])[\uFE00-\uFE0F]|[\uFE00-\uFE0F](?=[\uFE00-\uFE0F])/gu;
  // daemon 的驗證文字是英文。欄位常見的幾種換成中文一句話，原文留在 raw（顯示時放在 title）；
  // 認不得的原樣不動，不去翻譯每一句。
  const TYPE_ZH = { int: "整數", int64: "整數", uint: "整數", float64: "數字", float32: "數字", bool: "true 或 false", string: "文字" };
  const nice = (p) => {
    const why = String(p.why || ""), raw = why, key = p.key || "";
    const fixed = (w, f) => Object.assign({}, p, { why: w, fix: f, raw: raw + (p.fix ? "\n" + p.fix : "") });
    if (/cannot be written as/.test(why)) return fixed("這一格的值類型不對", "數字欄位只能填數字，填回正確的類型，或按「還原」。");
    if (/is not a duration/.test(why)) return fixed("時間格式不對", "寫成數字加單位，例如 30s、5m、1h30m。");
    if (why === "is empty" && /(^|\.|\])name$/.test(key)) return fixed("還沒有填名字", "每一項都要有名字。");
    if (/^toml:/.test(why)) return fixed("這段文字不是有效的 TOML 設定", "解析器指出的地方：" + why.replace(/^toml:\s*/, "") + "。照這處改正，或換回較早的版本。");
    const m = /'([^']+)' cannot parse value as '([^']+)'/.exec(why);
    if (m) return fixed(m[1] + " 要填" + (TYPE_ZH[m[2]] || m[2]) + "，現在寫的不是", "改成正確的類型，或換回較早的版本。");
    if (/^config: decode:/.test(why)) return fixed("有欄位的值類型不對", "改成正確的類型，或換回較早的版本。");
    return p;
  };
  const vis = (t) => String(t == null ? "" : t).replace(HIDDEN, (c) => "‹U+" + c.codePointAt(0).toString(16).toUpperCase().padStart(4, "0") + "›");
  const hidden = (t) => (String(t || "").match(HIDDEN) || []).length;
  // bytes 欄位在 JSON 裡是 base64。
  const utf8 = (b64) => (b64 ? new TextDecoder().decode(Uint8Array.from(atob(b64), (c) => c.charCodeAt(0))) : "");
  const srcOf = (s) => (s ? { repository: s.repository || "", commit: s.commit || "", path: s.path || "" } : { repository: "", commit: "", path: "" });
  const mapStaged = (s) => ({
    name: s.name || "", description: s.description || "", version: s.version || "", source: srcOf(s.source),
    treeDigest: s.treeDigest || "", size: Number(s.sizeBytes) || 0, replaces: !!s.replaces,
  });
  const DRIFT = {
    SKILL_DRIFT_EDITED: "SKILL.md 和安裝時不一樣",
    SKILL_DRIFT_TREE_EDITED: "旁邊的檔案和安裝時不一樣",
  };

  const blankDoc = () => ({ saved: "", draft: "", hash: "", runningHash: "", pending: false, version: 0, parseError: "", loaded: false, stale: false, busy: false, problems: [] });
  // 值從哪來：「已儲存」是寫在儲存的設定裡，不論是不是正在跑。
  const SRC = { env: "環境變數", saved: "已儲存", default: "預設", edit: "未寫入" };
  const VERSION_SOURCE = { web: "在網頁上儲存", file: "從舊版的檔案匯入", env: "環境變數給的初始內容", default: "範例設定的初始內容" };

  // 兩份文字逐行比，只留有改動的行和前後各兩行，其餘折成一行說明。
  function lineDiff(from, to) {
    const a = from.split("\n"), b = to.split("\n");
    let head = 0, tail = 0;
    while (head < a.length && head < b.length && a[head] === b[head]) head++;
    while (tail < a.length - head && tail < b.length - head && a[a.length - 1 - tail] === b[b.length - 1 - tail]) tail++;
    const x = a.slice(head, a.length - tail), y = b.slice(head, b.length - tail);
    const ops = a.slice(0, head).map((t) => ({ c: "ctx", t }));
    if (x.length * y.length > 1.5e6) {
      // shortcut: 改動的中段超過 150 萬格就不對齊、整段當成換掉，要更準的對齊時再換演算法。
      x.forEach((t) => ops.push({ c: "del", t })); y.forEach((t) => ops.push({ c: "add", t }));
    } else {
      const w = y.length + 1, L = new Uint32Array((x.length + 1) * w);
      for (let i = x.length - 1; i >= 0; i--) for (let j = y.length - 1; j >= 0; j--)
        L[i * w + j] = x[i] === y[j] ? L[(i + 1) * w + j + 1] + 1 : Math.max(L[(i + 1) * w + j], L[i * w + j + 1]);
      let i = 0, j = 0;
      while (i < x.length || j < y.length) {
        if (i < x.length && j < y.length && x[i] === y[j]) { ops.push({ c: "ctx", t: x[i] }); i++; j++; }
        else if (i < x.length && (j === y.length || L[(i + 1) * w + j] >= L[i * w + j + 1])) ops.push({ c: "del", t: x[i++] });
        else ops.push({ c: "add", t: y[j++] });
      }
    }
    a.slice(a.length - tail).forEach((t) => ops.push({ c: "ctx", t }));

    const near = new Array(ops.length).fill(false), out = [];
    ops.forEach((o, k) => { if (o.c !== "ctx") for (let d = -2; d <= 2; d++) if (k + d >= 0 && k + d < ops.length) near[k + d] = true; });
    // 一行的空隙就直接顯示，折起來反而比那一行還長。
    let skipped = [];
    const fold = () => { out.push(...(skipped.length > 1 ? [{ c: "ctx", t: "… " + skipped.length + " 行相同" }] : skipped)); skipped = []; };
    ops.forEach((o, k) => { if (near[k]) { fold(); out.push(o); } else skipped.push(o); });
    fold();
    return out;
  }

  function originLabel(o) {
    if (!o) return "未記錄";
    if (o.kind === "RUN_ORIGIN_KIND_GATEWAY") {
      // 顯示名稱是對方自己取的，會冒充別人；平台給的 id 不會，所以放在旁邊。
      const p = o.principal || {}, name = vis(PLATFORM[p.platform] || p.platform || "聊天"), id = p.principalId ? vis(p.principalId) : "";
      return p.displayName ? name + " · " + vis(p.displayName) + (id ? "（" + id + "）" : "") : id ? name + " · " + id : name;
    }
    if (o.kind === "RUN_ORIGIN_KIND_LOCAL_CLIENT") return "本機 · " + vis(o.clientId || "client");
    return "未記錄";
  }

  const mapRun = (r) => ({
    id: r.id, sessionId: r.sessionId, ui: RUN[r.status] || "idle", kind: r.kind || "", parent: r.parentRunId || "",
    start: at(r.createdAt), end: at(r.finishedAt), origin: originLabel(r.origin), originRaw: r.origin,
  });

  document.addEventListener("alpine:init", () => {
    Alpine.data("app", () => ({
      // ── 介面狀態 ──
      ready: false, settled: false, paired: false, pairFailed: false, pairWhy: "", online: true,
      // 對話讀不進來（loadErr）和連不上（online）是兩回事；lastOk / now 用來說「這是幾秒前的畫面」。
      loadErr: "", sessLoaded: false, lastOk: 0, now: Date.now(),
      newBusy: false, writing: false, _doing: {}, mapBad: {},
      mode: "", agentProblems: [], verdict: { state: "", problems: [] },
      view: "board", sid: null, filter: "all",
      // drawer 是「設定整頁開著」（它原本是側邊抽屜，變數名沿用）。
      drawer: false, tab: "section", section: "provider", docId: "persona", menu: false, q: "", helpAll: false,
      dialog: null, newTitle: "", newModel: "", target: null, toast: "", folds: {}, open: {}, busy: false,
      // ── 對話 ──
      sessions: [], runsBy: {}, approvals: [], questions: [], schedules: [], models: [], modelDefault: "",
      // ── 設定 ──
      stored: {}, running: {}, lock: {}, edits: {},
      check: { problems: [], diff: [], sig: "", busy: false },
      docs: { config: blankDoc(), persona: blankDoc(), agents: blankDoc() },
      hist: { doc: "config", versions: [], loaded: false, sel: null, problems: [], busy: false },
      // ── 金鑰：ListCredentials 說的狀態。金鑰本身不在任何狀態裡，只在輸入框裡。──
      // credForm 是開著輸入框的那一格，credExpect 是開它時看到的 modifiedAt（原字串）。
      // credAbsent：開輸入框時還沒有這個檔，送出時就告訴 daemon「應該還沒有」，這中間出現的檔不會被蓋掉。
      creds: [], credForm: "", credExpect: "", credAbsent: false, credBusy: false, credMsg: null,
      // ── 技能：ListSkills 說的狀態；rev 是正在審查的那一個暫存技能（GetStagedSkill 的結果）──
      skills: { loaded: false, error: "", installed: [], rejected: [], missing: [], staged: [], restart: false },
      skillAdd: false, skillSource: "", stageBusy: false, stageSecs: 0, stageErr: "", skillNote: "", skillErr: "", rev: null,
      // ── 排程：新增 ──
      schAdd: false, sf: { expression: "", prompt: "", zone: "", missed: "" }, schErr: "", schBusy: false,
      // ── 管理：技能、記憶、頻道三頁共用一個入口；mview 記得上次看的是哪一頁 ──
      mview: "skills",
      mem: { loaded: false, error: "", items: [], cut: false }, memAll: false, memNote: "",
      bind: { loaded: false, error: "", items: [] }, bindAdd: false, bindNote: "", bindErr: "", bindBusy: false,
      bf: { platform: "discord", account: "", tenant: "", channel: "", workspace: "default", profile: "gateway", users: "", roles: "" },
      // ── ChatGPT 登入：GetChatGPTAccount 說的狀態。沒有權杖、授權碼或貼上的網址，那些不進任何狀態。
      // gptFlow 是進行中的那一次登入；它的 url 是給操作者自己的瀏覽器的，只用 x-text 放進頁面，不放進任何連結。
      gpt: { loaded: false, error: "", signedIn: false, label: "", scopes: [], plan: false, expires: null, errCode: "", errMsg: "", restart: false },
      gptModels: { loaded: false, busy: false, list: [], error: "" },
      gptFlow: null, gptBusy: false, gptNote: "", gptOut: null,
      S, DOCS, daemonAddr: location.host,

      // ═══════════ 啟動 ═══════════
      async init() {
        const p = new URLSearchParams(location.search);
        if (p.get("section")) this.section = p.get("section");
        if (p.get("drawer")) this.drawer = true;
        if (p.get("q")) { this.q = p.get("q"); this.drawer = true; }

        try { await API.pairFromAddress(); } catch (e) { this.pairFailed = true; this.pairWhy = e instanceof API.Unauthorized ? "" : e.message; }
        this.paired = API.paired;
        this.settled = true;

        // 把新的連結貼進已經開著的分頁，只會改網址的片段、頁面不會重載；有配對碼就重載。
        window.addEventListener("hashchange", () => { if (/(^|[#&])pair=/.test(location.hash)) location.reload(); });
        window.addEventListener("keydown", (e) => { if ((e.metaKey || e.ctrlKey) && e.key === ",") { e.preventDefault(); this.drawer = !this.drawer; } });
        this.$watch("editSig", () => this.scheduleCheck());
        // 離開那一格、關掉抽屜或換搜尋，輸入框就收起來，打到一半的金鑰跟著丟掉。
        this.$watch("[drawer, tab, section, q].join('|')", () => this.credClose());
        // ChatGPT 的卡片只有一份，放在設定的「模型提供者」最上面；沒有 agent 的畫面要用它時，把它搬過去。
        this.$watch("[drawer, mode].join('|')", () => this.$nextTick(() => this.placeGpt()));
        if (this.paired) await this.start();
      },
      async start() {
        clearInterval(this._poll);
        // 看著才更新：分頁在背景時不打擾 daemon。
        this._poll = setInterval(() => { if (!document.hidden) this.tick(); }, 4000);
        await this.begin();
      },
      // 第一件事是問 daemon 有沒有 agent：沒有的話它只開著設定的服務，
      // 對話、記憶、頻道的呼叫一律不打，免得每一輪都得到一個「沒有這個東西」。
      async begin() {
        if (this._starting || !this.paired) return;
        this._starting = true;
        try {
          try { await this.loadState(); } catch (e) { this.fail(e, true); return; }
          // 哪一個失敗都不該讓整頁起不來：憑證失效回到連線畫面，其餘告訴使用者。
          const guard = (p) => p.catch((e) => this.fail(e, true));
          if (this.repair) {
            await Promise.all([guard(this.loadConfig()), guard(this.loadDocs()), guard(this.loadGpt())]);
          } else {
            await Promise.all([guard(this.loadConfig()), guard(this.loadDocs()), guard(this.loadGpt()), guard(this.loadModels()), guard(this.loadSkills()), this.refresh()]);
            if (!this.paired) return;
            const sid = new URLSearchParams(location.search).get("session");
            if (sid && this.sess(sid)) this.openSession(sid);
          }
          this.ready = true;
          this.placeGpt();
        } finally { this._starting = false; }
      },
      tick() {
        this.now = Date.now();
        if (!this.mode) return this.begin();
        // 打金鑰時不重畫那一塊；其餘時候順便看一眼金鑰檔有沒有被別處動過。
        // 儲存的設定讀不進來的時候不問：不知道檔名，這個呼叫只會被拒絕。
        if (!this.credForm && !this.docs.config.parseError) this.loadCreds().catch((e) => this.fail(e, true));
        this.loadGpt().catch((e) => this.fail(e, true));
        // 技能只在看著它的時候一直問：agent 自己取得的技能會出現在這裡。其他時候在開頁面和每次動作之後問。
        if (!this.repair && this.view === "skills") this.loadSkills().catch((e) => this.fail(e, true));
        return this.repair ? this.loadState().catch((e) => this.fail(e, true)) : this.refresh();
      },
      async loadState() {
        const r = await API.call("ConfigService", "GetAgentState", {});
        this.mode = r.mode === "repair" ? "repair" : "running";
        this.agentProblems = (r.problems || []).map((p) => ({ key: p.key || "", value: p.value || "", why: p.why || "", fix: p.fix || "", source: p.source || "" }));
        this.online = true; this.lastOk = Date.now();
      },
      // 重啟對進行中的對話的影響：停掉 daemon 時還在跑的會被中斷（記成已中斷）；停在等你核准或回答的、和已經結束的都還在。
      get restartNote() {
        const n = this.counts.run;
        return !this.repair && n ? "現在有 " + n + " 段對話正在執行，重啟會中斷它們；等你處理的和已經結束的都還在。" : "";
      },
      // 連不上的時候，畫面上的還是最後一次讀到的。
      get staleText() {
        const s = Math.max(0, Math.round((this.now - this.lastOk) / 1000));
        return s < 60 ? s + " 秒前" : rel(this.lastOk);
      },
      get repair() { return this.mode === "repair"; },
      // 啟動時的問題不會跟著修改變；現在儲存的設定已經沒有它了，就是改好、等重啟。
      fixed(p) {
        const v = this.verdict;
        // 缺的是 ChatGPT 登入：現在登入了、也允許用方案，就是改好、等重啟。
        if (p.source === "startup" && /^chatgpt:/i.test(p.why)) return this.gpt.signedIn && this.gpt.plan;
        return p.source === "settings" && (v.state === "ok" || v.state === "bad") && !v.problems.some((q) => q.key === p.key);
      },
      esc() { if (this.dialog) this.dialog = null; else if (this.credForm) this.credClose(); else if (this.menu) this.menu = false; else if (this.drawer && this.q) this.q = ""; else if (this.drawer) this.drawer = false; },
      // 通知裡常有對話標題、摘要這類別人寫的字，一律經過 vis()。
      say(t) { this.toast = vis(t); clearTimeout(this._t); this._t = setTimeout(() => (this.toast = ""), 4200); },

      // 呼叫失敗：憑證失效就回到連線畫面，其餘告訴使用者。quiet 是輪詢和讀取：連不上的時候由畫面上的常駐說明負責，不跳通知。
      fail(e, quiet) {
        if (e instanceof API.Unauthorized) {
          // 憑證失效（daemon 重啟過、或超過十二小時）：整頁重來，畫面上和記憶體裡的設定、核准、打到一半的字都不留。
          clearInterval(this._poll); this.paired = false; this.ready = false; this.settled = true; location.reload(); return;
        }
        if (e.code === "unavailable") { this.online = false; if (!quiet) this.say("連不上 daemon，這個動作沒有送出"); return; }
        this.say(e.message || "失敗了");
      },
      // key 是這個動作的身分：還沒做完時再按一次（雙擊）就不再送一次。做完、畫面更新之後才放開。
      async act(call, ok, key) {
        if (key && this._doing[key]) return;
        if (key) this._doing[key] = true;
        try {
          await call(); if (ok) this.say(ok); await this.refresh();
          // 做完一件事，下一個狀態常常一兩秒內就到，不必等下一輪輪詢。
          setTimeout(() => this.refresh(), 1200);
        } catch (e) { this.fail(e); } finally { if (key) delete this._doing[key]; }
      },

      // ═══════════ 對話：讀 ═══════════
      statusZh: (s) => STATUS[s] || s,
      rel, until, dur, cron, fmtBytes, vis, stamp,
      get boardKnown() { return this.sessLoaded && !this.loadErr; },
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
        if (this.busy || !this.paired || this.repair) return;
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
                effects: a.effects || [], preview: a.preview || "", arguments: a.arguments || "", readForeign: !!a.readForeign, created: at(a.createdAt),
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
            id: x.id, expression: x.expression, zone: x.zone || "", prompt: x.prompt, sessionId: x.sessionId,
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
          this.online = true; this.lastOk = Date.now(); this.sessLoaded = true; this.loadErr = "";
        } catch (e) {
          // 連不上或憑證失效交給 fail；其餘（例如 daemon 回的是錯誤）是常駐的說明，不是每四秒再跳一次的通知，
          // 而且空的看板不能被讀成「沒事」。
          if (e instanceof API.Unauthorized || e.code === "unavailable") this.fail(e, true);
          else this.loadErr = e.message || e.code || "失敗了";
        } finally { this.busy = false; }
      },
      sess(id) { return this.sessions.find((s) => s.id === id); },
      titleOf(id) { const s = this.sess(id); return vis(s ? s.title : id); },
      // 最舊的在前：新的請求排在後面，已經在看的那幾張不會被擠到別的位置，手正要按的按鈕不會換成另一個請求。
      get pending() {
        const a = this.approvals.map((x) => ({ type: "approval", id: x.id, sid: x.sessionId, x, created: x.created }));
        const q = this.questions.map((x) => ({ type: "question", id: x.id, sid: x.sessionId, x, created: x.created }));
        return a.concat(q).sort((m, n) => m.created - n.created);
      },
      // 核准的是這串參數，不是卡片上那行摘要：摘要每個參數只留前 60 個字。能解析就排整齊，不能就照原樣。
      argsText(a) { try { return JSON.stringify(JSON.parse(a), null, 2); } catch (_) { return a; } },
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
      back() { this.view = "board"; this.sid = null; this.drawer = false; },
      schedulesOf(sid) { return this.schedules.filter((s) => s.sessionId === sid); },

      // ═══════════ 對話：做 ═══════════
      setModel(s, id) {
        return this.act(() => API.call("SessionService", "SetSessionModel", { meta: API.meta(), sessionId: s.id, model: id }),
          "這段對話之後改用 " + (id || this.defaultModel));
      },
      interrupt(s) {
        const run = this.runsOf(s.id).find((r) => !r.kind && LIVE.has(r.ui));
        if (!run) return null;
        return this.act(() => API.call("SessionService", "InterruptRun", { meta: API.meta(), runId: run.id }), "已送出中斷「" + s.title + "」", "i:" + run.id);
      },
      // 按下去之後那張卡片就不再屬於「等你」：成功就先從清單拿掉，不必等下一輪讀取，也就不會有人對同一張再按一次。
      decide(p, ok) {
        return this.act(async () => {
          await API.call("SessionService", "DecideApproval", {
            meta: API.meta(), approvalId: p.id, remember: "REMEMBER_SCOPE_ONCE",
            decision: ok ? "APPROVAL_DECISION_ALLOW" : "APPROVAL_DECISION_DENY",
          });
          this.approvals = this.approvals.filter((a) => a.id !== p.id);
        }, (ok ? "已核准" : "已拒絕") + "：" + p.x.summary, "d:" + p.id);
      },
      answer(p, text) {
        if (!String(text || "").trim()) return null;
        return this.act(async () => {
          await API.call("SessionService", "AnswerQuestion", { meta: API.meta(), questionId: p.id, answer: String(text).trim() });
          this.questions = this.questions.filter((q) => q.id !== p.id);
        }, "已回答，這段對話接著執行", "a:" + p.id);
      },
      openNew() { this.newTitle = ""; this.newModel = ""; this.dialog = "new"; },
      async createSession() {
        if (this.newBusy) return;
        const title = this.newTitle.trim() || "未命名的對話";
        this.newBusy = true;
        try {
          const made = await API.call("SessionService", "CreateSession", { meta: API.meta(), title });
          const id = made.session.id;
          if (this.newModel) await API.call("SessionService", "SetSessionModel", { meta: API.meta(), sessionId: id, model: this.newModel });
          this.dialog = null; await this.refresh(); this.openSession(id); this.say("已建立「" + title + "」");
        } catch (e) { this.fail(e); } finally { this.newBusy = false; }
      },
      pause(sc) {
        return this.act(() => API.call("SessionService", "SetSchedulePaused", { meta: API.meta(), scheduleId: sc.id, paused: !sc.paused }),
          sc.paused ? "已恢復排程" : "已暫停排程", "p:" + sc.id);
      },
      askDelete(sc) { this.target = sc; this.dialog = "delsch"; },
      async doDelete() {
        const id = this.target.id; this.dialog = null;
        await this.act(() => API.call("SessionService", "DeleteSchedule", { meta: API.meta(), scheduleId: id }), "已刪除排程");
      },
      zoneLabel: (sc) => "（" + (sc.zone ? vis(sc.zone) : "daemon 所在機器的時區") + "）",
      // 新增：時間寫得對不對由 daemon 判斷，它的回答原樣放在表單上，這裡只擋空的。
      openSchedule() {
        this.schAdd = !this.schAdd; this.schErr = "";
        if (this.schAdd) this.sf = { expression: "", prompt: "", zone: browserZone(), missed: "" };
      },
      async createSchedule() {
        const f = this.sf, expression = f.expression.trim(), prompt = f.prompt.trim();
        if (this.schBusy || !expression || !prompt) return;
        this.schBusy = true; this.schErr = "";
        try {
          const r = await API.call("SessionService", "CreateSchedule", {
            meta: API.meta(), expression, zone: f.zone.trim(), prompt, missedPolicy: f.missed,
          });
          this.schAdd = false;
          const next = r.schedule && at(r.schedule.nextAt);
          this.say("已新增排程" + (next ? "，下次 " + until(next) : ""));
          await this.refresh(); setTimeout(() => this.refresh(), 1200);
        } catch (e) {
          if (e instanceof API.Unauthorized || e.code === "unavailable") this.fail(e);
          else this.schErr = e.message || e.code || "失敗了";
        } finally { this.schBusy = false; }
      },

      // ═══════════ 設定：值、來源、暫存 ═══════════
      async loadConfig() {
        const c = await API.call("ConfigService", "GetConfig", {});
        this.setDoc("config", c);
        this.stored = c.file || {}; this.running = c.running || {};
        this.lock = {}; (c.overrides || []).forEach((o) => { this.lock[o.key] = o; });
        // 金鑰檔是哪個檔名由儲存的設定決定，所以設定一變就問一次。
        await this.loadCreds();
        if (this.repair) await this.checkSaved();
      },
      // 沒有 agent 的時候，GetAgentState 說的是「啟動時」出了什麼事，不會跟著修改變。
      // 修改之後儲存的設定現在過不過，要另外問一次檢查。
      async checkSaved() {
        if (this.docs.config.parseError) { this.verdict = { state: "unreadable", problems: [] }; return; }
        const r = await API.call("ConfigService", "CheckConfig", { meta: API.meta(), edits: [] });
        const problems = (r.problems || []).map(nice);
        this.verdict = { state: problems.length ? "bad" : "ok", problems };
      },
      def(key) { return byKey[key] ? clone(byKey[key].def) : undefined; },
      // 儲存的設定裡的列表項可能少了空白欄位；補齊，這樣沒動過就是沒動過。
      filled(key, v) {
        const f = byKey[key];
        if (!f || f.type !== "list" || !Array.isArray(v)) return v;
        return v.map((item) => { const o = clone(item); f.item.fields.forEach((c) => { if (o[c.key] === undefined) o[c.key] = clone(c.def); }); return o; });
      },
      base(key) { return key in this.lock ? this.running[key] : key in this.stored ? this.filled(key, clone(this.stored[key])) : this.def(key); },
      runningVal(key) { return key in this.running ? this.running[key] : this.def(key); },
      val(key) { return key in this.edits ? this.edits[key] : this.base(key); },
      src(key) { return key in this.edits ? "edit" : key in this.lock ? "env" : key in this.stored ? "saved" : "default"; },
      srcZh: (s) => SRC[s],
      locked(key) { return key in this.lock; },
      lockNote(key) {
        const o = this.lock[key]; if (!o) return "";
        return (o.source === "flag" ? "被旗標 " : "被環境變數 ") + o.name;
      },
      // 兩邊都先補齊列表項的空白欄位再比：執行中的值是補過預設的，儲存的不一定。
      pendingKey(key) { return !!byKey[key] && !(key in this.lock) && !eq(this.filled(key, clone(key in this.stored ? this.stored[key] : this.def(key))), this.filled(key, clone(this.runningVal(key)))); },
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
      // 頭像字：名字的第一個字；還沒有名字就是「？」，不要拿「（還沒有名字）」的括號當頭像。
      itemInitial(f, i) {
        const v = this.itemGet(f.key, i, f.item.title), t = Array.isArray(v) ? v[0] : v;
        return (Array.from(String(t || ""))[0] || "？").toUpperCase();
      },
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
          this.check = { problems: (r.problems || []).map(nice), diff: r.diff || [], sig, busy: false };
        } catch (e) {
          if (e.code !== "invalid_argument") { this.check.busy = false; return this.fail(e, true); }
          // 值寫成了這個欄位吃不下的樣子（時間沒寫單位、數字欄位清空…）：daemon 不是用 problems 回，而是整個拒絕。
          // 把它掛在那個欄位上，和其他問題一樣顯示，寫入也才有個說法。
          if (sig !== this.editSig) return;
          const m = /^config: ([^\s:]+): (.*)$/s.exec(e.message || ""), key = m ? m[1] : this.editKeys[0];
          const why = m ? m[2] : e.message || "不能這樣寫";
          this.check = { problems: [nice({ key, why, fix: "" })], diff: [], sig, busy: false };
        }
      },
      get problems() { return this.check.problems; },
      get checked() { return !this.check.busy && this.check.sig === this.editSig; },
      // 一則問題的整句。中文句子用「。」收尾，daemon 的英文原樣用「.」。
      msg(p) { return (p.key ? p.key + " " : "") + p.why + (/[\u3000-\u303f\uff00-\uffef\u4e00-\u9fff]$/.test(p.why) ? "。" : "."); },
      probs(path) { return this.problems.filter((p) => p.key === path); },
      secEdits(id) { return this.editKeys.filter((k) => secOf[baseKey(k)] === id).length; },
      secErrs(id) { return this.problems.filter((p) => secOf[baseKey(p.key)] === id).length; },
      get restartKeys() { return Object.keys(Object.assign({}, this.stored, this.running)).filter((k) => this.pendingKey(k)); },
      // 帶你到出問題的那一格。不認得的鍵（或讀不進來的文字）沒有格子，就帶到設定原文。
      goProblem(p) {
        p = p || this.problems[0]; if (!p) return;
        const sec = secOf[baseKey(p.key)];
        this.q = ""; this.drawer = true;
        if (!sec) { this.pickTab("raw"); return; }
        this.tab = "section"; this.section = sec;
        // 欄位在收起來的「進階」或「其他後端」底下的話，先打開，才有地方可以對焦。
        const f = byKey[baseKey(p.key)];
        if (f && f._sec) {
          if (f.adv) this.folds["adv:" + f._sec.id + ":" + f._sec.blocks.indexOf(f._blk)] = true;
          if (!this.whenOk(f._blk)) this.folds["oth:" + f._sec.id] = true;
        }
        this.$nextTick(() => { const el = document.querySelector('[data-path="' + CSS.escape(p.key) + '"]'); if (el) { el.scrollIntoView({ block: "center" }); const i = el.querySelector("input,select,textarea") || el.querySelector("button"); if (i) i.focus(); } });
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
      // 設定頁的卡片：一個設定區塊一張卡，欄位是卡裡的列。收起來的進階欄位、沒選到的後端區塊不畫出來。
      // 「其他後端」的展開鈕不屬於任何一張卡，自己一項。
      get setCards() {
        const out = [];
        this.rows.forEach((r) => {
          if (r.kind === "sec" || r.kind === "blk") {
            const t = r.kind === "sec" ? r.sec : r.blk;
            out.push({ id: r.id, kind: "card", hide: r.hide, title: t.zh, code: "[" + t.table + "]", rows: [] });
          } else if (r.kind === "fold" && r.strong) out.push({ id: r.id, kind: "fold", row: r });
          else if (r.kind === "none") out.push({ id: r.id, kind: "none" });
          else if (!r.hide && out.length) out[out.length - 1].rows.push(r);
        });
        return out.filter((c) => !c.hide);
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
        if (f.key === "context.window" && v === 0) return "0 代表自動，向模型供應端查";
        return "";
      },
      num(r, raw) { const f = r.f; if (raw === "" || raw === null) return this.es(r, ""); this.es(r, f.type === "int" ? parseInt(raw, 10) : parseFloat(raw)); },
      // 字串清單
      addChip(r, text) { const t = String(text).trim().replace(/,$/, ""); if (!t) return; const v = (this.ev(r) || []).slice(); if (!v.includes(t)) v.push(t); this.es(r, v); },
      delChip(r, i) { const v = (this.ev(r) || []).slice(); v.splice(i, 1); this.es(r, v); },
      // map：一行一組 KEY=值
      mapText(r) { return Object.entries(this.ev(r) || {}).map(([k, v]) => k + "=" + v).join("\n"); },
      // 沒有「名稱=值」形式的行、和重複的名稱不會存，要說出來，不能靜靜地丟掉。
      mapSet(r, text) {
        const o = {}; let dropped = 0, dup = 0;
        text.split("\n").forEach((l) => {
          if (!l.trim()) return;
          const i = l.indexOf("=");
          if (i <= 0) { dropped++; return; }
          const k = l.slice(0, i).trim();
          if (Object.prototype.hasOwnProperty.call(o, k)) dup++;
          o[k] = l.slice(i + 1).trim();
        });
        this.mapBad[r.path] = [dropped ? dropped + " 行不是「名稱=值」的樣子，不會存" : "", dup ? dup + " 個名稱重複，只留最後一個" : ""].filter(Boolean).join("；");
        this.es(r, o);
      },

      // ═══════════ 寫入 ═══════════
      // daemon 回 kind/text；樣式用的是 ctx/del/add。
      get diff() { return this.check.diff.map((l) => ({ c: { context: "ctx", removed: "del", added: "add" }[l.kind] || "ctx", t: l.text })); },
      askWrite() { if (this.problems.length) return this.goProblem(); if (this.editKeys.length && this.checked) this.dialog = "write"; },
      async write() {
        if (this.writing) return;
        const n = this.editKeys.length;
        this.writing = true;
        try {
          const r = await API.call("ConfigService", "WriteConfig", { meta: API.meta(), hash: this.docs.config.hash, edits: this.editsPayload() });
          this.dialog = null;
          if (!r.written) { this.check = { problems: (r.problems || []).map(nice), diff: this.check.diff, sig: this.editSig, busy: false }; this.goProblem(); return; }
          this.edits = {};
          await this.loadConfig();
          this.say("已寫入 " + n + " 項" + this.afterSave("config"));
        } catch (e) {
          if (e.code === "failed_precondition") {
            // 有人在你編輯的時候改了設定：讀進新的，你的修改留著，確認後再寫。
            this.dialog = null; await this.loadConfig().catch(() => {});
            this.say("設定在你編輯時被改過，已重新載入。你的修改還在，確認後再寫入");
            return;
          }
          this.fail(e);
        } finally { this.writing = false; }
      },
      // 捨棄的是設定欄位的修改，或是目前這份文件的草稿；兩種都要先問過。
      get inDoc() { return (this.tab === "doc" || this.tab === "raw") && !this.q.trim(); },
      discard() {
        if (this.inDoc) this.revertDoc(this.curDoc); else this.edits = {};
        this.dialog = null; this.say("已捨棄尚未儲存的修改");
      },

      // ═══════════ 三份文件：設定原文、人格、指示 ═══════════
      // 設定原文也是一份文件，所以三份走同一條路：讀、存、版本紀錄。
      setDoc(id, r) {
        const d = this.docs[id], text = r.text || "";
        // 草稿沒改過就跟著新版本走；改過的留著，並標出底下的版本已經換了。
        if (d.draft === d.saved) d.draft = text; else if (d.saved !== text) d.stale = true;
        Object.assign(d, {
          saved: text, hash: r.hash || "", runningHash: r.runningHash || "", pending: !!r.pendingRestart,
          version: Number(r.version) || 0, parseError: r.parseError || "", loaded: true,
        });
        if (d.draft === d.saved) d.stale = false;
      },
      async loadDocs() { await Promise.all(["persona", "agents"].map((id) => this.loadDoc(id))); },
      loadDoc(id) {
        if (id === "config") return this.loadConfig();
        return API.call("ConfigService", "GetInstructionFile", { file: DOCS[id].file }).then((r) => this.setDoc(id, r));
      },
      dirty(id) { const d = this.docs[id]; return d.draft !== d.saved; },
      get unsaved() { return this.editKeys.length > 0 || Object.keys(this.docs).some((id) => this.dirty(id)); },
      // 存完該說的下一步：存回正在跑的那一份，就沒有什麼要重啟的。
      afterSave(id) { return this.repair || this.docs[id].pending ? "，重啟後才會生效" : "，和正在跑的一樣，不必重啟"; },
      docState(id) { return this.dirty(id) ? "有尚未儲存的修改" : this.docs[id].pending ? "已儲存，尚未生效" : "已儲存"; },
      // 存進驗證過的那條路：設定原文由 daemon 檢查，通不過就回問題、什麼都不存。
      async put(id, text) {
        const d = this.docs[id];
        if (id === "config") {
          const r = await API.call("ConfigService", "ReplaceConfigText", { meta: API.meta(), text, hash: d.hash });
          if (!r.written) return { problems: (r.problems || []).map(nice) };
          d.saved = d.draft = text; // loadConfig 再用 daemon 給的內容對一次
          await this.loadConfig();
        } else {
          const r = await API.call("ConfigService", "PutInstructionFile", { meta: API.meta(), file: DOCS[id].file, text, hash: d.hash });
          Object.assign(d, { saved: text, draft: text, hash: r.hash || "", pending: !!r.pendingRestart, version: Number(r.version) || 0, stale: false });
        }
        return { problems: [] };
      },
      async saveDoc(id) {
        const d = this.docs[id];
        if (d.busy || !this.dirty(id)) return;
        d.busy = true; d.problems = [];
        try {
          const r = await this.put(id, d.draft);
          if (r.problems.length) { d.problems = r.problems; return; }
          this.say("已儲存" + DOCS[id].zh + "（第 " + d.version + " 版）" + this.afterSave(id));
        } catch (e) {
          if (e.code === "failed_precondition") {
            // 有人在你編輯的時候存了新的：讀進新的，你的草稿留著。
            await this.loadDoc(id).catch(() => {});
            this.say("這份文件在你編輯時被改過，已載入新的版本。你的修改還在，確認後再儲存");
            return;
          }
          this.fail(e);
        } finally { d.busy = false; }
      },
      revertDoc(id) { const d = this.docs[id]; d.draft = d.saved; d.problems = []; d.stale = false; },

      // ═══════════ 版本紀錄 ═══════════
      versionSource: (s) => VERSION_SOURCE[s] || s,
      async openHistory(id) {
        const h = this.hist;
        if (h.doc !== id) { h.versions = []; h.loaded = false; }
        h.doc = id; h.sel = null; h.problems = [];
        try {
          // 現在這一版也重讀：比較和還原都以它為準，不以頁面上舊的那份為準。
          const [list] = await Promise.all([
            API.call("ConfigService", "ListDocumentVersions", { document: DOCS[id].enum }), this.loadDoc(id),
          ]);
          if (h.doc !== id) return;
          h.versions = (list.versions || []).map((v) => ({ version: Number(v.version), hash: v.hash || "", source: v.source || "", saved: at(v.savedAt) }));
          h.loaded = true;
        } catch (e) { this.fail(e, true); }
      },
      inUse(id, v) { const d = this.docs[id]; return !!d.runningHash && v.hash === d.runningHash; },
      async viewVersion(v) {
        const id = this.hist.doc, d = this.docs[id];
        this.hist.problems = [];
        try {
          const r = await API.call("ConfigService", "GetDocumentVersion", { document: DOCS[id].enum, version: v.version });
          const text = r.text || "";
          // 從現在這一版變成那一版會改哪些行：刪的是現在有的，加的是它有的。
          this.hist.sel = { version: v.version, text, same: text === d.saved, lines: lineDiff(d.saved, text) };
        } catch (e) { this.fail(e, true); }
      },
      async restore() {
        const h = this.hist, id = h.doc, s = h.sel;
        if (!s || s.same || this.dirty(id) || h.busy) return;
        h.busy = true; h.problems = [];
        try {
          const r = await this.put(id, s.text);
          if (r.problems.length) { h.problems = r.problems; return; }
          this.say("已把第 " + s.version + " 版存成第 " + this.docs[id].version + " 版" + this.afterSave(id));
          await this.openHistory(id);
        } catch (e) {
          if (e.code === "failed_precondition") {
            await this.openHistory(id);
            this.say("這份文件剛被改過，已重新載入。看過新的差別，再決定要不要還原");
            return;
          }
          this.fail(e);
        } finally { h.busy = false; }
      },

      // ═══════════ 金鑰 ═══════════
      // 金鑰不屬於設定的「修改」：它存成一個檔案，打的當下就寫，不進 edits、檢查、差異對話框，
      // 也不放進任何狀態。打進去的字只在那個密碼輸入框裡，送出時讀一次，不論成敗，用完就清掉。
      // 回來的狀態沒有金鑰、長度或任何由它算出的東西；modifiedAt 是原字串，原樣送回去比對。
      async loadCreds() {
        try {
          const r = await API.call("CredentialService", "ListCredentials", {});
          this.creds = r.credentials || [];
        } catch (e) {
          if (e.code !== "failed_precondition") throw e;
          this.creds = []; // 儲存的設定讀不進來，就不知道檔名；設定那邊已經說了
        }
      },
      setCred(c) { const i = this.creds.findIndex((x) => x.slot === c.slot); if (i < 0) this.creds.push(c); else this.creds.splice(i, 1, c); },
      credFor(key) { const c = this.creds.find((x) => x.fileSetting === key); return c ? [c] : []; },
      // 這把金鑰現在要你處理：檔名不能用、被擋住、或設定要它卻什麼都沒有。
      credProblem(c) {
        if (c.fileState === "CREDENTIAL_FILE_STATE_INVALID_NAME") return true;
        if (c.need === "CREDENTIAL_NEED_UNUSED") return false;
        return c.source === "CREDENTIAL_SOURCE_BLOCKED" || (c.need === "CREDENTIAL_NEED_REQUIRED" && c.source === "CREDENTIAL_SOURCE_NONE");
      },
      // 沒有 agent 的畫面只列可能害 daemon 起不來的（被擋住、daemon 要讀卻沒有），和改過還在等重啟的。
      // 檔名不能用的，設定的問題卡片已經在說；gateway 要讀的金鑰沒有，不影響 daemon 起不起得來，在設定裡看就好。
      get credsAttention() {
        return this.creds.filter((c) => c.restartNeeded || (this.credProblem(c) && c.fileState !== "CREDENTIAL_FILE_STATE_INVALID_NAME" &&
          (c.process === "CREDENTIAL_PROCESS_DAEMON" || c.source === "CREDENTIAL_SOURCE_BLOCKED")));
      },
      get restartCreds() { return this.creds.filter((c) => c.restartNeeded); },
      credLamp(c) {
        if (c.source === "CREDENTIAL_SOURCE_BLOCKED") return "failed";
        if (this.credProblem(c)) return "approval";
        // 有金鑰但目前的設定沒有選到會讀它的功能：沒有程序在用，不亮「進行中」的燈。
        return c.need !== "CREDENTIAL_NEED_UNUSED" && (c.source === "CREDENTIAL_SOURCE_ENVIRONMENT" || c.source === "CREDENTIAL_SOURCE_FILE") ? "run" : "idle";
      },
      credReader: (c) => (c.process === "CREDENTIAL_PROCESS_GATEWAY" ? "gateway" : "daemon"),
      credSource(c) {
        const idle = c.need === "CREDENTIAL_NEED_UNUSED"; // 有金鑰，只是目前的設定沒有選到會讀它的功能
        // 沒有 agent 在跑、或改了還沒重啟的時候，現在沒有程序在用它；說的是下一次啟動會用的。
        const use = this.repair || c.restartNeeded ? "重啟後用的是" : "用的是";
        switch (c.source) {
          case "CREDENTIAL_SOURCE_ENVIRONMENT": return idle ? "環境變數 " + c.envInUse + " 有值，目前用不到" : use + "環境變數 " + c.envInUse;
          case "CREDENTIAL_SOURCE_FILE": return idle ? "金鑰檔 " + c.fileName + " 有金鑰，目前用不到" : use + "金鑰檔 " + c.fileName;
          case "CREDENTIAL_SOURCE_BLOCKED": return "被擋住了：" + (CRED_WHY[c.fileState] || "") + "。要用它的程序不會啟動，不會跳過它。";
        }
        return "沒有金鑰：" + ((c.envNames || []).length ? "環境變數都沒有值，" : "沒有指定環境變數，") + "也沒有可用的金鑰檔";
      },
      credEnv(c) {
        const names = c.envNames || [];
        return names.length ? names.join("、") + "（依序檢查，第一個有值的就用）" : "沒有設定任何名稱";
      },
      credFile(c) {
        if (c.fileState === "CREDENTIAL_FILE_STATE_NO_NAME") return "上面這欄是空的，沒有金鑰檔，只能用環境變數";
        if (c.fileState === "CREDENTIAL_FILE_STATE_INVALID_NAME") return "上面這欄的檔名不能用，要先改";
        // 權限的說明是 daemon 的原話 "is mode 0644"；認得這個形式就說成中文，不認得就照原話。
        const mode = /^is mode (\d{3,4})$/.exec(c.fileDetail || "");
        const detail = c.fileDetail && (c.fileState === "CREDENTIAL_FILE_STATE_WRONG_MODE" || c.fileState === "CREDENTIAL_FILE_STATE_UNREADABLE") ? "（" + (mode ? "權限是 " + mode[1] : c.fileDetail) + "）" : "";
        const ts = c.modifiedAt ? at(c.modifiedAt) : null;
        return c.fileName + "：" + (CRED_FILE[c.fileState] || "") + detail + (ts ? "；更新於 " + stamp(ts) + "（" + rel(ts) + "）" : "");
      },
      credNeed: (c) => (CRED_NEED[c.need] || "") + "；由 " + (c.process === "CREDENTIAL_PROCESS_GATEWAY" ? "gateway" : "daemon") + " 讀",
      // 環境變數排在檔案前面：有值就不讀檔案，存好的金鑰沒被用到，原因在這裡。
      credShadow(c) {
        if (c.source !== "CREDENTIAL_SOURCE_ENVIRONMENT" || !["CREDENTIAL_FILE_STATE_PRESENT", "CREDENTIAL_FILE_STATE_WRONG_MODE"].includes(c.fileState)) return "";
        const lead = c.process === "CREDENTIAL_PROCESS_GATEWAY" ? "daemon 的環境裡 " + c.envInUse + " 有值；gateway 的環境若也有，" : "環境變數 " + c.envInUse + " 有值，";
        return lead + c.fileName + " 裡的金鑰就不會被用到。要改用檔案，把 " + c.envSetting + " 裡的 " + c.envInUse + " 拿掉，或啟動時不要設這個環境變數；兩種都要重啟才生效。";
      },
      credGateway: (c) => c.process === "CREDENTIAL_PROCESS_GATEWAY" && (c.envNames || []).length > 0,
      credHasName: (c) => c.fileState !== "CREDENTIAL_FILE_STATE_NO_NAME" && c.fileState !== "CREDENTIAL_FILE_STATE_INVALID_NAME",
      credCanPut(c) { return this.credHasName(c) && c.fileState !== "CREDENTIAL_FILE_STATE_NOT_REGULAR"; },
      credHasFile: (c) => ["CREDENTIAL_FILE_STATE_PRESENT", "CREDENTIAL_FILE_STATE_WRONG_MODE", "CREDENTIAL_FILE_STATE_UNREADABLE"].includes(c.fileState),
      credCanRemove: (c) => ["CREDENTIAL_FILE_STATE_PRESENT", "CREDENTIAL_FILE_STATE_WRONG_MODE", "CREDENTIAL_FILE_STATE_EMPTY"].includes(c.fileState),
      credEdited(c) { return this.src(c.fileSetting) === "edit"; },
      // 到那一格去：檔名是空的或不能用的時候，先改設定，才有地方放金鑰。
      credGo(c) { this.goProblem({ key: c.fileSetting }); },
      credClose() { this.credForm = ""; this.credMsg = null; },
      credOpen(c) {
        this.credForm = c.slot; this.credExpect = c.modifiedAt || ""; this.credMsg = null;
        this.credAbsent = !c.modifiedAt && c.fileState === "CREDENTIAL_FILE_STATE_ABSENT";
      },
      credAfter: (c) => (c.restartNeeded ? "，重啟後才會用上" : ""),
      // 失敗：憑證失效、連不上的交給 fail；其餘寫在那把金鑰底下，不隨 toast 消失。
      // 比對失敗時重新看一次檔案；modifiedAt 變了才算「在你打開之後被改過」，其餘照 daemon 的說法。
      async credFail(c, e, expect, absent) {
        if (e instanceof API.Unauthorized || e.code === "unavailable") return this.fail(e);
        let text;
        if (e.code === "invalid_argument") {
          text = "這個值不能當金鑰：要是一行文字、不能是空的，最長 8 KiB（前後的空白會先去掉）。沒有存。";
        } else {
          await this.loadCreds().catch(() => {});
          const now = this.creds.find((x) => x.slot === c.slot);
          const stale = e.code === "failed_precondition" && ((expect && ((now && now.modifiedAt) || "") !== expect) || (absent && !!(now && now.modifiedAt)));
          if (stale) this.credForm = ""; // 要先看過它現在的樣子，再決定要不要重打
          // 這個名字上是連結（或別的不是一般檔案的東西）：讀的時候會跟著連結走，寫、改權限、刪都不碰它。
          text = stale
            ? c.fileName + (absent ? " 在你打開輸入框之後出現了" : " 在你打開輸入框之後被改過或刪掉了") + "，這次沒有存，打進去的字也已清掉。狀態已經更新，看過之後再存一次。"
            : /is not a credential file/.test(e.message || "") ? c.fileName + " 不是一般的金鑰檔（可能是連結）。這裡不會寫它、改它的權限或刪它；要換請直接在那台機器上處理。"
            : "沒有成功：" + (e.message || e.code);
        }
        this.credMsg = { slot: c.slot, text };
      },
      async putCred(c, input) {
        if (this.credBusy) return;
        if (!input.value.trim()) { input.value = ""; this.credMsg = { slot: c.slot, text: "先輸入金鑰。" }; return; }
        const expect = this.credExpect, absent = this.credAbsent;
        this.credBusy = true; this.credMsg = null;
        try {
          // 金鑰只在這個物件和輸入框裡；送出之後物件就沒人拿著了。
          const body = { meta: API.meta(), slot: c.slot, value: input.value };
          if (expect) body.expectModifiedAt = expect; else if (absent) body.expectAbsent = true;
          const r = await API.call("CredentialService", "PutCredential", body);
          this.setCred(r.credential); this.credForm = "";
          const n = r.credential;
          this.say("已存進 " + n.fileName + (n.source === "CREDENTIAL_SOURCE_ENVIRONMENT" ? "；環境變數 " + n.envInUse + " 優先，這份目前不會被用到" : this.credAfter(n)));
        } catch (e) { await this.credFail(c, e, expect, absent); } finally { input.value = ""; this.credBusy = false; }
      },
      async restrictCred(c) {
        this.credMsg = null;
        try {
          const r = await API.call("CredentialService", "RestrictCredentialFile", { meta: API.meta(), slot: c.slot });
          this.setCred(r.credential);
          this.say("已把 " + c.fileName + " 改成只有擁有者讀得到" + this.credAfter(r.credential));
        } catch (e) { await this.credFail(c, e); }
      },
      askRemoveCred(c) { this.target = c; this.dialog = "delcred"; },
      async removeCred() {
        const c = this.target; this.dialog = null; this.credMsg = null;
        if (!c) return;
        try {
          const r = await API.call("CredentialService", "DeleteCredential", { meta: API.meta(), slot: c.slot });
          this.setCred(r.credential);
          this.say("已刪除 " + c.fileName + (r.credential.restartNeeded ? "，重啟後才會生效" : ""));
        } catch (e) { await this.credFail(c, e); }
      },

      // ═══════════ 技能 ═══════════
      // 技能是別人寫的指示：它的名字、描述、SKILL.md、檔案清單，和取得失敗時 git 說的話，一律只用 x-text 放進頁面。
      // 取得（StageSkill）不安裝任何東西；啟用（ActivateSkill）只認審查畫面上那一份的 treeDigest。
      get manage() { return this.view === "skills" || this.view === "memory" || this.view === "channels"; },
      // 管理的三頁（技能、記憶、頻道）只在打開、切換和每次動作之後讀；只有技能在看著的時候還會一直問。
      openManage(v) {
        v = v || this.mview; this.view = this.mview = v; this.sid = null; this.drawer = false; this.skillErr = ""; this.memNote = ""; this.bindNote = "";
        const load = v === "memory" ? this.loadMemory() : v === "channels" ? this.loadBindings() : this.loadSkills();
        load.catch((e) => this.fail(e, true));
      },
      async loadSkills() {
        try {
          const r = await API.call("SkillService", "ListSkills", {});
          const at_ = (ts) => (ts ? at(ts) : null);
          this.skills = {
            loaded: true, error: "", restart: !!r.restartRequired,
            installed: (r.installed || []).map((s) => ({
              name: s.name || "", directory: s.directory || "", description: s.description || "", version: s.version || "",
              source: s.source ? srcOf(s.source) : null, hand: !!s.placedByHand, at: at_(s.installedAt),
              drift: s.drift || "SKILL_DRIFT_NONE", inPrompt: !!s.inCatalogue,
            })),
            rejected: (r.rejected || []).map((s) => ({ directory: s.directory || "", reason: s.reason || "" })),
            missing: (r.missing || []).map((s) => ({ name: s.name || "", source: srcOf(s.source), at: at_(s.installedAt) })),
            staged: (r.staged || []).map(mapStaged),
          };
        } catch (e) {
          if (e instanceof API.Unauthorized || e.code === "unavailable") throw e;
          // 例如安裝紀錄讀不了：寫在這一頁上，不隨每 4 秒的輪詢一直跳通知。
          this.skills = Object.assign({}, this.skills, { loaded: true, error: e.message || e.code });
        }
      },
      // 審查和結果都在頁面最上面；人是在底下的按鈕按的，換了內容之後要把他帶回上面。
      boardTop() { this.$nextTick(() => { const b = document.querySelector(".board"); if (b) b.scrollTop = 0; }); },
      srcLine(s) { return s ? vis("git:" + s.repository + "#" + s.commit.slice(0, 12) + (s.path ? ":" + s.path : "")) : ""; },
      driftZh: (d) => DRIFT[d] || "",
      // 失敗：憑證失效、連不上的交給 fail；回來的是這一頁要說的話（憑證失效時沒有話，整頁已經回到連線畫面）。
      skillFail(e) {
        if (e instanceof API.Unauthorized) { this.fail(e); return ""; }
        if (e.code === "unavailable") this.fail(e, true);
        return e.message || e.code || "失敗了";
      },

      // 取得：一次只能有一個，最久 3 分鐘。成功了就直接開審查，失敗就把 git 的話原樣放在 <pre>。
      async stageSkill() {
        const source = this.skillSource.trim();
        if (this.stageBusy || !source) return;
        this.stageBusy = true; this.stageErr = ""; this.skillErr = ""; this.skillNote = ""; this.stageSecs = 0;
        const t0 = Date.now(), timer = setInterval(() => { this.stageSecs = Math.floor((Date.now() - t0) / 1000); }, 1000);
        try {
          const r = await API.call("SkillService", "StageSkill", { meta: API.meta(), source });
          const name = r.staged.name;
          this.skillSource = ""; this.skillAdd = false;
          await this.loadSkills();
          if (this.view === "skills" && !this.rev) await this.review(name);
          else this.say("已取得「" + vis(name) + "」，到技能頁審查");
        } catch (e) { this.stageErr = this.skillFail(e); } finally { clearInterval(timer); this.stageBusy = false; }
      },

      // 審查：GetStagedSkill 一次回來的 SKILL.md、檔案清單、摘要是同一份。要看某個檔案就帶 filePath 再問一次，
      // 摘要若變了，就換成新的整份審查，不把兩份拼在一起。
      async fetchReview(name, file) {
        const r = await API.call("SkillService", "GetStagedSkill", file ? { name, filePath: file } : { name });
        const st = mapStaged(r.staged || {}), md = utf8(r.skillMd), oldMd = utf8(r.installedSkillMd);
        return Object.assign(st, {
          md, lines: Number(r.skillMdLines) || 0, hidden: hidden(md), matches: !!r.treeMatches,
          files: (r.files || []).map((f) => ({ path: f.path || "", size: Number(f.size) || 0, isText: !!f.isText })),
          oldMd, oldCut: !!r.installedSkillMdTruncated,
          // 取代別人的時候，差異只算一次。
          diff: st.replaces && oldMd ? lineDiff(oldMd, md) : [],
          file: r.filePath ? { path: r.filePath, text: r.fileText || "", cut: !!r.fileTruncated, hidden: hidden(r.fileText) } : null,
          mode: "md", busy: false, msg: "",
        });
      },
      async review(name) {
        this.skillErr = ""; this.skillNote = "";
        try { this.rev = await this.fetchReview(name); this.boardTop(); } catch (e) { this.skillErr = this.skillFail(e); await this.loadSkills().catch((x) => this.fail(x, true)); }
      },
      async viewFile(path) {
        const rv = this.rev;
        if (!rv || rv.busy) return;
        rv.busy = true; rv.msg = "";
        try {
          const fresh = await this.fetchReview(rv.name, path);
          if (fresh.treeDigest !== rv.treeDigest || fresh.matches !== rv.matches) {
            fresh.msg = "你在看的時候，暫存的技能被換掉或改動了。下面是現在的內容，請重新看過。";
            this.rev = fresh; this.boardTop();
          } else rv.file = fresh.file;
        } catch (e) { rv.msg = this.skillFail(e); } finally { rv.busy = false; }
      },
      closeReview() { this.rev = null; this.skillErr = ""; this.boardTop(); this.loadSkills().catch((e) => this.fail(e, true)); },

      // 啟用：treeDigest 是畫面上這一份的。同名的東西已經在的時候，要再按一次明確的確認才帶 replace。
      askActivate() {
        const rv = this.rev;
        if (!rv || !rv.matches || rv.busy) return;
        if (rv.replaces) this.dialog = "replaceskill"; else this.activate(false);
      },
      async activate(replace) {
        const rv = this.rev; this.dialog = null;
        if (!rv || !rv.matches || rv.busy) return;
        rv.busy = true; rv.msg = "";
        try {
          const r = await API.call("SkillService", "ActivateSkill", { meta: API.meta(), name: rv.name, treeDigest: rv.treeDigest, replace: replace === true });
          this.rev = null; this.boardTop();
          await this.loadSkills();
          // restartRequired 說的是整個技能目錄和提示差了什麼，別的技能的變動也算在裡面；
          // 這一個技能自己的名字和描述 agent 知不知道，要看列表裡它的 inCatalogue。
          const now = this.skills.installed.find((x) => x.directory === rv.name), live = !!(now && now.inPrompt);
          this.skillNote = "已" + (replace ? "取代並" : "") + "安裝「" + vis(rv.name) + "」。" + (replace ? "舊的那一份已經換掉，沒有備份。" : "") +
            (live ? "提示裡的名字和描述沒變，agent 下次載入它時讀到的就是新的內容。" + (r.restartRequired ? "" : "不必重啟。")
              : "agent 的提示是啟動時建的，重啟後它才會被告知有這個技能。");
          this.say("已安裝「" + rv.name + "」");
        } catch (e) {
          if (e.code === "failed_precondition") await this.restale(rv, e.message);
          else rv.msg = this.skillFail(e);
        } finally { rv.busy = false; }
      },
      // 被拒絕了：多半是暫存的那一份不是你看過的那一份。重新讀一次放上畫面，由你重新看過。
      async restale(old, said) {
        try {
          const fresh = await this.fetchReview(old.name);
          fresh.msg = fresh.treeDigest !== old.treeDigest
            ? "你看過的那一份已經不是現在暫存的這一份（有人重新取得了同名的技能，或暫存的檔案被改過），所以沒有安裝。下面已換成現在的內容，請重新看過。"
            : !old.replaces && fresh.replaces ? "現在已經有同名的技能了，啟用會取代它。看過下面的差異，再決定。" : said;
          this.rev = fresh; this.boardTop();
        } catch (e) {
          if (e.code === "failed_precondition") { this.rev = null; this.boardTop(); this.skillErr = "暫存的「" + vis(old.name) + "」已經不在了，沒有安裝。"; await this.loadSkills().catch((x) => this.fail(x, true)); }
          else this.skillErr = this.skillFail(e);
        }
      },
      async discardStaged(name) {
        this.skillErr = ""; this.skillNote = "";
        try {
          await API.call("SkillService", "DiscardStagedSkill", { meta: API.meta(), name });
          this.skillNote = "已捨棄暫存的「" + vis(name) + "」。它沒有安裝過，agent 也不知道它。"; this.say("已捨棄「" + vis(name) + "」");
        } catch (e) { if (e.code !== "failed_precondition") this.skillErr = this.skillFail(e); }
        if (this.rev && this.rev.name === name) { this.rev = null; this.boardTop(); }
        await this.loadSkills().catch((e) => this.fail(e, true));
      },

      // 移除：刪的是資料夾（RemoveSkill 認的是 directory，不是技能自稱的名字）。
      askRemoveSkill(s) { this.target = s; this.dialog = "delskill"; },
      async removeSkill() {
        const s = this.target; this.dialog = null; this.skillErr = ""; this.skillNote = "";
        if (!s) return;
        try {
          const r = await API.call("SkillService", "RemoveSkill", { meta: API.meta(), name: s.directory });
          await this.loadSkills();
          this.skillNote = "已移除「" + vis(s.name) + "」（資料夾 " + vis(s.directory) + "）。" +
            (s.inPrompt ? "agent 的提示是啟動時建的，重啟後它才會從提示裡消失。" : "agent 啟動時沒有被告知有它，提示不受影響。" + (r.restartRequired ? "" : "不必重啟。"));
          this.say("已移除「" + s.name + "」");
        } catch (e) { this.skillErr = this.skillFail(e); await this.loadSkills().catch((x) => this.fail(x, true)); }
      },

      // ═══════════ 記憶 ═══════════
      // 記憶的字可能是聊天平台上的人說的，一律只用 x-text，並經過 vis()。忘掉一則是刪除，要先確認。
      async loadMemory() {
        try {
          // 多要一則，才知道有沒有被截掉。
          const r = await API.call("MemoryService", "ListMemories", { includeInvalidated: this.memAll, limit: MEM_LIMIT + 1 });
          const all = r.memories || [], items = all.slice(0, MEM_LIMIT).map((m) => ({
            id: m.id, text: m.text || "", standing: m.activation === "standing", scope: m.scope || "", scopeRef: m.scopeRef || "",
            trust: m.trust || "", origin: m.origin || null, sessionId: m.sourceSessionId || "", seq: m.sourceSeq || "",
            approvedBy: m.approvedBy || "", created: at(m.createdAt), invalidated: at(m.invalidatedAt),
            supersededBy: m.supersededBy || "", until: at(m.validUntil),
          }));
          this.mem = { loaded: true, error: "", items, cut: all.length > MEM_LIMIT };
        } catch (e) {
          if (e instanceof API.Unauthorized || e.code === "unavailable") throw e;
          this.mem = Object.assign({}, this.mem, { loaded: true, error: e.message || e.code });
        }
      },
      showMem(all) { this.memAll = all; this.memNote = ""; this.loadMemory().catch((e) => this.fail(e, true)); },
      // 記憶關著的時候，agent 沒有記憶的工具，也不會帶著它們；以前留下的還在，這裡看得到。
      get memoryOn() { return this.runningVal("memory.enabled") !== false; },
      memFrom(m) {
        const o = m.origin;
        if (o && o.kind === "RUN_ORIGIN_KIND_GATEWAY") {
          const p = o.principal || {};
          return (PLATFORM[p.platform] || p.platform || "聊天平台") + " 帳號 " + (p.principalId || "（沒有記錄）") + "，來自這台機器以外";
        }
        if (o && o.kind === "RUN_ORIGIN_KIND_LOCAL_CLIENT") return "這台機器上的 client" + (o.clientId ? "（" + o.clientId + "）" : "");
        return "來源沒有記錄";
      },
      trustZh: (t) => TRUST[t] || "沒有記錄",
      // 為什麼不再相信它：被另一則取代、或說好的期限過了。沒有「太久沒用到就過期」這回事。
      memState(m) {
        if (!m.invalidated) return "";
        return m.supersededBy ? "已被另一則取代" : "已不再成立";
      },
      // 期限存的是不含的終點：整天的期限存成隔天零時，說的時候要說最後一天。
      memUntil(ts) {
        const d = new Date(ts);
        if (!d.getHours() && !d.getMinutes() && !d.getSeconds()) { d.setDate(d.getDate() - 1); return "到 " + ymd(d) + " 為止有效"; }
        return "到 " + stamp(ts) + " 為止有效";
      },
      askForget(m) { this.target = m; this.dialog = "forgetmem"; },
      async forgetMem() {
        const m = this.target; this.dialog = null; this.memNote = "";
        if (!m) return;
        try {
          await API.call("MemoryService", "ForgetMemory", { meta: API.meta(), id: m.id });
          this.say("已忘掉一則記憶");
        } catch (e) {
          if (e instanceof API.Unauthorized || e.code === "unavailable") return this.fail(e);
          this.memNote = e.code === "not_found" ? "這則記憶已經不在了。" : "沒有忘掉：" + (e.message || e.code);
        }
        await this.loadMemory().catch((e) => this.fail(e, true));
      },

      // ═══════════ 通道綁定 ═══════════
      // 綁定存在 daemon 的資料庫，沒有記它從哪裡來。儲存的設定裡 channels / consoles 寫的頻道，每次啟動都會再套用一次，
      // 所以能說的只有一件事：現在儲存的設定有沒有宣告這個頻道。
      async loadBindings() {
        try {
          const r = await API.call("ChannelService", "ListBindings", {});
          this.bind = {
            loaded: true, error: "",
            items: (r.bindings || []).map((b) => ({
              id: b.id, platform: b.platform || "", account: b.accountId || "", tenant: b.tenantId || "", channel: b.channelId || "",
              workspace: b.workspaceId || "", profile: b.permissionProfile || "", users: b.allowedPrincipals || [],
              claims: (b.allowedClaims || []).map((c) => ({ namespace: c.namespace || "", value: c.value || "" })), created: at(b.createdAt),
            })),
          };
        } catch (e) {
          if (e instanceof API.Unauthorized || e.code === "unavailable") throw e;
          this.bind = Object.assign({}, this.bind, { loaded: true, error: e.message || e.code });
        }
      },
      // 誰能觸發，說法和 `jingclaw bindings list` 一致：兩份名單都空的，是平台放進這個頻道的任何人，不是沒有人。
      who(b) {
        const named = b.users.map((u) => "使用者 " + u).concat(b.claims.map((c) => (c.namespace.endsWith(".role") ? "角色 " + c.value : c.namespace + ":" + c.value)));
        return named.length ? named.join("、") : "";
      },
      profileZh: (p) => PROFILE[p] || p,
      platformZh: (p) => PLATFORM[p] || p,
      // 儲存的設定在下一次啟動會寫成哪些綁定：只有 gateway.platform 選到的那個平台，帳號用那個平台的 account_id。
      // 儲存的設定讀不進來時回 null，這時看不出來。
      get declared() {
        if (this.docs.config.parseError) return null;
        const platform = this.base("gateway.platform"), account = this.base("gateway." + platform + ".account_id") || "", out = [];
        [["channels", "gateway"], ["consoles", "console"]].forEach(([list, profile]) => {
          const key = "gateway." + platform + "." + list;
          (this.base(key) || []).forEach((item, i) => (item.channel_ids || []).forEach((channel) => out.push({
            key, n: i + 1, platform, account, tenant: item.tenant_id || "", channel, workspace: item.workspace_id || "", profile,
          })));
        });
        return out;
      },
      declaredFor(b) {
        return (this.declared || []).find((d) => d.platform === b.platform && d.account === b.account && d.tenant === b.tenant && d.channel === b.channel) || null;
      },
      get unbound() {
        if (!this.bind.loaded) return [];
        return (this.declared || []).filter((d) => !this.bind.items.some((b) => b.platform === d.platform && b.account === d.account && b.tenant === d.tenant && b.channel === d.channel));
      },
      openBind() {
        this.bindAdd = !this.bindAdd; this.bindErr = ""; this.bindNote = "";
        if (this.bindAdd) { this.bf = { platform: "discord", account: "", tenant: "", channel: "", workspace: "default", profile: "gateway", users: "", roles: "" }; this.bfPlatform(this.base("gateway.platform") === "telegram" ? "telegram" : "discord"); }
      },
      // 帳號名稱要和閘道設定裡那個平台的 account_id 一樣，閘道才對得到這個綁定。
      bfPlatform(p) { this.bf.platform = p; this.bf.account = this.base("gateway." + p + ".account_id") || ""; this.bf.roles = p === "telegram" ? "" : this.bf.roles; },
      // 只新增，不蓋過現有的：蓋過會把核准的人一起清掉，而核准的人只能寫在設定裡。送出前重讀一次清單再比對。
      async addBinding() {
        const f = this.bf, tg = f.platform === "telegram", channel = f.channel.trim(), workspace = f.workspace.trim();
        if (this.bindBusy || !channel || !workspace) return;
        // 平台給的 id 都是一串數字（Telegram 的群組是負數）。填了名字（像 general）也會被存起來，
        // 只是永遠對不上任何訊息，頁面還說「已加入」，所以在送出前擋下。
        const odd = [channel, tg ? "" : f.tenant.trim()].concat(words(f.users), tg ? [] : words(f.roles)).find((x) => x && !/^-?\d+$/.test(x));
        if (odd) { this.bindErr = "「" + odd + "」不是 id。頻道、伺服器、使用者和角色都要填平台給的 id，一串數字（Telegram 的群組是負數），不是名字。"; return; }
        this.bindBusy = true; this.bindErr = ""; this.bindNote = "";
        try {
          const binding = {
            platform: f.platform, accountId: f.account.trim(), tenantId: tg ? channel : f.tenant.trim(), channelId: channel,
            workspaceId: workspace, permissionProfile: f.profile, allowedPrincipals: words(f.users),
            allowedClaims: tg ? [] : words(f.roles).map((value) => ({ namespace: f.platform + ".role", value })),
          };
          await this.loadBindings();
          if (this.bind.error) { this.bindErr = "讀不到現有的綁定，沒辦法確認這個頻道有沒有綁過，所以沒有加入：" + this.bind.error; return; }
          const dup = this.bind.items.find((b) => b.platform === binding.platform && b.account === binding.accountId && b.tenant === binding.tenantId && b.channel === channel);
          if (dup) {
            const d = this.declaredFor(dup);
            this.bindErr = "這個頻道已經綁定了（工作區 " + vis(dup.workspace) + "，" + this.profileZh(dup.profile) + "），這裡不改現有的綁定。" +
              (d ? "它寫在儲存的設定裡（" + d.key + " 第 " + d.n + " 筆），要改請改設定：這裡改的，下次啟動會被設定蓋回去。" : "要換成別的設定，先移除它，再加一次。");
            return;
          }
          await API.call("ChannelService", "UpsertBinding", { meta: API.meta(), binding });
          this.bindAdd = false;
          this.bindNote = "已綁定頻道 " + vis(channel) + "，立刻生效，不必重啟。";
          this.say("已加入綁定");
          await this.loadBindings();
        } catch (e) {
          if (e instanceof API.Unauthorized || e.code === "unavailable") this.fail(e);
          else this.bindErr = e.message || e.code || "失敗了";
        } finally { this.bindBusy = false; }
      },
      askUnbind(b) { this.target = b; this.dialog = "delbind"; },
      async removeBinding() {
        const b = this.target; this.dialog = null; this.bindNote = "";
        if (!b) return;
        const d = this.declaredFor(b);
        try {
          await API.call("ChannelService", "DeleteBinding", { meta: API.meta(), bindingId: b.id });
          this.bindNote = "已移除頻道 " + vis(b.channel) + " 的綁定，立刻生效，這個頻道傳來的訊息不會再被受理。" +
            (d ? "儲存的設定裡還寫著它（" + d.key + " 第 " + d.n + " 筆），下次啟動會再綁回來；要讓它不再回來，也把設定裡這一筆拿掉。" : "");
          this.say("已移除綁定");
        } catch (e) {
          if (e instanceof API.Unauthorized || e.code === "unavailable") return this.fail(e);
          this.bindNote = "沒有移除：" + (e.message || e.code);
        }
        await this.loadBindings().catch((e) => this.fail(e, true));
      },

      // ═══════════ ChatGPT 登入 ═══════════
      // 登入的是操作者自己的帳號，權杖只在 daemon 的部署目錄裡：這裡的回應沒有權杖、授權碼，
      // 也沒有發給這個部署的 client id。頁面只拿到狀態，和一個要在操作者自己的瀏覽器開的授權網址。
      // 授權網址和貼回來的網址都不放進連結或標記：網址只用 x-text 顯示，只在確定來源是 auth.openai.com 時開啟；
      // 貼回來的網址只在輸入框裡，送出時讀一次，不論成敗都清掉。
      async loadGpt() {
        try {
          const r = await API.call("ChatGPTAuthService", "GetChatGPTAccount", {});
          this.gpt = {
            loaded: true, error: "", signedIn: !!r.signedIn, label: r.label || r.email || "", scopes: r.grantedScopes || [],
            plan: !!r.planUsageGranted, expires: at(r.accessTokenExpiresAt), errCode: r.lastErrorCode || "", errMsg: r.lastErrorMessage || "",
            restart: !!r.restartNeeded,
          };
          // 模型清單要問 ChatGPT，所以只在剛登入、或按再讀一次時問一次，不跟著每四秒的輪詢走。
          if (!(this.gpt.signedIn && this.gpt.plan)) this.gptModels = { loaded: false, busy: false, list: [], error: "" };
          else if (!this.gptModels.loaded && !this.gptModels.busy) await this.loadGptModels();
        } catch (e) {
          if (e instanceof API.Unauthorized || e.code === "unavailable") throw e;
          this.gpt = Object.assign({}, this.gpt, { loaded: true, error: e.message || e.code });
        }
      },
      async loadGptModels() {
        this.gptModels = Object.assign({}, this.gptModels, { busy: true });
        try {
          const r = await API.call("ChatGPTAuthService", "ListChatGPTModels", {});
          this.gptModels = { loaded: true, busy: false, error: "", list: (r.models || []).map((m) => ({ id: m.id, name: m.displayName || m.id })) };
        } catch (e) {
          if (e instanceof API.Unauthorized || e.code === "unavailable") { this.gptModels = Object.assign({}, this.gptModels, { busy: false }); throw e; }
          this.gptModels = { loaded: true, busy: false, list: [], error: e.message || e.code };
        }
      },
      // 目前設定的模型不在清單裡的話，也列出來，不要讓下拉選單悄悄換成別的值。
      get gptModelChoices() {
        const cur = this.val("provider.chatgpt.model"), list = this.gptModels.list;
        return cur && !list.some((m) => m.id === cur) ? [{ id: cur, name: cur + "（不在清單裡）" }].concat(list) : list;
      },
      // 卡片只有一份：設定頁開著、或有 agent 的時候放在設定裡，沒有 agent 又沒開設定頁時放在修復畫面上。
      get gptRepair() { return this.repair && this.agentProblems.some((p) => /^chatgpt:/i.test(p.why)); },
      get gptInSettings() { return this.drawer && this.tab === "section" && this.section === "provider" && !this.q.trim(); },
      placeGpt() {
        const card = this.$refs.gptc, host = document.getElementById(this.drawer || !this.repair ? "gpt-in-settings" : "gpt-in-repair");
        if (card && host && card.parentNode !== host) host.appendChild(card);
      },
      gptExpiry() {
        const t = this.gpt.expires;
        if (!t) return "沒有記錄";
        return stamp(t) + (t > Date.now() ? "（" + until(t) + "）" : "（已過期）");
      },
      // 方案用量到上限：OpenAI 沒有公開查用量或重置時間的介面，所以只說去哪裡看，不猜時間。
      get gptLimited() { return /usage_limit_exceeded/.test(this.gpt.errCode); },
      // 登入網址的來源只認 https://auth.openai.com，其他一律不由頁面代開。
      // 假的授權伺服器（開發與測試用，只認 127.0.0.1 的 http）也不會被開。
      openAuthorizeUrl(url) {
        let u;
        try { u = new URL(url); } catch (_) { return "bad"; }
        if (u.origin !== "https://auth.openai.com") return "foreign";
        // 不用 noopener 特徵字串：它讓 window.open 即使開成了也回傳 null，頁面就永遠說被擋。
        // 改成取得視窗後立刻切斷 opener，授權頁拿不到這個儀表板。
        const w = window.open(u.href, "_blank");
        if (!w) return "blocked";
        w.opener = null;
        return "opened";
      },
      gptPort() { try { return new URL(this.gptFlow.redirect).port; } catch (_) { return ""; } },
      async gptStart() {
        if (this.gptBusy) return;
        this.gptBusy = true; this.gptNote = ""; this.gptOut = null; clearTimeout(this._gp);
        try {
          const r = await API.call("ChatGPTAuthService", "StartChatGPTSignIn", {});
          const opened = this.openAuthorizeUrl(r.authorizationUrl || "");
          this.gptFlow = { id: r.flowId, url: r.authorizationUrl || "", redirect: r.redirectUri || "", state: "pending", error: "", opened, expires: at(r.expiresAt) };
          this.gptWatch(r.flowId);
        } catch (e) {
          if (e instanceof API.Unauthorized || e.code === "unavailable") this.fail(e);
          else this.gptNote = "沒有開始登入：" + (e.message || e.code);
        } finally { this.gptBusy = false; }
      },
      // 每 1.5 秒問一次這次登入有沒有完成（瀏覽器的轉址由 daemon 在本機收），直到成功、失敗或過期。
      gptWatch(id) {
        clearTimeout(this._gp);
        const step = async () => {
          const f = this.gptFlow;
          if (!f || f.id !== id || f.state !== "pending") return;
          if (f.expires && f.expires < Date.now()) { f.state = "failed"; f.url = ""; f.error = "這次登入已經過期，重新開始一次。"; return; }
          try {
            const r = await API.call("ChatGPTAuthService", "GetChatGPTSignIn", { flowId: id });
            if (this.gptFlow !== f) return;
            if (r.state === "CHAT_GPT_SIGN_IN_STATE_SUCCEEDED") return this.gptDone();
            if (r.state === "CHAT_GPT_SIGN_IN_STATE_FAILED") { f.state = "failed"; f.url = ""; f.error = r.error || "登入沒有成功。"; return; }
          } catch (e) {
            if (e instanceof API.Unauthorized) return this.fail(e);
            if (e.code === "not_found") { this.gptFlow = null; this.gptNote = "這次登入已經不在了（被另一次取代，或 daemon 重啟過）。重新開始一次。"; return; }
            if (e.code !== "unavailable") { f.state = "failed"; f.url = ""; f.error = e.message || e.code; return; }
          }
          this._gp = setTimeout(step, 1500);
        };
        this._gp = setTimeout(step, 1000);
      },
      async gptDone() {
        this.gptFlow = null; this.gptNote = "";
        this.gptModels = { loaded: false, busy: false, list: [], error: "" };
        await this.loadGpt().catch((e) => this.fail(e, true));
        this.say("已登入 ChatGPT" + (this.gpt.restart ? "，重啟後才會用上" : ""));
      },
      gptCancel() { clearTimeout(this._gp); this.gptFlow = null; this.gptNote = ""; },
      async gptCopy() {
        try { await navigator.clipboard.writeText(this.gptFlow.url); this.say("已複製登入網址"); } catch (_) { this.say("複製不了，請自己選取上面的網址"); }
      },
      // 貼回來的網址：瀏覽器不在這台機器上時，它轉去 127.0.0.1 會載入失敗，網址列裡那一串就是要貼回來的。
      async gptComplete(box) {
        const f = this.gptFlow;
        if (!f || this.gptBusy) return;
        if (!box.value.trim()) { box.value = ""; this.gptNote = "先貼上瀏覽器網址列的網址。"; return; }
        this.gptBusy = true; this.gptNote = "";
        try {
          await API.call("ChatGPTAuthService", "CompleteChatGPTSignIn", { flowId: f.id, redirectedAddress: box.value });
          await this.gptDone();
        } catch (e) {
          if (e instanceof API.Unauthorized || e.code === "unavailable") this.fail(e);
          else if (e.code === "invalid_argument") this.gptNote = this.gptPasteNote(e.message || "");
          else if (/declined/.test(e.message || "")) { this.gptNote = "你在 ChatGPT 那邊選了不同意，這次登入結束了。要用的話重新開始，並在授權頁按同意。"; this.gptFlow = null; clearTimeout(this._gp); }
          else if (e.code === "not_found") { this.gptFlow = null; this.gptNote = "這次登入已經不在了。重新開始一次。"; }
          else this.gptNote = "登入沒有成功：" + (e.message || e.code);
        } finally { box.value = ""; this.gptBusy = false; }
      },
      // daemon 對貼回的網址只回它自己的幾句固定的話（不含網址內容），這裡依字句分辨原因。
      gptPasteNote(why) {
        const again = "什麼都沒有送出，登入還在等；回到瀏覽器，複製網址列整段再貼一次。";
        if (/does not belong/.test(why)) return "這個網址是別次登入的，對不上這次。" + again;
        if (/no authorization code or no state/.test(why)) return "網址裡缺了 code 或 state，看起來沒複製完整。" + again;
        if (/too long/.test(why)) return "貼上的內容太長，不像轉址網址。" + again;
        return "這不是網址。" + again;
      },
      askGptOut() { this.dialog = "gptout"; },
      async gptSignOut() {
        this.dialog = null; this.gptNote = ""; this.gptOut = null;
        try {
          const r = await API.call("ChatGPTAuthService", "SignOutChatGPT", {});
          this.gptOut = { revoked: !!r.revoked, detail: r.detail || "" };
          this.gptModels = { loaded: false, busy: false, list: [], error: "" };
          await this.loadGpt();
          this.say("已登出 ChatGPT");
        } catch (e) { this.fail(e); }
      },

      top() { this.$nextTick(() => { const el = document.querySelector(".setp .smain"); if (el) el.scrollTop = 0; }); },
      pick(id) { this.tab = "section"; this.section = id; this.menu = false; this.q = ""; this.top(); },
      pickDoc(id) { this.tab = "doc"; this.docId = id; this.menu = false; this.q = ""; this.top(); },
      pickTab(t) { this.tab = t; this.menu = false; this.q = ""; this.drawer = true; this.top(); if (t === "history") this.openHistory(this.hist.doc); },
      get curDoc() { return this.tab === "raw" ? "config" : this.docId; },
      get where() {
        if (this.q.trim()) return { zh: "搜尋結果", code: "" };
        if (this.tab === "doc") return { zh: DOCS[this.docId].zh, code: DOCS[this.docId].name };
        if (this.tab === "raw") return { zh: "設定原文", code: "config" };
        if (this.tab === "history") return { zh: "版本紀錄", code: "" };
        const sec = this.sectionNow;
        return { zh: sec ? sec.zh : "", code: sec ? "[" + sec.table + "]" : "" };
      },
      // 標題底下那一行：這一頁是什麼。
      get headDesc() {
        if (this.q.trim()) return "";
        if (this.tab === "doc" || this.tab === "raw") return DOCS[this.curDoc].lead;
        if (this.tab === "history") return "每份文件最近 20 次儲存的版本。選一個，看它和現在儲存的差在哪；也可以還原成它，還原會存成新的一版。";
        return this.sectionNow ? this.sectionNow.blurb : "";
      },
      get pendingDocs() {
        return ["config", "persona", "agents"].filter((id) => this.docs[id].pending)
          .map((id) => ({ id, zh: DOCS[id].zh, n: id === "config" ? this.restartKeys.length : 0 }));
      },
      get pendingText() {
        const parts = this.pendingDocs.map((d) => d.zh + (d.n ? "（" + d.n + " 項）" : ""));
        if (this.restartCreds.length) parts.push("金鑰（" + this.restartCreds.length + " 把）");
        if (this.skills.restart) parts.push("技能");
        if (this.gpt.restart) parts.push("ChatGPT 登入");
        return parts.join("、");
      },
      get hasPending() { return this.pendingDocs.length > 0 || this.restartCreds.length > 0 || this.skills.restart || this.gpt.restart; },
      // 把 `x` 轉成 <code>。只用在設定架構裡寫死的說明，先把 < > & 轉掉。
      md(t) { return String(t || "").replace(/[&<>]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;" }[c])).replace(/`([^`]+)`/g, "<code>$1</code>"); },
    }));
  });
})();

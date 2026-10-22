/* 和 daemon 說話：配對、Connect JSON 呼叫。
   Connect 的單次呼叫就是 POST 一段 JSON，所以這裡不需要任何套件。 */
(function () {
  const PREFIX = "/jingclaw.control.v1.";
  const STORAGE = "jingclaw.browser";

  // 憑證放在這個分頁的 sessionStorage：關掉分頁就沒了，也不會被別的分頁讀到。
  // 用密碼登入時可以選「記住這個瀏覽器」，那就放在 localStorage，同一個瀏覽器的別的分頁和下次開啟都讀得到。
  // where 記得目前這一把是從哪一邊讀的，失效時只拿掉那一邊。
  const KINDS = { session: () => sessionStorage, local: () => localStorage };
  const ENDED = "jingclaw.ended";
  let where = "session";
  function stored() {
    for (const kind of ["session", "local"]) {
      try {
        const saved = JSON.parse(KINDS[kind]().getItem(STORAGE) || "null");
        if (saved && saved.token && Date.parse(saved.expiresAt) > Date.now()) { where = kind; return saved; }
      } catch (_) { /* 沒有也沒關係：就是還沒登入 */ }
    }
    return null;
  }
  function keep(saved, kind) {
    try {
      if (saved) {
        where = kind || where;
        KINDS[where]().setItem(STORAGE, JSON.stringify(saved));
        KINDS[where === "local" ? "session" : "local"]().removeItem(STORAGE);
      } else KINDS[where]().removeItem(STORAGE);
    } catch (_) { /* 同上 */ }
  }

  class Unauthorized extends Error {}
  class Failure extends Error {
    constructor(code, message) { super(message || code); this.code = code; }
  }

  let session = stored();

  const api = {
    Unauthorized, Failure,

    get paired() { return !!session; },

    // 連結裡的一次性配對碼換成這個瀏覽器自己的憑證。碼在網址的片段，
    // 讀完就從網址上拿掉，不留在歷史紀錄裡。
    async pairFromAddress() {
      const match = /(?:^|[#&])pair=([^&]+)/.exec(location.hash);
      if (!match) return false;

      history.replaceState(null, "", location.pathname + location.search);

      let res;
      try {
        res = await fetch("/ui/pair", { method: "POST", body: JSON.stringify({ code: decodeURIComponent(match[1]) }) });
      } catch (_) {
        throw new Failure("unavailable", "連不上 daemon。確認它還在跑，再執行一次 jingclaw dashboard 取得新的連結。");
      }
      // 只有 401 是「這個連結不能用」。其他（例如 daemon 忙不過來）是別的問題，不要說成連結用過了。
      if (res.status === 401) throw new Unauthorized("pairing refused");
      if (!res.ok) throw new Failure("pair_failed", "daemon 這次沒有讓這個瀏覽器連上（HTTP " + res.status + "）。再執行一次 jingclaw dashboard 取得新的連結；還是不行，看 daemon 的紀錄。");

      const granted = await res.json();
      session = { token: granted.token, expiresAt: granted.expires_at };
      keep(session, "session");
      return true;
    },

    forget() { session = null; keep(null); },

    // daemon 拒絕了這個瀏覽器的憑證之後頁面會重載；重載後的登入畫面用這個知道為什麼回到這裡。
    takeEnded() {
      try { const v = sessionStorage.getItem(ENDED); sessionStorage.removeItem(ENDED); return !!v; } catch (_) { return false; }
    },

    // 登入密碼還是不是初始密碼（initial）。這條路不需要憑證，也不會回傳任何密碼。
    async authState() {
      let res;
      try { res = await fetch("/ui/auth", { cache: "no-store", signal: AbortSignal.timeout(4000) }); } catch (_) { throw new Failure("unavailable", "連不上 daemon"); }
      if (!res.ok) throw new Failure("auth_failed", "HTTP " + res.status);
      return { initial: !!(await res.json().catch(() => ({}))).initial };
    },

    // 改密碼成功時 daemon 回的新憑證（用初始密碼登入的那一把只能改密碼，改完就換這一把）。
    adopt(token, expiresAt, remember) {
      session = { token, expiresAt };
      keep(session, remember ? "local" : "session");
    },

    // 用密碼換這個瀏覽器自己的憑證。密碼只出現在這一次請求裡：不存、不記、不回傳。
    // 失敗分成三種：Failure("wrong")、Failure("throttled")（帶 wait 秒數）、其餘。
    // 回傳憑證的 scope：初始密碼換到的是 "setup"（只能改密碼），只放在記憶體裡，不存、重載就沒了。
    async login(password, remember) {
      let res;
      try {
        res = await fetch("/ui/login", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ password }) });
      } catch (_) {
        throw new Failure("unavailable", "連不上 daemon");
      }
      if (res.status === 401) throw new Failure("wrong", "密碼不對");
      if (res.status === 429) {
        const wait = Number(res.headers.get("Retry-After"));
        const failure = new Failure("throttled", "嘗試太多次");
        failure.wait = Number.isFinite(wait) && wait > 0 ? Math.min(Math.ceil(wait), 3600) : 10;
        throw failure;
      }
      if (!res.ok) throw new Failure("login_failed", "HTTP " + res.status);
      const granted = await res.json();
      session = { token: granted.token, expiresAt: granted.expires_at };
      if (granted.scope === "setup") return "setup";
      keep(session, remember ? "local" : "session");
      return granted.scope || "control";
    },

    // timeout（毫秒）只給輪詢用：等 daemon 回來的時候，不能讓一個不回應的連線卡住。
    async call(service, method, body, timeout) {
      if (!session) throw new Unauthorized("not paired");

      let res;
      try {
        res = await fetch(PREFIX + service + "/" + method, {
          method: "POST",
          headers: { "Content-Type": "application/json", Authorization: "Bearer " + session.token },
          body: JSON.stringify(body || {}),
          signal: timeout ? AbortSignal.timeout(timeout) : undefined,
        });
      } catch (e) {
        throw new Failure("unavailable", "連不上 daemon");
      }

      if (res.status === 401) {
        api.forget();
        try { sessionStorage.setItem(ENDED, "1"); } catch (_) { /* 只是少一句說明 */ }
        throw new Unauthorized("session ended");
      }

      const data = await res.json().catch(() => ({}));
      if (!res.ok) throw new Failure(data.code || "unknown", data.message || res.statusText);
      return data;
    },

    // 會改動東西的請求要帶一個 request id：重試時用同一個，daemon 才能去重。
    meta() {
      return { clientId: "web", requestId: crypto.randomUUID ? crypto.randomUUID() : String(Date.now()) + Math.random() };
    },
  };

  window.API = api;
})();

/* 和 daemon 說話：配對、Connect JSON 呼叫。
   Connect 的單次呼叫就是 POST 一段 JSON，所以這裡不需要任何套件。 */
(function () {
  const PREFIX = "/jingclaw.control.v1.";
  const STORAGE = "jingclaw.browser";

  // 憑證只放在這個分頁的 sessionStorage：關掉分頁就沒了，也不會被別的分頁讀到。
  function stored() {
    try {
      const saved = JSON.parse(sessionStorage.getItem(STORAGE) || "null");
      if (saved && saved.token && Date.parse(saved.expiresAt) > Date.now()) return saved;
    } catch (_) { /* 沒有也沒關係：就是還沒配對 */ }
    return null;
  }
  function keep(saved) {
    try {
      if (saved) sessionStorage.setItem(STORAGE, JSON.stringify(saved));
      else sessionStorage.removeItem(STORAGE);
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
      keep(session);
      return true;
    },

    forget() { session = null; keep(null); },

    async call(service, method, body) {
      if (!session) throw new Unauthorized("not paired");

      let res;
      try {
        res = await fetch(PREFIX + service + "/" + method, {
          method: "POST",
          headers: { "Content-Type": "application/json", Authorization: "Bearer " + session.token },
          body: JSON.stringify(body || {}),
        });
      } catch (e) {
        throw new Failure("unavailable", "連不上 daemon");
      }

      if (res.status === 401) { api.forget(); throw new Unauthorized("session ended"); }

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

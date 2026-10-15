# ChatGPT sign-in, one process, and a card-style settings page

**Status: phases 1 to 3 are implemented; phase 4 (docs) is done, and the independent review and full verification are still to do.** Not verified against OpenAI's live
service (no ChatGPT account here): everything about the sign-in is built from
OpenAI's documentation and tested against fake servers. The riskiest guess is the
shape of function tools in the Responses request, which the documentation does not
show; see docs/STATUS.md, "Sign in with ChatGPT and a settings page of cards" and
"One process". Asked for by the operator: run as one process, a complete
settings UI in the style of ChatGPT's own settings pages, and "Sign in with
ChatGPT" as a model source.

## 1. Sign in with ChatGPT (model source)

Built on OpenAI's documented flow for open-source and locally hosted apps
(developers.openai.com/siwc/token-sharing-open-source). Nothing here imitates
another client's identity.

**What the documentation fixes**

| Thing | Value |
|---|---|
| Authorization | `https://auth.openai.com/api/accounts/authorize`, system browser, PKCE `S256` |
| Token / refresh | `POST https://auth.openai.com/api/accounts/oauth/token`, form-encoded |
| First sign-in | `client_id=dynamic_agent_client`, `agent_name_hint=JingClaw` (registration only), `ext_agent_host_id` required |
| Later sign-ins | the issued `oaiapp_…` client id from registration; never `dynamic_agent_client` |
| Redirect | `http://127.0.0.1:<port>/auth/callback` (127.0.0.1, not localhost; only the port may change; same URI in both steps) |
| Scope | `openid profile email offline_access resource.invoke chatgpt.tokens.use.direct`, `resource=https://api.openai.com/v1` |
| Callback | `code`, `state`, `scope`, and on registration `client_id`; `error=access_denied` possible |
| After exchange | validate ID token (JWKS signature, `iss=https://auth.openai.com`, `aud`=issued client id, `exp`, `nonce`); require `chatgpt.tokens.use.direct` in granted scopes; account identity is `sub` |
| Tokens | access 1 h, refresh 30 d and rotating; serialize refreshes; refresh request: `grant_type=refresh_token`, issued `client_id`, `refresh_token`, `resource`, no `scope` |
| Sign out | `revocation_endpoint` from `https://auth.openai.com/.well-known/openid-configuration`; `token`, `token_type_hint=refresh_token`, `client_id`; empty 200 = done |
| Models | `GET https://api.openai.com/v1/models` → `models[]`, keep `visibility == "list"`, fields `slug`, `display_name` |
| Inference | `POST https://api.openai.com/v1/responses`, bearer token, **`store:false`, `stream:true`**, history resent in `input`, system text in `instructions` (a `system` message item is rejected), omit `temperature`, `top_p`, `max_output_tokens`, `metadata`, `user`, `truncation`, no `previous_response_id`. Function tools allowed. Success only at `response.completed`. Do not use ChatGPT `backend-api` endpoints. |
| Errors | `subscription_sharing_usage_limit_exceeded` (429: pause, point to ChatGPT Settings → Usage, do not guess a reset time), `…_usage_unavailable` (503: backoff), `…_user_not_eligible` (403), `…_unsupported_capability` (400, see `error.param`), `…_route_not_supported`, `…_invalid_user` (401), `chatpass_v2_*` (403); token errors `invalid_grant`, `refresh_token_expired/invalidated/reused` → sign in again. Never fall back to another billing path silently. |
| Restrictions | open-source / locally hosted apps; paid or remotely hosted apps must file OpenAI's interest form; the grant gives no access to conversations. |

**Decisions**
- Provider name `chatgpt` (`provider.backend = "chatgpt"`), settings `[provider.chatgpt] model`, `reasoning_effort`. No API key.
- One signed-in account. The registration (issued client id), the per-host `ext_agent_host_id` (a UUIDv4 `urn:uuid:` generated once) and the tokens live in one owner-only file `chatgpt-auth.json` in the deployment directory, written atomically like the MCP OAuth store. Never in the database, logs, events, responses or the page. The name is reserved in the credential-file name rule.
- A control-scope `ChatGPTAuthService` (normal **and** repair mode: the agent cannot start without a sign-in, so repair mode is where one signs in): `GetChatGPTAccount`, `StartChatGPTSignIn` (starts a loopback listener on `127.0.0.1`, port 1455 preferred then any free port, returns the authorization URL and a flow id), `GetChatGPTSignIn` (poll flow), `CompleteChatGPTSignIn` (paste-back of the redirected address, for when the browser is not on this machine), `SignOutChatGPT`, `ListChatGPTModels`.
- Headless machines (an LXC): the browser that opens the link is elsewhere, so its redirect to `127.0.0.1:<port>` fails; the page therefore always offers paste-back of the address bar contents, and also says an SSH forward of that port completes it automatically.
- Usage and limits are managed by ChatGPT (Settings → Usage). OpenAI documents no usage endpoint; the page links there and shows only what JingClaw itself knows (signed in, scopes, token expiry, last error code). No invented quota numbers.
- Not verified against OpenAI's live service from this machine (no ChatGPT account): tests use fake OAuth and Responses servers built from the documented shapes. Said plainly in the docs.

## 2. One process

`jingclaw` (no arguments) runs the daemon, the chat gateway and the console **in one process**. It no longer starts a supervisor + daemon + gateway + watchdog.
- The gateway still talks to the daemon through the same gateway-scoped credential over loopback, so the scope boundary stays a code boundary (it was never an OS boundary: same user, same discovery file).
- Lost, and said so in the docs and in `gateway/main.go`: crash isolation. A panic in the platform library's own goroutines now takes the whole process down. Mitigation: the gateway runs under a recover wrapper and restarts itself with backoff; a gateway that cannot start for a configuration reason (exit 78 today) just leaves chat off.
- `jingclaw daemon` and `jingclaw gateway` stay as entry points (scripts, tests, people who want them apart). Repair mode, `jingclaw stop`, `status`, `dashboard`, attach-to-running behave as before.
- The `__watch` watchdog process goes away with the children it watched.
- git (skills) and python (web fetch) remain external tools; this is about JingClaw's own parts.

## 3. Settings UI in the style of the reference

Reference: ChatGPT settings: near-black page, a section title with a one-line description and a pill action at its right, grouped large-radius cards whose rows are separated by hairlines, avatar-chip + name on the left and a pill select / toggle on the right, one toggle row with a description.
- The settings area becomes a full page with a section list on the left (provider, ChatGPT sign-in, agent, tools, memory, web, MCP, gateway, server, …) and grouped cards on the right, instead of a 520 px drawer. Booleans are toggles; enums are pill selects; the existing write flow (diff dialog, hash conflict, pending-restart marks, repair mode, history, raw text, credential controls) is kept.
- The write action sits at the section header like the reference's save pill; disabled until there are changes.
- A "Model source" section shows the ChatGPT account card (signed in / out, scopes, plan-usage permission, expiry, sign-in and sign-out, model choice, link to ChatGPT usage settings) next to the API-key providers.
- Visual language stays JingClaw's tokens where they do not conflict; new tokens for radius/toggle are added in one place.

## Phases
1. ChatGPT auth store + OAuth flow + `chatgpt` provider + `ChatGPTAuthService` + config + daemon wiring + tests with fake servers + `scripts/verify-chatgpt.sh`.
2. One process. **Done**: `supervise.Run` composes `daemon.Run` and `gateway.Run` in one process; the watchdog and process-group code are gone; the gateway is retried with bounded backoff unless it is a configuration failure.
3. Settings page restyle and the ChatGPT card. **Done**.
4. Docs **done** (using-it, configuration, README in both languages, architecture, STATUS, roadmap, design notes); independent review and full verification still to do.

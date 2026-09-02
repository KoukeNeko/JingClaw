<h1 align="center">JingClaw</h1>

<p align="center">
  <strong>關掉視窗之後還在工作的 AI agent。</strong><br>
  它跑在你自己的機器上，在 Discord 回話，而每一段對話都活過重啟、崩潰，
  以及你下一次從哪個客戶端接回去。
</p>

<p align="center">
  <a href="https://github.com/KoukeNeko/JingClaw/actions/workflows/ci.yml"><img alt="CI status" src="https://img.shields.io/github/actions/workflow/status/KoukeNeko/JingClaw/ci.yml?branch=main&style=for-the-badge&logo=githubactions&logoColor=white&label=CI"></a>
  <a href="https://github.com/KoukeNeko/JingClaw/pkgs/container/jingclaw"><img alt="Container image" src="https://img.shields.io/badge/GHCR-amd64_%C2%B7_arm64-2496ED?style=for-the-badge&logo=docker&logoColor=white"></a>
  <img alt="Go 1.26" src="https://img.shields.io/badge/GO-1.26+-00ADD8?style=for-the-badge&logo=go&logoColor=white">
  <img alt="Preview" src="https://img.shields.io/badge/STATUS-PREVIEW-E8A33D?style=for-the-badge">
  <a href="LICENSE"><img alt="MIT licence" src="https://img.shields.io/badge/LICENSE-MIT-4CAF50?style=for-the-badge&logo=github"></a>
</p>

<p align="center">
  <a href="README.md">English</a> · <strong>繁體中文</strong>
</p>

<p align="center">
  <a href="#開始使用">開始使用</a>
  · <a href="docs/using-it.md">怎麼用</a>
  · <a href="docs/configuration.md">設定</a>
  · <a href="docs/container.md">Docker</a>
  · <a href="docs/architecture.md">架構</a>
  · <a href="docs/roadmap.md">路線圖</a>
</p>

---

> 說明文件目前只有英文版。這一頁是入口，深入的內容還在 `docs/` 底下。

## 開始使用

```bash
go install github.com/KoukeNeko/JingClaw/core/cmd/jingclaw@latest
jingclaw
```

這樣就是全部了。它會在沒有 `~/.jingclaw` 時建一個，同時啟動 daemon 和聊天
gateway，並給你一個 console 看著它們。在另一個終端機再跑一次，它會說已經有
一個在跑，而不是再開第二個。

第一次執行還沒有模型、也還沒有聊天帳號，所以它會告訴你它要什麼、該放哪裡。
把那些寫進 `~/.jingclaw/config.toml`，再啟動一次，就可以在 Discord 跟它說話。

```bash
jingclaw status      # 有沒有在跑，跑在哪
jingclaw stop        # 停掉正在跑的那個
```

想用容器？看 [`docs/container.md`](docs/container.md)——image 放在 GHCR，
amd64 和 arm64 都有，而且裡面沒有設定、沒有資料庫、也沒有任何憑證。如果平台
只給你一個掛載的 volume 和一串環境變數，`JINGCLAW_CONFIG`、`JINGCLAW_PERSONA`
和 `JINGCLAW_AGENTS` 各帶一整份檔案進去。[`compose.yaml`](compose.yaml)
則是連 Ollama 一起跑起來的整套。

## 它做什麼

**工作活得比客戶端久。** session、run 和每一個事件都在發生的當下寫進 SQLite。
關掉終端機、重啟 daemon、明天換一台機器回來：一個在等核准的 run 還在等，
幾小時後回答它照樣有效。

**由人決定重要的事。** 讀取類的操作不打擾你。任何會改動 workspace、執行指令
或連上網路的動作都會停下來問——在頻道裡問，而且把它即將執行的指令原樣寫出來。
這個暫停也是持久的。

**對話發生在聊天室。** Discord 是你跟它說話的地方；終端機是你看著它、回答它的
地方。它會在做事的同時說出自己在做什麼——開了哪個檔案、讀了哪一頁——而答案是
一邊寫一邊送出來的。

**一個執行檔，兩個行程。** gateway 拿著 bot token，並且對網際網路開著一個
socket。而擁有你的 shell、你的 workspace 和事件記錄的那個行程，不會跟著它一起
倒下。

```
Discord ──→ gateway ─┐
                     ├──→ daemon ──→ SQLite
CLI, console ────────┘
```

## 專案狀態

**Preview。** 上面描述的事它每天都在做，在 macOS 和 Linux 上，而且它的測試在
Windows 上也通過了。

這還不承諾的部分：設定與儲存格式在版本之間仍可能變動；daemon 只聽 loopback，
所以沒有遠端存取。Windows 比其他部分新，而且有兩件事在那裡是**還沒測到**而不是
能用的——race detector 和真正的終端機——[`docs/STATUS.md`](docs/STATUS.md)
把它們寫出來，而不是往上圓。

`docs/STATUS.md` 是誠實的帳：什麼能用、什麼缺、以及一路上找到的缺陷。
[`docs/roadmap.md`](docs/roadmap.md) 是里程碑。

## 說明文件

| | |
|---|---|
| [怎麼用](docs/using-it.md) | 核准、記憶、圖片、工具伺服器、長對話 |
| [設定](docs/configuration.md) | 每一項設定，以及它為什麼是這樣 |
| [在聊天頻道裡](docs/discord.md) | 一段對話從 Discord 看起來是什麼樣子 |
| [Docker](docs/container.md) | image、volume，以及憑證放哪裡 |
| [架構](docs/architecture.md) | 它的形狀，以及建造它時遵守的規則 |
| [開發](docs/development.md) | 怎麼建置，以及一個改動要通過哪些檢查 |
| [狀態](docs/STATUS.md) | 做完了什麼、還沒做什麼、以及出過什麼錯 |

## 需求

建置需要 Go 1.26+。一個 provider——Gemini、Ollama、Anthropic，或任何說
OpenAI 那套格式的服務——想要聊天那一半的話還需要一個 Discord bot token。
macOS、Linux 和 Windows，但要注意上面狀態那節寫的但書。

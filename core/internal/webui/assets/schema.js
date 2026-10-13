// 設定架構：網頁怎麼顯示每一個設定。內容是 JSON；鍵、型別與預設值由 webui_test.go 對照 config.Settings() 驗證。
window.SCHEMA = {
 "groups": [
  "模型與回合",
  "能力與邊界",
  "記憶",
  "連線"
 ],
 "sections": [
  {
   "id": "agent",
   "zh": "代理",
   "table": "agent",
   "group": "模型與回合",
   "blurb": "決定單次執行最多跑幾輪，以及本機的對話不必問你就能做到哪裡。",
   "blocks": [
    {
     "table": "agent",
     "zh": "",
     "fields": [
      {
       "key": "agent.max_iterations",
       "zh": "工具回合上限",
       "type": "int",
       "def": 12,
       "min": 1,
       "unit": "count",
       "adv": false,
       "help": "一次執行最多讓模型呼叫幾輪工具，超過就停下來；至少要 1，否則模型連一輪都跑不了。"
      },
      {
       "key": "agent.permission_profile",
       "zh": "本機權限",
       "type": "enum",
       "def": "local",
       "adv": false,
       "options": [
        {
         "v": "local",
         "zh": "本機",
         "note": "做變更與執行程式前會停下來問你"
        },
        {
         "v": "gateway",
         "zh": "閘道",
         "note": "更嚴格：連本機的執行也一律拒絕執行程式"
        }
       ],
       "help": "決定本機的對話在不問你的情況下能做什麼；聊天頻道不受這個值影響，頻道有各自固定的權限。設成 `gateway` 後這台機器上也不能執行程式，共用的機器可以考慮。"
      }
     ]
    }
   ]
  },
  {
   "id": "provider",
   "zh": "模型提供者",
   "table": "provider",
   "group": "模型與回合",
   "blurb": "選用哪一家模型服務、用哪個模型、金鑰從哪來，以及請求失敗時怎麼重試。",
   "blocks": [
    {
     "table": "provider",
     "zh": "",
     "fields": [
      {
       "key": "provider.backend",
       "zh": "模型後端",
       "type": "enum",
       "def": "fake",
       "adv": false,
       "options": [
        {
         "v": "anthropic",
         "zh": "Anthropic",
         "note": "Anthropic 的 Messages API，需要金鑰"
        },
        {
         "v": "gemini",
         "zh": "Gemini",
         "note": "Google 的 Gemini API，需要金鑰"
        },
        {
         "v": "ollama",
         "zh": "Ollama",
         "note": "這台機器上的 Ollama，或 Ollama 雲端服務"
        },
        {
         "v": "openai_compat",
         "zh": "OpenAI 相容",
         "note": "說 OpenAI 聊天協定的伺服器，如 vLLM、LM Studio、llama.cpp、OpenRouter、Groq、Together"
        },
        {
         "v": "fake",
         "zh": "離線替身",
         "note": "不連網、不需金鑰，只用來檢查流程"
        }
       ],
       "help": "選用哪一家模型服務。預設的 `fake` 是離線替身，不連網也不做真正的事，所以剛裝好時要先改這裡；各後端的模型與金鑰分開記著，換後端只需要改這一行。"
      }
     ]
    },
    {
     "table": "provider.retry",
     "zh": "重試",
     "fields": [
      {
       "key": "provider.retry.max_attempts",
       "zh": "最多嘗試次數",
       "type": "int",
       "def": 4,
       "min": 1,
       "unit": "count",
       "adv": false,
       "help": "一個請求失敗後最多送幾次（含第一次）；`1` 代表不重試，`0` 等於不送出請求，所以至少要 1。"
      },
      {
       "key": "provider.retry.base_delay",
       "zh": "起始等待",
       "type": "duration",
       "def": "500ms",
       "adv": true,
       "help": "第一次重試前先等多久，之後每次加倍，直到 `provider.retry.max_delay`。不能比 `max_delay` 長。"
      },
      {
       "key": "provider.retry.max_delay",
       "zh": "等待上限",
       "type": "duration",
       "def": "30s",
       "adv": true,
       "help": "程式自己決定的等待最長多久。伺服器明講的等待時間（`Retry-After`）不受這個值限制，照它說的等，不會縮短；太長等不起的由 `budget` 擋下。"
      },
      {
       "key": "provider.retry.jitter",
       "zh": "抖動比例",
       "type": "float",
       "def": 0.3,
       "min": 0,
       "max": 1,
       "unit": "ratio",
       "adv": true,
       "help": "把等待時間隨機散開的比例，`0.3` 代表最多上下浮動 30%，免得許多客戶端在同一刻一起重送。範圍是 0 到 1。"
      },
      {
       "key": "provider.retry.budget",
       "zh": "重試總時限",
       "type": "duration",
       "def": "1m30s",
       "adv": false,
       "help": "同一個請求所有重試加起來最多等多久。伺服器要求的等待比剩下的時間長時，會直接停下並說明何時能再試，不會提早重送；`0` 代表只用次數限制。"
      }
     ]
    },
    {
     "table": "provider.anthropic",
     "zh": "Anthropic",
     "when": {
      "key": "provider.backend",
      "eq": "anthropic"
     },
     "fields": [
      {
       "key": "provider.anthropic.model",
       "zh": "模型",
       "type": "string",
       "def": "",
       "adv": false,
       "help": "模型名稱，例如 `claude-sonnet-4-5`，寫法要與服務列出的一致（`--list-models` 可查）；不一致時啟動會發現，daemon 進入修復模式、不啟動代理。留空只在服務剛好只有一個模型時可行。"
      },
      {
       "key": "provider.anthropic.api_key_env",
       "zh": "金鑰環境變數",
       "type": "strings",
       "def": [],
       "kind": "envnames",
       "adv": false,
       "help": "先依序檢查這些環境變數（填的是名稱，不是金鑰），第一個有值的就是金鑰；都沒有才讀 `api_key_file`。預設是空的，要用環境變數得自己填，例如 `ANTHROPIC_API_KEY`。"
      },
      {
       "key": "provider.anthropic.api_key_file",
       "zh": "金鑰檔名",
       "type": "string",
       "def": "",
       "kind": "secretfile",
       "adv": false,
       "help": "環境變數都沒有值時，才讀部署目錄（預設 `~/.jingclaw`）下這個檔案；檔案權限必須是 600，別人讀得到就拒絕使用。預設是空的，代表不讀檔案。"
      },
      {
       "key": "provider.anthropic.base_url",
       "zh": "服務位址",
       "type": "string",
       "def": "",
       "kind": "url",
       "adv": false,
       "help": "前面有代理或閘道時才填；留空使用 Anthropic 官方位址。"
      }
     ]
    },
    {
     "table": "provider.gemini",
     "zh": "Gemini",
     "when": {
      "key": "provider.backend",
      "eq": "gemini"
     },
     "fields": [
      {
       "key": "provider.gemini.model",
       "zh": "模型",
       "type": "string",
       "def": "",
       "adv": false,
       "help": "模型名稱，例如 `gemma-4-31b-it`，寫法要與服務列出的一致（`--list-models` 可查）；不一致時啟動會發現，daemon 進入修復模式、不啟動代理。留空只在服務剛好只有一個模型時可行。"
      },
      {
       "key": "provider.gemini.api_key_env",
       "zh": "金鑰環境變數",
       "type": "strings",
       "def": [
        "GEMINI_API_KEY",
        "GOOGLE_API_KEY"
       ],
       "kind": "envnames",
       "adv": false,
       "help": "先依序檢查這些環境變數（填的是名稱，不是金鑰），第一個有值的就是金鑰；都沒有才讀 `api_key_file`。"
      },
      {
       "key": "provider.gemini.api_key_file",
       "zh": "金鑰檔名",
       "type": "string",
       "def": "gemini.key",
       "kind": "secretfile",
       "adv": false,
       "help": "環境變數都沒有值時，才讀部署目錄（預設 `~/.jingclaw`）下這個檔案；檔案權限必須是 600，別人讀得到就拒絕使用，金鑰也不會寫進記錄。"
      }
     ]
    },
    {
     "table": "provider.ollama",
     "zh": "Ollama",
     "when": {
      "key": "provider.backend",
      "eq": "ollama"
     },
     "fields": [
      {
       "key": "provider.ollama.model",
       "zh": "模型",
       "type": "string",
       "def": "",
       "adv": false,
       "help": "模型名稱，例如 `qwen3:8b`，寫法要與 Ollama 列出的一致（`--list-models` 可查）；不一致時啟動會發現，daemon 進入修復模式、不啟動代理。留空只在服務剛好只有一個模型時可行。"
      },
      {
       "key": "provider.ollama.api_key_env",
       "zh": "金鑰環境變數",
       "type": "strings",
       "def": [],
       "kind": "envnames",
       "adv": false,
       "help": "只有 Ollama 雲端服務需要金鑰；本機的 Ollama 沒有東西可以驗證，連它代理的雲端模型也不用，留空即可。填的是環境變數名稱，不是金鑰。"
      },
      {
       "key": "provider.ollama.api_key_file",
       "zh": "金鑰檔名",
       "type": "string",
       "def": "",
       "kind": "secretfile",
       "adv": false,
       "help": "環境變數都沒有值時，才讀部署目錄下這個檔案，權限必須是 600；本機的 Ollama 不需要。預設是空的，代表不讀檔案。"
      },
      {
       "key": "provider.ollama.base_url",
       "zh": "服務位址",
       "type": "string",
       "def": "http://localhost:11434",
       "kind": "url",
       "adv": false,
       "help": "Ollama 的位址，預設是這台機器上的服務；改用雲端服務時填 `https://ollama.com`，並設定上面的金鑰。"
      },
      {
       "key": "provider.ollama.keep_alive",
       "zh": "常駐時間",
       "type": "string",
       "def": "",
       "kind": "duration-opt",
       "adv": true,
       "help": "模型在請求結束後留在記憶體多久，寫法如 `30m`、`1h`，`0` 代表用完立刻卸載。留空就是用 Ollama 伺服器自己的預設，機器同時在做別的事時通常最合適。"
      },
      {
       "key": "provider.ollama.num_ctx",
       "zh": "上下文長度",
       "type": "int",
       "def": 0,
       "min": 0,
       "unit": "tokens",
       "adv": false,
       "help": "要求 Ollama 用多大的上下文載入模型。`0` 讓伺服器依可用記憶體決定，忙碌的機器上常常只有模型能力的一小部分，整段對話就照那個小數字規劃；不能是負數。"
      },
      {
       "key": "provider.ollama.think",
       "zh": "思考模式",
       "type": "enum",
       "def": "",
       "adv": true,
       "options": [
        {
         "v": "",
         "zh": "跟隨模型",
         "note": "模型自己回報能思考時才要求，否則不要求"
        },
        {
         "v": "off",
         "zh": "關閉",
         "note": "一律不要求"
        },
        {
         "v": "on",
         "zh": "開啟",
         "note": "一律要求，深度由伺服器決定"
        },
        {
         "v": "low",
         "zh": "淺",
         "note": "一律要求，淺度思考"
        },
        {
         "v": "medium",
         "zh": "中",
         "note": "一律要求，中等深度"
        },
        {
         "v": "high",
         "zh": "深",
         "note": "一律要求，深度思考"
        },
        {
         "v": "max",
         "zh": "最深",
         "note": "一律要求，最大深度"
        }
       ],
       "help": "要不要請模型把推理過程和答案分開回報。空字串代表跟隨模型，因為對不能思考的模型提出要求會出錯，而不是被忽略；推理內容不論怎麼設都不會送進聊天頻道。"
      }
     ]
    },
    {
     "table": "provider.openai_compat",
     "zh": "OpenAI 相容",
     "when": {
      "key": "provider.backend",
      "eq": "openai_compat"
     },
     "fields": [
      {
       "key": "provider.openai_compat.model",
       "zh": "模型",
       "type": "string",
       "def": "",
       "adv": false,
       "help": "模型名稱，寫法要與伺服器的 `/v1/models` 列出的一致（`--list-models` 可查）；不一致時啟動會發現，daemon 進入修復模式、不啟動代理。留空只在伺服器剛好只有一個模型時可行。"
      },
      {
       "key": "provider.openai_compat.api_key_env",
       "zh": "金鑰環境變數",
       "type": "strings",
       "def": [],
       "kind": "envnames",
       "adv": false,
       "help": "伺服器需要金鑰時才填，本機的服務通常沒有東西可驗證，留空即可。填的是環境變數名稱，不是金鑰。"
      },
      {
       "key": "provider.openai_compat.api_key_file",
       "zh": "金鑰檔名",
       "type": "string",
       "def": "",
       "kind": "secretfile",
       "adv": false,
       "help": "環境變數都沒有值時，才讀部署目錄下這個檔案，權限必須是 600。預設是空的，代表不讀檔案。"
      },
      {
       "key": "provider.openai_compat.base_url",
       "zh": "服務位址",
       "type": "string",
       "def": "",
       "kind": "url",
       "adv": false,
       "help": "聊天路徑接在這個位址後面，通常以 `/v1` 結尾，例如 `http://localhost:8000/v1`。選這個後端時必填，因為沒有預設位址可以退回。"
      },
      {
       "key": "provider.openai_compat.profile",
       "zh": "伺服器類型",
       "type": "enum",
       "def": "generic",
       "adv": true,
       "options": [
        {
         "v": "generic",
         "zh": "一般",
         "note": "照標準協定，沒有特別處理"
        },
        {
         "v": "groq",
         "zh": "Groq",
         "note": "Groq"
        },
        {
         "v": "llamacpp",
         "zh": "llama.cpp",
         "note": "把 503 分成「模型還在載入」與「沒有空閒槽位」兩種"
        },
        {
         "v": "lmstudio",
         "zh": "LM Studio",
         "note": "LM Studio"
        },
        {
         "v": "openrouter",
         "zh": "OpenRouter",
         "note": "依它自己標的錯誤類型判斷額度、速率與內容過長"
        },
        {
         "v": "together",
         "zh": "Together",
         "note": "把它用 403 回報的「內容超過上下文長度」當成上下文過長處理"
        },
        {
         "v": "vllm",
         "zh": "vLLM",
         "note": "vLLM"
        }
       ],
       "help": "標示這台伺服器和它宣稱的協定有哪裡不同，主要影響失敗時要不要重試、怎麼回報。用名字指定而不是從位址猜，因為代理或轉發會讓位址看不出後面是誰；名字拼錯會被拒絕，不會悄悄退回 `generic`。"
      },
      {
       "key": "provider.openai_compat.name",
       "zh": "端點名稱",
       "type": "string",
       "def": "",
       "adv": true,
       "help": "這個端點在記錄裡的名字，同時接兩個以上端點時用來分辨是哪一個出問題；留空就用上面類型的名稱。"
      }
     ]
    }
   ]
  },
  {
   "id": "context",
   "zh": "上下文",
   "table": "context",
   "group": "模型與回合",
   "blurb": "對話太長時，什麼時候、怎麼把較早的部分摺成摘要。",
   "blocks": [
    {
     "table": "context",
     "zh": "",
     "fields": [
      {
       "key": "context.window",
       "zh": "上下文長度",
       "type": "int",
       "def": 0,
       "min": 0,
       "unit": "tokens",
       "adv": false,
       "help": "覆寫模型的上下文長度。`0` 代表向模型供應端查，供應端知道時這樣最準；只有本機模型的伺服器沒回報長度時才需要自己填，兩邊都不知道就不會壓縮對話，因為拿猜的長度來摘要，不是太早丟掉工作，就是來不及救。"
      },
      {
       "key": "context.compact_at",
       "zh": "摘要觸發比例",
       "type": "float",
       "def": 0.7,
       "min": 0,
       "max": 1,
       "minExcl": true,
       "maxExcl": true,
       "unit": "ratio",
       "adv": true,
       "help": "對話長度達到上下文長度的這個比例時，把較早的部分摺成摘要。必須小於 1，因為請求大小是估的、不是算的，估計值要容許出錯。"
      },
      {
       "key": "context.keep_fraction",
       "zh": "保留原文比例",
       "type": "float",
       "def": 0.3,
       "min": 0,
       "max": 1,
       "minExcl": true,
       "maxExcl": true,
       "unit": "ratio",
       "adv": true,
       "help": "摺成摘要之後，保留原文的尾段最多佔上下文長度的多少比例。必須小於 `context.compact_at`，否則摺完不會比觸發時更短。"
      },
      {
       "key": "context.summary_tokens",
       "zh": "摘要長度上限",
       "type": "int",
       "def": 1024,
       "min": 1,
       "unit": "tokens",
       "adv": true,
       "help": "一份摘要最多可以有幾個 token；沒有上限的摘要不算變短。必須大於 0。"
      },
      {
       "key": "context.keep_after_fold",
       "zh": "摺疊後保留事件",
       "type": "int",
       "def": 200,
       "unit": "count",
       "adv": true,
       "help": "摺成摘要後，被摘要取代的舊事件會從記錄刪掉，只保留最後這麼多筆，讓事後還查得到那一段發生了什麼；`0` 全部刪除，負數代表一筆都不刪（要完整稽核記錄時用，但記錄會一直變大）。"
      }
     ]
    }
   ]
  },
  {
   "id": "tools",
   "zh": "內建工具",
   "table": "tools",
   "group": "能力與邊界",
   "blurb": "內建的讀檔、寫檔、搜尋與執行指令工具，單次能讀、寫、搜尋、回傳多少。",
   "blocks": [
    {
     "table": "tools",
     "zh": "",
     "fields": [
      {
       "key": "tools.read_limit",
       "zh": "單次讀檔上限",
       "type": "int",
       "def": 65536,
       "min": 1,
       "unit": "bytes",
       "adv": true,
       "help": "單次讀檔最多回給模型多少位元組，沒讀完的部分要分段再讀（`read_artifact` 也用這個值）。一次呼叫就能塞滿上下文的工具，一次呼叫就能讓整段對話跑不下去，所以才有上限；必須大於 0。"
      },
      {
       "key": "tools.max_readable_file",
       "zh": "可讀檔案大小",
       "type": "int",
       "def": 8388608,
       "min": 1,
       "unit": "bytes",
       "adv": true,
       "help": "超過這個大小的檔案直接拒絕讀取，要查內容就改用搜尋。必須大於 0，否則每個檔案都太大。"
      },
      {
       "key": "tools.max_overwrite_bytes",
       "zh": "整檔覆寫上限",
       "type": "int",
       "def": 131072,
       "min": 1,
       "unit": "bytes",
       "adv": true,
       "help": "整個檔案重寫時的大小上限。模型要重寫大檔，幾乎都是選錯了修改方式，局部編輯才對；必須大於 0。"
      },
      {
       "key": "tools.max_searchable_file",
       "zh": "可搜尋檔案大小",
       "type": "int",
       "def": 2097152,
       "min": 1,
       "unit": "bytes",
       "adv": true,
       "help": "搜尋時跳過大於這個大小的檔案；這種檔案多半是產生出來或壓縮過的，很少是要找的答案。必須大於 0。"
      },
      {
       "key": "tools.glob_results",
       "zh": "檔名比對筆數",
       "type": "int",
       "def": 200,
       "min": 1,
       "unit": "count",
       "adv": true,
       "help": "模型沒指定筆數時，`glob` 一次最多回傳幾筆。必須大於 0。"
      },
      {
       "key": "tools.grep_results",
       "zh": "內容搜尋筆數",
       "type": "int",
       "def": 100,
       "min": 1,
       "unit": "count",
       "adv": true,
       "help": "模型沒指定筆數時，`grep` 一次最多回傳幾筆。必須大於 0。"
      },
      {
       "key": "tools.command_timeout",
       "zh": "指令預設逾時",
       "type": "duration",
       "def": "2m",
       "adv": false,
       "help": "指令沒特別指定時最多跑多久，超過就被終止；常跑很久的建置或測試可以調長。不能比 `tools.max_command_timeout` 長。"
      },
      {
       "key": "tools.max_command_timeout",
       "zh": "指令逾時上限",
       "type": "duration",
       "def": "10m",
       "adv": true,
       "help": "模型自己要求更長的逾時時，最多只給到這個值（超過的會被壓回這個值，不會被拒絕）。"
      },
      {
       "key": "tools.max_command_output",
       "zh": "指令輸出上限",
       "type": "int",
       "def": 32768,
       "min": 1,
       "unit": "bytes",
       "adv": true,
       "help": "指令輸出超過這個大小時，只把頭尾給模型，完整內容存成產出物並附上編號，需要時用 `read_artifact` 讀。必須大於 0，否則程式印的東西全被丟掉。"
      }
     ]
    }
   ]
  },
  {
   "id": "sandbox",
   "zh": "沙盒",
   "table": "sandbox",
   "group": "能力與邊界",
   "blurb": "把核准後執行的指令關進沙盒，限制它能寫哪裡、讀哪裡、能不能連網。",
   "blocks": [
    {
     "table": "sandbox",
     "zh": "",
     "fields": [
      {
       "key": "sandbox.enabled",
       "zh": "限制指令範圍",
       "type": "bool",
       "def": false,
       "adv": false,
       "help": "把 `exec_command` 執行的每個指令關進沙盒，只能寫工作區與部署自己的快取目錄；核准管得到「有人想跑這個指令」，管不到建置時帶進來的相依套件各自做什麼。預設關閉，因為它會改變指令能做的事；開了而這台機器擋不住，daemon 不會啟動代理，而是進入修復模式並說明原因，不會不設防地照跑。目前只支援 macOS（`sandbox-exec`）；在 Linux 開啟，daemon 就沒有代理。"
      },
      {
       "key": "sandbox.network",
       "zh": "允許連網",
       "type": "bool",
       "def": false,
       "adv": false,
       "help": "允許沙盒內的指令對外連線；預設不允許，因為代理跑的多半是建置或測試，用不到網路。刻意只做成一個總開關，因為逐個程式放行的清單，一項一項加下去最後就是誰都能連。"
      },
      {
       "key": "sandbox.hidden",
       "zh": "不可讀目錄",
       "type": "strings",
       "def": [],
       "kind": "path",
       "adv": false,
       "help": "沙盒內的指令不能讀的目錄，例如 `~/.ssh`、`~/.aws`，開頭的 `~/` 會展開成你的家目錄。只限制寫入擋不住讀取，沒有網路也只是今天送不出去，指令仍可以把讀到的東西印出來，或留給之後有網路的指令；部署目錄（放著憑證）不論這裡怎麼設都一定隱藏。Linux 的 Landlock 只能授予、無法隱藏目錄，所以目前在 Linux 上開啟沙盒，daemon 不會啟動代理。"
      }
     ]
    }
   ]
  },
  {
   "id": "artifacts",
   "zh": "產出物",
   "table": "artifacts",
   "group": "能力與邊界",
   "blurb": "太大而沒有整段給模型看的輸出，以及送給模型的圖片，各自的大小上限。",
   "blocks": [
    {
     "table": "artifacts",
     "zh": "",
     "fields": [
      {
       "key": "artifacts.max_bytes",
       "zh": "產出物大小上限",
       "type": "int",
       "def": 67108864,
       "min": 1,
       "unit": "bytes",
       "adv": true,
       "help": "太長而沒整段給模型看的輸出會存成產出物，模型可以再用工具讀回來；單一產出物以此為上限，更大的東西該是工作區裡的檔案。必須大於 0，否則什麼都存不下。"
      },
      {
       "key": "artifacts.max_image_bytes",
       "zh": "圖片大小上限",
       "type": "int",
       "def": 8388608,
       "min": 1,
       "unit": "bytes",
       "adv": true,
       "help": "送給模型看的單張圖片大小上限。更大的圖改用文字描述，因為供應端會拒絕過大的請求，整個請求被拒比少看一張圖更糟；必須大於 0。"
      }
     ]
    }
   ]
  },
  {
   "id": "process",
   "zh": "長駐程式",
   "table": "process",
   "group": "能力與邊界",
   "blurb": "代理啟動的長駐程式（例如開發伺服器）的輸出保留多少。",
   "blocks": [
    {
     "table": "process",
     "zh": "",
     "fields": [
      {
       "key": "process.buffer_bytes",
       "zh": "輸出保留量",
       "type": "int",
       "def": 262144,
       "min": 1,
       "unit": "bytes",
       "adv": true,
       "help": "每個長駐程式最多保留多少輸出，超過時先丟最舊的。丟開頭而不丟結尾，因為要看的是它剛剛說了什麼，伺服器記錄的開頭多半只是橫幅；沒有上限的話，放一整夜的開發伺服器會把記憶體吃光。"
      }
     ]
    }
   ]
  },
  {
   "id": "web",
   "zh": "網頁讀取",
   "table": "web",
   "group": "能力與邊界",
   "blurb": "讓代理讀網頁，以及要不要讓它自己搜尋。",
   "blocks": [
    {
     "table": "web",
     "zh": "",
     "fields": [
      {
       "key": "web.enabled",
       "zh": "允許讀網頁",
       "type": "bool",
       "def": false,
       "adv": false,
       "help": "讓代理讀網頁，只讀：不點擊、不輸入、不登入、不送出表單。預設關閉，因為這會讓平常只讀自己工作區的代理，也讀得到別人寫的文字，應該由操作者決定；本機與 `consoles` 直接抓，一般 `channels` 會停下來等核准，因為那裡的網址是別人挑的。"
      },
      {
       "key": "web.backend",
       "zh": "抓取方式",
       "type": "enum",
       "def": "browser",
       "adv": false,
       "options": [
        {
         "v": "browser",
         "zh": "瀏覽器",
         "note": "用真的瀏覽器抓，較慢，但抓得到只回驗證頁的網站；需要 Python 與 cloakbrowser 套件"
        },
        {
         "v": "none",
         "zh": "不抓取",
         "note": "停用抓取，但保留這一區其他設定"
        }
       ],
       "help": "怎麼抓網頁。選 `browser` 時，啟動就會檢查直譯器存在、`cloakbrowser` 套件載得進來，不是等到模型第一次要抓才發現；只在 `web.enabled` 開啟時才檢查。"
      },
      {
       "key": "web.python",
       "zh": "直譯器路徑",
       "type": "string",
       "def": "",
       "kind": "path",
       "adv": true,
       "help": "瀏覽器後端執行的 Python 直譯器路徑；留空就在 `PATH` 找 `python3`。"
      },
      {
       "key": "web.timeout",
       "zh": "抓取逾時",
       "type": "duration",
       "def": "45s",
       "adv": true,
       "help": "抓一個網頁最多等多久；時間到了頁面還沒畫完，就回傳目前有的內容，不是空手而回。必須大於 0。"
      },
      {
       "key": "web.max_characters",
       "zh": "字數上限",
       "type": "int",
       "def": 40000,
       "min": 1,
       "unit": "count",
       "adv": true,
       "help": "一個網頁最多把多少字元交給模型；完整文字不論如何都會存成產出物。必須大於 0。"
      },
      {
       "key": "web.max_links",
       "zh": "連結數上限",
       "type": "int",
       "def": 50,
       "min": 1,
       "unit": "count",
       "adv": true,
       "help": "連結清單最多列幾個；導覽頁動輒上千個，全放進去不值得佔上下文。設 `0` 不會關掉清單，程式會改用預設的 50。"
      }
     ]
    },
    {
     "table": "web.search",
     "zh": "搜尋",
     "fields": [
      {
       "key": "web.search.backend",
       "zh": "搜尋服務",
       "type": "enum",
       "def": "none",
       "adv": false,
       "options": [
        {
         "v": "none",
         "zh": "不搜尋",
         "note": "關閉搜尋，但保留這一區其他設定"
        },
        {
         "v": "brave",
         "zh": "Brave",
         "note": "Brave Search，需要訂閱金鑰"
        }
       ],
       "help": "找網址和讀網址是兩種不同的信任：讀是抓人選好的網頁，搜尋是讓代理自己選，所以另設開關，而且要同時開 `web.enabled`。目前只實作 `brave`，因為每家服務的金鑰、額度、結果格式都不同，沒跑過的轉接層是負債。"
      },
      {
       "key": "web.search.key_env",
       "zh": "金鑰環境變數",
       "type": "strings",
       "def": [
        "BRAVE_SEARCH_TOKEN"
       ],
       "kind": "envnames",
       "adv": false,
       "help": "Brave 搜尋訂閱金鑰的環境變數名稱，依序檢查，第一個有值的就用；填的是名稱，不是金鑰。"
      },
      {
       "key": "web.search.key_file",
       "zh": "金鑰檔名",
       "type": "string",
       "def": "brave-search.token",
       "kind": "secretfile",
       "adv": false,
       "help": "環境變數都沒有值時，才讀部署目錄下這個檔案，權限必須是 600，金鑰不會寫進記錄。"
      },
      {
       "key": "web.search.endpoint",
       "zh": "服務位址",
       "type": "string",
       "def": "",
       "kind": "url",
       "adv": true,
       "help": "API 根位址，放在代理後面時才需要；留空使用 Brave 自己的。"
      },
      {
       "key": "web.search.max_results",
       "zh": "預設筆數",
       "type": "int",
       "def": 8,
       "min": 0,
       "max": 20,
       "unit": "count",
       "adv": true,
       "help": "模型沒指定時，一次搜尋回傳幾筆；單次最多 20 筆，寫 `0` 會用預設的 8。"
      }
     ]
    }
   ]
  },
  {
   "id": "mcp",
   "zh": "MCP 伺服器",
   "table": "mcp",
   "group": "能力與邊界",
   "blurb": "接上外部的工具伺服器（MCP），並決定各伺服器的工具算哪一級權限。",
   "blocks": [
    {
     "table": "mcp",
     "zh": "",
     "fields": [
      {
       "key": "mcp.start_timeout",
       "zh": "啟動逾時",
       "type": "duration",
       "def": "30s",
       "adv": true,
       "help": "連上一個 MCP 伺服器並取得它的工具清單最多等多久；逾時這個伺服器就被略過並寫進記錄，不會讓整個 daemon 起不來。"
      },
      {
       "key": "mcp.call_timeout",
       "zh": "呼叫逾時",
       "type": "duration",
       "def": "2m",
       "adv": true,
       "help": "一次工具呼叫最多等多久，逾時就回報該工具沒有回應。"
      },
      {
       "key": "mcp.max_output",
       "zh": "輸出上限",
       "type": "int",
       "def": 32768,
       "min": 1,
       "unit": "bytes",
       "adv": true,
       "help": "一次工具結果最多回給模型多少位元組，超過時留頭尾、完整內容存成產出物。一次就能塞滿上下文的工具，一次就能讓整段對話結束，所以要設上限；必須大於 0。"
      },
      {
       "key": "mcp.servers",
       "zh": "伺服器清單",
       "type": "list",
       "def": [],
       "adv": false,
       "help": "工具伺服器是別人的程式，這裡決定怎麼接、算哪一級權限。工具在 daemon 啟動時才被找出來，所以新增或修改要重啟；工具會以 `mcp_<名稱>_<工具>` 的名字出現。",
       "item": {
        "title": "name",
        "fields": [
         {
          "key": "name",
          "zh": "名稱",
          "type": "string",
          "def": "",
          "adv": false,
          "help": "會當成這個伺服器所有工具名稱的前綴，所以裝一個伺服器不可能蓋掉內建的 `read_file`。不能空白，也不能和別的伺服器重複。"
         },
         {
          "key": "command",
          "zh": "啟動指令",
          "type": "string",
          "def": "",
          "kind": "path",
          "adv": false,
          "help": "要當成 daemon 的子行程啟動的程式。`command` 與 `url` 二選一，不能兩個都填，也不能都空。"
         },
         {
          "key": "args",
          "zh": "指令參數",
          "type": "strings",
          "def": [],
          "adv": false,
          "help": "傳給 `command` 的參數，一個參數一項。"
         },
         {
          "key": "url",
          "zh": "網址",
          "type": "string",
          "def": "",
          "kind": "url",
          "adv": false,
          "help": "已經在跑的 Streamable HTTP 端點，必須是 `http` 或 `https`。這種伺服器不是由 daemon 啟動或結束的，每次工具呼叫都是一次連到這個位址的網路請求。"
         },
         {
          "key": "headers",
          "zh": "請求標頭",
          "type": "map",
          "def": {},
          "adv": false,
          "help": "隨每個請求送給 `url` 伺服器的標頭，授權用的 `authorization` 就放這裡；它的值是敏感資料，會原樣存在儲存的設定裡，連同舊的版本也讀得到，別讓不該看的人讀到。"
         },
         {
          "key": "env",
          "zh": "環境變數",
          "type": "map",
          "def": {},
          "adv": false,
          "help": "直接給子行程的環境變數與值。子行程除了 `PATH` 這類基本變數，只會拿到這裡與 `pass_env` 指名的東西，因為 daemon 的環境裡有模型供應端的金鑰；值若是金鑰，會原樣存在儲存的設定裡，連同舊的版本也讀得到，改用 `pass_env` 轉送較好。"
         },
         {
          "key": "pass_env",
          "zh": "轉送環境變數",
          "type": "strings",
          "def": [],
          "kind": "envnames",
          "adv": false,
          "help": "從 daemon 自己的環境轉送給子行程的變數名稱；這裡只有名稱，值不會出現在設定裡。"
         },
         {
          "key": "level",
          "zh": "權限等級",
          "type": "enum",
          "def": "",
          "adv": false,
          "options": [
           {
            "v": "",
            "zh": "預設",
            "note": "等同 execute"
           },
           {
            "v": "internal",
            "zh": "內部",
            "note": "只改代理自己的狀態"
           },
           {
            "v": "workspace_read",
            "zh": "讀工作區",
            "note": "讀工作區，沒有副作用"
           },
           {
            "v": "workspace_write",
            "zh": "寫工作區",
            "note": "會修改工作區"
           },
           {
            "v": "remember",
            "zh": "寫入記憶",
            "note": "寫下之後的對話會相信的記憶"
           },
           {
            "v": "execute",
            "zh": "執行",
            "note": "執行程式或連網"
           },
           {
            "v": "high_impact",
            "zh": "高影響",
            "note": "動到工作區以外、刪除、部署或改系統狀態"
           }
          ],
          "help": "這個伺服器的工具在權限引擎眼中算哪一級，由這裡決定、不聽伺服器自己的說法，否則自稱唯讀的伺服器就能繞過本來該停下來的核准。留空是 `execute`，聊天頻道與 console 會直接拒絕這一級，所以預設的伺服器只有本機看得到；只轉換文字的伺服器（格式化、linter）可以降到 `workspace_read` 或 `internal`。"
         },
         {
          "key": "oauth",
          "zh": "OAuth 登入",
          "type": "bool",
          "def": false,
          "adv": false,
          "help": "這個伺服器放在授權伺服器後面，要人用 `jingclaw mcp login <名稱>` 登入一次。只適用 `url` 伺服器，`command` 的憑證用 `env` 或 `pass_env` 給；daemon 不會自己開瀏覽器，因為沒有人在看，登入過期時只會回報需要登入，不會卡在沒人會看的頁面上。"
         },
         {
          "key": "defer",
          "zh": "延後載入",
          "type": "bool",
          "def": false,
          "adv": true,
          "help": "先不把這個伺服器的工具放進提示，等模型用 `tool_search` 與 `tool_load` 點名才載入。工具多的伺服器，每個請求都要付名稱、說明與結構的成本，模型也選得更差；只有一個小伺服器就沒什麼好省，所以預設關閉。權限不受影響，載入的工具照伺服器的等級管。"
         }
        ]
       }
      }
     ]
    }
   ]
  },
  {
   "id": "memory",
   "zh": "記憶",
   "table": "memory",
   "group": "記憶",
   "blurb": "代理跨對話記得什麼，以及怎麼把它找回來。",
   "blocks": [
    {
     "table": "memory",
     "zh": "",
     "fields": [
      {
       "key": "memory.enabled",
       "zh": "啟用記憶",
       "type": "bool",
       "def": true,
       "adv": false,
       "help": "代理跨對話記得東西的總開關，預設開啟。寫一則「要用時才查」的筆記不用核准，因為被查到之前它不改變任何事；會常駐進每一輪的記憶則在任何權限設定下都要等人核准。"
      },
      {
       "key": "memory.max_instruction_bytes",
       "zh": "常駐指示上限",
       "type": "int",
       "def": 2000,
       "min": 1,
       "unit": "bytes",
       "adv": true,
       "help": "每一輪都放在模型面前的「常駐指示」總共最多多少位元組，放得越多，工作能用的上下文越少。記憶開啟時必須大於 0。"
      },
      {
       "key": "memory.expand_queries",
       "zh": "換說法重查",
       "type": "bool",
       "def": true,
       "adv": false,
       "help": "查記憶一筆都沒找到時，請模型換幾個說法再查一次。記憶是按字詞比對，同一件事用不同的話寫就查不到、也不會有任何提示；代價是一次小型模型呼叫，且只發生在已經失敗的查詢上，關掉後記憶查詢完全不會碰到模型供應端。"
      },
      {
       "key": "memory.curate",
       "zh": "自動記筆記",
       "type": "bool",
       "def": true,
       "adv": false,
       "help": "每次回答完一輪，由模型從你自己的訊息（不含代理的回覆與工具輸出）挑出關於你和專案的事實、偏好、決定，記成「要用時才查」的筆記。每輪多一次小型模型呼叫；這些筆記標記為沒有人核准過，不會常駐，也不會被當成指令。"
      },
      {
       "key": "memory.auto_recall",
       "zh": "自動帶入筆數",
       "type": "int",
       "def": 3,
       "min": 0,
       "unit": "count",
       "adv": false,
       "help": "每一輪回答前，依你說的內容挑幾則筆記放到模型面前，只取該發話者讀得到的範圍（專案的與他自己的，不含別人帳號的）。`0` 代表不自動放，只剩 `recall` 工具可以查。"
      },
      {
       "key": "memory.auto_recall_bytes",
       "zh": "帶入位元組上限",
       "type": "int",
       "def": 1024,
       "min": 0,
       "unit": "bytes",
       "adv": true,
       "help": "自動放入的筆記合計最多多少位元組。`memory.auto_recall` 大於 0 時必須大於 0。"
      }
     ]
    },
    {
     "table": "memory.embedding",
     "zh": "以意思搜尋",
     "fields": [
      {
       "key": "memory.embedding.model",
       "zh": "嵌入模型",
       "type": "string",
       "def": "",
       "adv": false,
       "help": "用來按意思比對記憶的嵌入模型，例如 `qwen3-embedding:0.6b`；留空就只按字詞搜尋。填了之後，每則記憶和你輸入的每一輪都會送到 `base_url` 的服務，這同時是在決定你的話送去哪裡；換模型時啟動會重新嵌入所有記憶，因為舊向量和新的不能比。"
      },
      {
       "key": "memory.embedding.base_url",
       "zh": "服務位址",
       "type": "string",
       "def": "",
       "kind": "url",
       "adv": false,
       "help": "`/embeddings` 接在這個位址後面，通常以 `/v1` 結尾，例如本機 Ollama 是 `http://127.0.0.1:11434/v1`。填了 `model` 就必填。"
      },
      {
       "key": "memory.embedding.api_key_env",
       "zh": "金鑰環境變數",
       "type": "strings",
       "def": [],
       "kind": "envnames",
       "adv": false,
       "help": "端點需要金鑰時才填，本機的服務通常不用。填的是環境變數名稱，不是金鑰。"
      },
      {
       "key": "memory.embedding.api_key_file",
       "zh": "金鑰檔名",
       "type": "string",
       "def": "",
       "kind": "secretfile",
       "adv": false,
       "help": "環境變數都沒有值時，才讀部署目錄下這個檔案，權限必須是 600。預設是空的，代表不讀檔案。"
      },
      {
       "key": "memory.embedding.min_similarity",
       "zh": "自動帶入門檻",
       "type": "float",
       "def": 0.56,
       "min": 0,
       "max": 1,
       "minExcl": true,
       "maxExcl": true,
       "unit": "ratio",
       "adv": true,
       "help": "一則記憶要和這一輪有多相近（0 到 1 的餘弦相似度），才會不經詢問、單靠意思就放到模型面前。`0.56` 是用 `qwen3-embedding:0.6b` 量的（不相干的輪次最接近的筆記從未超過 0.553），換模型這條線會不同。"
      },
      {
       "key": "memory.embedding.recall_min_similarity",
       "zh": "搜尋門檻",
       "type": "float",
       "def": 0.45,
       "min": 0,
       "max": 1,
       "minExcl": true,
       "maxExcl": true,
       "unit": "ratio",
       "adv": true,
       "help": "同一條線，用在模型自己用 `recall` 搜尋、以及檢查是否已有相同筆記的時候。設得比較低（`0.45`），因為搜尋結果會由模型讀過再判斷，不像自動放入那樣直接放到輪次前面。"
      }
     ]
    }
   ]
  },
  {
   "id": "gateway",
   "zh": "閘道",
   "table": "gateway",
   "group": "連線",
   "blurb": "接聊天平台（Discord、Telegram），決定哪些頻道、哪些人可以對話與核准。",
   "blocks": [
    {
     "table": "gateway",
     "zh": "",
     "fields": [
      {
       "key": "gateway.platform",
       "zh": "平台",
       "type": "enum",
       "def": "discord",
       "adv": false,
       "options": [
        {
         "v": "discord",
         "zh": "Discord",
         "note": "讀 `gateway.discord` 那一區"
        },
        {
         "v": "telegram",
         "zh": "Telegram",
         "note": "讀 `gateway.telegram` 那一區；專案狀態文件寫明只對假的 Telegram API 測過，尚未連過真的"
        }
       ],
       "help": "閘道行程要接哪個聊天平台，下面對應的那一區才會被讀。頻道、使用者、角色 id 都只屬於一個平台，所以各平台各有自己的清單，不共用一份，免得為 Discord 寫的設定被 Telegram 讀成別的意思。"
      },
      {
       "key": "gateway.working_interval",
       "zh": "狀態更新間隔",
       "type": "duration",
       "def": "2s",
       "adv": true,
       "help": "「現在在做什麼」那一行的最短更新間隔。一秒內讀六個檔案不會送六行，那比人讀得完的多，也比平台肯收的多；必須大於 0。"
      },
      {
       "key": "gateway.stream_interval",
       "zh": "串流更新間隔",
       "type": "duration",
       "def": "1.5s",
       "adv": true,
       "help": "還在寫的回答，兩次更新之間的最短間隔。一個間隔內寫完的回答不會串流，整段直接送出；必須大於 0。"
      }
     ]
    },
    {
     "table": "gateway.discord",
     "zh": "Discord",
     "when": {
      "key": "gateway.platform",
      "eq": "discord"
     },
     "fields": [
      {
       "key": "gateway.discord.account_id",
       "zh": "帳號名稱",
       "type": "string",
       "def": "main",
       "adv": false,
       "help": "這個機器人在 JingClaw 裡的名字，綁定與送出佇列都按它分開，不是 Discord 那邊的帳號名稱；第二個機器人要用不同的 `account_id`。"
      },
      {
       "key": "gateway.discord.token_env",
       "zh": "權杖環境變數",
       "type": "strings",
       "def": [
        "DISCORD_BOT_TOKEN"
       ],
       "kind": "envnames",
       "adv": false,
       "help": "機器人權杖的環境變數名稱，依序檢查，第一個有值的就用；填的是名稱，不是權杖。"
      },
      {
       "key": "gateway.discord.token_file",
       "zh": "權杖檔名",
       "type": "string",
       "def": "discord.token",
       "kind": "secretfile",
       "adv": false,
       "help": "環境變數都沒有值時，才讀部署目錄下這個檔案，權限必須是 600，權杖不會寫進記錄。"
      },
      {
       "key": "gateway.discord.channels",
       "zh": "對話頻道",
       "type": "list",
       "def": [],
       "adv": false,
       "help": "別人可以在裡面對話的頻道，啟動時套用成綁定。這裡的頻道是 `gateway` 權限：讀取不用問，變更、記憶、網頁會停下來等核准，執行程式一律拒絕。從設定移除一筆不會解除綁定，因為只寫了一半的設定一啟動就悄悄拿掉頻道，而綁定決定誰找得到代理；設定裡有宣告頻道時，啟動記錄會列出設定裡沒有的綁定。",
       "item": {
        "title": "channel_ids",
        "fields": [
         {
          "key": "channel_ids",
          "zh": "頻道 id",
          "type": "strings",
          "def": [],
          "adv": false,
          "help": "這組規則適用的頻道，填 Discord 的頻道 id。工作區與成員相同的頻道通常只差在 id，放同一組，才不會各複製一份、久了彼此不一致；不能空，一個頻道也只能出現在一個清單（`channels` 與 `consoles` 都算）。"
         },
         {
          "key": "tenant_id",
          "zh": "伺服器 id",
          "type": "string",
          "def": "",
          "adv": false,
          "help": "頻道所在的 Discord 伺服器（guild）id。"
         },
         {
          "key": "workspace_id",
          "zh": "工作區 id",
          "type": "string",
          "def": "",
          "adv": false,
          "help": "從這些頻道發起的執行，使用 JingClaw 內哪個工作區。"
         },
         {
          "key": "users",
          "zh": "使用者",
          "type": "strings",
          "def": [],
          "adv": false,
          "help": "可以對代理提出要求的使用者 id，和 `roles` 符合任一個即可。兩個都留空代表不限，誰進得了這個頻道誰就能問，因為頻道本來就有成員名單，再維護第二份很容易出現「被加進頻道卻被默默忽略」；機器人帳號一律不受理，留空時也仍是 `gateway` 權限。"
         },
         {
          "key": "roles",
          "zh": "角色",
          "type": "strings",
          "def": [],
          "adv": false,
          "help": "可以對代理提出要求的 Discord 角色 id，和 `users` 符合任一個即可；兩個都留空代表不限。"
         },
         {
          "key": "approvers",
          "zh": "核准者",
          "type": "strings",
          "def": [],
          "adv": false,
          "help": "可以按核准按鈕的使用者 id。和 `users` 刻意分開，因為能請代理做事和能放行它做的事是兩種權力；這裡留空就是沒有人，訊息會改成叫你去別處決定。在共用頻道裡打字永遠不算，因為訊息只說明是哪個帳號發的，按鈕才由平台告知是誰按的。"
         },
         {
          "key": "approver_roles",
          "zh": "核准角色",
          "type": "strings",
          "def": [],
          "adv": false,
          "help": "擁有這些 Discord 角色的人可以按核准按鈕，規則同 `approvers`；兩個都留空就是沒有人能在這裡核准。"
         }
        ]
       }
      },
      {
       "key": "gateway.discord.consoles",
       "zh": "私人控制頻道",
       "type": "list",
       "def": [],
       "adv": false,
       "help": "你自己控制的私人頻道，規則同 `channels`，另外：抓網頁不用核准、核准改用回覆 `approve <id>` 或 `deny <id>`，並且會鏡像每一次執行的完整過程（含其他頻道的執行；那些頻道本身看不到這些）。這裡仍然不能執行程式。",
       "item": {
        "title": "channel_ids",
        "fields": [
         {
          "key": "channel_ids",
          "zh": "頻道 id",
          "type": "strings",
          "def": [],
          "adv": false,
          "help": "私人頻道的 Discord 頻道 id。不能空，一個頻道也只能出現在一個清單（`channels` 與 `consoles` 都算）。"
         },
         {
          "key": "tenant_id",
          "zh": "伺服器 id",
          "type": "string",
          "def": "",
          "adv": false,
          "help": "頻道所在的 Discord 伺服器（guild）id。"
         },
         {
          "key": "workspace_id",
          "zh": "工作區 id",
          "type": "string",
          "def": "",
          "adv": false,
          "help": "從這些頻道發起的執行，使用 JingClaw 內哪個工作區。"
         },
         {
          "key": "users",
          "zh": "使用者",
          "type": "strings",
          "def": [],
          "adv": false,
          "help": "可以對代理提出要求的使用者 id，和 `roles` 符合任一個即可；兩個都留空代表頻道成員都行。console 裡能發話的人也能用回覆核准，所以私人頻道建議一定要填。"
         },
         {
          "key": "roles",
          "zh": "角色",
          "type": "strings",
          "def": [],
          "adv": false,
          "help": "可以對代理提出要求的 Discord 角色 id，和 `users` 符合任一個即可；兩個都留空代表頻道成員都行。"
         },
         {
          "key": "approvers",
          "zh": "核准者",
          "type": "strings",
          "def": [],
          "adv": false,
          "help": "console 用回覆 `approve <id>` 核准，能發話的人（`users`／`roles`）就能核准，並不看這個欄位，所以對 `consoles` 沒有作用，留空即可。"
         },
         {
          "key": "approver_roles",
          "zh": "核准角色",
          "type": "strings",
          "def": [],
          "adv": false,
          "help": "和 `approvers` 一樣，對 `consoles` 沒有作用，留空即可。"
         }
        ]
       }
      },
      {
       "key": "gateway.discord.tables_as_images",
       "zh": "表格轉圖片",
       "type": "bool",
       "def": false,
       "adv": false,
       "help": "把表格畫成圖片，而不是用程式碼區塊寫出來。圖片的欄位真的會對齊，因為排版由這裡決定，不是讀者的客戶端字型；代價是圖片不能夾在文字中間，含表格的回答會變成好幾則訊息，而且不能選取、搜尋或朗讀，所以預設關閉。"
      },
      {
       "key": "gateway.discord.max_messages",
       "zh": "單答訊息上限",
       "type": "int",
       "def": 10,
       "min": 1,
       "unit": "count",
       "adv": true,
       "help": "一個回答最多拆成幾則訊息，超過就改成一個檔案送出；拆成很多則的回答，會讓頻道整天都得捲過去。必須大於 0，否則每個回答都變成檔案。"
      },
      {
       "key": "gateway.discord.max_attachment_bytes",
       "zh": "附件大小上限",
       "type": "int",
       "def": 4194304,
       "min": 1,
       "unit": "bytes",
       "adv": true,
       "help": "上傳附件的大小上限，刻意設得比 Discord 允許的低很多：目的是讓人讀得下去，不是越大越好。必須大於 0。"
      }
     ]
    },
    {
     "table": "gateway.telegram",
     "zh": "Telegram",
     "when": {
      "key": "gateway.platform",
      "eq": "telegram"
     },
     "fields": [
      {
       "key": "gateway.telegram.account_id",
       "zh": "帳號名稱",
       "type": "string",
       "def": "",
       "adv": false,
       "help": "這個機器人在 JingClaw 裡的名字，綁定與送出佇列都按它分開，不是 Telegram 那邊的帳號名稱。預設是空的；第二個機器人要用不同的 `account_id`。"
      },
      {
       "key": "gateway.telegram.token_env",
       "zh": "權杖環境變數",
       "type": "strings",
       "def": [
        "TELEGRAM_BOT_TOKEN"
       ],
       "kind": "envnames",
       "adv": false,
       "help": "機器人權杖的環境變數名稱，依序檢查，第一個有值的就用；填的是名稱，不是權杖。"
      },
      {
       "key": "gateway.telegram.token_file",
       "zh": "權杖檔名",
       "type": "string",
       "def": "telegram.token",
       "kind": "secretfile",
       "adv": false,
       "help": "環境變數都沒有值時，才讀部署目錄下這個檔案，權限必須是 600，權杖不會寫進記錄。"
      },
      {
       "key": "gateway.telegram.channels",
       "zh": "對話頻道",
       "type": "list",
       "def": [],
       "adv": false,
       "help": "別人可以在裡面對話的聊天，啟動時套用成綁定。規則與 Discord 的 `channels` 相同：`gateway` 權限，讀取不用問，變更、記憶、網頁會停下來等核准，執行程式一律拒絕；從設定移除一筆不會解除綁定。",
       "item": {
        "title": "channel_ids",
        "fields": [
         {
          "key": "channel_ids",
          "zh": "頻道 id",
          "type": "strings",
          "def": [],
          "adv": false,
          "help": "Telegram 的聊天 id。轉接層把聊天 id 同時當成 `tenant_id` 與頻道 id 去對綁定，所以一筆只放一個聊天，並讓 `tenant_id` 填同一個值；不能空，一個聊天也只能出現在一個清單。"
         },
         {
          "key": "tenant_id",
          "zh": "租戶 id",
          "type": "string",
          "def": "",
          "adv": false,
          "help": "Telegram 沒有伺服器這一層，轉接層用聊天 id 填這一格，所以這裡要寫成和 `channel_ids` 裡那個聊天 id 一樣，否則對不到綁定。"
         },
         {
          "key": "workspace_id",
          "zh": "工作區 id",
          "type": "string",
          "def": "",
          "adv": false,
          "help": "從這個聊天發起的執行，使用 JingClaw 內哪個工作區。"
         },
         {
          "key": "users",
          "zh": "使用者",
          "type": "strings",
          "def": [],
          "adv": false,
          "help": "可以對代理提出要求的 Telegram 使用者 id，和 `roles` 符合任一個即可。兩個都留空代表不限，誰進得了這個聊天誰就能問；機器人帳號一律不受理，留空時也仍是 `gateway` 權限。"
         },
         {
          "key": "roles",
          "zh": "角色",
          "type": "strings",
          "def": [],
          "adv": false,
          "help": "目前 Telegram 轉接層不會回報使用者的角色，所以這個欄位在 Telegram 上不會有作用。"
         },
         {
          "key": "approvers",
          "zh": "核准者",
          "type": "strings",
          "def": [],
          "adv": false,
          "help": "可以按核准按鈕的使用者 id。目前 Telegram 轉接層沒有實作核准按鈕，所以這個欄位暫時不會有作用；留空就是沒有人，訊息會叫你去別處決定。"
         },
         {
          "key": "approver_roles",
          "zh": "核准角色",
          "type": "strings",
          "def": [],
          "adv": false,
          "help": "和 `roles` 一樣，Telegram 轉接層不會回報角色，加上沒有核准按鈕，這個欄位暫時不會有作用。"
         }
        ]
       }
      },
      {
       "key": "gateway.telegram.consoles",
       "zh": "私人控制頻道",
       "type": "list",
       "def": [],
       "adv": false,
       "help": "你自己控制的私人聊天，規則同 `channels`，另外：抓網頁不用核准、核准改用回覆 `approve <id>` 或 `deny <id>`，並且會鏡像每一次執行的完整過程。這裡仍然不能執行程式。",
       "item": {
        "title": "channel_ids",
        "fields": [
         {
          "key": "channel_ids",
          "zh": "頻道 id",
          "type": "strings",
          "def": [],
          "adv": false,
          "help": "私人聊天的 Telegram 聊天 id。轉接層把聊天 id 同時當成 `tenant_id` 與頻道 id 去對綁定，所以一筆只放一個聊天，並讓 `tenant_id` 填同一個值；不能空，一個聊天也只能出現在一個清單。"
         },
         {
          "key": "tenant_id",
          "zh": "租戶 id",
          "type": "string",
          "def": "",
          "adv": false,
          "help": "Telegram 沒有伺服器這一層，轉接層用聊天 id 填這一格，所以這裡要寫成和 `channel_ids` 裡那個聊天 id 一樣，否則對不到綁定。"
         },
         {
          "key": "workspace_id",
          "zh": "工作區 id",
          "type": "string",
          "def": "",
          "adv": false,
          "help": "從這個聊天發起的執行，使用 JingClaw 內哪個工作區。"
         },
         {
          "key": "users",
          "zh": "使用者",
          "type": "strings",
          "def": [],
          "adv": false,
          "help": "可以對代理提出要求的 Telegram 使用者 id；留空代表聊天成員都行。console 裡能發話的人也能用回覆核准，所以私人聊天建議一定要填。"
         },
         {
          "key": "roles",
          "zh": "角色",
          "type": "strings",
          "def": [],
          "adv": false,
          "help": "目前 Telegram 轉接層不會回報使用者的角色，所以這個欄位在 Telegram 上不會有作用。"
         },
         {
          "key": "approvers",
          "zh": "核准者",
          "type": "strings",
          "def": [],
          "adv": false,
          "help": "console 用回覆 `approve <id>` 核准，能發話的人就能核准，並不看這個欄位，所以對 `consoles` 沒有作用，留空即可。"
         },
         {
          "key": "approver_roles",
          "zh": "核准角色",
          "type": "strings",
          "def": [],
          "adv": false,
          "help": "和 `approvers` 一樣，對 `consoles` 沒有作用，留空即可。"
         }
        ]
       }
      },
      {
       "key": "gateway.telegram.max_upload_bytes",
       "zh": "上傳大小上限",
       "type": "int",
       "def": 4194304,
       "min": 1,
       "unit": "bytes",
       "adv": true,
       "help": "上傳檔案的大小上限，刻意設得比 Telegram 允許的低很多。"
      },
      {
       "key": "gateway.telegram.api_base",
       "zh": "API 位址",
       "type": "string",
       "def": "",
       "kind": "url",
       "adv": true,
       "help": "Telegram API 的根位址；留空就是 Telegram 官方，要連到本機代理或測試用的假伺服器時才設。"
      }
     ]
    }
   ]
  },
  {
   "id": "server",
   "zh": "伺服器",
   "table": "server",
   "group": "連線",
   "blurb": "daemon 的控制 API 監聽哪裡、記錄多詳細。資料目錄與執行期目錄決定設定放在哪裡，所以不能存在設定裡：資料目錄在啟動時用 --data-dir 或 JINGCLAW_SERVER_DATA_DIR 給，執行期目錄用 JINGCLAW_SERVER_RUNTIME_DIR。",
   "blocks": [
    {
     "table": "server",
     "zh": "",
     "fields": [
      {
       "key": "server.addr",
       "zh": "監聽位址",
       "type": "string",
       "def": "127.0.0.1:0",
       "kind": "addr",
       "adv": false,
       "help": "daemon 的控制 API 監聽的位址，只能是 loopback（`127.0.0.1`、`::1` 或 `localhost`）。這個 API 能讀檔案、執行程式，綁到別的網路介面等於交給網路上的人，所以非 loopback 一律拒絕（儲存時就會被擋下），而不是相信寫這行的人知道自己在暴露什麼；埠號 `0` 會挑一個空的，並寫進探索檔讓 client 找到。"
      },
      {
       "key": "server.log_level",
       "zh": "記錄等級",
       "type": "enum",
       "def": "info",
       "adv": false,
       "options": [
        {
         "v": "debug",
         "zh": "除錯",
         "note": "最詳細"
        },
        {
         "v": "info",
         "zh": "資訊",
         "note": "預設"
        },
        {
         "v": "warn",
         "zh": "警告",
         "note": "只記警告與錯誤"
        },
        {
         "v": "error",
         "zh": "錯誤",
         "note": "只記錯誤"
        }
       ],
       "help": "daemon 記錄的詳細程度。金鑰與權杖不論哪一級都不會寫進記錄。"
      }
     ]
    }
   ]
  },
  {
   "id": "delivery",
   "zh": "輸出節奏",
   "table": "delivery",
   "group": "連線",
   "blurb": "模型邊寫邊串流的文字與用量，多久整理成一個事件。",
   "blocks": [
    {
     "table": "delivery",
     "zh": "",
     "fields": [
      {
       "key": "delivery.text_flush_bytes",
       "zh": "文字寫入量",
       "type": "int",
       "def": 240,
       "min": 1,
       "unit": "bytes",
       "adv": true,
       "help": "模型串流出來的文字累積到這麼多位元組，就寫成一個事件，免得供應端每吐幾個字就變成一次資料庫寫入。必須大於 0，因為 `0` 等於每個字元寫一次。"
      },
      {
       "key": "delivery.text_flush_interval",
       "zh": "文字寫入間隔",
       "type": "duration",
       "def": "200ms",
       "adv": true,
       "help": "文字沒累積到上面的量時，最久多久也要寫一次，慢慢吐字的模型才不會看起來卡住；兩個條件先到的先寫。"
      },
      {
       "key": "delivery.usage_flush_interval",
       "zh": "用量更新間隔",
       "type": "duration",
       "def": "2s",
       "adv": true,
       "help": "token 用量更新寫成事件的最短間隔。用量是累計值，中間的數字只是進度提示，不需要每次都記。"
      }
     ]
    }
   ]
  }
 ]
};

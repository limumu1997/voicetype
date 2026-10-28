# voicetype

全局语音输入法，面向 **GNOME Wayland**（Ubuntu 24.04 验证）。

按一下热键开始录音，再按一下结束 → 本地 GPU 转写（Fun-ASR-Nano / Qwen3-ASR / faster-whisper）→ 文字自动粘贴到光标处。中英文自动识别，支持热词，按焦点自动选择粘贴方式，带设置页和提示音。

- **一个 Go 二进制，零第三方 Go 依赖**：守护进程、热键客户端、安装命令、设置页都在里面。Python 只用来跑模型（推理库只有 Python 生态），通过 `go:embed` 打包进二进制，首次 `setup` 自解压并用 **uv** 建独立环境。
- **模型常驻显存**：停止录音后 0.2–0.4 秒出字。
- **绕过 Wayland 的两条安全限制**：全局热键交给 GNOME 自己的快捷键系统；键盘注入用纯 Go 直接驱动内核 `/dev/uinput`。CJK 无法逐键模拟，所以走"剪贴板 + 粘贴快捷键"。

---

## 调度全景图

按一次热键，从键盘到文字上屏经过的所有组件。方框外的标注是**跨进程 / 跨内核边界的通道**。

```
 ┌──────────────────────────────────────── 用户态：GNOME 会话 ─────────────────────────────────────────┐
 │                                                                                                      │
 │  键盘 ──evdev──▶ gnome-shell (Mutter + libinput)                                                       │
 │                   │ 命中 Alt+V（gsd 托管给 shell 抓的加速键）                                          │
 │                   │ D-Bus: AcceleratorActivated                                                      │
 │                   ▼                                                                                  │
 │            gsd-media-keys ──fork/exec──▶ voicetype toggle   (瞬时进程，几 ms 即退)                     │
 │                                              │                                                       │
 │                                              │ AF_UNIX 流套接字  $XDG_RUNTIME_DIR/voicetype.sock       │
 │                                              │ 一行文本 "toggle\n"  ⇄  一行回复 "recording|ok|…"        │
 │                                              ▼                                                       │
 │  ┌──────────── systemd --user ── cgroup: app.slice/voicetype.service ─────────────────────────────┐  │
 │  │                                                                                                 │  │
 │  │   voicetype daemon  (Go，常驻；状态机 idle ⇄ recording；goroutine 并发处理每个连接)              │  │
 │  │     │                                                                                           │  │
 │  │     ├─ ① 提示音    fork/exec pw-play ─────────────▶ PipeWire ──▶ 扬声器                          │  │
 │  │     │              (Go 合成的 WAV，放在 $XDG_RUNTIME_DIR)                                        │  │
 │  │     │                                                                                           │  │
 │  │     ├─ ② 录音      fork/exec pw-record ──unix 套接字 pipewire-0──▶ pipewire 守护进程             │  │
 │  │     │              16kHz/mono/s16 → /tmp/voicetype-*.wav            │                            │  │
 │  │     │              停止 = 发 SIGINT，让它写好 WAV 头再退出           ▼                            │  │
 │  │     │                                                     ALSA ─▶ 内核 snd-sof 驱动 ─▶ 内置 DMIC  │  │
 │  │     │                                                                                           │  │
 │  │     ├─ ③ 转写      sidecar (venv python，常驻子进程，prctl PDEATHSIG=SIGTERM)                    │  │
 │  │     │              stdin  ◀── {"wav": "/tmp/…wav"}\n         (行分隔 JSON，匿名管道)             │  │
 │  │     │              stdout ──▶ {"text": "…"}\n                                                   │  │
 │  │     │              模型权重常驻显存 ──ioctl──▶ /dev/nvidia* ──▶ NVIDIA 驱动 ──▶ GPU              │  │
 │  │     │              热词: 每次转写前按 mtime 重读 ~/.config/voicetype/hotwords.txt               │  │
 │  │     │                                                                                           │  │
 │  │     ├─ ④ 焦点      fork/exec python3 -c <focus.py>   (与 ③ 并行，约 40ms)                        │  │
 │  │     │              D-Bus 会话总线 → org.a11y.Bus.GetAddress → AT-SPI 私有总线                    │  │
 │  │     │              → 遍历 ACTIVE 窗口找 FOCUSED 控件 → editable | terminal | none | unknown      │  │
 │  │     │                                                                                           │  │
 │  │     ├─ ⑤ 剪贴板    fork/exec wl-copy ──Wayland 套接字 wayland-0──▶ Mutter 设置 selection          │  │
 │  │     │              wl-copy 再 fork 一个后台子进程持有内容，供应用来取；GPaste 只记录不接管       │  │
 │  │     │                                                                                           │  │
 │  │     ├─ ⑥ 按键      write(/dev/uinput)  ←  启动时 ioctl(UI_SET_EVBIT/KEYBIT, UI_DEV_CREATE)       │  │
 │  │     │              Ctrl[+Shift]+V 共 8 个 input_event（含 EV_SYN）                              │  │
 │  │     │                        │                                                                  │  │
 │  │     │                        ▼  内核 input 子系统 → /dev/input/eventN（虚拟键盘）               │  │
 │  │     │                           → Mutter/libinput 当作真实键盘 → wl_keyboard 发给焦点窗口         │  │
 │  │     │                           → 目标应用向 selection 持有者(wl-copy) 要数据 → 文字上屏          │  │
 │  │     │                                                                                           │  │
 │  │     ├─ ⑦ 通知      fork/exec notify-send -p/-r ──D-Bus──▶ org.freedesktop.Notifications          │  │
 │  │     │              记住通知 ID 原地替换；一停录音就 CloseNotification 关掉"录音中"             │  │
 │  │     │                                                                                           │  │
 │  │     └─ ⑧ 设置页    net/http 监听 127.0.0.1:27865（只认 loopback Host；POST 只收 JSON）           │  │
 │  │                    读写 config.toml / hotwords.txt；"保存并重启" = systemctl --user restart       │  │
 │  └─────────────────────────────────────────────────────────────────────────────────────────────────┘  │
 └──────────────────────────────────────────────────────────────────────────────────────────────────────┘
```

### 单次按键时序（实测，RTX 3060 Laptop + Fun-ASR-Nano）

```
 时间轴          toggle    daemon                         子进程 / 系统
 ─────────────────────────────────────────────────────────────────────────────────────────
 第一次按 Alt+V
   0 ms          连接 ───▶ 加锁，状态 idle
                           播开始音 ─────────────────────▶ pw-play（等 250ms，让最响的主体播完）
 ~250 ms                   启动 pw-record ───────────────▶ 开始写 wav（提示音尾音在此期间渐弱）
 ~280 ms         收到 ◀─── "recording"，弹"🎤 录音中"
                                 …… 用户说话 ……
 第二次按 Alt+V
   0 ms          连接 ───▶ 取走 recorder，释放锁；立刻关闭"录音中"通知（~20ms）
                           SIGINT pw-record，等它写完 WAV 头
                           播结束音（异步）
                           ├── 写 JSON 到 sidecar ───────▶ GPU 推理 150–350ms
                           └── 并行：focus.py ───────────▶ AT-SPI 查询约 40ms
 ~0.3 s                    收到文字；按焦点决定动作
                           wl-copy → 等 80ms 让 Mutter 登记新 selection → uinput 注入 8 个事件
 ~0.45 s                   文字上屏（成功不弹通知：文字本身和结束音就是反馈）
```

### 进程生命周期与自愈

```
 登录桌面
   └─▶ systemd --user 启动 voicetype.service（WantedBy=default.target）
         └─▶ voicetype daemon
               ① 建 uinput 虚拟键盘   ② 监听 socket + 设置页   ← 此后热键可用（加载中会提示）
               ③ 后台 loadSidecar：同步 transcribe.py → exec venv python → 等 {"ready":true}
                    │                                   约 12.5s（Fun-ASR-Nano）
                    ├─ 成功 ──▶ 通知"🎙️ 语音输入已就绪（funasr，加载 13 秒）"
                    └─ 失败 ──▶ 5s / 15s 后重试，共 3 次；仍失败 → daemon 不退出，
                                错误显示在设置页（坏配置靠重启解决不了，只会崩溃循环）

 运行中 sidecar 意外死亡（OOM、CUDA 错误、被 kill），或单次转写超过 120s 被 kill
   └─▶ daemon 留下恢复标记 → exit(1)
         └─▶ systemd Restart=on-failure，2s 后重启 → 重新加载 → 通知"🎙️ 语音输入已恢复"

 daemon 被停止（systemctl stop / 注销）
   └─▶ SIGTERM → 关 socket、销毁虚拟键盘、关闭 sidecar 的 stdin（3s 不退就 SIGKILL）
       兜底：sidecar 设了 PDEATHSIG，父进程无论怎么死它都会收到 SIGTERM，不会成为占着显存的孤儿；
             cgroup 里残留的 pw-record 等由 systemd 一并清理（KillMode=control-group）
```

### 用到的操作系统机制

| 机制 | 用在哪里 | 为什么这样做 |
|---|---|---|
| GNOME 自定义快捷键（gsettings → gsd-media-keys） | 全局热键 | Wayland 禁止普通程序抓全局按键；交给桌面自己的快捷键系统 |
| `/dev/uinput` + `ioctl(UI_DEV_CREATE)` | 注入粘贴快捷键 | GNOME 不支持 `wtype` 用的虚拟键盘协议，Ubuntu 的 `ydotool` 太旧；内核级虚拟键盘对所有应用都有效 |
| udev 规则（`/dev/uinput` 0666） | 让普通用户能建虚拟键盘 | 默认只有 root 可写 |
| AF_UNIX 流套接字（`$XDG_RUNTIME_DIR`） | toggle ⇄ daemon | 本机进程通信最轻量，目录本身只有本用户可访问 |
| 匿名管道 + 行分隔 JSON | daemon ⇄ sidecar | 父子进程天然通道，请求串行化 |
| `prctl(PR_SET_PDEATHSIG)` | sidecar | 父进程死了子进程跟着死，避免孤儿进程长期占着显存 |
| systemd `--user` 服务 + cgroup | daemon 常驻、开机自启、崩溃重启 | 随登录会话启动；cgroup 保证停止时子进程一个不漏 |
| 把会话环境变量写进 unit 文件 | `WAYLAND_DISPLAY` 等 | systemd 用户管理器默认没有这些变量，没有它们 wl-copy / notify-send 连不上显示服务 |
| Wayland 剪贴板（wl-copy） | 放文字 | CJK 文本无法用按键序列打出来 |
| PipeWire（pw-record / pw-play / pactl） | 录音、提示音、列麦克风 | 系统音频服务；`--target` 可以固定到指定麦克风 |
| AT-SPI（D-Bus 上的无障碍总线） | 判断焦点类型 | Wayland 不让查询键盘焦点，无障碍总线是唯一合法渠道 |
| D-Bus 通知服务（notify-send `-p`/`-r`、`CloseNotification`） | 状态提示 | gnome-shell 不认 notify-osd 的"同步替换"提示，必须靠通知 ID 原地替换和主动关闭，否则常驻的"录音中"会挡住后续通知 |
| 信号：SIGINT / SIGTERM / SIGKILL | 停止录音 / 优雅退出 / 超时强杀 | pw-record 收到 SIGINT 才会写好 WAV 头 |

---

## 安装

```bash
# 0. 编译
go build -o voicetype ./cmd
install -m 755 voicetype ~/.local/bin/          # 装到 PATH；服务和热键都指向这个路径

# 1. 系统包（PipeWire / libnotify / python3-gi 系统已自带）
sudo apt install wl-clipboard

# 2. 开放 /dev/uinput（一次性，立即生效，无需重新登录）
echo 'KERNEL=="uinput", MODE="0666", OPTIONS+="static_node=uinput"' \
  | sudo tee /etc/udev/rules.d/99-uinput.rules
sudo udevadm control --reload-rules && sudo udevadm trigger
sudo chmod 0666 /dev/uinput   # 对已存在的节点立即放权
#   0666 = 任何本地进程都能注入输入（单用户笔记本可接受）。
#   想更严格可改用 MODE="0660" GROUP="input" + `sudo usermod -aG input $USER`，
#   但加组后要【注销重新登录】才对 systemd --user 服务生效。

# 3. 构建 Python 边车环境（首次较慢；默认只建 config 里选的后端）
#    同时生成默认的 ~/.config/voicetype/config.toml 和 hotwords.txt
voicetype setup              # 或 voicetype setup funasr / qwen / whisper / all

# 4. 安装并启动守护进程（必须在 GNOME 图形终端里运行，才能把会话环境变量写进去）
voicetype install-service

# 5. 绑定全局热键（不要用 Super+D，GNOME 把它当作"显示桌面"）
voicetype install-hotkey '<Alt>v'

# 检查一切就绪
voicetype doctor
```

> 模型名写的是 Hub 名称（如 `Qwen/Qwen3-ASR-0.6B`）时，权重在**第一次启动**时下载；写本地目录的绝对路径则直接加载。

## 使用

任何窗口里：

1. 按 **Alt+V** → 提示音 + 通知"🎤 录音中"
2. 说话
3. 再按 **Alt+V** → 提示音，"录音中"通知立刻消失 → 文字出现在光标处

粘贴方式按焦点自动选：

| 焦点在哪里 | 动作 |
|---|---|
| 文本框、编辑器 | Ctrl+V |
| 终端（ptyxis、gnome-terminal 等基于 VTE 的） | Ctrl+Shift+V |
| 不是输入框（如文件管理器的文件列表） | 不按键，文字留在剪贴板，GPaste 历史里能找回 |
| 判断不了（Chrome / Electron 默认不暴露无障碍信息） | Ctrl+V |

> 转写结果会留在剪贴板里，**不会**恢复你原来的剪贴板内容。早期版本做过恢复，但和目标程序读取剪贴板存在竞态，偶尔会粘出旧内容，所以去掉了。好处是自动粘贴没生效时可以手动粘贴兜底。

## 配置

最方便的是 **`voicetype config`** 打开设置页（`http://127.0.0.1:27865/`）：切换后端和模型、选麦克风并试录看电平、编辑热词、试听提示音，一键保存并重启。

配置文件是 `~/.config/voicetype/config.toml`（`voicetype setup` 会生成带注释的默认文件）。手改后执行 `systemctl --user restart voicetype` 生效；设置页写回时会保留注释。

```toml
backend = "funasr"   # funasr | qwen | whisper
# model 必须与 backend 匹配，也可以写本地模型目录的绝对路径（不展开 ~）:
#   funasr  -> FunAudioLLM/Fun-ASR-Nano-2512
#   qwen    -> Qwen/Qwen3-ASR-0.6B / Qwen/Qwen3-ASR-1.7B
#   whisper -> large-v3 / large-v3-turbo / medium / small
model = "/home/you/models/asr/fun-asr-nano"
device = "cuda"      # cuda | cpu
compute = "float16"  # float16 | int8_float16 | int8 (仅 whisper)
lang = ""            # "" = 自动; zh / en / ja
vad = "fsmn-vad"     # 长录音按停顿切段 (仅 funasr); 可填本地目录; "off" 关闭
source = ""          # 录音输入设备名; "" = 系统默认 (pw-record --target)
sound = "halo"       # 提示音: halo | crystal | pulse (清亮) | warp | nebula | sonar (低沉) | off
web = "127.0.0.1:27865"  # 设置页地址; "off" 关闭
```

| 项 | 默认 | 说明 |
|---|---|---|
| `backend` | `whisper` | 转写引擎，各有独立 venv。**推荐 `funasr`**（见下方对比） |
| `model` | 随 backend | 必须与 backend 匹配，或写本地模型目录的绝对路径 |
| `device` | `cuda` | `cuda` 或 `cpu`（GPU 初始化失败会自动回退到 CPU，日志里有记录） |
| `compute` | `float16` | `float16` / `int8_float16` / `int8`（仅 whisper） |
| `lang` | 空（自动） | 强制语言，如 `zh`、`en` |
| `vad` | `fsmn-vad` | 仅 funasr：先按停顿把录音切成 ≤30 秒的段再识别。不切的话超过 30 秒就会截断丢字，5 分钟会爆显存；切段后长录音完整，显存峰值固定在约 3.6G。可填本地目录，`off` 关闭 |
| `source` | 空（默认） | 固定录音设备。Intel SOF 笔记本的默认源常常是空着的耳机孔；**设备名可能变**，失效后会悄悄回退到默认源。用设置页试录确认 |
| `sound` | `crystal` | 开始/结束提示音，程序内合成。设置页可试听，切换立即生效 |
| `web` | `127.0.0.1:27865` | 设置页地址，`off` 关闭。避开了 Gradio 默认的 7860+ 和代理常用的 7890 |

**热词**：`~/.config/voicetype/hotwords.txt`，每行一个专有名词（项目名、人名、术语），保存即生效，不用重启。偏置作用很强：实测一个写错的同音词就能把本来认对的结果带偏，所以宁缺毋滥，不要从输入法词库整包导入。

> 同名环境变量 `VOICETYPE_*` 会**覆盖**配置文件，方便临时实验，例如 `VOICETYPE_MODEL=medium voicetype daemon`。

### 后端怎么选（实测，6GB 显存）

| | Fun-ASR-Nano ✅ | Qwen3-ASR 0.6B | Qwen3-ASR 1.7B | faster-whisper |
|---|---|---|---|---|
| 中文专名（不加热词） | 准 | 常错 | 常错 | 最弱 |
| 英文 | 准 | 偶有错词 | 准 | 准 |
| 转写耗时 | 0.15–0.35s | 0.15–0.27s | 0.23–0.45s | — |
| 显存 | ~3.5GB | ~2.0GB | ~4.6GB | large-v3-turbo ~2GB |
| 加载耗时 | ~12.5s | ~9s | ~2s（已缓存时） | — |
| 适合 | 中文为主、夹英文术语（推荐） | 显存紧张 | 方言、多语种 | 主要说外语 |

## 常用命令

```bash
voicetype config                       # 打开设置页
voicetype doctor                       # 自检：依赖 / GPU / 麦克风 / 热词 / 焦点识别 / 服务
journalctl --user -u voicetype -f      # 守护进程日志（出问题先看这里）
journalctl --user -u voicetype -o cat | grep -E 'transcribed|focus'   # 每次转写的焦点与结果
systemctl --user restart voicetype     # 手改配置后重启
systemctl --user stop voicetype        # 临时腾出显存
```

## 子命令

| 命令 | 作用 |
|---|---|
| `voicetype setup [后端\|all]` | 解压边车 + `uv sync`（默认只建 config 里选的后端） |
| `voicetype install-service` | 写入并启用 systemd --user 服务 |
| `voicetype install-hotkey [键]` | 注册 GNOME 自定义快捷键（默认 `<Alt>v`） |
| `voicetype uninstall` | 停用并移除服务与快捷键 |
| `voicetype daemon` | 前台运行守护进程（调试用） |
| `voicetype toggle` | 开始/停止录音（热键绑定的就是它） |
| `voicetype doctor` | 自检 |
| `voicetype config` | 在浏览器打开设置页 |

## 排错

| 现象 | 原因与处理 |
|---|---|
| 按热键提示"模型加载中" | 刚登录或刚重启，等十几秒，就绪时会有通知 |
| 转写总是空，或识别成乱码 | 麦克风选错（常见：录到空耳机孔）。设置页试录：说话时电平应高于 −40 dB，只有 −60 dB 左右说明录到的是底噪 |
| 忘了按第二下 | 录音满 5 分钟会自动结束并转写，弹通知说明 |
| 文字进了剪贴板但没粘上 | 看日志里的 `focus=`：`none` 说明当时焦点不在输入框；`unknown` 说明是 Chrome/Electron 或不支持无障碍的终端（如 kitty），按 Ctrl+V 或手动粘贴 |
| 换了模型但没生效 | 手改配置后需要 `systemctl --user restart voicetype`；设置页的"保存并重启"会自动处理 |
| 显存不足 / OOM | `systemctl --user stop voicetype` 腾出显存，或换成 qwen 0.6B |
| 显存会一直涨吗 | 不会。实测连续转写不泄漏；每次推理临时多占的显存用完就还，常驻的约 3.5G 是模型本身，停服务才释放 |
| 念字母缩写时字母间有空格 | 已自动合并（"H T T P" → "HTTP"，字母数字混念也行："E S P 3 2" → "ESP32"）。若字母本身认错（如 H 听成 S），把这个缩写加进热词 |
| 长段口述丢内容 | 确认 `vad` 没有设成 `off`（funasr 需要靠它分段） |
| 设置页打不开 | 守护进程没在运行，或 `web = "off"` |

更深入的设计取舍与踩坑记录见 [ARCHITECTURE.md](ARCHITECTURE.md)。

# voicetype 架构设计文档

> 全局语音输入转文字工具，面向 **GNOME Wayland**（Ubuntu 24.04 验证）。
> 按一下热键开始录音，再按一下结束 → 本地 GPU 转写（faster-whisper / Qwen3-ASR / Fun-ASR-Nano）→ 自动粘贴到当前光标处。

本文记录整体架构、运行时行为、关键设计取舍和运维方式，便于日后维护。

---

## 1. 一句话概述

后台常驻一个守护进程（含一个常驻显存的语音模型）；按热键时由一个瞬时小程序通知它"开始/停止"；它录音、在 GPU 上转写、再模拟键盘把文字粘到光标处。

## 2. 设计目标与约束

| 目标 | 说明 |
|---|---|
| 交付一个二进制 | Go 编译产物即可用；Python 边车用 `go:embed` 打包进二进制，首次自解压 |
| 本地、离线、免费 | 转写全在本机 GPU（faster-whisper / CTranslate2），不依赖云服务 |
| 全局可用 | 任意窗口按热键即可口述 |
| 低延迟 | 模型常驻显存，停止录音后秒级出字 |
| Wayland 兼容 | 不依赖 X11；绕过 Wayland 对全局键、键盘注入的限制 |

**核心约束**：Wayland 出于安全，禁止普通程序①捕获全局快捷键、②向其它窗口注入按键。整个架构很大程度是围绕绕开这两条限制设计的。

## 3. 架构总览

```
┌─────────────┐   按 Alt+V    ┌──────────────────┐
│ GNOME/Mutter│──────────────▶│ voicetype toggle │  (瞬时进程，发完即退)
│ (全局快捷键) │   执行命令     └────────┬─────────┘
└─────────────┘                         │ unix socket: "toggle"
                                         ▼
                              ┌──────────────────────┐
                              │   voicetype daemon    │  (systemd 常驻)
                              │  状态机 / 总指挥        │
                              ├──────────────────────┤
                              │ pw-record  录音→wav    │
                              │ /dev/uinput 虚拟键盘   │
                              │ focus.py   AT-SPI 焦点 │
                              │ HTTP 设置页 :27865     │
                              └─────┬───────────▲─────┘
                          stdin/JSON│           │stdout/JSON
                                    ▼           │
                              ┌──────────────────────┐
                              │  sidecar (Python)     │  (daemon 的子进程)
                              │ funasr / qwen / whisper│
                              │  常驻模型 @ GPU        │
                              └──────────────────────┘
```

## 4. 运行时三个进程

| 进程 | 实体 | 职责 | 生命周期 |
|---|---|---|---|
| **daemon** | Go 二进制 `voicetype daemon` | 监听 socket；管录音、转写编排、粘贴；持有虚拟键盘 | systemd 开机自启，常驻 |
| **sidecar** | venv Python 跑 `transcribe.py` | 把转写模型(funasr/qwen/whisper)常驻显存，收 wav 路径→返回文字 | daemon 拉起，随 daemon 同生共死（Pdeathsig） |
| **toggle** | Go 二进制 `voicetype toggle` | 连 socket 发一个 "toggle" 指令 | 每次按键起灭一次 |

> daemon 和 toggle 是**同一个二进制**的不同子命令；sidecar 是 Python。此外 daemon 还会按需派生短命子进程：`pw-record`(录音)、`pw-play`(提示音)、`python3 -c focus.py`(焦点)、`wl-copy`(剪贴板)、`notify-send`/`gdbus`(通知)。完整调度图见 README「调度全景图」。

### 4.1 进程模型：一个二进制扮演多角色

代码里**没有三个程序**——`daemon` 和 `toggle` 是同一个二进制 `voicetype`，靠命令行第一个参数决定走哪条路。分发在 `cmd/main.go`：

```go
switch os.Args[1] {
case "daemon":  err = dictation.RunDaemon()  // 常驻服务器
case "toggle":  err = dictation.Toggle()     // 发个消息就退的客户端
// setup / install-service / install-hotkey / uninstall / doctor ...
}
```

| 执行的命令 | `os.Args[1]` | 函数 | 谁来启动 | 进程 |
|---|---|---|---|---|
| `voicetype daemon` | `daemon` | `RunDaemon()` | systemd（开机一次） | 常驻守护 |
| `voicetype toggle` | `toggle` | `Toggle()` | GNOME（每次按键） | 瞬时客户端 |

`Toggle()` 极简：拨号 socket → 发 "toggle" → 退出。`RunDaemon()` 则进入 `for` 死循环守在 socket 上，所以常驻不退。

> 一句话：**三个进程 = 同一个 Go 二进制的两次不同调用（daemon/toggle）+ daemon 派生的一个 Python 子进程（sidecar）**。两份代码（Go 一份、Python 一份），一个 Go 二进制。

### 4.2 启动流程

```
开机/登录 → systemd --user 读 voicetype.service → 执行 `voicetype daemon`
                                                        │
RunDaemon() 依次：                                       ▼
  ① LoadConfig()        读 config.toml（后端 / 模型 / 设备 / 麦克风源 / 设置页地址）
  ② openKeyboard()      打开 /dev/uinput 建虚拟键盘
  ③ net.Listen(socket)  监听 toggle；同时起设置页 HTTP（127.0.0.1:27865）
  ④ go loadSidecar()    后台：同步 transcribe.py → exec python → 等 {"ready":true}
                        失败则 5s/15s 后重试(共 3 次)；成功发"已就绪(加载 N 秒)"通知
                                                        │
                                                        ▼
                                    加载中按 Alt+V → 通知"⏳ 模型加载中"；就绪后正常工作
```

**先服务、后加载**：socket 和设置页先起来，模型在后台加载。加载期间按热键得到"加载中"提示（旧版此时 socket 不存在，会误报 "daemon not running"）；模型**加载失败时 daemon 不退出**，错误显示在设置页，可直接改配置重启——坏配置交给 systemd 重启只会无限崩溃循环。

### 4.3 sidecar 是"服务"吗？

不是 systemd 服务，但它常驻。区别在于：

| | daemon | sidecar |
|---|---|---|
| 谁管理 | systemd（自启 / 崩溃重启） | daemon（代码 `exec` 出来的子进程） |
| systemd 可见 | 是（`systemctl --user status voicetype`） | 否，只是 daemon 的子进程 |
| 对外接口 | 有（unix socket） | 无，只跟父进程走 stdin/stdout 管道 |
| 生命周期 | 常驻 | 常驻，但随 daemon 同生共死（Pdeathsig） |

sidecar 是"**daemon 豢养的常驻工人**"，不能单独重启；要重启用 `systemctl --user restart voicetype`（daemon 连带 sidecar 一起重生）。

## 5. 端到端数据流（按一次 Alt+V）

**第一次按（空闲 → 录音）**
1. GNOME 检测到 Alt+V，执行 `voicetype toggle`
2. toggle 连 `/run/user/1000/voicetype.sock`，发 `toggle\n`
3. daemon 状态机：idle → recording；先播开始提示音(等 250ms)，再启动 `pw-record` 把内置麦录成临时 `.wav`
4. 弹"🎤 录音中"通知(常驻，记下通知 ID)

**第二次按（录音 → 停止）**
1. 同样经 toggle → socket → daemon
2. daemon 立刻关闭"录音中"通知；给 `pw-record` 发 SIGINT，等它收尾出合法 wav；异步播结束提示音
3. 把 wav 路径以 JSON 写入 sidecar 的 stdin；**同时**起 `focus.py` 查焦点(§7.9)
4. sidecar 在 GPU 上转写(热词按 mtime 重读)，JSON 返回文字到 stdout
5. daemon 用 `wl-copy` 把文字放进剪贴板
6. 按焦点：文本框注入 **Ctrl+V**、终端 **Ctrl+Shift+V**、非输入框不按键(只留剪贴板)、未知按 Ctrl+V
7. 删除临时 wav；成功不弹通知，"copied"/出错才提示

## 6. 代码结构

```
voicetype/
├── cmd/main.go                    # CLI 分发：daemon/toggle/setup/install-*/uninstall/doctor/config
├── internal/dictation/            # 守护进程与设备 I/O
│   ├── daemon.go                  #   socket 服务 + toggle 状态机编排
│   ├── sidecar.go                 #   常驻 Python 进程管理 + JSON 行协议（按 backend 选 venv）
│   ├── recorder.go                #   pw-record 录音封装（SIGINT 收尾）
│   ├── uinput.go                  #   纯 Go /dev/uinput 虚拟键盘（Ctrl+V）
│   ├── output.go                  #   wl-copy + 触发粘贴
│   ├── notify.go                  #   notify-send 桌面通知封装
│   └── paths.go                   #   路径 + config.toml 解析（极简 TOML，零依赖）
├── internal/setup/                # 安装/卸载/自检
│   ├── setup.go                   #   解压边车 + uv sync（默认只建选中后端）+ 生成默认 config.toml / hotwords.txt
│   ├── service.go                 #   生成 systemd --user 单元（只烤入会话环境）
│   ├── hotkey.go                  #   gsettings 注册 GNOME 自定义快捷键
│   ├── uninstall.go               #   反向移除服务与快捷键
│   ├── doctor.go                  #   依赖/GPU/麦克风/热词/焦点/服务体检
│   └── config.go                  #   `voicetype config`：xdg-open 设置页
└── internal/assets/
    ├── assets.go                  # go:embed（transcribe.py + 两个后端的 pyproject）
    ├── sidecar/
    │   ├── transcribe.py          # 常驻转写循环；按 VOICETYPE_BACKEND 分派 whisper / qwen / funasr
    │   └── pyproject.toml         # faster-whisper 后端依赖（CUDA-12）
    ├── sidecar-qwen/
    │   └── pyproject.toml         # Qwen3-ASR 后端依赖（qwen-asr + torch，CUDA-13）
    └── sidecar-funasr/
        └── pyproject.toml         # Fun-ASR-Nano 后端依赖（funasr + torch）
```

`internal/dictation/` 另有：`focus.go` + `focus.py`（AT-SPI 焦点识别，§7.9）、`web.go` + `web/index.html`（设置页，§7.11）。

> transcribe.py 由两个后端共用；`sidecar/` 与 `sidecar-qwen/` 是**各自独立的 uv 环境**——
> faster-whisper 要 CUDA-12 cuDNN、torch(qwen) 拉 CUDA-13 cuDNN，两套 cuDNN 同处一个 venv
> 会在推理时报 `CUDNN_STATUS_SUBLIBRARY_VERSION_MISMATCH`，故必须隔离。daemon 用
> `BackendDir()` 按 backend 选对应 venv 的 python 去跑那份共用脚本。

## 7. 关键设计决策与取舍

### 7.1 全局快捷键 —— 交给 GNOME，而非自己抓键
Wayland 禁止程序捕获全局键。**方案**：用 `gsettings` 注册 GNOME 自定义快捷键 `<Alt>v → voicetype toggle`，由合成器监听、按下时启动我们的程序。无需任何特权。
> 代价：依赖 GNOME。换桌面环境需改用各自的快捷键机制，或直接读 `/dev/input/event*` 原始事件（要 root/input 组、自己解析键码）。

### 7.2 文字注入 —— 纯 Go 驱动 /dev/uinput
Wayland 不让向其它窗口注入按键。可选项里：`wtype` 用的虚拟键盘协议 **GNOME 的 Mutter 不支持**；Ubuntu 仓库的 `ydotool` 是 0.1.8 老版本（无 ydotoold、要 root）。**方案**：自己用纯 Go 打开 `/dev/uinput`，造一个内核级虚拟键盘按 Ctrl+V，无外部依赖。
- **为什么是"剪贴板 + Ctrl+V"而不是逐字模拟**：按键注入是按键码，无法输入中文/CJK 字形；走剪贴板能精确还原任意文本。
- **不做剪贴板备份/恢复**：早期版本会先备份旧剪贴板、粘贴后恢复，但"恢复"与目标程序读取剪贴板存在**竞态**，偶发粘成旧内容。现在直接把结果留在剪贴板（也可作为手动粘贴的兜底）。

### 7.3 模型常驻 —— 长生命周期 daemon + 边车
加载 large-v3 进显存要数秒，每次按键重载无法接受。**方案**：daemon 启动时拉起 sidecar，模型常驻显存，之后每次转写秒级返回。

### 7.4 单二进制交付 —— go:embed + uv
既要"一个二进制"，又要用 Python 的转写引擎。**方案**：把 `transcribe.py` 和各后端 `pyproject.toml` 用 `go:embed` 打进二进制，`voicetype setup` 时自解压到 `~/.local/share/voicetype/` 并用 **uv** 建环境。

### 7.4a 多后端 —— faster-whisper / Qwen3-ASR / Fun-ASR-Nano，各用独立 venv
支持三个转写引擎,由 `backend` 选择:`whisper`(faster-whisper,默认)、`qwen`(Qwen3-ASR)、`funasr`(Fun-ASR-Nano,中文专名更准、热词偏置强)。`setup` 默认只建 config 里选中的后端(每个 venv 都是数 GB 的 torch/CUDA)。funasr 与 qwen 虽都用 torch,但各自钉 transformers 版本,同样分开。三者**各建一个 uv 环境**(`sidecar/` 与 `sidecar-qwen/`),不能合并:faster-whisper 依赖 CUDA-12 的 `nvidia-cudnn-cu12`,而 torch(qwen)拉 CUDA-13 的 cuDNN,两套 cuDNN 同处一个 venv 推理时会报 `CUDNN_STATUS_SUBLIBRARY_VERSION_MISMATCH`。`transcribe.py` 由两后端共用,daemon 用 `BackendDir()` 按 backend 选对应 venv 的 python 运行它。
> 显存参考(6GB 卡,实测):qwen `0.6B` ≈ 1.9GB、`1.7B` ≈ 4.6GB;Fun-ASR-Nano ≈ 3.5GB(首次推理后);whisper `large-v3-turbo` ≈ 2GB。
>
> **脚本自动同步**:daemon 每次启动比对磁盘上的 `transcribe.py` 与二进制内嵌版本,不同就覆盖(`SyncScript`)。曾因升级二进制后没重跑 `setup`,旧脚本读不到 `model` 配置、静默加载默认模型。

### 7.5 边车进程管理 —— 直接 exec venv python + Pdeathsig
- daemon **直接 exec** venv 里的 `python`（而非 `uv run`），让持有显存的进程是 daemon 的直接子进程；设 `Pdeathsig=SIGTERM`，daemon 一死它必死，**杜绝孤儿进程长期占用显存**（曾因此 OOM）。
- uv 用**绝对路径**解析（`exec.LookPath` 失败则回退 `~/.local/bin/uv`），因为 systemd 用户服务的 PATH 不含 `~/.local/bin`。

### 7.6 进程间通信
| 通道 | 用途 | 为何 |
|---|---|---|
| unix socket | toggle ↔ daemon | 控制指令仅几十字节，socket 简单可靠；非共享内存 |
| stdin/stdout 管道 + JSON 行 | daemon ↔ sidecar | 父子进程天然管道，行分隔 JSON 易实现、串行化请求 |

### 7.7 麦克风源固定（Intel SOF 坑）
此机默认输入源是没插设备的耳机口麦克风（录到静音），真正的内置数字麦在 SOF 的 `_6_` 源上。**方案**：① 把系统默认源切到内置麦；② 在 config.toml 用 `source` **按设备名钉死**内置麦（`recorder.go` 经 `cfg.Source` 传给 `pw-record --target`），防止默认源被 WirePlumber 重置。

### 7.8 systemd 用户服务烤入会话环境
systemd `--user` 管理器默认**不含** `WAYLAND_DISPLAY` 等图形会话变量，导致 `wl-copy`/`notify-send` 连不上显示。**方案**：`install-service` 在图形终端运行时，把当时环境里的 `WAYLAND_DISPLAY`/`DISPLAY`/`XDG_CURRENT_DESKTOP` **写进 unit 文件**。（ASR/麦克风等配置则放在 config.toml,不进 unit——否则重跑 `install-service` 会冲掉,见 §8。）

### 7.9 按焦点决定怎么粘 —— AT-SPI
Wayland 不让查"键盘焦点在哪",但无障碍总线(AT-SPI)能。转写的同时,daemon 用**系统 python3 + PyGObject**(GNOME 自带,Go 侧仍零依赖)跑内嵌的 `focus.py`:只在 ACTIVE 窗口里找 FOCUSED 控件,分四类——
| 结果 | 动作 |
|---|---|
| `editable`(文本框) | Ctrl+V |
| `terminal`(VTE:ptyxis/gnome-terminal) | **Ctrl+Shift+V** |
| `none`(焦点在非输入控件,如文件列表) | **不按键**,只留剪贴板;GPaste 自动收进历史。避免在文件管理器里 Ctrl+V 粘出文件 |
| `unknown`(Chrome/Electron 在 a11y 关闭时不暴露树、python3-gi 缺失、超时) | Ctrl+V(旧行为) |

Chrome 会留一个"陈旧的 ACTIVE 窗口"但树是空的,自然落到 unknown。**不全局开 a11y**:那会拖慢所有 Chromium/Electron 应用。

### 7.10 自愈 —— 边车死了就让 systemd 重来
**加载阶段**失败(进程被杀、暂时 OOM)先在 daemon 内重试:5s、15s 后各一次,共 3 次;仍失败才停在错误状态等人改配置。**录音上限 5 分钟**(`maxRecording`):忘了按第二下时自动结束并转写,避免录音无限增长、再撞上转写超时把边车杀掉。**运行阶段**边车进程退出(OOM、CUDA 错误)或单次转写超时(120s,超时即 kill)时,daemon **以非零码退出**,由 `Restart=on-failure` 连同模型一起拉起。否则 daemon 看似健康、每次热键都"转写失败"。区分正常关停(SIGTERM)用 `stopping` 标志。

### 7.11 设置页 —— daemon 内嵌 HTTP
`voicetype config` 打开 `http://127.0.0.1:27865/`(`web` 键可改,`off` 关闭)。可改后端/模型/设备/语言/麦克风(写回 config.toml,**按行替换值、保留注释**,保存并重启走 `systemctl --user restart --no-block`——systemctl 在本服务 cgroup 里会被一起杀,`--no-block` 让重启任务留在 systemd 侧)、编辑热词(即时生效)、**试录 3 秒**看电平并用当前模型识别(选麦克风靠这个)。防护:只认 loopback Host(防 DNS rebinding);POST 只收 `application/json`(跨站页面发 JSON 需 CORS 预检,我们从不应答),其他网站改不了配置。

### 7.12 热词
`~/.config/voicetype/hotwords.txt`,每行一词。sidecar 每次转写前按 mtime 重读,改完即生效。funasr 传 `hotwords`,qwen 传 `context`,whisper 传 `hotwords` 字符串。**偏置很强**:实测给 funasr 一个错误同音词,能把本来认对的结果拽错——所以热词表宁缺毋滥,不要从输入法词库(充满打字碎片)整包导入。

### 7.12a 长录音 —— VAD 分段
Fun-ASR-Nano 一次吃不下长音频。实测(不分段):30s 只出第一句(截断)、1–2 分钟大段丢失且错字增多、5 分钟推理 OOM(sidecar 返回 error,该段口述全丢)。**方案**:funasr `AutoModel(vad_model=..., vad_kwargs={"max_single_segment_time": 30000})`,先用 fsmn-vad(4MB)按停顿切成 ≤30s 的段再逐段识别——字数随时长线性增长(5 分钟 999 字)、推理峰值显存与时长无关(~3.58G)、5 分钟约 10s 转完,短句延迟不变(0.24s),热词逐段生效。funasr 用空格拼接各段,sidecar 去掉 CJK 字符/标点之间的空格(保留英文词间空格)。**局限**:一口气不停顿超过 30s 时只能硬切,切口处可能有个别字重复/遗漏。配置键 `vad`(默认 `fsmn-vad`,可填本地目录,`off` 关闭),只作用于 funasr——whisper 自带 `vad_filter`。

**显存不会持续上涨**:连续 10 次短句转写进程显存恒定 3556MiB;长音频推理时的临时峰值用完即还(torch reserved 回落到 ~3.42G)。常驻的 ~3.5G 是模型权重 + 缓存,是"按完即出字"的代价,停服务才释放。

### 7.13 提示音,不做动画
按下开始/结束各播一声提示音。系统音效主题太"系统",故在 `sound.go` 里**用 Go 实时合成**(正弦音 + 钟形泛音 + 可选滑音 + 短回声,零音频文件、零依赖),预设分两组:低沉科幻的 `warp`(锯齿味音色升降"充能/断电")/`nebula`(低频 FM 金属音 + 高频闪光)/`sonar`(深沉声呐 + 远处回声),清亮的 `crystal`(玻璃钟音)/`pulse`(电子扫频)/`halo`(错落的 Cmaj7 和弦、尾音最长,刻意压低一个八度免得尾音尖)。**低沉预设的基频在 100–200Hz,但笔记本扬声器放不出 ~250Hz 以下**,所以刻意削弱基频、加重 2–8 次谐波,靠"缺失基频"效应让耳朵脑补出低音;改音色时用频谱检查 300Hz 以上能量占比(目标 ≥40%),否则在笔记本上会又小又闷;开始上行、结束下行,首次使用时渲染到 `$XDG_RUNTIME_DIR` 再用 `pw-play` 播。**开始音先播 250ms(响的主体)才启动 `pw-record`**,免得被麦克风录进去,弱尾音与录音重叠无碍;结束音在录音停止后异步播。设置页可试听,切换即时生效(daemon 的 `sound` 字段,不需重启)。**不做录音动画**:Wayland 下普通程序无法在其他窗口之上绘制悬浮层,只能写 GNOME Shell 扩展(多一套代码);而 GNOME 录音时顶栏自带麦克风指示,加上"🎤 录音中"通知已足够。

### 7.14 通知 —— 靠 ID 原地替换,停录音即关闭
gnome-shell(50.x)**不认** notify-osd 的 `x-canonical-private-synchronous` 提示,每次 `notify-send` 都会新开一条;而"🎤 录音中"是常驻的,后续通知只能排在它后面,表现为"字都上屏了提示还挂着"。**方案**(`notify.go`):所有通知共用一个槽位——`notify-send -p` 记下 ID,下一条用 `-r <ID>` 原地替换;按下停止的瞬间同步调用 D-Bus `CloseNotification` 关掉录音提示(约 20ms,同步执行以免晚于后续报错通知把它误关);成功不再弹"✅"(文字上屏 + 结束音即反馈);全部标为 transient(`-e`),不进通知中心。

### 7.15 wl-copy 不能接管道
`wl-copy` 会 fork 一个后台子进程持有剪贴板内容,它继承父进程的 stdout/stderr。若用 `CombinedOutput` 之类把输出接成管道,`Wait` 要等管道关闭——也就是等**别的程序接管剪贴板**才返回。GPaste 只记录不接管,于是粘贴键迟迟发不出,等你下次复制别的东西时才补发到不知哪个窗口。**方案**:stdout 丢弃、stderr 直接给 daemon 自己的 fd(journal),`Run` 在父 wl-copy 退出时即返回。

## 8. 配置（config.toml）

配置在 **`~/.config/voicetype/config.toml`**（`setup` 会生成带注释的默认文件），daemon 启动时由 `LoadConfig` 读取(`paths.go` 里一个零依赖的极简 TOML 解析)。改完 `systemctl --user restart voicetype` 生效。**刻意不放在 systemd unit 里**——否则重跑 `install-service`(整文件重写)会把这些设置冲掉。

| 键 | 默认 | 说明 |
|---|---|---|
| `backend` | `whisper` | 转写引擎:`whisper` / `qwen` / `funasr`,各自独立 venv(见 §7.4a) |
| `model` | 随 backend | **必须与 backend 匹配**,或写本地模型目录绝对路径:whisper `large-v3`/`large-v3-turbo`/`medium`/`small`;qwen `Qwen/Qwen3-ASR-0.6B`/`-1.7B`;funasr `FunAudioLLM/Fun-ASR-Nano-2512` |
| `device` | `cuda` | `cuda` 或 `cpu`(GPU 初始化失败自动回退 CPU) |
| `compute` | `float16` | `float16`/`int8_float16`/`int8`,越往后越省显存(仅 whisper) |
| `lang` | 空(自动) | 强制语言,如 `zh`、`en` |
| `source` | 空(系统默认) | 钉死录音输入设备(见 §7.7);设备改名后会静默回退默认源,用设置页试录排查 |
| `sound` | `crystal` | 提示音风格:清亮 `halo`/`crystal`/`pulse`,低沉 `warp`/`nebula`/`sonar`,`off` 关闭(见 §7.13) |
| `web` | `127.0.0.1:27865` | 设置页地址,`off` 关闭(见 §7.11);避开 Gradio 默认的 7860+ 与代理常用的 7890 |

> 同名环境变量 `VOICETYPE_*` 若设置会**覆盖**配置文件,方便前台调试/临时实验(如 `VOICETYPE_MODEL=medium voicetype daemon`)。

## 9. 系统集成与安装产物

| 位置 | 作用 |
|---|---|
| `~/.local/bin/voicetype` | 二进制（装到 PATH；ExecStart 与热键命令都指它） |
| `~/.local/share/voicetype/sidecar/`（含 `.venv`） | faster-whisper 边车 + uv 环境 |
| `~/.local/share/voicetype/sidecar-qwen/`（含 `.venv`） | Qwen3-ASR 边车 uv 环境 |
| `~/.local/share/voicetype/sidecar-funasr/`（含 `.venv`） | Fun-ASR-Nano 边车 uv 环境 |
| `~/.config/voicetype/config.toml` | 用户配置（后端/模型/设备/语言/麦克风/设置页） |
| `~/.config/voicetype/hotwords.txt` | 热词表（即时生效） |
| `~/.cache/huggingface/hub/models--*` | 下载的模型权重（whisper / qwen） |
| `~/.config/systemd/user/voicetype.service` | 常驻服务定义（只含会话环境变量，不含 ASR 配置） |
| `/etc/udev/rules.d/99-uinput.rules` | 把 `/dev/uinput` 设为 `0666`，允许造虚拟键盘 |
| gsettings `.../custom-keybindings/voicetype/` | `<Alt>v → voicetype toggle` |
| `/run/user/1000/voicetype.sock` | toggle↔daemon 通信 |
| `127.0.0.1:27865` | 设置页（`voicetype config`） |

**首装顺序**（详见 README）：`sudo apt install wl-clipboard` → 配 `/dev/uinput` 权限 → `voicetype setup` → `install-service` → `install-hotkey`。

## 10. 运维与排错

```bash
voicetype doctor                      # 一键体检：依赖/GPU/麦克风/热词/焦点识别/服务
voicetype config                      # 打开设置页
systemctl --user status voicetype     # 服务状态
journalctl --user -u voicetype -f     # 实时日志（出问题先看这个）
systemctl --user restart voicetype    # 改配置后重启
voicetype toggle                      # 命令行手动触发（等于按一次热键）
voicetype uninstall                   # 移除服务与快捷键
```

**已踩过的典型坑**（也是各设计取舍的由来）：
1. 服务空转重启：systemd PATH 找不到 `uv` → 改绝对路径（§7.5）。
2. GPU OOM 回退 CPU：旧 daemon 的孤儿 sidecar 占满显存 → 清理 + Pdeathsig（§7.5）。
3. 一直录到静音：默认麦是空的耳机口 → 固定内置麦源（§7.7）。
4. 粘贴串入旧内容/偶发粘错：剪贴板恢复竞态 → 去掉恢复（§7.2）。
5. 终端里不自动粘贴：终端粘贴是 Ctrl+Shift+V 而非 Ctrl+V → AT-SPI 识别出终端后改发 Ctrl+Shift+V（§7.9）；识别不了的终端（kitty/alacritty 等无 a11y）仍需手动粘贴。
6. 换了模型却没生效：磁盘上的 transcribe.py 是旧版 → daemon 启动时自动同步（§7.4a）。
7. 麦克风设备改名（SOF 的 `hw_sofhdadsp_6` 变成 `HiFi__Mic1`）→ 钉死的 source 失效、静默回退到空耳机口 → 设置页列出现有设备并标出失效项，试录看电平。
8. 转写结果进了剪贴板却不自动粘贴：wl-copy 输出接了管道，等剪贴板被接管才返回（§7.15）。
9. "录音中"通知挂着不走、出字后才消失：gnome-shell 不认同步替换提示（§7.14）。

## 11. 已知限制与未来方向

- **焦点识别盲区**：Chrome/Electron(a11y 关闭时)与非 VTE 终端(kitty/alacritty)识别为 unknown，一律 Ctrl+V；在这类终端里需手动 Ctrl+Shift+V。全局开 a11y 能看进 Chromium，但会拖慢所有 Chromium 应用，故不做(§7.9)。
- **录音动画**：Wayland 下需 GNOME Shell 扩展才能画悬浮层，暂不做；靠提示音 + 通知 + GNOME 顶栏自带的麦克风指示(§7.13)。
- **流式转写**：现为"录完整段再转"。结论是**不值得做真流式**,原因有二:① 模型侧——faster-whisper 不原生支持(只能滑窗 hack、临时结果跳改),Qwen3-ASR 的流式只在 vLLM 后端(重型服务层,与轻量 sidecar 取向冲突);② **输出方式封顶收益**——本工具靠"剪贴板 + 注入一次 Ctrl+V"粘进别人窗口,粘完无句柄可改,且 CJK 无法逐字按键注入,所以即便内部流式,**粘贴仍只能在松手时一次性发生**,拿不到"边说边冒字"的体验。短句场景批量转写已是秒级,够快。若确有长段口述的尾部等待痛点,**中间方案**是按静音(VAD)切段、录音时逐段转、松手时拼好一次粘贴——不动输出模型、不上 vLLM。
- **对外服务**：sidecar 目前只服务父 daemon。若要给其它程序用，可给 daemon 的 socket 加 `transcribe <文件>` 指令，或开 HTTP `POST /transcribe`，使其成为本机转写服务。
- **跨桌面**：全局键依赖 GNOME gsettings；KDE/wlroots 等需各自适配。
```

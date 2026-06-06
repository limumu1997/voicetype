# voicetype 架构设计文档

> 全局语音输入转文字工具，面向 **GNOME Wayland**（Ubuntu 24.04 验证）。
> 按一下热键开始录音，再按一下结束 → 本地 faster-whisper（GPU）转写 → 自动粘贴到当前光标处。

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
                              └─────┬───────────▲─────┘
                          stdin/JSON│           │stdout/JSON
                                    ▼           │
                              ┌──────────────────────┐
                              │  sidecar (Python)     │  (daemon 的子进程)
                              │  faster-whisper        │
                              │  large-v3-turbo @ GPU │
                              └──────────────────────┘
```

## 4. 运行时三个进程

| 进程 | 实体 | 职责 | 生命周期 |
|---|---|---|---|
| **daemon** | Go 二进制 `voicetype daemon` | 监听 socket；管录音、转写编排、粘贴；持有虚拟键盘 | systemd 开机自启，常驻 |
| **sidecar** | venv Python 跑 `transcribe.py` | 把 Whisper 模型常驻显存，收 wav 路径→返回文字 | daemon 拉起，随 daemon 同生共死（Pdeathsig） |
| **toggle** | Go 二进制 `voicetype toggle` | 连 socket 发一个 "toggle" 指令 | 每次按键起灭一次 |

> daemon 和 toggle 是**同一个二进制**的不同子命令；sidecar 是 Python。

### 4.1 进程模型：一个二进制扮演多角色

代码里**没有三个程序**——`daemon` 和 `toggle` 是同一个二进制 `voicetype`，靠命令行第一个参数决定走哪条路。分发在 `cmd/voicetype/main.go`：

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
  ① LoadConfig()        读环境变量（模型 / 设备 / 麦克风源）
  ② startSidecar()      exec python → 等模型加载进 GPU(数秒) → 收到 {"ready":true} 才继续
  ③ openKeyboard()      打开 /dev/uinput 建虚拟键盘
  ④ net.Listen(socket)  监听 → 进入 for 循环等 toggle
                                                        │
                                                        ▼
                                                  就绪，等待 Alt+V
```

注意：**socket 在第 ④ 步才出现**，而 ② 加载模型最慢——模型没好之前按 Alt+V 会报 "daemon not running"（实为尚未就绪）。

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
3. daemon 状态机：idle → recording，启动 `pw-record` 把内置麦录成临时 `.wav`
4. 弹"🎤 录音中"通知

**第二次按（录音 → 停止）**
1. 同样经 toggle → socket → daemon
2. daemon 给 `pw-record` 发 SIGINT，等它收尾出合法 wav
3. 把 wav 路径以 JSON 写入 sidecar 的 stdin
4. sidecar 在 GPU 上转写，JSON 返回文字到 stdout
5. daemon 用 `wl-copy` 把文字放进剪贴板
6. daemon 通过 `/dev/uinput` 虚拟键盘注入 **Ctrl+V** 粘贴
7. 删除临时 wav，弹一个短暂"✅"

## 6. 代码结构

```
voicetype/
├── cmd/voicetype/main.go          # CLI 分发：daemon/toggle/setup/install-*/uninstall/doctor
├── internal/dictation/            # 守护进程与设备 I/O
│   ├── daemon.go                  #   socket 服务 + toggle 状态机编排
│   ├── sidecar.go                 #   常驻 Python 进程管理 + JSON 行协议
│   ├── recorder.go                #   pw-record 录音封装（SIGINT 收尾）
│   ├── uinput.go                  #   纯 Go /dev/uinput 虚拟键盘（Ctrl+V）
│   ├── output.go                  #   wl-copy + 触发粘贴
│   ├── notify.go                  #   notify-send 桌面通知封装
│   └── paths.go                   #   路径与环境变量配置
├── internal/setup/                # 安装/卸载/自检
│   ├── setup.go                   #   解压边车 + uv sync
│   ├── service.go                 #   生成 systemd --user 单元（烤入会话环境）
│   ├── hotkey.go                  #   gsettings 注册 GNOME 自定义快捷键
│   ├── uninstall.go               #   反向移除服务与快捷键
│   └── doctor.go                  #   依赖/GPU/麦克风/服务体检
└── internal/assets/
    ├── assets.go                  # go:embed
    └── sidecar/
        ├── transcribe.py          # faster-whisper 常驻转写循环
        └── pyproject.toml         # uv 项目依赖
```

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
既要"一个二进制"，又要用最好的转写引擎（Python 的 faster-whisper）。**方案**：把 `transcribe.py` 和 `pyproject.toml` 用 `go:embed` 打进二进制，`voicetype setup` 时自解压到 `~/.local/share/voicetype/sidecar` 并用 **uv** 建独立环境。

### 7.5 边车进程管理 —— 直接 exec venv python + Pdeathsig
- daemon **直接 exec** venv 里的 `python`（而非 `uv run`），让持有显存的进程是 daemon 的直接子进程；设 `Pdeathsig=SIGTERM`，daemon 一死它必死，**杜绝孤儿进程长期占用显存**（曾因此 OOM）。
- uv 用**绝对路径**解析（`exec.LookPath` 失败则回退 `~/.local/bin/uv`），因为 systemd 用户服务的 PATH 不含 `~/.local/bin`。

### 7.6 进程间通信
| 通道 | 用途 | 为何 |
|---|---|---|
| unix socket | toggle ↔ daemon | 控制指令仅几十字节，socket 简单可靠；非共享内存 |
| stdin/stdout 管道 + JSON 行 | daemon ↔ sidecar | 父子进程天然管道，行分隔 JSON 易实现、串行化请求 |

### 7.7 麦克风源固定（Intel SOF 坑）
此机默认输入源是没插设备的耳机口麦克风（录到静音），真正的内置数字麦在 SOF 的 `_6_` 源上。**方案**：① 把系统默认源切到内置麦；② 在服务里用 `VOICETYPE_SOURCE` **按设备名钉死**内置麦，防止默认源被 WirePlumber 重置。

### 7.8 systemd 用户服务烤入会话环境
systemd `--user` 管理器默认**不含** `WAYLAND_DISPLAY` 等图形会话变量，导致 `wl-copy`/`notify-send` 连不上显示。**方案**：`install-service` 在图形终端运行时，把当时环境里的 `WAYLAND_DISPLAY`/`DISPLAY`/`XDG_CURRENT_DESKTOP`/`VOICETYPE_SOURCE` 等**写进 unit 文件**。

## 8. 配置（环境变量）

在 `~/.config/systemd/user/voicetype.service` 里用 `Environment=` 设置：

| 变量 | 当前值 | 默认 | 说明 |
|---|---|---|---|
| `VOICETYPE_MODEL` | `large-v3-turbo` | `large-v3` | 模型；turbo 多语言、快数倍、省显存；要更准用 `large-v3` |
| `VOICETYPE_DEVICE` | `cuda` | `cuda` | `cuda` 或 `cpu`（GPU 初始化失败自动回退 CPU/int8） |
| `VOICETYPE_COMPUTE` | `float16` | `float16` | `float16`/`int8_float16`/`int8`，越往后越省显存 |
| `VOICETYPE_LANG` | （空，自动） | 空 | 强制语言，如 `zh`、`en` |
| `VOICETYPE_SOURCE` | 内置麦设备名 | 空(系统默认源) | 钉死录音输入设备 |

## 9. 系统集成与安装产物

| 位置 | 作用 |
|---|---|
| `~/Projects/golang/voicetype/voicetype` | 二进制 |
| `~/.local/share/voicetype/sidecar/`（含 `.venv` ~2.5G） | 解压的边车 + uv 环境 |
| `~/.cache/huggingface/hub/models--*` | 下载的 Whisper 模型 |
| `~/.config/systemd/user/voicetype.service` | 常驻服务定义（含烤入的环境变量） |
| `/etc/udev/rules.d/99-uinput.rules` | 把 `/dev/uinput` 设为 `0666`，允许造虚拟键盘 |
| gsettings `.../custom-keybindings/voicetype/` | `<Alt>v → voicetype toggle` |
| `/run/user/1000/voicetype.sock` | toggle↔daemon 通信 |

**首装顺序**（详见 README）：`sudo apt install wl-clipboard` → 配 `/dev/uinput` 权限 → `voicetype setup` → `install-service` → `install-hotkey`。

## 10. 运维与排错

```bash
voicetype doctor                      # 一键体检：依赖/GPU/麦克风/服务
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
5. 终端里不自动粘贴：终端粘贴是 Ctrl+Shift+V 而非 Ctrl+V → 文字已在剪贴板，终端里手动 Ctrl+Shift+V 兜底。

## 11. 已知限制与未来方向

- **终端粘贴**：注入的 Ctrl+V 在终端无效（终端用 Ctrl+Shift+V）。当前靠"结果留剪贴板，手动粘"兜底。彻底解决需识别前台窗口类型（Wayland 下困难）或同时写主选区(primary)。
- **流式转写**：现为"录完整段再转"。流式（边说边出字）感知延迟更低，但 faster-whisper 不原生支持、临时结果会跳改、复杂度高。当前用 `large-v3-turbo` 降低单次延迟作为折中。
- **对外服务**：sidecar 目前只服务父 daemon。若要给其它程序用，可给 daemon 的 socket 加 `transcribe <文件>` 指令，或开 HTTP `POST /transcribe`，使其成为本机转写服务。
- **跨桌面**：全局键依赖 GNOME gsettings；KDE/wlroots 等需各自适配。
```

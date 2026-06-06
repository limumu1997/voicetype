# voicetype

全局语音输入转文字，面向 **GNOME Wayland**（Ubuntu 24.04 验证）。

按一下热键开始录音，再按一下结束 → 本地 GPU 转写（faster-whisper 或 Qwen3-ASR）→ 自动粘贴到当前光标处。中文/英文自动识别。

## 架构

```
[GNOME 热键] → voicetype toggle ─unix socket─→ voicetype daemon
                                                  ├─ 空闲   → pw-record 开始录音
                                                  └─ 录音中 → 停止 → 边车转写(whisper/qwen)
                                                              → wl-copy 入剪贴板
                                                              → uinput 注入 Ctrl+V 粘贴
```

- **一个 Go 二进制**：守护进程 + 客户端 + 安装命令。Python 边车通过 `go:embed` 打包进二进制，首次 `setup` 时自解压并用 **uv** 建独立环境。支持两个转写后端 faster-whisper / Qwen3-ASR(各一个 venv,见「配置」)。
- **常驻模型**：守护进程启动时把转写模型常驻显存，停止录音后秒出结果。可选后端 faster-whisper / Qwen3-ASR（各有独立 venv），见「配置」。
- **Wayland 文字注入**：GNOME 不支持 `wtype` 的虚拟键盘协议，Ubuntu 仓库的 `ydotool` 又太旧；因此本工具用**纯 Go 直接驱动 `/dev/uinput`** 注入 `Ctrl+V`，无外部注入工具。CJK 无法用按键模拟，故走"剪贴板 + 粘贴"。

## 依赖

运行期只需要一个系统包（其余 PipeWire / libnotify / uv 系统已自带）：

```bash
sudo apt install wl-clipboard
```

GPU 转写需要 NVIDIA 驱动（已装）。CUDA 运行库由 uv 通过 pip 的 `nvidia-cublas-cu12` / `nvidia-cudnn-cu12` 提供，无需系统级 CUDA。

## 安装

```bash
# 0. 编译
go build -o voicetype ./cmd

# 1. 系统包
sudo apt install wl-clipboard

# 2. 开放 /dev/uinput（一次性，立即生效，无需重新登录）
echo 'KERNEL=="uinput", MODE="0666", OPTIONS+="static_node=uinput"' \
  | sudo tee /etc/udev/rules.d/99-uinput.rules
sudo udevadm control --reload-rules && sudo udevadm trigger
sudo chmod 0666 /dev/uinput   # 对已加载的节点立即放权
#   说明: 0666 = 任意本地进程可注入输入(单用户笔记本可接受)。
#   想更严格可改用 MODE="0660" GROUP="input" + `sudo usermod -aG input $USER`,
#   但加组需【注销重新登录】才对 systemd --user 服务生效。

# 3. 构建 Python 边车环境（首次较慢；建两个 venv：faster-whisper + CUDA 库、qwen-asr + torch）
#    同时会生成默认 ~/.config/voicetype/config.toml
./voicetype setup

# 4. 安装并启动守护进程（systemd --user）
./voicetype install-service

# 5. 绑定全局热键（Super+D 会触发 GNOME 显示桌面，故用 Alt+V）
./voicetype install-hotkey '<Alt>v'

# 检查一切就绪
./voicetype doctor
```

> 模型权重在**第一次转写**时下载（whisper large-v3 约 3GB；qwen 0.6B 约几百 MB），首次会慢一点。

## 使用

把二进制装到 PATH（可选）后，任何窗口里：

1. 按 **Alt+V** → 通知"🎤 录音中"
2. 说话
3. 再按 **Alt+V** → 通知"⏳ 转写中" → 文字自动出现在光标处

## 配置

配置在 `~/.config/voicetype/config.toml`（`voicetype setup` 会生成带注释的默认文件）。改完执行 `systemctl --user restart voicetype` 生效。

```toml
backend = "whisper"  # whisper | qwen
# model 必须与 backend 匹配:
#   whisper -> large-v3 / large-v3-turbo / medium / small
#   qwen    -> Qwen/Qwen3-ASR-0.6B / Qwen/Qwen3-ASR-1.7B
model = "large-v3"
device = "cuda"      # cuda | cpu
compute = "float16"  # float16 | int8_float16 | int8 (仅 whisper)
lang = ""            # "" = 自动; zh / en ...
source = ""          # 录音输入设备名; "" = 系统默认 (pw-record --target)
```

| 项 | 默认 | 说明 |
|---|---|---|
| `backend` | `whisper` | 转写引擎：`whisper`（faster-whisper）或 `qwen`（Qwen3-ASR，开源权重）。两个后端各有独立 venv |
| `model` | 随 backend | **必须与 backend 匹配**。whisper 可改 `medium`/`small` 省显存；qwen 可换 `1.7B` |
| `device` | `cuda` | `cuda` 或 `cpu` |
| `compute` | `float16` | `float16`/`int8_float16`/`int8`（仅 whisper） |
| `lang` | 空(自动) | 强制语言，如 `zh`、`en` |
| `source` | 空(默认) | 钉死录音输入设备（Intel SOF 等默认源不对时用） |

> 同名环境变量 `VOICETYPE_*` 若设置会**覆盖**配置文件（方便临时实验，如 `VOICETYPE_MODEL=medium voicetype daemon`）。
>
> 6GB 显存：whisper `large-v3` float16 够用，OOM 就换 `int8_float16` 或 `medium`；qwen `0.6B` 约 2GB、`1.7B` 约 3.5GB+。

## 常用命令

```bash
voicetype doctor                       # 依赖/状态自检
journalctl --user -u voicetype -f      # 看守护进程日志
systemctl --user restart voicetype     # 改配置后重启
```

## 子命令

| 命令 | 作用 |
|---|---|
| `voicetype setup` | 解压边车 + `uv sync` |
| `voicetype install-service` | 写并启用 systemd --user 服务 |
| `voicetype install-hotkey [键]` | 注册 GNOME 自定义快捷键 |
| `voicetype uninstall` | 停用并移除 systemd 服务与 GNOME 快捷键 |
| `voicetype daemon` | 前台跑守护进程 |
| `voicetype toggle` | 开始/停止（绑到热键的就是它） |
| `voicetype doctor` | 自检 |

> 剪贴板：转写结果会留在剪贴板里（**不会**恢复你原有的剪贴板——早期版本恢复时与目标程序读取存在竞态，会偶发粘错，故移除）。好处是若注入的 Ctrl+V 在某些程序没生效（如终端要 Ctrl+Shift+V），可手动粘贴兜底。

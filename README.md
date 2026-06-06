# voicetype

全局语音输入转文字，面向 **GNOME Wayland**（Ubuntu 24.04 验证）。

按一下热键开始录音，再按一下结束 → 本地 faster-whisper（GPU）转写 → 自动粘贴到当前光标处。中文/英文自动识别。

## 架构

```
[GNOME 热键] → voicetype toggle ─unix socket─→ voicetype daemon
                                                  ├─ 空闲   → pw-record 开始录音
                                                  └─ 录音中 → 停止 → faster-whisper 转写
                                                              → wl-copy 入剪贴板
                                                              → uinput 注入 Ctrl+V 粘贴
```

- **一个 Go 二进制**：守护进程 + 客户端 + 安装命令。Python 边车（faster-whisper）通过 `go:embed` 打包进二进制，首次 `setup` 时自解压并用 **uv** 建独立环境。
- **常驻模型**：守护进程启动时把 Whisper 模型常驻显存，停止录音后秒出结果。
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
go build -o voicetype ./cmd/voicetype

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

# 3. 构建 Python 边车环境（首次较慢，下载 faster-whisper + CUDA 库）
./voicetype setup

# 4. 安装并启动守护进程（systemd --user）
./voicetype install-service

# 5. 绑定全局热键（Super+D 会触发 GNOME 显示桌面，故用 Alt+V）
./voicetype install-hotkey '<Alt>v'

# 检查一切就绪
./voicetype doctor
```

> Whisper 模型本身在**第一次转写**时下载（large-v3 约 3GB），首次会慢一点。

## 使用

把二进制装到 PATH（可选）后，任何窗口里：

1. 按 **Alt+V** → 通知"🎤 录音中"
2. 说话
3. 再按 **Alt+V** → 通知"⏳ 转写中" → 文字自动出现在光标处

## 配置（环境变量）

守护进程读取以下变量（在 `~/.config/systemd/user/voicetype.service` 里加 `Environment=` 即可）：

| 变量 | 默认 | 说明 |
|---|---|---|
| `VOICETYPE_MODEL` | `large-v3` | Whisper 模型，可改 `medium`/`small` 省显存 |
| `VOICETYPE_DEVICE` | `cuda` | `cuda` 或 `cpu` |
| `VOICETYPE_COMPUTE` | `float16` | `float16`/`int8_float16`/`int8` |
| `VOICETYPE_LANG` | 空(自动) | 强制语言，如 `zh`、`en` |

6GB 显存跑 `large-v3` float16 够用；若 OOM 改 `VOICETYPE_COMPUTE=int8_float16` 或换 `medium`。

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

> 剪贴板：粘贴前会自动备份你原有的剪贴板文本，粘贴完成后恢复（图片等非文本内容无法保留）。

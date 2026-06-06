#!/usr/bin/env python3
"""faster-whisper 常驻转写边车。

由 Go 守护进程拉起，模型常驻显存。通过 stdin/stdout 的行分隔 JSON 通信：

  Go -> py : {"wav": "/path/a.wav", "language": "auto"}\n
  py -> Go : {"text": "..."}\n            # 成功
             {"error": "..."}\n           # 失败

启动就绪后先打印一行 {"ready": true}。

配置来自环境变量（由 Go 设置，均有默认值）：
  VOICETYPE_MODEL   默认 large-v3
  VOICETYPE_DEVICE  默认 cuda
  VOICETYPE_COMPUTE 默认 float16
  VOICETYPE_LANG    默认空（自动检测）
"""

import json
import os
import sys


def _ensure_cuda_libs():
    """faster-whisper(CTranslate2) 需要能找到 pip 安装的 cuBLAS/cuDNN。
    把它们的目录加进 LD_LIBRARY_PATH 后 re-exec 自身一次。"""
    if os.environ.get("VOICETYPE_LDPATH_SET") == "1":
        return
    dirs = []
    try:
        import nvidia.cublas.lib as _cublas
        import nvidia.cudnn.lib as _cudnn
        dirs = [os.path.dirname(_cublas.__file__), os.path.dirname(_cudnn.__file__)]
    except Exception:
        # 没装 nvidia wheel（纯 CPU 场景），直接继续
        os.environ["VOICETYPE_LDPATH_SET"] = "1"
        return
    old = os.environ.get("LD_LIBRARY_PATH", "")
    os.environ["LD_LIBRARY_PATH"] = os.pathsep.join(dirs + ([old] if old else []))
    os.environ["VOICETYPE_LDPATH_SET"] = "1"
    os.execv(sys.executable, [sys.executable] + sys.argv)


def _send(obj):
    sys.stdout.write(json.dumps(obj, ensure_ascii=False) + "\n")
    sys.stdout.flush()


def _log(*a):
    print(*a, file=sys.stderr, flush=True)


def main():
    _ensure_cuda_libs()

    model_name = os.environ.get("VOICETYPE_MODEL", "large-v3")
    device = os.environ.get("VOICETYPE_DEVICE", "cuda")
    compute = os.environ.get("VOICETYPE_COMPUTE", "float16")
    default_lang = os.environ.get("VOICETYPE_LANG", "").strip()

    from faster_whisper import WhisperModel

    _log(f"[sidecar] loading model={model_name} device={device} compute={compute}")
    try:
        model = WhisperModel(model_name, device=device, compute_type=compute)
    except Exception as e:
        # GPU 初始化失败时回退到 CPU，保证可用
        _log(f"[sidecar] {device}/{compute} init failed ({e}); fallback to cpu/int8")
        model = WhisperModel(model_name, device="cpu", compute_type="int8")

    _send({"ready": True})
    _log("[sidecar] ready")

    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            req = json.loads(line)
            wav = req["wav"]
            lang = req.get("language", default_lang) or None
            if lang in ("auto", ""):
                lang = None
            segments, _info = model.transcribe(
                wav,
                language=lang,
                vad_filter=True,
                beam_size=5,
            )
            text = "".join(s.text for s in segments).strip()
            _send({"text": text})
        except Exception as e:
            _send({"error": str(e)})


if __name__ == "__main__":
    main()

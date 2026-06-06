#!/usr/bin/env python3
"""常驻转写边车（可切换后端）。

由 Go 守护进程拉起，模型常驻显存。通过 stdin/stdout 的行分隔 JSON 通信：

  Go -> py : {"wav": "/path/a.wav", "language": "auto"}\n
  py -> Go : {"text": "..."}\n            # 成功
             {"error": "..."}\n           # 失败

启动就绪后先打印一行 {"ready": true}。

配置来自环境变量（由 Go 设置，均有默认值）：
  VOICETYPE_BACKEND  默认 whisper；可选 qwen（Qwen3-ASR 开源权重）
  VOICETYPE_MODEL    当前后端的模型；whisper 用 large-v3 等，qwen 用 Qwen/Qwen3-ASR-0.6B 等
  VOICETYPE_DEVICE   默认 cuda
  VOICETYPE_COMPUTE  默认 float16（仅 whisper 用）
  VOICETYPE_LANG     默认空（自动检测）
"""

import json
import os
import sys


def _ensure_cuda_libs():
    """faster-whisper(CTranslate2) 需要能找到 pip 安装的 cuBLAS/cuDNN。
    把它们的目录加进 LD_LIBRARY_PATH 后 re-exec 自身一次。
    （仅 whisper 后端需要；torch 自带 CUDA，不走这里。）"""
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


def make_whisper_backend(model_name, device, compute):
    """返回 transcribe(wav, lang) -> text。"""
    from faster_whisper import WhisperModel

    _log(f"[sidecar] whisper: loading model={model_name} device={device} compute={compute}")
    try:
        model = WhisperModel(model_name, device=device, compute_type=compute)
    except Exception as e:
        # GPU 初始化失败时回退到 CPU，保证可用
        _log(f"[sidecar] {device}/{compute} init failed ({e}); fallback to cpu/int8")
        model = WhisperModel(model_name, device="cpu", compute_type="int8")

    def transcribe(wav, lang):
        if lang in ("auto", ""):
            lang = None
        segments, _info = model.transcribe(
            wav,
            language=lang,
            vad_filter=True,
            beam_size=5,
        )
        return "".join(s.text for s in segments).strip()

    return transcribe


# VOICETYPE_LANG 用的是 whisper 风格的语言码；Qwen 要的是语言全名。
_QWEN_LANG = {"zh": "Chinese", "en": "English"}


def make_qwen_backend(model_name, device):
    """返回 transcribe(wav, lang) -> text。model_name 来自 VOICETYPE_QWEN_MODEL。"""
    import torch
    from qwen_asr import Qwen3ASRModel

    device_map = {"cuda": "cuda:0", "cpu": "cpu"}.get(device, device)

    _log(f"[sidecar] qwen: loading model={model_name} device={device_map}")
    try:
        model = Qwen3ASRModel.from_pretrained(
            model_name,
            dtype=torch.bfloat16,
            device_map=device_map,
            max_new_tokens=256,
        )
    except Exception as e:
        _log(f"[sidecar] {device_map} init failed ({e}); fallback to cpu/float32")
        model = Qwen3ASRModel.from_pretrained(
            model_name,
            dtype=torch.float32,
            device_map="cpu",
            max_new_tokens=256,
        )

    def transcribe(wav, lang):
        lang = (lang or "").strip()
        if lang in ("", "auto"):
            qlang = None
        else:
            qlang = _QWEN_LANG.get(lang.lower(), lang)
        results = model.transcribe(audio=wav, language=qlang)
        if not results:
            return ""
        return (results[0].text or "").strip()

    return transcribe


def main():
    backend = os.environ.get("VOICETYPE_BACKEND", "whisper").strip().lower()
    device = os.environ.get("VOICETYPE_DEVICE", "cuda")
    compute = os.environ.get("VOICETYPE_COMPUTE", "float16")
    default_lang = os.environ.get("VOICETYPE_LANG", "").strip()

    model_name = os.environ.get("VOICETYPE_MODEL", "").strip()
    if backend == "qwen":
        transcribe = make_qwen_backend(model_name or "Qwen/Qwen3-ASR-0.6B", device)
    else:
        _ensure_cuda_libs()
        transcribe = make_whisper_backend(model_name or "large-v3", device, compute)

    _send({"ready": True})
    _log(f"[sidecar] ready (backend={backend})")

    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            req = json.loads(line)
            wav = req["wav"]
            lang = req.get("language", default_lang) or ""
            text = transcribe(wav, lang)
            _send({"text": text})
        except Exception as e:
            _send({"error": str(e)})


if __name__ == "__main__":
    main()

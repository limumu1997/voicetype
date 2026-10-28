#!/usr/bin/env python3
"""常驻转写边车（可切换后端）。

由 Go 守护进程拉起，模型常驻显存。通过 stdin/stdout 的行分隔 JSON 通信：

  Go -> py : {"wav": "/path/a.wav", "language": "auto"}\n
  py -> Go : {"text": "..."}\n            # 成功
             {"error": "..."}\n           # 失败

启动就绪后先打印一行 {"ready": true}。

配置来自环境变量（由 Go 设置，均有默认值）：
  VOICETYPE_BACKEND   默认 whisper；可选 qwen（Qwen3-ASR）、funasr（Fun-ASR-Nano）
  VOICETYPE_MODEL     当前后端的模型名或本地目录
  VOICETYPE_DEVICE    默认 cuda
  VOICETYPE_COMPUTE   默认 float16（仅 whisper 用）
  VOICETYPE_LANG      默认空（自动检测）
  VOICETYPE_HOTWORDS  热词文件路径（每行一个，# 注释）；每次转写前按 mtime 重读，改完即生效
"""

import json
import os
import sys

# 协议通道：main() 里指向原 stdout 的副本，而 fd 1 被重定向到 stderr，
# 这样模型库的任何 print 都进日志，不会混进协议流把 Go 端读错位。
_proto = sys.stdout


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
    _proto.write(json.dumps(obj, ensure_ascii=False) + "\n")
    _proto.flush()


def _log(*a):
    print(*a, file=sys.stderr, flush=True)


_hotwords_cache = {"mtime": None, "words": []}


def _hotwords():
    """读热词文件，按 mtime 缓存；文件缺失或读失败返回空列表。"""
    path = os.environ.get("VOICETYPE_HOTWORDS", "")
    try:
        mtime = os.path.getmtime(path)
    except OSError:
        return []
    if mtime != _hotwords_cache["mtime"]:
        words = []
        with open(path, encoding="utf-8") as f:
            for line in f:
                w = line.split("#", 1)[0].strip()
                if w:
                    words.append(w)
        _hotwords_cache.update(mtime=mtime, words=words)
        _log(f"[sidecar] loaded {len(words)} hotwords from {path}")
    return _hotwords_cache["words"]


def make_whisper_backend(model_name, device, compute):
    """返回 transcribe(wav, lang, hotwords) -> text。"""
    from faster_whisper import WhisperModel

    _log(f"[sidecar] whisper: loading model={model_name} device={device} compute={compute}")
    try:
        model = WhisperModel(model_name, device=device, compute_type=compute)
    except Exception as e:
        # GPU 初始化失败时回退到 CPU，保证可用
        _log(f"[sidecar] {device}/{compute} init failed ({e}); fallback to cpu/int8")
        model = WhisperModel(model_name, device="cpu", compute_type="int8")

    def transcribe(wav, lang, hotwords):
        if lang in ("auto", ""):
            lang = None
        segments, _info = model.transcribe(
            wav,
            language=lang,
            vad_filter=True,
            beam_size=5,
            hotwords=" ".join(hotwords) or None,
        )
        return "".join(s.text for s in segments).strip()

    return transcribe


# VOICETYPE_LANG 用的是 whisper 风格的语言码；Qwen 要的是语言全名。
_QWEN_LANG = {"zh": "Chinese", "en": "English"}


def make_qwen_backend(model_name, device):
    """返回 transcribe(wav, lang, hotwords) -> text。"""
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

    def transcribe(wav, lang, hotwords):
        lang = (lang or "").strip()
        if lang in ("", "auto"):
            qlang = None
        else:
            qlang = _QWEN_LANG.get(lang.lower(), lang)
        # Qwen3-ASR 没有专门的热词参数，用 context 文本做上下文偏置。
        results = model.transcribe(audio=wav, context=" ".join(hotwords), language=qlang)
        if not results:
            return ""
        return (results[0].text or "").strip()

    return transcribe


# Fun-ASR-Nano 的 language 参数要中文名。
_FUNASR_LANG = {"zh": "中文", "en": "英文", "ja": "日文"}


def make_funasr_backend(model_name, device):
    """返回 transcribe(wav, lang, hotwords) -> text。"""
    from funasr import AutoModel

    dev = {"cuda": "cuda:0"}.get(device, device)

    _log(f"[sidecar] funasr: loading model={model_name} device={dev}")
    try:
        model = AutoModel(model=model_name, trust_remote_code=True, device=dev, disable_update=True)
    except Exception as e:
        _log(f"[sidecar] {dev} init failed ({e}); fallback to cpu")
        model = AutoModel(model=model_name, trust_remote_code=True, device="cpu", disable_update=True)

    def transcribe(wav, lang, hotwords):
        kw = {"itn": True, "disable_pbar": True}  # itn: 数字/日期规范化 + 标点
        lang = (lang or "").strip()
        if lang not in ("", "auto"):
            kw["language"] = _FUNASR_LANG.get(lang.lower(), lang)
        if hotwords:
            kw["hotwords"] = hotwords
        res = model.generate(input=[wav], cache={}, batch_size=1, **kw)
        if not res:
            return ""
        return (res[0].get("text") or "").strip()

    return transcribe


def main():
    global _proto
    backend = os.environ.get("VOICETYPE_BACKEND", "whisper").strip().lower()
    device = os.environ.get("VOICETYPE_DEVICE", "cuda")
    compute = os.environ.get("VOICETYPE_COMPUTE", "float16")
    default_lang = os.environ.get("VOICETYPE_LANG", "").strip()

    if backend not in ("qwen", "funasr"):
        _ensure_cuda_libs()  # 可能 re-exec，必须在重定向 stdout 之前

    _proto = os.fdopen(os.dup(1), "w", encoding="utf-8")
    os.dup2(2, 1)
    sys.stdout = sys.stderr

    model_name = os.environ.get("VOICETYPE_MODEL", "").strip()
    if backend == "qwen":
        transcribe = make_qwen_backend(model_name or "Qwen/Qwen3-ASR-0.6B", device)
    elif backend == "funasr":
        transcribe = make_funasr_backend(model_name or "FunAudioLLM/Fun-ASR-Nano-2512", device)
    else:
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
            text = transcribe(wav, lang, _hotwords())
            _send({"text": text})
        except Exception as e:
            _send({"error": str(e)})


if __name__ == "__main__":
    main()

#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
视觉能力（图输入）运行时探测工具
================================
复刻 deepseekharness 的「现成」判定逻辑：给模型附带一张图片（images 输入参数），
上游**拒识 / 报错**即判定「不能识图（non-vision）」，正常接收即「能识图（vision）」。
本网关的本地等价信号：

  * WB 域（经网关 /v1/chat/completions）：网关 hint 对 ``supports_images=false`` 的模型
    返回 ``11133 / MODEL_DOES_NOT_SUPPORT_IMAGES``（"does not support images"）。
    注意：本环境 WB 目录 ``supports_images`` 多为 true，故 WB 侧大多命中 200 → 判 vision；
    只有上游真拒时才落 non-vision（与 harness 面对同一批模型的行为一致）。
  * Trae 域（直连 trae.base_url /v1/chat/completions）：Trae 上游 /v1/models 无视觉字段，
    网关也不发 hint，故直连上游、看其业务信封里的图片不支持类错误来判定。

判定三态（不猜测）：
  - ``vision``     ：请求被正常接收（HTTP 2xx 且业务信封无图片不支持错误）。
  - ``non-vision`` ：响应明确拒绝图片（11133 / "does not support images" / "not support image"
                     / 上游 invalid_request_error 且文案指向图片不支持）。
  - ``unknown``    ：网络不可达 / 超时 / 其他非确定性错误——**不猜**，保留上一轮结果或留空。

用法：
  # 探测网关 WB 域 + Trae 上游（默认读同目录 config.json 取 trae 配置）
  python probe_vision.py --config config.json

  # 显式指定两端
  python probe_vision.py --wb-base http://127.0.0.1:7890/v1 --trae-base http://127.0.0.1:7862/v1 \
      --trae-key sk-xxx

  # 只测部分模型 / 只看计划
  python probe_vision.py --models cn:glm-5v-turbo,cn:deepseek-v4-flash
  python probe_vision.py --dry-run

  # 输出与节制
  python probe_vision.py --out data/vision_probes.json --interval 3

结果（面板契约，与 output_probes.json 同口径：网关零重启即生效）：
  {"version":1,"probes":{"cn:glm-5v-turbo":{"vision":true,"state":"vision","note":"...",
   "tested_at":"...","source":"probe_vision.py"}},"exists":true,"updated_at":"..."}

调度：每日 03:00（由网关 scheduler 或外部 cron 调用本脚本）+ 面板手动触发按钮（POST 后再跑一次）。
"""
import argparse
import base64
import json
import os
import re
import socket
import sys
import time
import urllib.error
import urllib.request
from datetime import datetime, timezone
from pathlib import Path

# 探测用图片：上游会拒绝「纯色/无效」合成图（code 11135 image_invalid）。
# 故运行时用 PIL 生成一张有真实内容的图（渐变+噪点，约 96x96），更贴近真实用户截图，
# 才能通过上游图像校验，从而让「模型是否支持图」这一真正信号浮现。
PROBE_PNG_B64 = None  # 延迟生成


def _build_probe_png_b64() -> str:
    global PROBE_PNG_B64
    if PROBE_PNG_B64:
        return PROBE_PNG_B64
    try:
        from io import BytesIO
        from PIL import Image
        import random
        import struct
        random.seed(20261012)
        w = h = 96
        img = Image.new("RGB", (w, h))
        px = img.load()
        for y in range(h):
            for x in range(w):
                # 渐变底 + 噪点，构成有真实像素内容的图
                r = int(120 + 100 * (x / w) + random.randint(-25, 25))
                g = int(80 + 120 * (y / h) + random.randint(-25, 25))
                b = int(200 - 80 * (x / w) + random.randint(-25, 25))
                px[x, y] = (max(0, min(255, r)), max(0, min(255, g)), max(0, min(255, b)))
        buf = BytesIO()
        img.save(buf, format="PNG")
        PROBE_PNG_B64 = base64.b64encode(buf.getvalue()).decode("ascii")
    except Exception:
        # 兜底：仍用一张较小的真实渐变图（若 PIL 不可用），但不依赖纯色合成图。
        PROBE_PNG_B64 = (
            "iVBORw0KGgoAAAANSUhEUgAAAEAAAABACAIAAAAlC+aJAAAAUElEQVR42u3PQQkAAAgEsOufxxCGMIwRfAu"
            "DFViq57UICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgICFwWtX6CLEb+bA0AAAAASU"
            "VORK5CYII="
        )
    return PROBE_PNG_B64

# 真实浏览器 UA + 常规头，避免被上游当成脚本流量（意图伪装成真用户）。
UA = (
    "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
    "(KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"
)
EXTRA_HEADERS = {
    "Accept": "application/json",
    "Accept-Language": "zh-CN,zh;q=0.9,en;q=0.8",
    "Origin": "http://127.0.0.1:7890",
    "Referer": "http://127.0.0.1:7890/",
}

# 拒识信号：命中任一即判 non-vision（不区分大小写）。
# 注意：上游对「图片本身无效」会返回 code 11135 / image_invalid / "start a new conversation"，
# 这与「模型不支持图」无关，不能当非视觉信号——落入 unknown。
NON_VISION_MARKERS = [
    "11133",
    "model_does_not_support_images",
    "does not support images",
    "doesn't support images",
    "not support image",
    "not support images",
    "image input is not supported",
    "images are not supported",
    "this model does not support",
    "model cannot process image",
    "model can't process image",
]

# 图片本身无效（非模型能力问题）→ 不猜，归 unknown。
IMAGE_INVALID_MARKERS = [
    "11135",
    "image_invalid",
    "image is invalid",
    "invalid image data",
    "start a new conversation",
    "replace the image",
    "image data rejected",
]


def now_iso() -> str:
    return datetime.now(timezone.utc).astimezone().isoformat(timespec="seconds")


def load_cfg(path: str) -> dict:
    try:
        with open(path, "r", encoding="utf-8") as f:
            return json.load(f)
    except Exception:
        return {}


def fetch_models(base: str, key: str, timeout: int) -> list:
    """取 /v1/models，返回 [id, ...]（只取 id，本探测不需要目录里的视觉字段）。"""
    url = base.rstrip("/") + "/models"
    hdrs = {"Accept": "application/json"}
    if key:
        hdrs["Authorization"] = f"Bearer {key}"
        hdrs["x-api-key"] = key
    req = urllib.request.Request(url, headers=hdrs)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            data = json.loads(r.read().decode("utf-8"))
    except Exception as e:
        print(f"  [warn] 取模型列表失败 {url}: {e}")
        return []
    items = data.get("data") if isinstance(data, dict) else data
    out = []
    for it in items or []:
        if isinstance(it, str):
            out.append(it.strip())
        elif isinstance(it, dict):
            mid = (it.get("id") or it.get("name") or "").strip()
            if mid:
                out.append(mid)
    return out


def chat_with_image(base: str, key: str, model: str, timeout: int, ua: str):
    """
    向 base/chat/completions 发一条带图文本请求。
    返回 (http_status, body_text, error_or_none)。
    error 仅在「明确网络/超时」时为字符串；HTTP 层与业务信封解析交回调用方判。
    """
    url = base.rstrip("/") + "/chat/completions"
    payload = {
        "model": model,
        "messages": [
            {
                "role": "user",
                "content": [
                    {"type": "image_url", "image_url": {"url": f"data:image/png;base64,{_build_probe_png_b64()}"}},
                    {"type": "text", "text": "hi"},
                ],
            }
        ],
        "max_tokens": 1,
        "stream": False,
    }
    data = json.dumps(payload).encode("utf-8")
    req = urllib.request.Request(url, data=data, method="POST")
    if key:  # 空 key 时不要发空鉴权头（网关会 400）
        req.add_header("Authorization", f"Bearer {key}")
        req.add_header("x-api-key", key)
    req.add_header("Content-Type", "application/json")
    req.add_header("User-Agent", ua)
    for k, v in EXTRA_HEADERS.items():
        req.add_header(k, v)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, r.read().decode("utf-8", "replace"), None
    except urllib.error.HTTPError as e:
        try:
            body = e.read().decode("utf-8", "replace")
        except Exception:
            body = ""
        return e.code, body, None
    except (urllib.error.URLError, socket.timeout, ConnectionError, TimeoutError) as e:
        return None, "", f"{type(e).__name__}: {e}"
    except Exception as e:  # 其它（含 timeout 的其它形态）
        return None, "", f"{type(e).__name__}: {e}"


def classify(status, body, transport_err) -> tuple:
    """
    三态判定。返回 (state, note)。
    state ∈ {vision, non-vision, unknown}
    """
    if transport_err:
        return "unknown", f"transport error: {transport_err}"
    text = (body or "").lower()
    # 图片本身无效（与模型能力无关）→ 不猜。
    if any(m in text for m in IMAGE_INVALID_MARKERS):
        return "unknown", f"图片本身无效/需新会话（非能力判定，不猜）"
    if status and (200 <= status < 300):
        # 即便 2xx，也要看业务信封是否带了图片不支持错误（网关可能透传上游错误体）。
        if any(m in text for m in NON_VISION_MARKERS):
            return "non-vision", f"业务信封拒识 (2xx, markers matched)"
        # 正常接收。
        return "vision", f"HTTP {status} 正常接收"
    # 非 2xx：看 body 是否有拒识信号。
    if status and any(m in text for m in NON_VISION_MARKERS):
        return "non-vision", f"HTTP {status} 拒识（图片不支持）"
    if status:
        return "unknown", f"HTTP {status} 但无明确拒识信号（不猜）"
    return "unknown", "无状态码且无拒识信号（不猜）"


def probe_endpoint(label: str, base: str, key: str, models: list, interval: int, timeout: int, only: set):
    results = {}
    print(f"\n=== {label}  ({base})  共 {len(models)} 个模型 ===")
    for m in models:
        if only and m not in only:
            continue
        st, body, terr = chat_with_image(base, key, m, timeout, UA)
        state, note = classify(st, body, terr)
        results[m] = {
            "vision": state == "vision",
            "state": state,
            "note": note,
            "tested_at": now_iso(),
            "source": "probe_vision.py",
        }
        print(f"  {m:32} -> {state:10} | {note}")
        if interval and interval > 0:
            time.sleep(interval)
    return results


def merge_previous(out_path: str, current: dict) -> dict:
    """合并：current 里 state==unknown 的模型保留上一轮结果（不覆盖、不猜）。"""
    if not out_path or not os.path.exists(out_path):
        return current
    try:
        with open(out_path, "r", encoding="utf-8") as f:
            prev = json.load(f)
    except Exception:
        return current
    prev_probes = prev.get("probes", {}) if isinstance(prev, dict) else {}
    for mid, rec in current.items():
        if rec.get("state") == "unknown" and mid in prev_probes:
            current[mid] = prev_probes[mid]  # 保留旧判定
    return current


def main():
    ap = argparse.ArgumentParser(description="视觉能力（图输入）运行时探测")
    ap.add_argument("--config", default="config.json", help="网关配置文件（读取 trae.base_url/trae.api_key；WB 用 --wb-* 覆盖）")
    ap.add_argument("--wb-base", default=None, help="WB 域网关 /v1 基址（默认 http://127.0.0.1:7890/v1）")
    ap.add_argument("--wb-key", default="", help="WB 域网关 api_key（无鉴权留空）")
    ap.add_argument("--trae-base", default=None, help="Trae 上游 /v1 基址（默认读 config trae.base_url+/v1）")
    ap.add_argument("--trae-key", default=None, help="Trae 上游 api_key（默认读 config trae.api_key）")
    ap.add_argument("--out", default="data/vision_probes.json", help="面板契约输出文件")
    ap.add_argument("--interval", type=float, default=3.0, help="每模型间隔秒（默认 3）")
    ap.add_argument("--timeout", type=int, default=60, help="单请求超时秒")
    ap.add_argument("--models", default="", help="逗号分隔只测这些（WB+Trae 通用 id；Trae 用裸名）")
    ap.add_argument("--wb-only", action="store_true", help="只测 WB 域")
    ap.add_argument("--trae-only", action="store_true", help="只测 Trae 域")
    ap.add_argument("--dry-run", action="store_true", help="只列计划不实际请求")
    args = ap.parse_args()

    cfg = load_cfg(args.config)
    wb_base = args.wb_base or os.environ.get("WB_BASE") or "http://127.0.0.1:7890/v1"
    trae_base = args.trae_base or os.environ.get("TRAE_BASE")
    if not trae_base:
        tb = (cfg.get("trae") or {}).get("base_url", "")
        trae_base = (tb.rstrip("/") + "/v1") if tb else None
    trae_key = args.trae_key if args.trae_key is not None else (cfg.get("trae") or {}).get("api_key", "")
    only = set(x.strip() for x in args.models.split(",") if x.strip())

    if args.dry_run:
        print(f"[dry-run] WB base={wb_base}")
        print(f"[dry-run] Trae base={trae_base} key_set={bool(trae_key)}")
        if only:
            print(f"[dry-run] 限定模型: {', '.join(sorted(only))}")
        return

    all_probes = {}
    if not args.trae_only:
        wb_models = fetch_models(wb_base, args.wb_key, args.timeout)
        if only:
            wb_models = [m for m in wb_models if m in only]
        wb_res = probe_endpoint("WB 域（经网关）", wb_base, args.wb_key, wb_models, args.interval, args.timeout, only)
        all_probes.update(wb_res)

    if not args.wb_only and trae_base:
        trae_models = fetch_models(trae_base, trae_key, args.timeout)
        if only:
            trae_models = [m for m in trae_models if m in only]
        trae_res = probe_endpoint("Trae 域（直连上游）", trae_base, trae_key, trae_models, args.interval, args.timeout, only)
        all_probes.update(trae_res)

    # unknown 保留上一轮，避免误判。
    all_probes = merge_previous(args.out, all_probes)

    out = {
        "version": 1,
        "probes": all_probes,
        "exists": True,
        "updated_at": now_iso(),
        "source": "probe_vision.py",
    }
    out_path = args.out
    os.makedirs(os.path.dirname(os.path.abspath(out_path)), exist_ok=True)
    with open(out_path, "w", encoding="utf-8") as f:
        json.dump(out, f, ensure_ascii=False, indent=2)
    print(f"\n结果已写入 {out_path}（{len(all_probes)} 个模型，网关零重启即生效）")
    n_v = sum(1 for v in all_probes.values() if v.get("state") == "vision")
    n_nv = sum(1 for v in all_probes.values() if v.get("state") == "non-vision")
    n_u = sum(1 for v in all_probes.values() if v.get("state") == "unknown")
    print(f"  vision={n_v}  non-vision={n_nv}  unknown={n_u}")


if __name__ == "__main__":
    main()

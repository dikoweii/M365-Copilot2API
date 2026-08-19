#!/usr/bin/env python3
"""Probe M365 Copilot ChatHub image generation: dump raw events for analysis.

Sends an image-generation prompt using the same wire format as the Go gateway
(see internal/chathub/client.go) and dumps every raw SignalR frame to a file
so the image result structure can be reverse-engineered.
"""

from __future__ import annotations

import argparse
import asyncio
import json
import sys
import uuid
import urllib.parse
from pathlib import Path

import websockets

sys.path.insert(0, str(Path(__file__).parent))
from chathub_probe import load_token, build_ws_url  # noqa: E402

RS = "\x1e"

OPTIONS_SETS = [
    "search_result_progress_messages_with_search_queries",
    "update_textdoc_response_after_streaming",
    "deepleo_networking_timeout_10minutes_canmore",
    "cwc_flux_image",
    "cwc_code_interpreter",
    "cwc_code_interpreter_amsfix",
    "cwcfluxgptv",
    "flux_v3_gptv_enable_upload_multi_image_in_turn_wo_ch",
    "gptvnorm2048",
    "cwc_code_interpreter_citation_fix",
    "code_interpreter_interactive_charts_inline_image",
    "code_interpreter_matplotlib_patching",
    "code_interpreter_interactive_charts",
    "cwc_fileupload_odb",
    "update_memory_plugin",
    "add_custom_instructions",
    "cwc_flux_v3",
    "flux_v3_progress_messages",
    "enable_batch_token_processing",
    "enable_gg_gpt",
]


def gen_payload(text: str, session_id: str, conversation_id: str, request_id: str) -> str:
    chat = {
        "arguments": [
            {
                "source": "officeweb",
                "clientCorrelationId": str(uuid.uuid4()),
                "sessionId": session_id,
                "optionsSets": OPTIONS_SETS,
                "options": {},
                "allowedMessageTypes": [
                    "Chat",
                    "Suggestion",
                    "Disengaged",
                    "Progress",
                    "EndOfRequest",
                    "InternalLoaderMessage",
                ],
                "sliceIds": [],
                "threadLevelGptId": {},
                "conversationId": conversation_id,
                "traceId": str(uuid.uuid4()),
                "isStartOfSession": True,
                "productThreadType": "Office",
                "clientInfo": {
                    "clientPlatform": "mcmcopilot-web",
                    "clientAppName": "Office",
                },
                "tone": "magic",
                "streamingMode": "ConciseWithPadding",
                "message": {
                    "author": "user",
                    "inputMethod": "Keyboard",
                    "text": text,
                    "requestId": request_id,
                    "locationInfo": {
                        "timeZoneOffset": 8,
                        "timeZone": "Asia/Shanghai",
                    },
                    "locale": "en-US",
                    "messageType": "Chat",
                    "experienceType": "Default",
                },
                "plugins": [{"Id": "BingWebSearch", "Source": "BuiltIn"}],
            }
        ],
        "invocationId": "0",
        "target": "chat",
        "type": 4,
    }
    metrics = {
        "arguments": [
            {
                "Timestamps": {
                    "ConnectionStart": "",
                    "UserInputStart": "",
                    "ConnectionEstablished": "",
                    "UserInputSubmit": "",
                }
            }
        ],
        "target": "Metrics",
        "type": 1,
    }
    return (
        json.dumps(chat, separators=(",", ":"))
        + RS
        + json.dumps(metrics, separators=(",", ":"))
        + RS
    )


def walk(v, out, prefix="", depth=0):
    """Collect string values whose key hints at images/urls, plus event shapes."""
    if depth > 8:
        return
    if isinstance(v, dict):
        for k, e in v.items():
            lk = str(k).lower()
            if isinstance(e, str) and (
                "url" in lk or "image" in lk or lk == "src" or lk == "value"
            ):
                out.append((prefix + k, e))
            else:
                walk(e, out, prefix + k + ".", depth + 1)
    elif isinstance(v, list):
        for i, e in enumerate(v):
            walk(e, out, f"{prefix}[{i}].", depth + 1)


async def run(token_file: str, text: str, dump: Path) -> str:
    acc = load_token(token_file)
    session_id = str(uuid.uuid4())
    conversation_id = str(uuid.uuid4())
    request_id = str(uuid.uuid4())
    ws_url = build_ws_url(acc, session_id, conversation_id, request_id)
    headers = {
        "Origin": "https://m365.cloud.microsoft",
        "User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/132.0.0.0 Safari/537.36 Edg/132.0.0.0",
    }
    frames = []
    urls = []
    final = ""

    async with websockets.connect(
        ws_url, additional_headers=headers, max_size=64 * 1024 * 1024
    ) as ws:
        await ws.send(json.dumps({"protocol": "json", "version": 1}) + RS)
        hs = await ws.recv()
        print("handshake:", (hs if isinstance(hs, str) else hs.decode("utf-8", "replace"))[:80])
        await ws.send(gen_payload(text, session_id, conversation_id, request_id))
        print("sent gen payload")

        while True:
            try:
                raw = await asyncio.wait_for(ws.recv(), timeout=150)
            except asyncio.TimeoutError:
                print("timeout waiting for more frames")
                break
            frames.append(raw)
            with dump.open("a", encoding="utf-8") as f:
                f.write(raw + "\n\x1e\n")
            for part in raw.split(RS):
                if not part.strip():
                    continue
                try:
                    obj = json.loads(part)
                except json.JSONDecodeError:
                    print("raw:", part[:200])
                    continue
                t = obj.get("type")
                if t == 6:
                    await ws.send(json.dumps({"type": 6}) + RS)
                    continue
                if t == 1 and obj.get("target") == "update":
                    for arg in obj.get("arguments") or []:
                        if not isinstance(arg, dict):
                            continue
                        if "writeAtCursor" in arg:
                            print("delta:", arg["writeAtCursor"][:100].replace("\n", " "))
                        for m in arg.get("messages") or []:
                            if isinstance(m, dict):
                                walk(m, urls)
                        walk(arg, urls)
                elif t == 2:
                    walk(obj, urls)
                    item = obj.get("item") or {}
                    final = str((item.get("result") or {}).get("message") or "")
                    print("== result ==")
                    print(final[:500])
                    break
                elif t == 3 and obj.get("error"):
                    print("completion error:", json.dumps(obj.get("error"), ensure_ascii=False)[:500])
                    break

    dump.write_text("\n\x1e\n".join(frames), encoding="utf-8")
    print(f"\n== dumped {len(frames)} frames to {dump} ==")
    print("== image/url-like values found ==")
    seen = set()
    for k, v in urls:
        if v not in seen and len(v) < 500:
            seen.add(v)
            print(f"  {k}: {v[:200]}")
    return final


def main() -> None:
    parser = argparse.ArgumentParser(description="Probe M365 image generation")
    parser.add_argument("--token-file", default="~/.config/m365-copilot2api/accounts.json")
    parser.add_argument("--text", default="Create an image of a red apple on a white background")
    parser.add_argument("--dump", default="genprobe-dump.txt")
    args = parser.parse_args()
    answer = asyncio.run(run(args.token_file, args.text, Path(args.dump)))
    print("ANSWER:", answer[:300])


if __name__ == "__main__":
    main()

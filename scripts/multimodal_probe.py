#!/usr/bin/env python3
"""Probe M365 Copilot ChatHub multimodal image input.

Uploads a local image via substrate UploadFile (same flow as the Go gateway's
uploadAttachments), then sends a chat message carrying the returned docId as
an ImageFile messageAnnotation, and dumps the raw frames for analysis.
"""

from __future__ import annotations

import argparse
import asyncio
import base64
import json
import mimetypes
import sys
import uuid
import urllib.parse
from pathlib import Path

import urllib.request
import http.client

import websockets

sys.path.insert(0, str(Path(__file__).parent))
from chathub_probe import load_token, build_ws_url, RS  # noqa: E402

UPLOAD_URL = "https://substrate.office.com/m365Copilot/UploadFile"

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


def upload_image(acc: dict, path: str, conversation_id: str) -> dict:
    mime = mimetypes.guess_type(path)[0] or "image/png"
    bytes_data = Path(path).read_bytes()
    data_url = f"data:{mime};base64," + base64.b64encode(bytes_data).decode()

    form = {
        "scenario": "UploadImage",
        "conversationId": conversation_id,
        "FileBase64": data_url,
        "optionsSets": [
            "cwcgptvsan",
            "flux_v3_gptv_enable_upload_multi_image_in_turn_wo_ch",
        ],
    }
    body = urllib.parse.urlencode(form, doseq=True).encode()

    req = urllib.request.Request(UPLOAD_URL, data=body, method="POST")
    req.add_header("Content-Type", "application/x-www-form-urlencoded")
    req.add_header("Accept", "application/json")
    req.add_header("Authorization", "Bearer " + acc["accessToken"])
    req.add_header("X-Variants", "feature.EnableImageSupportInUploadFile")
    req.add_header("X-Scenario", "OfficeWebIncludedCopilot")
    req.add_header("Origin", "https://m365.cloud.microsoft")
    req.add_header("Referer", "https://m365.cloud.microsoft/")
    req.add_header(
        "User-Agent",
        "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:148.0) Gecko/20100101 Firefox/148.0",
    )
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            raw = resp.read().decode()
            print(f"[upload] status={resp.status} body={raw[:500]}")
            return json.loads(raw)
    except urllib.error.HTTPError as e:
        print(f"[upload] HTTP {e.code}: {e.read().decode()[:500]}")
        raise


def multimodal_payload(
    text: str,
    session_id: str,
    conversation_id: str,
    request_id: str,
    doc_id: str,
    file_type: str,
    file_name: str,
) -> str:
    message = {
        "author": "user",
        "inputMethod": "Keyboard",
        "text": text,
        "requestId": request_id,
        "locationInfo": {"timeZoneOffset": 8, "timeZone": "Asia/Shanghai"},
        "locale": "en-US",
        "messageType": "Chat",
        "experienceType": "Default",
        "messageAnnotations": [
            {
                "id": doc_id,
                "messageAnnotationMetadata": {
                    "@type": "File",
                    "annotationType": "File",
                    "fileType": file_type,
                    "fileName": file_name,
                },
                "messageAnnotationType": "ImageFile",
            }
        ],
        "connectedFederatedConnections": ["dummyId"],
    }
    chat = {
        "arguments": [
            {
                "source": "officeweb",
                "clientCorrelationId": str(uuid.uuid4()),
                "sessionId": session_id,
                "optionsSets": OPTIONS_SETS,
                "options": {},
                "allowedMessageTypes": [
                    "Chat", "Suggestion", "Disengaged", "Progress", "EndOfRequest", "InternalLoaderMessage",
                ],
                "sliceIds": [],
                "threadLevelGptId": {},
                "conversationId": conversation_id,
                "traceId": str(uuid.uuid4()),
                "isStartOfSession": True,
                "productThreadType": "Office",
                "clientInfo": {"clientPlatform": "mcmcopilot-web", "clientAppName": "Office"},
                "tone": "magic",
                "streamingMode": "ConciseWithPadding",
                "message": message,
                "plugins": [{"Id": "BingWebSearch", "Source": "BuiltIn"}],
            }
        ],
        "invocationId": "0",
        "target": "chat",
        "type": 4,
    }
    metrics = {
        "arguments": [{"Timestamps": {"ConnectionStart": "", "UserInputStart": "", "ConnectionEstablished": "", "UserInputSubmit": ""}}],
        "target": "Metrics",
        "type": 1,
    }
    return (
        json.dumps(chat, separators=(",", ":"))
        + RS
        + json.dumps(metrics, separators=(",", ":"))
        + RS
    )


async def run(token_file: str, image: str, text: str, dump: Path) -> str:
    acc = load_token(token_file)
    session_id = str(uuid.uuid4())
    conversation_id = str(uuid.uuid4())
    request_id = str(uuid.uuid4())

    print(f"uploading {image} ...")
    up = upload_image(acc, image, conversation_id)
    doc_id = up.get("docId") or (up.get("result") or {}).get("value")
    print("docId:", doc_id)

    ws_url = build_ws_url(acc, session_id, conversation_id, request_id)
    headers = {
        "Origin": "https://m365.cloud.microsoft",
        "User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:148.0) Gecko/20100101 Firefox/148.0",
    }
    frames = []
    final = ""

    async with websockets.connect(
        ws_url, additional_headers=headers, max_size=64 * 1024 * 1024
    ) as ws:
        await ws.send(json.dumps({"protocol": "json", "version": 1}) + RS)
        hs = await ws.recv()
        print("handshake:", (hs if isinstance(hs, str) else hs.decode("utf-8", "replace"))[:80])

        file_type = "png"
        name = Path(image).name
        await ws.send(multimodal_payload(text, session_id, conversation_id, request_id, doc_id, file_type, name))
        print("sent multimodal chat+metrics")

        while True:
            try:
                raw = await asyncio.wait_for(ws.recv(), timeout=90)
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
                            print("delta:", arg["writeAtCursor"][:150].replace("\n", " "))
                        for m in arg.get("messages") or []:
                            if isinstance(m, dict) and m.get("text") and m.get("author") == "bot":
                                print("bot msg:", m["text"][:150].replace("\n", " "))
                elif t == 2:
                    item = obj.get("item") or {}
                    final = str((item.get("result") or {}).get("message") or "")
                    print("== result ==")
                    print(final[:400])
                    break
                elif t == 3:
                    print("completion frame")
                    break

    print(f"\n== dumped {len(frames)} frames to {dump} ==")
    return final


def main() -> None:
    parser = argparse.ArgumentParser(description="Probe M365 multimodal image input")
    parser.add_argument("--token-file", default="~/.config/m365-copilot2api/accounts.json")
    parser.add_argument("--image", default="example-image.png")
    parser.add_argument("--text", default="What color is this image? Answer in one word.")
    parser.add_argument("--dump", default="multimodalprobe-dump.txt")
    args = parser.parse_args()
    answer = asyncio.run(run(args.token_file, args.image, args.text, Path(args.dump)))
    print("ANSWER:", answer[:400])


if __name__ == "__main__":
    main()
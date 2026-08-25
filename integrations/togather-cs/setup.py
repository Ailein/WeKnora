#!/usr/bin/env python3
"""One-shot setup for the Togather Cafe customer-service agent in WeKnora.

Migrates the openclaw togather-cs workspace into WeKnora:

  1. Create the knowledge base (markdown parsed locally, category-sized chunks).
  2. Split MENU/MENU_CN by category and STORES by branch, upload every
     knowledge document (skips files already present — safe to re-run).
  3. Register the togather-distance stdio MCP service and test it.
  4. Create (or update) the "Togather 客服小灵" custom agent wired to the KB
     and the MCP service, with the rewritten system prompt.

Zero third-party dependencies. Usage:

    python3 setup.py --api-key sk-... \
        [--base-url http://localhost:8080] \
        [--workspace ~/.openclaw/workspace-togather-cs]
"""

from __future__ import annotations

import argparse
import json
import re
import sys
import time
import urllib.error
import urllib.request
import uuid
from pathlib import Path

HERE = Path(__file__).parent

KB_NAME = "Togather 客服知识库"
MCP_SERVICE_NAME = "togather-distance"
AGENT_NAME = "Togather 客服小灵"

# WeKnora disables stdio MCP for security, so the distance server runs as its
# own compose container (service `togather-distance`, Streamable HTTP). The
# hostname must be in the app's SSRF_WHITELIST.
MCP_SERVER_URL = "http://togather-distance:9310/mcp"

ALLOWED_TOOLS = [
    "thinking",
    "knowledge_search",
    "grep_chunks",
    "list_knowledge_chunks",
    "get_document_info",
]


class Api:
    def __init__(self, base_url: str, api_key: str):
        self.base = base_url.rstrip("/") + "/api/v1"
        self.key = api_key

    def request(self, method: str, path: str, body=None, headers=None, raw=False):
        url = self.base + path
        data = None
        hdrs = {"X-API-Key": self.key}
        if body is not None and not raw:
            data = json.dumps(body).encode("utf-8")
            hdrs["Content-Type"] = "application/json"
        elif raw:
            data = body
        if headers:
            hdrs.update(headers)
        req = urllib.request.Request(url, data=data, method=method, headers=hdrs)
        try:
            with urllib.request.urlopen(req, timeout=120) as resp:
                return json.loads(resp.read().decode("utf-8"))
        except urllib.error.HTTPError as e:
            detail = e.read().decode("utf-8", "replace")
            raise RuntimeError(f"{method} {path} -> HTTP {e.code}: {detail[:500]}") from e

    def get(self, path):
        return self.request("GET", path)

    def post(self, path, body):
        return self.request("POST", path, body)

    def put(self, path, body):
        return self.request("PUT", path, body)

    def upload_file(self, path: str, filename: str, content: bytes):
        boundary = "----weknora" + uuid.uuid4().hex
        parts = []
        parts.append(f"--{boundary}\r\n".encode())
        parts.append(
            f'Content-Disposition: form-data; name="file"; filename="{filename}"\r\n'
            "Content-Type: text/markdown\r\n\r\n".encode()
        )
        parts.append(content)
        parts.append(f"\r\n--{boundary}--\r\n".encode())
        return self.request(
            "POST", path, b"".join(parts), raw=True,
            headers={"Content-Type": f"multipart/form-data; boundary={boundary}"},
        )


# ---------- knowledge file splitting ----------

def split_by_heading(text: str, level: int):
    """Split markdown into (title, content) sections at the given heading
    level. Content preceding the first heading becomes ("", preamble)."""
    marker = "#" * level + " "
    sections, title, buf = [], "", []
    for line in text.splitlines():
        if line.startswith(marker):
            sections.append((title, "\n".join(buf).strip()))
            title, buf = line[len(marker):].strip(), [line]
        else:
            buf.append(line)
    sections.append((title, "\n".join(buf).strip()))
    return [(t, c) for t, c in sections if c]


def safe_name(title: str) -> str:
    cleaned = re.sub(r'[\\/:*?"<>|]+', " ", title).strip()
    return re.sub(r"\s+", " ", cleaned)[:80]


def collect_documents(workspace: Path):
    """Return [(filename, content)] for every knowledge document to upload."""
    docs = []

    menu = (workspace / "MENU.md").read_text(encoding="utf-8")
    for title, content in split_by_heading(menu, 1):
        name = safe_name(title) if title else "Overview"
        # Guard against duplicate category headings in the source (e.g. the
        # doubled "# Fried Rice"): merge by appending.
        docs.append((f"MENU - {name}.md", content))

    menu_cn = (workspace / "MENU_CN.md").read_text(encoding="utf-8")
    for title, content in split_by_heading(menu_cn, 1):
        name = safe_name(title) if title else "Overview"
        docs.append((f"MENU_CN - {name}.md", content))

    stores = (workspace / "STORES.md").read_text(encoding="utf-8")
    for title, content in split_by_heading(stores, 2):
        name = safe_name(title) if title else "Overview"
        docs.append((f"STORES - {name}.md", content))

    for fname in ("FAQ.md", "POLICY.md", "MEMBERSHIP.md", "TOP_10.md", "BRAND.md"):
        docs.append((fname, (workspace / fname).read_text(encoding="utf-8")))

    # Merge duplicate filenames (doubled headings) instead of uploading twice.
    merged: dict[str, str] = {}
    for fname, content in docs:
        merged[fname] = merged[fname] + "\n\n" + content if fname in merged else content
    return list(merged.items())


# ---------- setup steps ----------

def ensure_kb(api: Api, embedding_model: str, summary_model: str) -> str:
    for kb in api.get("/knowledge-bases").get("data") or []:
        if kb.get("name") == KB_NAME:
            print(f"[kb] reuse existing: {kb['id']}")
            return kb["id"]
    body = {
        "name": KB_NAME,
        "description": "Togather Cafe 菜单、门店、政策、FAQ、会员知识（自 openclaw togather-cs workspace 迁移）",
        "type": "document",
        "embedding_model_id": embedding_model,
        "summary_model_id": summary_model,
        "chunking_config": {
            "chunk_size": 2000,
            "chunk_overlap": 100,
            "enable_parent_child": True,
            "parent_chunk_size": 4096,
            "child_chunk_size": 384,
            "separators": ["\n\n", "\n", "。", "！", "？", ";", "；"],
            "strategy": "auto",
            # Parse markdown locally; the cloud parser is unnecessary for
            # these small hand-written files.
            "parser_engine_rules": [{"engine": "builtin", "file_types": ["md", "markdown"]}],
        },
    }
    kb = api.post("/knowledge-bases", body)["data"]
    print(f"[kb] created: {kb['id']}")
    return kb["id"]


def existing_knowledge_titles(api: Api, kb_id: str):
    titles = set()
    page = 1
    while True:
        resp = api.get(f"/knowledge-bases/{kb_id}/knowledge?page={page}&page_size=100")
        items = resp.get("data") or []
        for item in items:
            titles.add(item.get("file_name") or item.get("title") or "")
        total = resp.get("total", 0)
        if page * 100 >= total or not items:
            return titles
        page += 1


def upload_documents(api: Api, kb_id: str, docs):
    present = existing_knowledge_titles(api, kb_id)
    uploaded = skipped = 0
    for fname, content in docs:
        if fname in present:
            skipped += 1
            continue
        api.upload_file(f"/knowledge-bases/{kb_id}/knowledge/file", fname, content.encode("utf-8"))
        uploaded += 1
        print(f"[doc] uploaded {fname}")
        time.sleep(0.2)  # keep the async parse queue civilised
    print(f"[doc] done: {uploaded} uploaded, {skipped} already present")


def ensure_mcp_service(api: Api) -> str:
    services = api.get("/mcp-services").get("data") or []
    svc = next((s for s in services if s.get("name") == MCP_SERVICE_NAME), None)
    if svc:
        print(f"[mcp] reuse existing: {svc['id']}")
        return svc["id"]
    body = {
        "name": MCP_SERVICE_NAME,
        "description": "查询距离客人最近的 Togather 门店（地址/邮编/Google Maps 链接/WhatsApp 定位）",
        "enabled": True,
        "transport_type": "http-streamable",
        "url": MCP_SERVER_URL,
    }
    svc = api.post("/mcp-services", body)["data"]
    print(f"[mcp] created: {svc['id']}")
    try:
        result = api.post(f"/mcp-services/{svc['id']}/test", {})
        print(f"[mcp] connection test: {json.dumps(result.get('data'), ensure_ascii=False)[:200]}")
    except RuntimeError as e:
        print(f"[mcp] WARNING: connection test failed — {e}\n"
              "      (expected until the togather-distance container is up and SSRF_WHITELIST applied)")
    return svc["id"]


def ensure_agent(api: Api, kb_id: str, mcp_id: str, chat_model: str,
                 rerank_model: str, asr_model: str) -> str:
    prompt = (HERE / "system_prompt.md").read_text(encoding="utf-8")
    config = {
        "agent_mode": "smart-reasoning",
        "agent_type": "custom",
        "system_prompt": prompt,
        "model_id": chat_model,
        "rerank_model_id": rerank_model,
        # Low temperature: menu listings must be copied verbatim from retrieved
        # documents; at 0.7 the model drifted mid-table and invented items.
        "temperature": 0.3,
        "max_iterations": 10,
        # The model must call at least one tool on round 1 (tool_choice=
        # "required"): stops it answering menu questions from memory with
        # fabricated items. Prompt-only guards proved unreliable here.
        "force_tool_first_round": True,
        "allowed_tools": ALLOWED_TOOLS,
        "mcp_selection_mode": "selected",
        "mcp_services": [mcp_id],
        "kb_selection_mode": "selected",
        "knowledge_bases": [kb_id],
        "citation_enabled": False,
    }
    if asr_model:
        # WhatsApp voice notes get transcribed with this model before QA
        # (internal/im/voice.go requires both fields on the agent).
        config["audio_upload_enabled"] = True
        config["asr_model_id"] = asr_model
    agents = api.get("/agents").get("data") or []
    existing = next((a for a in agents if a.get("name") == AGENT_NAME), None)
    body = {
        "name": AGENT_NAME,
        "description": "Togather Cafe WhatsApp 智能客服（三语：EN/中文/BM），知识库 + 最近门店查询",
        "avatar": "🍽️",
        "config": config,
    }
    if existing:
        api.put(f"/agents/{existing['id']}", body)
        print(f"[agent] updated: {existing['id']}")
        return existing["id"]
    agent = api.post("/agents", body)["data"]
    print(f"[agent] created: {agent['id']}")
    return agent["id"]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--api-key", required=True)
    ap.add_argument("--base-url", default="http://localhost:8080")
    ap.add_argument("--workspace", default=str(Path.home() / ".openclaw/workspace-togather-cs"))
    ap.add_argument("--chat-model", default="", help="chat model id (default: auto-pick)")
    args = ap.parse_args()

    api = Api(args.base_url, args.api_key)
    workspace = Path(args.workspace).expanduser()
    if not workspace.is_dir():
        sys.exit(f"workspace not found: {workspace}")

    models = api.get("/models").get("data") or []
    def pick(mtype, name_hint=None):
        cands = [m for m in models if m.get("type") == mtype]
        if name_hint:
            for m in cands:
                if name_hint.lower() in (m.get("name") or "").lower():
                    return m["id"]
        return cands[0]["id"] if cands else ""

    chat_model = args.chat_model or pick("KnowledgeQA", "minimax") or pick("KnowledgeQA")
    # Prefer a local (Ollama) embedder when present — the WeKnoraCloud free
    # tier runs out of quota quickly on a 72-document import.
    embedding_model = pick("Embedding", "bge") or pick("Embedding")
    rerank_model = pick("Rerank")
    asr_model = pick("ASR")  # optional: enables WhatsApp voice-note transcription
    if not (chat_model and embedding_model):
        sys.exit("need at least one KnowledgeQA model and one Embedding model configured in WeKnora")
    print(f"[models] chat={chat_model} embedding={embedding_model} "
          f"rerank={rerank_model or '(none)'} asr={asr_model or '(none)'}")

    kb_id = ensure_kb(api, embedding_model, chat_model)
    upload_documents(api, kb_id, collect_documents(workspace))
    mcp_id = ensure_mcp_service(api)
    agent_id = ensure_agent(api, kb_id, mcp_id, chat_model, rerank_model, asr_model)

    print("\n=== done ===")
    print(f"knowledge base: {kb_id}\nmcp service:    {mcp_id}\nagent:          {agent_id}")
    print("Bind a WhatsApp IM channel to this agent in the WeKnora UI, scan the QR, and test.")


if __name__ == "__main__":
    main()

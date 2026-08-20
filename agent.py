"""LangChain agent → MCP tools → Go scorecard API.

Prefer Bedrock (langchain-aws). Fall back to Anthropic API if AWS creds are
missing — same pattern as other demos in this repo — so ./run.sh still answers
'Which services aren't ready and why?' on camera.
"""

from __future__ import annotations

import asyncio
import json
import os
import sys
from pathlib import Path

from langchain_core.messages import HumanMessage
from langchain_core.tools import StructuredTool
from mcp import ClientSession, StdioServerParameters
from mcp.client.stdio import stdio_client
from mcp.types import TextContent

ROOT = Path(__file__).resolve().parent
PYTHON = sys.executable

QUESTION = "Which services aren't ready and why?"

SYSTEM_HINT = (
    "You are a production-readiness assistant for an internal developer portal. "
    "Prefer why_not_ready for the gap list; use get_scorecard if you need scores. "
    "Cite scores and concrete gaps. Suggest brief remediations. Be concise."
)


def _text(result) -> str:
    parts = []
    for block in result.content or []:
        if isinstance(block, TextContent):
            parts.append(block.text)
        else:
            parts.append(str(block))
    return "\n".join(parts) if parts else str(result)


def _make_llm():
    region = os.environ.get("AWS_REGION", "us-east-1")
    try:
        import boto3
        from langchain_aws import ChatBedrock

        boto3.client("sts", region_name=region).get_caller_identity()
        model = os.environ.get(
            "BEDROCK_MODEL",
            "anthropic.claude-3-haiku-20240307-v1:0",
        )
        print(f"llm: Bedrock ({model})", flush=True)
        return ChatBedrock(
            model_id=model,
            region_name=region,
            model_kwargs={"temperature": 0},
        )
    except Exception as exc:  # noqa: BLE001
        print(f"llm: Bedrock unavailable ({exc})", flush=True)

    key = os.environ.get("ANTHROPIC_API_KEY", "")
    if key:
        try:
            from langchain_anthropic import ChatAnthropic

            model = os.environ.get("ANTHROPIC_MODEL", "claude-haiku-4-5")
            print(f"llm: Anthropic API ({model})", flush=True)
            return ChatAnthropic(model=model, temperature=0, api_key=key)
        except Exception as exc:  # noqa: BLE001
            print(f"llm: Anthropic unavailable ({exc})", flush=True)

    print("llm: none — scripted MCP tool walk", flush=True)
    return None


async def _bind(session: ClientSession) -> dict[str, StructuredTool]:
    async def list_services() -> str:
        """List services in the context graph."""
        return _text(await session.call_tool("list_services", {}))

    async def get_scorecard(service: str = "") -> str:
        """Get readiness scorecard for one service or all."""
        return _text(await session.call_tool("get_scorecard", {"service": service}))

    async def why_not_ready(service: str = "") -> str:
        """Explain why services are not production-ready."""
        return _text(await session.call_tool("why_not_ready", {"service": service}))

    return {
        "list_services": StructuredTool.from_function(
            name="list_services",
            description="List all services in the context graph.",
            coroutine=list_services,
        ),
        "get_scorecard": StructuredTool.from_function(
            name="get_scorecard",
            description="Get production-readiness scorecard (0-100).",
            coroutine=get_scorecard,
        ),
        "why_not_ready": StructuredTool.from_function(
            name="why_not_ready",
            description="Explain why services are not production-ready.",
            coroutine=why_not_ready,
        ),
    }


async def _scripted(tools: dict[str, StructuredTool]) -> None:
    print("\n=== scripted tool walk (MCP → Go API) ===\n")
    print("→ list_services()")
    print(await tools["list_services"].ainvoke({}))
    print("\n→ get_scorecard()")
    print(await tools["get_scorecard"].ainvoke({"service": ""}))
    print("\n→ why_not_ready()")
    print(await tools["why_not_ready"].ainvoke({"service": ""}))
    print("\n(scripted summary) Services below the ready threshold need the gaps above fixed.")


async def _llm_loop(llm, tools: dict[str, StructuredTool]) -> None:
    from langgraph.prebuilt import create_react_agent

    print(f"\n=== LLM agent: {QUESTION!r} ===\n")
    agent = create_react_agent(llm, list(tools.values()), prompt=SYSTEM_HINT)
    result = await agent.ainvoke({"messages": [HumanMessage(content=QUESTION)]})
    for msg in result["messages"]:
        kind = type(msg).__name__
        content = getattr(msg, "content", "")
        tool_calls = getattr(msg, "tool_calls", None)
        if tool_calls:
            for tc in tool_calls:
                print(f"→ tool {tc.get('name')}({json.dumps(tc.get('args', {}))})")
        text = _message_text(content)
        if text and kind in ("AIMessage", "ToolMessage", "HumanMessage"):
            # Tool payloads are large JSON; keep the recording readable.
            if kind == "ToolMessage" and len(text) > 400:
                text = text[:400] + "…"
            print(f"[{kind}] {text}")
            print()


def _message_text(content) -> str:
    if isinstance(content, str):
        return content.strip()
    if isinstance(content, list):
        parts = []
        for block in content:
            if isinstance(block, dict) and block.get("type") == "text":
                parts.append(str(block.get("text", "")))
            elif isinstance(block, str):
                parts.append(block)
        return "\n".join(p for p in parts if p).strip()
    return str(content).strip() if content else ""


async def run() -> None:
    env = {**os.environ}
    env.setdefault("SCORECARD_API", "http://127.0.0.1:8091")
    params = StdioServerParameters(
        command=PYTHON,
        args=[str(ROOT / "mcp_server.py")],
        cwd=str(ROOT),
        env=env,
    )
    async with stdio_client(params) as (read, write):
        async with ClientSession(read, write) as session:
            await session.initialize()
            tools = await _bind(session)
            llm = _make_llm()
            if llm is None:
                await _scripted(tools)
                return
            try:
                await _llm_loop(llm, tools)
            except Exception as exc:  # noqa: BLE001
                print(f"llm agent failed ({exc}); falling back to scripted walk", flush=True)
                await _scripted(tools)


if __name__ == "__main__":
    asyncio.run(run())

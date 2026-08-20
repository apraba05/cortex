# Context Graph + MCP Scorecard Agent

Building an agent-queryable service catalog with graph modeling, deterministic scoring rules, and MCP tool exposure for LLM consumption.

**Live demo:** https://cortex.ashanpraba.com

The demo runs entirely in the browser against seeded data — no API keys,
no accounts, and no external services required.

## Stack

- Go
- Python
- Redis
- LangChain
- Bedrock
- MCP

## How it works

- Write 5-6 YAML fake service manifests with fields: helm_chart, argocd_status, terraform_managed, ci_status, owner, ai_agent_usage.
- Load them into Redis as simple node/edge structures (service->owner, service->infra, service->repo).
- A small Go HTTP API that computes a readiness score (0-100) from weighted rule checks per service.
- Wrap 'list_services', 'get_scorecard', and 'why_not_ready' as MCP tools (Python MCP server) backed by that Go API.
- Connect a LangChain/Bedrock agent to the MCP server and ask it live: 'Which services aren't ready and why?'.
- Record a 60-90s take: show the raw data, the score, and the agent's natural-language remediation explanation.

## Running locally

```bash
cd src
bash run.sh
```

Then open the printed URL. A prebuilt static version of the UI lives in
`src/web/` and can be opened directly with no server.

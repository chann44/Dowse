# Benchmark

Does Claude Code find code faster with dowse than with grep alone?

## Method

- **Questions:** [`questions.json`](questions.json) asks about behaviour without naming functions. For example, "how does a sandbox figure out which parts of the screen changed between two captures?" Each question lists the files that count as a correct answer. An answer is correct if it cites one of them.
- **Setups:** Claude Code runs headless (`claude -p`) in the target repo with only `Read`, `Grep` and `Glob` allowed. The dowse setup also has `mcp__dowse__search_code`. Edits, shell, web and subagents are disabled. The prompt never mentions dowse.
- **Order:** each question runs in both setups, in alternating order, `--reps` times.
- **Metrics:** tool calls (excluding `ToolSearch`, which only loads the MCP tool's schema), total input tokens including cached ones, time, cost and correctness, all from Claude Code's `stream-json` output.

## Run it

```bash
cd ~/code/zoo && dowse index .
python3 bench/run.py --repo ~/code/zoo --reps 2 --out bench/results/my-run.jsonl
python3 bench/summarize.py bench/results/my-run.jsonl
```

Each run is a real Claude Code session and costs about $0.15–0.20 with Opus.

## Results

| File | Repo | Model | Notes |
| --- | --- | --- | --- |
| [`2026-10-07-zoo-opus-5.5.jsonl`](results/2026-10-07-zoo-opus-5.5.jsonl) | zoo (197 files, 1,615 chunks) | Opus 5.5 | 8 questions × 2 reps |

An earlier run is not included. dowse's MCP server had no `instructions` at the time, and Claude Code loads MCP tool schemas lazily, so Claude saw only the tool's name and never called it. Adding server instructions fixed that.

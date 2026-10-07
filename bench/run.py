"""Ask Claude Code each question with and without dowse; append one JSON line per run.

Usage: python3 bench/run.py --repo ~/code/zoo [--reps 2] [--out bench/results/new.jsonl]

The repo must already be indexed (`dowse index`). Claude Code runs headless with
only Read/Grep/Glob (plus search_code in the dowse runs), no edits or shell.
"""
import argparse, json, os, shutil, subprocess, tempfile, time

HERE = os.path.dirname(os.path.abspath(__file__))
BASE_TOOLS = ["Read", "Grep", "Glob"]
DENY = ["Edit", "Write", "Bash", "NotebookEdit", "WebFetch", "WebSearch", "Agent", "Task"]
SUFFIX = "\n\nAnswer in at most 4 sentences and cite the relevant file:line locations."


def mcp_configs(tmp):
    dowse = shutil.which("dowse") or "dowse"
    configs = {
        "none": {"mcpServers": {}},
        "dowse": {"mcpServers": {"dowse": {"command": dowse, "args": ["mcp"]}}},
    }
    paths = {}
    for mode, cfg in configs.items():
        paths[mode] = os.path.join(tmp, f"mcp-{mode}.json")
        with open(paths[mode], "w") as f:
            json.dump(cfg, f)
    return paths


def run(repo, q, mode, rep, mcp, raw_dir):
    tools = BASE_TOOLS + (["mcp__dowse__search_code"] if mode == "dowse" else [])
    cmd = ["claude", "-p", q["q"] + SUFFIX, "--output-format", "stream-json", "--verbose",
           "--strict-mcp-config", "--mcp-config", mcp[mode], "--allowedTools", ",".join(tools),
           "--disallowedTools", ",".join(DENY), "--no-session-persistence"]
    p = subprocess.run(cmd, cwd=repo, capture_output=True, text=True, timeout=600)
    with open(os.path.join(raw_dir, f"{q['id']}-{mode}-{rep}.jsonl"), "w") as f:
        f.write(p.stdout)
    calls, result, model = [], {}, None
    for line in p.stdout.splitlines():
        try:
            m = json.loads(line)
        except ValueError:
            continue
        if m.get("subtype") == "init":
            model = m.get("model")
        if m.get("type") == "assistant":
            calls += [c["name"] for c in m["message"]["content"] if c.get("type") == "tool_use"]
        if m.get("type") == "result":
            result = m
    ans = result.get("result") or ""
    u = result.get("usage", {})
    return {
        "id": q["id"], "mode": mode, "rep": rep,
        "duration_s": round(result.get("duration_ms", 0) / 1000, 1),
        # ToolSearch only loads the deferred MCP schema; it is not a search step
        "tool_calls": len([c for c in calls if c != "ToolSearch"]), "calls": calls,
        "turns": result.get("num_turns"), "cost_usd": result.get("total_cost_usd"),
        "input_tokens": u.get("input_tokens", 0) + u.get("cache_creation_input_tokens", 0)
        + u.get("cache_read_input_tokens", 0),
        "output_tokens": u.get("output_tokens", 0),
        "model": model,
        "correct": any(e in ans for e in q["expect"]), "answer": ans,
    }


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--repo", required=True)
    ap.add_argument("--questions", default=os.path.join(HERE, "questions.json"))
    ap.add_argument("--reps", type=int, default=2)
    ap.add_argument("--out", default=os.path.join(HERE, "results", "latest.jsonl"))
    a = ap.parse_args()

    qs = json.load(open(a.questions))
    tmp = tempfile.mkdtemp(prefix="dowse-bench-")
    mcp = mcp_configs(tmp)
    raw_dir = os.path.join(tmp, "raw")
    os.makedirs(raw_dir)
    print("raw transcripts:", raw_dir)
    with open(a.out, "a") as out:
        for rep in range(a.reps):
            for q in qs:
                # alternate order so neither mode always runs first
                for mode in (["none", "dowse"] if (q["id"] + rep) % 2 else ["dowse", "none"]):
                    r = run(os.path.expanduser(a.repo), q, mode, rep, mcp, raw_dir)
                    out.write(json.dumps(r) + "\n")
                    out.flush()
                    print(f"q{r['id']} {mode:5} {r['duration_s']:6}s calls={r['tool_calls']:2} "
                          f"in={r['input_tokens']:7} ${r['cost_usd']} correct={r['correct']}", flush=True)


if __name__ == "__main__":
    main()

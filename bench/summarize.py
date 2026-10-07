"""Summarise a results file: python3 bench/summarize.py bench/results/<file>.jsonl"""
import json, statistics as st, sys


def row(label, rs):
    if not rs:
        return
    m = lambda k: st.mean(r[k] for r in rs)
    print(f"| {label} | {len(rs)} | {sum(r['correct'] for r in rs)}/{len(rs)} | {m('tool_calls'):.2f} "
          f"| {m('input_tokens') / 1000:.0f}k | {m('duration_s'):.1f}s | ${m('cost_usd'):.3f} |")


rs = [json.loads(l) for l in open(sys.argv[1])]
none = [r for r in rs if r["mode"] == "none"]
dowse = [r for r in rs if r["mode"] == "dowse"]
used = [r for r in dowse if "mcp__dowse__search_code" in r["calls"]]
pairs = {(r["id"], r["rep"]) for r in used}

print("| | Runs | Correct | Tool calls | Input tokens | Time | Cost |")
print("| --- | --- | --- | --- | --- | --- | --- |")
row("Claude Code", none)
row("Claude Code + dowse", dowse)
row("+ dowse, runs where it was called", used)
row("Claude Code, same questions", [r for r in none if (r["id"], r["rep"]) in pairs])
print(f"\nsearch_code called in {len(used)}/{len(dowse)} dowse runs; model: {sorted({r['model'] for r in rs})}")

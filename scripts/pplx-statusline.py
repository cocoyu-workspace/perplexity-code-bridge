#!/usr/bin/env python3
"""Render Claude Code statusLine with local bridge quota and git context."""
import json
import os
import subprocess
import sys
import urllib.request

C = {
    "cyan": "\033[36m",
    "brightBlack": "\033[90m",
    "brightMagenta": "\033[95m",
    "green": "\033[32m",
    "blue": "\033[34m",
    "magenta": "\033[35m",
    "brightGreen": "\033[92m",
    "yellow": "\033[33m",
    "brightBlue": "\033[94m",
    "red": "\033[31m",
    "purple": "\033[38;2;124;58;237m",
    "reset": "\033[0m",
}

SOL_MODELS = ("gpt-6.1-sol", "gpt-6.1-sol[1m]", "claude-sol-6-1", "claude-sol-6.1")


def c(text, color):
    return f"{C[color]}{text}{C['reset']}"


def turn_count(transcript_path):
    if not transcript_path or not os.path.isfile(transcript_path):
        return None
    n = 0
    try:
        with open(transcript_path, "r", encoding="utf-8") as fh:
            for line in fh:
                line = line.strip()
                if not line:
                    continue
                try:
                    d = json.loads(line)
                except Exception:
                    continue
                if d.get("type") != "user" or d.get("isSidechain") or d.get("isMeta"):
                    continue
                content = d.get("message", {}).get("content")
                if isinstance(content, str):
                    n += 1
                elif isinstance(content, list) and all(
                    isinstance(b, dict) and b.get("type") == "text" for b in content
                ):
                    n += 1
    except Exception:
        return None
    return n


def session_title(transcript_path):
    if not transcript_path or not os.path.isfile(transcript_path):
        return None
    title = None
    try:
        with open(transcript_path, "r", encoding="utf-8") as fh:
            for line in fh:
                if '"custom-title"' not in line:
                    continue
                try:
                    d = json.loads(line)
                except Exception:
                    continue
                if d.get("type") == "custom-title" and d.get("customTitle"):
                    title = d["customTitle"]
    except Exception:
        return None
    return title


def git_info(cwd):
    def run(args):
        try:
            r = subprocess.run(
                ["git", *args], cwd=cwd or None, capture_output=True, text=True, timeout=2
            )
            return r.stdout.strip() if r.returncode == 0 else None
        except Exception:
            return None

    branch = run(["rev-parse", "--abbrev-ref", "HEAD"])
    if branch is None:
        return "no git", ""
    stat = run(["diff", "--shortstat", "HEAD"]) or ""
    added = removed = 0
    for tok in stat.split(","):
        tok = tok.strip()
        if "insertion" in tok:
            added = int(tok.split()[0])
        elif "deletion" in tok:
            removed = int(tok.split()[0])
    changes = f"+{added} -{removed}" if (added or removed) else "clean"
    return branch, changes


def fmt_k(n):
    return f"{n / 1000:.0f}k" if isinstance(n, (int, float)) else "?"


def resolve_context(ctx, model_name, env=os.environ, model_id=None):
    used = ctx.get("total_input_tokens")
    pct = ctx.get("used_percentage")
    candidates = (model_id, model_name) if model_id else (model_name, env.get("CLAUDE_PPLX_MODEL"))
    is_sol = any(
        (m or "").strip().lower() in SOL_MODELS or "gpt-6.1 sol" in (m or "").strip().lower()
        for m in candidates
    )
    raw_max = (env.get("CLAUDE_CODE_MAX_CONTEXT_TOKENS") or "").strip() if is_sol else ""
    size = int(raw_max) if raw_max.isdigit() else (1000000 if is_sol else 200000)
    if isinstance(used, (int, float)) and size > 0:
        pct = round(used * 100 / size)
    return fmt_k(used), fmt_k(size), (pct if pct is not None else "?")


def fetch_pool_quota(base_url):
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    try:
        with opener.open(base_url.rstrip("/") + "/admin/accounts", timeout=0.4) as resp:
            payload = json.loads(resp.read().decode("utf-8"))
    except Exception:
        return None, None, None
    accounts = payload.get("accounts") or []
    if not isinstance(accounts, list) or not accounts:
        return None, None, None
    usable = [
        a for a in accounts
        if isinstance(a, dict) and (a.get("available") is True or a.get("status") == "active")
    ]
    acc = usable[0] if usable else (accounts[0] if isinstance(accounts[0], dict) else {})
    quota = acc.get("quota") or acc.get("quota_info") or {}
    pro_rem = quota.get("remaining_pro")
    if pro_rem is None:
        pro_rem = quota.get("premium_balance")
    if pro_rem is None:
        limit = quota.get("limit") if quota.get("limit") is not None else quota.get("space_limit")
        usage = quota.get("usage") if quota.get("usage") is not None else quota.get("space_usage")
        if isinstance(limit, int) and isinstance(usage, int) and limit > 0:
            pro_rem = max(0, limit - usage)
    max_rem = quota.get("remaining_agentic_research")
    if max_rem is None:
        max_rem = quota.get("remaining_research")
    plan = (acc.get("plan_type") or "").strip().lower() or None
    return pro_rem, max_rem, plan


def render_statusline(data, env=os.environ):
    model_obj = data.get("model") or {}
    model_id = (model_obj.get("id") or "").strip() or None
    model = (model_obj.get("display_name") or model_id or env.get("CLAUDE_PPLX_MODEL") or "?").strip() or "?"
    effort = ((data.get("effort") or {}).get("level") or env.get("CLAUDE_PPLX_EFFORT") or "?").strip() or "?"
    ctx_used_k, ctx_size_k, ctx_pct = resolve_context(data.get("context_window") or {}, model, env, model_id=model_id)

    base_url = env.get("ANTHROPIC_BASE_URL") or "http://127.0.0.1:18082"
    pro_rem, max_rem, plan = fetch_pool_quota(base_url)

    cwd = (data.get("workspace") or {}).get("current_dir") or data.get("cwd", "")
    transcript = data.get("transcript_path") or ""
    session_id = data.get("session_id") or ""
    if not session_id and transcript.endswith(".jsonl"):
        session_id = os.path.basename(transcript)[: -len(".jsonl")]

    turns = turn_count(transcript)
    turns_s = str(turns) if turns is not None else "?"
    branch, changes = git_info(cwd)

    pro_s = str(pro_rem) if isinstance(pro_rem, int) else "N/A"
    max_s = str(max_rem) if isinstance(max_rem, int) else "N/A"
    tier_tag = f" ({plan})" if plan else ""

    line1 = " | ".join([
        c(f"Model: {model}{tier_tag}", "cyan"),
        c(f"Turn:{turns_s}", "magenta"),
        c(f"Ctx: {ctx_used_k}/{ctx_size_k} ({ctx_pct}%)", "red"),
        c(f"Effort:{effort}", "brightMagenta"),
        c(f"Pro:{pro_s}", "green"),
        c(f"Max:{max_s}", "blue"),
        c(f"⎇ {branch}", "purple"),
        c(changes, "yellow"),
    ])
    line2 = c(cwd, "brightBlue")
    title = session_title(transcript)
    if title:
        line2 += "  " + c(title, "brightGreen")
    if session_id:
        line2 += "  " + c(f"resume: {session_id}", "brightBlack")
    return line1 + "\n" + line2


def main():
    print(render_statusline(json.load(sys.stdin)))


if __name__ == "__main__":
    main()

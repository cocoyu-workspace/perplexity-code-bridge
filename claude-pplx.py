#!/usr/bin/env python3
"""Run an isolated Claude Code CLI session against the local Perplexity bridge."""
import json
import os
import re
import shlex
import shutil
import subprocess
import sys
import time
import urllib.request
from pathlib import Path
import yaml

ROOT = Path(__file__).resolve().parent
STATUSLINE_SCRIPT = ROOT / "scripts" / "pplx-statusline.py"

DEFAULT_PORT = "18082"
DEFAULT_MODEL = "sonnet-5.5"
DEFAULT_EFFORT = "medium"
DEFAULT_SUBAGENT_MODEL = "sonnet-5.5"
DEFAULT_AUTO_COMPACT_WINDOW = "200000"
DEFAULT_SETTING_SOURCES = "user,project,local"

SUBAGENT_TOOL = "Agent"
BASE_TOOLS = ["Read", "Edit", "Write", "Bash", "Glob", "Grep", "Skill"]
DEFAULT_EXTRA_TOOLS = ["SendMessage", "ToolSearch"]
EXTRA_TOOLS_ENV = "CLAUDE_PPLX_EXTRA_TOOLS"
DISABLE_TOOLS_ENV = "CLAUDE_PPLX_DISABLE_TOOLS"

SUPPORTED_MODELS = [
    "auto",
    "sonnet-5.5",
    "opus-5.5",
    "gpt-6.1-sol",
    "gpt-6.1-sol[1m]",
    "claude-sonnet-5",
    "claude-sonnet-5-5",
    "claude-opus-5-5",
    "claude-sol-6-1",
    "claude-sol-6-1[1m]",
]

MODEL_PICKER_OPTIONS = [
    {
        "model": "auto",
        "label": "Auto",
        "description": "Default coding model",
        "behavesAs": "claude-sonnet-5",
    },
    {
        "model": "sonnet-5.5",
        "label": "Claude Sonnet 5.5",
        "description": "Fast & balanced coding model",
        "behavesAs": "claude-sonnet-5",
    },
    {
        "model": "opus-5.5",
        "label": "Claude Opus 5.5",
        "description": "Deep reasoning & complex architecture (Max tier)",
        "behavesAs": "claude-opus-5-5",
    },
    {
        "model": "gpt-6.1-sol",
        "label": "GPT-6.1 Sol",
        "description": "1M context window",
        "behavesAs": "claude-opus-5-5[1m]",
    },
]

DEFAULT_MODEL_SETTINGS = {
    "claude-sonnet-5": {"effortLevel": DEFAULT_EFFORT, "autoCompactWindow": int(DEFAULT_AUTO_COMPACT_WINDOW)},
    "claude-sonnet-5-5": {"effortLevel": DEFAULT_EFFORT, "autoCompactWindow": int(DEFAULT_AUTO_COMPACT_WINDOW)},
    "claude-opus-5-5": {"autoCompactWindow": int(DEFAULT_AUTO_COMPACT_WINDOW)},
    "sonnet-5.5": {"effortLevel": DEFAULT_EFFORT, "autoCompactWindow": int(DEFAULT_AUTO_COMPACT_WINDOW)},
    "opus-5.5": {"autoCompactWindow": int(DEFAULT_AUTO_COMPACT_WINDOW)},
}

DOCUMENTED_SYSTEM_PROMPT = (
    "You are an interactive CLI tool that helps users with software engineering tasks. "
    "Use the instructions below and the tools available to you to assist the user."
)

_TOOL_NAME = re.compile(r"[A-Za-z][A-Za-z0-9_]{0,63}")


def has_flag(argv, name):
    return any(a == name or a.startswith(name + "=") for a in argv)


def flag_value(argv, name, default):
    value = default
    for i, a in enumerate(argv):
        if a == name and i + 1 < len(argv):
            value = argv[i + 1]
        elif a.startswith(name + "="):
            value = a.split("=", 1)[1]
    return value


def default_model_args(argv):
    out = []
    if not has_flag(argv, "--model"):
        out += ["--model", DEFAULT_MODEL]
    if not has_flag(argv, "--effort"):
        out += ["--effort", DEFAULT_EFFORT]
    return out


def explicit_permission_override(argv):
    override = None
    for i, a in enumerate(argv):
        if a == "--permission-mode" and i + 1 < len(argv):
            override = argv[i + 1]
        elif a.startswith("--permission-mode="):
            override = a.split("=", 1)[1]
        elif a == "--dangerously-skip-permissions":
            override = "bypassPermissions"
    return override


def permission_args(argv):
    if explicit_permission_override(argv) is not None:
        return []
    return ["--permission-mode", "bypassPermissions"]


def _tool_names_from_env(environ, env_name):
    names = []
    for name in environ.get(env_name, "").split(","):
        name = name.strip()
        if not name:
            continue
        if not _TOOL_NAME.fullmatch(name):
            raise ValueError("invalid tool name in " + env_name + ": " + repr(name))
        if name not in names:
            names.append(name)
    return names


def extra_tools_from_env(environ):
    return [
        n for n in _tool_names_from_env(environ, EXTRA_TOOLS_ENV)
        if n not in BASE_TOOLS + [SUBAGENT_TOOL]
    ]


def disabled_tools_from_env(environ):
    names = _tool_names_from_env(environ, DISABLE_TOOLS_ENV)
    for name in names:
        if name in BASE_TOOLS + [SUBAGENT_TOOL]:
            raise ValueError("core tool cannot be disabled via " + DISABLE_TOOLS_ENV + ": " + repr(name))
    return names


def effective_extra_tools(environ):
    disabled = disabled_tools_from_env(environ)
    extra = []
    for name in DEFAULT_EXTRA_TOOLS + extra_tools_from_env(environ):
        if name not in disabled and name not in extra:
            extra.append(name)
    return extra


def subagent_tools(extra=()):
    return BASE_TOOLS + [SUBAGENT_TOOL] + [t for t in extra if t not in BASE_TOOLS + [SUBAGENT_TOOL]]


def sent_tools(args):
    tools = None
    for i, a in enumerate(args):
        if a == "--tools" and i + 1 < len(args):
            tools = args[i + 1]
        elif a.startswith("--tools="):
            tools = a.split("=", 1)[1]
    return [t for t in (tools or "").split(",") if t]


def statusline_command(script_path=STATUSLINE_SCRIPT):
    return "python3 " + shlex.quote(str(script_path))


def _merge_model_settings(existing=None):
    merged = {}
    if isinstance(existing, dict):
        for model_key, val in existing.items():
            if isinstance(val, dict):
                merged[model_key] = dict(val)
    for model_key, defaults in DEFAULT_MODEL_SETTINGS.items():
        entry = dict(merged.get(model_key) or {})
        for k, v in defaults.items():
            if k == "autoCompactWindow" or not entry.get(k):
                entry[k] = v
        merged[model_key] = entry
    return merged


def subagent_env(argv, environ=None):
    src = os.environ if environ is None else environ
    model = flag_value(argv, "--model", DEFAULT_MODEL)
    effort = flag_value(argv, "--effort", DEFAULT_EFFORT)
    sol = model.strip().lower() in (
        "gpt-6.1-sol",
        "gpt-6.1-sol[1m]",
        "claude-sol-6-1",
        "claude-sol-6-1[1m]",
        "claude-sol-6.1",
        "claude-sol-6.1[1m]",
    )
    window = src.get("CLAUDE_CODE_AUTO_COMPACT_WINDOW", "").strip() or (
        "1000000" if sol else DEFAULT_AUTO_COMPACT_WINDOW
    )
    subagent_model = src.get("CLAUDE_CODE_SUBAGENT_MODEL", "").strip() or DEFAULT_SUBAGENT_MODEL
    env = {
        "CLAUDE_CODE_SUBAGENT_MODEL": subagent_model,
        "CLAUDE_CODE_DISABLE_AUTO_MEMORY": "1",
        "CLAUDE_CODE_AUTO_COMPACT_WINDOW": window,
        "ENABLE_TOOL_SEARCH": "true",
        "CLAUDE_PPLX_MODEL": model,
        "CLAUDE_PPLX_EFFORT": effort,
    }
    if sol:
        env["CLAUDE_CODE_MAX_CONTEXT_TOKENS"] = (
            src.get("CLAUDE_CODE_MAX_CONTEXT_TOKENS", "").strip() or "1000000"
        )
    return env


def subagent_isolation_args(argv=(), base=None, api_key=None, environ=None):
    model = flag_value(argv, "--model", DEFAULT_MODEL)
    child_env = subagent_env(argv, environ=environ)
    settings_env = dict(child_env)
    settings_env.update({
        "ANTHROPIC_MODEL": model,
        "ANTHROPIC_DEFAULT_MODEL": DEFAULT_MODEL,
        "ANTHROPIC_DEFAULT_SONNET_MODEL": DEFAULT_MODEL,
        "ANTHROPIC_DEFAULT_OPUS_MODEL": "opus-5.5",
        "ANTHROPIC_DEFAULT_HAIKU_MODEL": child_env["CLAUDE_CODE_SUBAGENT_MODEL"],
        "ANTHROPIC_SMALL_FAST_MODEL": child_env["CLAUDE_CODE_SUBAGENT_MODEL"],
        "CLAUDE_CODE_AUTO_MODE_MODEL": child_env["CLAUDE_CODE_SUBAGENT_MODEL"],
        "CLAUDE_CODE_BG_CLASSIFIER_MODEL": child_env["CLAUDE_CODE_SUBAGENT_MODEL"],
        "CLAUDE_CODE_EFFORT_LEVEL": "",
        "CLAUDE_CODE_MAX_CONTEXT_TOKENS": child_env.get("CLAUDE_CODE_MAX_CONTEXT_TOKENS", ""),
        "ANTHROPIC_AUTH_TOKEN": "",
        "ENABLE_TOOL_SEARCH": "true",
        "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
    })
    if base:
        settings_env["ANTHROPIC_BASE_URL"] = base
    if api_key:
        settings_env["ANTHROPIC_API_KEY"] = api_key

    picker_options = [dict(item) for item in MODEL_PICKER_OPTIONS]
    if not any(item.get("model") == model for item in picker_options):
        picker_options.append({
            "model": model,
            "label": model,
            "description": "",
            "behavesAs": "claude-opus-5-5" if "opus" in model.lower() else "claude-sonnet-5",
        })
    settings = {
        "model": model,
        "availableModels": list(SUPPORTED_MODELS),
        "modelPicker": {
            "replaceBuiltInOptions": True,
            "options": picker_options,
        },
        "modelSettings": _merge_model_settings(),
        "env": settings_env,
        "statusLine": {
            "type": "command",
            "command": statusline_command(),
            "padding": 0,
            "refreshInterval": 10,
        },
    }
    out = []
    if not has_flag(argv, "--setting-sources"):
        out += ["--setting-sources", DEFAULT_SETTING_SOURCES]
    out += ["--settings", json.dumps(settings)]
    return out


def build_claude_args(claude, skill_context, argv, extra_tools=None, agents=None, base=None, api_key=None):
    if extra_tools is None:
        extra_tools = DEFAULT_EXTRA_TOOLS
    agent_args = []
    if agents and not has_flag(argv, "--agents"):
        agent_args = ["--agents", json.dumps(agents, ensure_ascii=False)]
    return (
        [claude]
        + subagent_isolation_args(argv, base=base, api_key=api_key)
        + default_model_args(argv)
        + permission_args(argv)
        + [
            "--tools", ",".join(subagent_tools(extra_tools)),
            "--system-prompt", DOCUMENTED_SYSTEM_PROMPT,
            "--append-system-prompt", skill_context,
        ]
        + agent_args
        + list(argv)
    )


def load_local_config(config_path=None):
    path = Path(config_path) if config_path else (ROOT / "config.yaml")
    if not path.is_file():
        return {}
    try:
        return yaml.safe_load(path.read_text(encoding="utf-8")) or {}
    except Exception:
        return {}


def resolve_bridge_endpoint(cfg=None, environ=None):
    src = os.environ if environ is None else environ
    cfg = cfg if isinstance(cfg, dict) else load_local_config()
    srv = cfg.get("server") if isinstance(cfg.get("server"), dict) else {}
    port = str(srv.get("port") or DEFAULT_PORT).strip() or DEFAULT_PORT
    base = src.get("PPLX_BRIDGE_BASE_URL", "").strip() or f"http://127.0.0.1:{port}"
    key = src.get("PPLX_BRIDGE_API_KEY", "").strip() or str(srv.get("api_key") or "local-dev-key").strip()
    return base, key


def ensure_bridge_running(base, key):
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

    def probe():
        try:
            req = urllib.request.Request(
                f"{base.rstrip('/')}/v1/models",
                headers={"Authorization": f"Bearer {key}"},
            )
            with opener.open(req, timeout=2) as r:
                return r.status == 200
        except Exception:
            return False

    if probe():
        return
    binary = ROOT / "perplexity-bridge"
    if not binary.is_file():
        subprocess.run(
            ["go", "build", "-o", str(binary), "./cmd/perplexity-bridge"],
            cwd=ROOT,
            check=True,
        )
    subprocess.Popen(
        [str(binary)],
        cwd=ROOT,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
        start_new_session=True,
    )
    for _ in range(30):
        if probe():
            return
        time.sleep(0.2)
    raise SystemExit(f"perplexity-bridge did not become ready on {base}")


def main():
    argv = sys.argv[1:]
    cfg = load_local_config()
    base, key = resolve_bridge_endpoint(cfg)
    ensure_bridge_running(base, key)

    env = os.environ.copy()
    for name in list(env):
        if name.startswith("ANTHROPIC_") or name == "CLAUDE_CODE_MAX_CONTEXT_TOKENS":
            env.pop(name)
    env.update(
        ANTHROPIC_BASE_URL=base,
        ANTHROPIC_API_KEY=key,
        CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC="1",
        NO_PROXY="127.0.0.1,localhost",
    )
    env.update(subagent_env(argv))

    skill_context = (
        "Local skills are available via the Skill tool or by reading SKILL.md directly. "
        "Do not modify skill files unless explicitly requested."
    )
    claude = shutil.which("claude")
    if not claude:
        raise SystemExit("Claude Code is not installed or is not on PATH")
    extra_tools = effective_extra_tools(os.environ)
    args = build_claude_args(claude, skill_context, argv, extra_tools=extra_tools, base=base, api_key=key)
    model = flag_value(argv, "--model", DEFAULT_MODEL)
    effort = flag_value(argv, "--effort", DEFAULT_EFFORT)
    print(f"[claude-pplx] model={model} effort={effort} via {base}", file=sys.stderr)
    os.execvpe(args[0], args, env)


if __name__ == "__main__":
    main()

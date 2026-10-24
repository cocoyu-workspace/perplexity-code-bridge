import importlib.util
import json
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]


def load_module(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


launcher = load_module("claude_pplx", ROOT / "claude-pplx.py")
statusline = load_module("pplx_statusline", ROOT / "scripts" / "pplx-statusline.py")


class TestClaudePplxLauncher(unittest.TestCase):
    def test_default_model_and_tools(self):
        args = launcher.build_claude_args(
            "/usr/local/bin/claude",
            "skill prompt",
            [],
            base="http://127.0.0.1:18082",
            api_key="test-key",
        )
        self.assertIn("--model", args)
        self.assertEqual(launcher.flag_value(args, "--model", ""), "sonnet-5.5")
        self.assertEqual(launcher.flag_value(args, "--effort", ""), "medium")
        tools = launcher.sent_tools(args)
        for core in ("Read", "Edit", "Write", "Bash", "Glob", "Grep", "Skill", "Agent", "SendMessage", "ToolSearch"):
            self.assertIn(core, tools)

    def test_settings_isolation_and_model_picker(self):
        iso = launcher.subagent_isolation_args(
            ["--model", "opus-5.5"],
            base="http://127.0.0.1:18082",
            api_key="test-key",
            environ={},
        )
        settings_raw = launcher.flag_value(iso, "--settings", "{}")
        settings = json.loads(settings_raw)
        self.assertEqual(settings["model"], "opus-5.5")
        self.assertEqual(settings["env"]["ANTHROPIC_BASE_URL"], "http://127.0.0.1:18082")
        self.assertEqual(settings["env"]["ANTHROPIC_API_KEY"], "test-key")
        self.assertEqual(settings["env"]["ENABLE_TOOL_SEARCH"], "true")
        self.assertTrue(settings["modelPicker"]["replaceBuiltInOptions"])
        models = [m["model"] for m in settings["modelPicker"]["options"]]
        self.assertIn("sonnet-5.5", models)
        self.assertIn("opus-5.5", models)
        self.assertIn("gpt-6.1-sol", models)

    def test_sol_1m_context_window(self):
        env = launcher.subagent_env(["--model", "gpt-6.1-sol"], environ={})
        self.assertEqual(env["CLAUDE_CODE_AUTO_COMPACT_WINDOW"], "1000000")
        self.assertEqual(env["CLAUDE_CODE_MAX_CONTEXT_TOKENS"], "1000000")

    def test_disable_and_extra_tools_env(self):
        env = {
            "CLAUDE_PPLX_EXTRA_TOOLS": "CustomTool",
            "CLAUDE_PPLX_DISABLE_TOOLS": "SendMessage",
        }
        extra = launcher.effective_extra_tools(env)
        self.assertIn("CustomTool", extra)
        self.assertNotIn("SendMessage", extra)
        self.assertIn("ToolSearch", extra)
        with self.assertRaises(ValueError):
            launcher.disabled_tools_from_env({"CLAUDE_PPLX_DISABLE_TOOLS": "Bash"})

    def test_statusline_format(self):
        line = statusline.render_statusline(
            {
                "model": {"display_name": "Claude Sonnet 5.5"},
                "context_window": {"used_percentage": 42, "context_window_size": 200000},
                "workspace": {"current_dir": "/tmp/demo-project"},
            },
            env={"ANTHROPIC_BASE_URL": "http://127.0.0.1:1"},
        )
        self.assertIn("Claude Sonnet 5.5", line)
        self.assertIn("42%", line)
        self.assertIn("demo-project", line)

    def test_discover_skill_context_and_custom_agents(self):
        with tempfile.TemporaryDirectory() as tmp:
            tmp_path = Path(tmp)
            skill_dir = tmp_path / ".agents" / "skills" / "demo-skill"
            skill_dir.mkdir(parents=True)
            (skill_dir / "SKILL.md").write_text(
                "---\nname: demo-skill\ndescription: Demo workflow skill\n---\n# Demo\n",
                encoding="utf-8",
            )
            ctx = launcher.discover_skill_context([], cwd=tmp_path, home=tmp_path)
            self.assertIn("- demo-skill: Demo workflow skill", ctx)
            self.assertIn("SKILL.md", ctx)

            agents_dir = tmp_path / "agents"
            agents_dir.mkdir()
            (agents_dir / "reviewer.md").write_text(
                "---\nname: reviewer\ndescription: Code reviewer\nmodel: sonnet-5.5\ntools:\n  - Read\n  - Grep\n---\nReview carefully.\n",
                encoding="utf-8",
            )
            agents = launcher.load_custom_agents(agents_dir=agents_dir)
            self.assertIn("reviewer", agents)
            self.assertEqual(agents["reviewer"]["description"], "Code reviewer")
            self.assertEqual(agents["reviewer"]["tools"], ["Read", "Grep"])


if __name__ == "__main__":
    unittest.main()

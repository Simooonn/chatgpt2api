#!/usr/bin/env python3
"""Offline unit tests for chatgpt2api-upgrade-agent.py."""

import importlib.util
import json
import os
import sqlite3
import tempfile
import unittest
from pathlib import Path
from unittest import mock

AGENT_PATH = Path(__file__).parent / "chatgpt2api-upgrade-agent.py"
spec = importlib.util.spec_from_file_location("chatgpt2api_upgrade_agent", AGENT_PATH)
agent = importlib.util.module_from_spec(spec)
spec.loader.exec_module(agent)


class UpgradeAgentTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        root = Path(self.tmp.name)
        agent.PROJECT_DIR = root
        agent.CONTROL_DIR = root / "upgrade-control"
        agent.STATE_DIR = root / ".upgrade"
        agent.INBOX_DIR = agent.CONTROL_DIR / "inbox"
        agent.OUTBOX_DIR = agent.CONTROL_DIR / "outbox"
        agent.AGENT_DIR = agent.STATE_DIR / "agent"
        agent.BACKUP_DIR = agent.STATE_DIR / "backups"
        agent.REQUEST_FILE = agent.INBOX_DIR / "request.json"
        agent.STATUS_FILE = agent.OUTBOX_DIR / "status.json"
        agent.AGENT_FILE = agent.OUTBOX_DIR / "agent.json"
        agent.LOCK_FILE = agent.AGENT_DIR / "agent.lock"
        agent.LOG_FILE = agent.AGENT_DIR / "last-run.log"
        agent.COMPOSE_FILE = root / "docker-compose.yml"
        agent.IMAGE_REPO = "ghcr.io/simooonn/chatgpt2api"
        agent.COMPOSE_FILE.write_text(
            "name: chatgpt2api\nservices:\n  chatgpt2api:\n    container_name: chatgpt2api\n"
            "    image: ghcr.io/simooonn/chatgpt2api:v2.1.0-roamai.1\n    restart: unless-stopped\n",
            encoding="utf-8",
        )
        agent.ensure_dirs()

    def tearDown(self):
        self.tmp.cleanup()

    def test_compose_parse_and_rewrite(self):
        self.assertEqual(agent.parse_compose_image(agent.COMPOSE_FILE), "ghcr.io/simooonn/chatgpt2api:v2.1.0-roamai.1")
        prev = agent.rewrite_compose_image(agent.COMPOSE_FILE, "ghcr.io/simooonn/chatgpt2api:v2.1.0-roamai.2")
        self.assertEqual(prev, "ghcr.io/simooonn/chatgpt2api:v2.1.0-roamai.1")
        self.assertEqual(agent.parse_compose_image(agent.COMPOSE_FILE), "ghcr.io/simooonn/chatgpt2api:v2.1.0-roamai.2")

    def test_claim_request_ignores_symlink(self):
        secret = Path(self.tmp.name) / "secret.txt"
        secret.write_text('{"id":"0123456789abcdef","tag":"v2.1.0-roamai.2"}', encoding="utf-8")
        os.symlink(secret, agent.REQUEST_FILE)
        data = agent.claim_request()
        self.assertEqual(data, {})
        self.assertTrue(secret.exists())
        self.assertFalse(agent.REQUEST_FILE.exists())

    def test_sqlite_online_backup(self):
        data_dir = Path(self.tmp.name) / "data"
        data_dir.mkdir()
        db = data_dir / "chatgpt2api.db"
        conn = sqlite3.connect(str(db))
        conn.execute("CREATE TABLE accounts (id INTEGER PRIMARY KEY, email TEXT)")
        conn.execute("INSERT INTO accounts (email) VALUES ('a@example.com')")
        conn.commit()
        conn.close()

        bak = agent.backup_sqlite("v2.1.0-roamai.1")
        self.assertIsNotNone(bak)
        self.assertTrue(bak.is_file())
        verify = sqlite3.connect(str(bak))
        rows = verify.execute("SELECT email FROM accounts").fetchall()
        verify.close()
        self.assertEqual(rows, [("a@example.com",)])

    def test_rejects_non_fork_tag(self):
        rc = agent.execute_upgrade("0123456789abcdef", "v2.1.0", "v2.1.0-roamai.1", "admin")
        self.assertEqual(rc, 1)
        st = json.loads(agent.STATUS_FILE.read_text(encoding="utf-8"))
        self.assertEqual(st["phase"], "failed")

    def test_auto_rollback_on_unhealthy(self):
        with (
            mock.patch.object(agent, "backup_sqlite", return_value=None),
            mock.patch.object(agent, "run_cmd", return_value=""),
            mock.patch.object(agent, "wait_healthy", side_effect=[(False, "timed out"), (True, "healthy")]),
        ):
            rc = agent.execute_upgrade("0123456789abcdef", "v2.1.0-roamai.2", "v2.1.0-roamai.1", "admin")
        self.assertEqual(rc, 1)
        self.assertEqual(agent.parse_compose_image(agent.COMPOSE_FILE), "ghcr.io/simooonn/chatgpt2api:v2.1.0-roamai.1")
        st = json.loads(agent.STATUS_FILE.read_text(encoding="utf-8"))
        self.assertEqual(st["phase"], "failed")
        self.assertIn("已自动回滚", st["message"])

    def test_successful_upgrade_updates_compose_and_heartbeat(self):
        with (
            mock.patch.object(agent, "backup_sqlite", return_value=None),
            mock.patch.object(agent, "run_cmd", return_value=""),
            mock.patch.object(agent, "wait_healthy", return_value=(True, "healthy")),
        ):
            rc = agent.execute_upgrade("0123456789abcdef", "v2.1.0-roamai.2", "v2.1.0-roamai.1", "admin")
        self.assertEqual(rc, 0)
        self.assertEqual(agent.parse_compose_image(agent.COMPOSE_FILE), "ghcr.io/simooonn/chatgpt2api:v2.1.0-roamai.2")
        st = json.loads(agent.STATUS_FILE.read_text(encoding="utf-8"))
        self.assertEqual(st["phase"], "succeeded")
        hb = json.loads(agent.AGENT_FILE.read_text(encoding="utf-8"))
        self.assertEqual(hb["current_tag"], "v2.1.0-roamai.2")


if __name__ == "__main__":
    unittest.main()

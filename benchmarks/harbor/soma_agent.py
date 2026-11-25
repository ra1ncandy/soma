"""Harbor BaseAgent adapter for running soma on Terminal-Bench 2.0 / 2.1."""

import asyncio
import json
import os
import tempfile
from pathlib import Path

from aiohttp import web

from harbor.agents.base import BaseAgent
from harbor.environments.base import BaseEnvironment
from harbor.models.agent.context import AgentContext


class SomaHarborAgent(BaseAgent):
    """Runs soma's Go kernel on the host while executing all shell/file ops inside Harbor's container."""

    @staticmethod
    def name() -> str:
        return "soma"

    def version(self) -> str | None:
        return "0.5.0"

    async def setup(self, environment: BaseEnvironment) -> None:
        return None

    async def run(
        self,
        instruction: str,
        environment: BaseEnvironment,
        context: AgentContext,
    ) -> None:
        repo_root = Path(__file__).resolve().parents[2]
        soma_bin = os.environ.get("SOMA_BIN") or str(repo_root / "bin" / "soma")
        max_turns = str(os.environ.get("SOMA_HARBOR_MAX_TURNS", "30"))

        async def handle_exec(request: web.Request) -> web.Response:
            body = await request.json()
            cmd = str(body.get("command") or "")
            timeout_sec = int(body.get("timeout_sec") or 60)
            res = await environment.exec(command=cmd, timeout_sec=timeout_sec)
            return web.json_response(
                {
                    "exit_code": res.return_code,
                    "stdout": res.stdout or "",
                    "stderr": res.stderr or "",
                }
            )

        app = web.Application()
        app.router.add_post("/exec", handle_exec)
        runner = web.AppRunner(app)
        await runner.setup()
        site = web.TCPSite(runner, "127.0.0.1", 0)
        await site.start()
        port = site._server.sockets[0].getsockname()[1]
        exec_url = f"http://127.0.0.1:{port}/exec"

        with tempfile.TemporaryDirectory(prefix="soma-harbor-") as tmp_dir:
            cmd = [
                soma_bin,
                "harbor-run",
                "--dir",
                tmp_dir,
                "--harness-dir",
                str(repo_root),
                "--exec-url",
                exec_url,
                "--max-turns",
                max_turns,
                "--goal",
                instruction,
                "--json",
            ]
            if self.model_name:
                model_id = self.model_name.split("/", 1)[-1]
                cmd.extend(["--model", model_id])

            try:
                proc = await asyncio.create_subprocess_exec(
                    *cmd,
                    stdout=asyncio.subprocess.PIPE,
                    stderr=asyncio.subprocess.PIPE,
                )
                stdout_b, stderr_b = await proc.communicate()
                stdout_txt = (stdout_b or b"").decode("utf-8", errors="replace").strip()
                stderr_txt = (stderr_b or b"").decode("utf-8", errors="replace").strip()
                if stderr_txt:
                    self.logger.info("soma stderr: %s", stderr_txt[-1000:])
                if stdout_txt:
                    last_line = stdout_txt.splitlines()[-1]
                    try:
                        data = json.loads(last_line)
                        context.n_input_tokens = int(data.get("input_tokens") or 0)
                        context.n_cache_tokens = int(data.get("cache_read_tokens") or 0)
                        context.n_output_tokens = int(data.get("output_tokens") or 0)
                        context.cost_usd = float(data.get("cost_usd") or 0.0)
                        context.metadata = {
                            "status": data.get("status"),
                            "turns_executed": data.get("turns_executed"),
                            "final_summary": data.get("final_summary"),
                            "error": data.get("error"),
                        }
                    except json.JSONDecodeError:
                        self.logger.warning("soma output not JSON: %s", last_line[:400])
            finally:
                await runner.cleanup()

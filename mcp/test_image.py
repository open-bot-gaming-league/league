"""The image (deploy/cloudrun/Dockerfile) copies mcp/'s modules by name:
every module the server and the ranked-table job import must be one of
them, or the image starts and fails on its first import."""

import ast
import os
import re

MCP = os.path.dirname(os.path.abspath(__file__))
DOCKERFILE = os.path.join(MCP, "..", "deploy", "cloudrun", "Dockerfile")
ENTRY = ("server", "ranked")  # the service's command, and the job's (ranked-job.yaml)


def _local_imports(name):
    with open(os.path.join(MCP, name + ".py"), encoding="utf-8") as f:
        tree = ast.parse(f.read())
    for node in ast.walk(tree):
        names = [a.name for a in node.names] if isinstance(node, ast.Import) else \
            [node.module] if isinstance(node, ast.ImportFrom) and node.module and node.level == 0 else []
        for n in names:
            top = n.split(".")[0]
            if os.path.isfile(os.path.join(MCP, top + ".py")):
                yield top


def test_the_image_copies_every_module_the_server_and_the_job_import():
    with open(DOCKERFILE, encoding="utf-8") as f:
        copied = set(re.findall(r"\bmcp/(\w+)\.py\b", "\n".join(l for l in f if l.startswith("COPY "))))
    needed, todo = set(), list(ENTRY)
    while todo:
        name = todo.pop()
        if name not in needed:
            needed.add(name)
            todo.extend(_local_imports(name))
    assert "badges" in needed and needed <= copied, sorted(needed - copied)

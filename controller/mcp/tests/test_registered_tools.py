import os

PROJECT_ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
SERVER_SRC = os.path.join(PROJECT_ROOT, "src", "server.py")
TOOLS_SRC = os.path.join(PROJECT_ROOT, "src", "tools.py")

EXPECTED_TOOLS = {
    "get_user",
    "create_storage_pool",
    "list_storage_pools",
    "get_storage_pool",
    "create_project",
    "list_projects",
    "get_project",
    "delete_project",
    "create_branch",
    "list_branches",
    "get_branch",
    "delete_branch",
    "create_endpoint",
    "list_endpoints",
    "get_endpoint",
    "delete_endpoint",
    "list_tasks",
    "get_task",
}


def _read(path: str) -> str:
    with open(path) as f:
        return f.read()


def test_excluded_user_tools_absent():
    server_src = _read(SERVER_SRC)
    tools_src = _read(TOOLS_SRC)
    for forbidden in ("create_user", "list_users", "createUser", "listUsers"):
        assert forbidden not in server_src
        assert forbidden not in tools_src


def test_all_eighteen_tools_present_in_tools_module():
    tools_src = _read(TOOLS_SRC)
    for name in EXPECTED_TOOLS:
        assert f"def {name}(" in tools_src, f"missing tool def: {name}"
    assert len(EXPECTED_TOOLS) == 18


def test_all_eighteen_registered_in_server():
    server_src = _read(SERVER_SRC)
    for name in EXPECTED_TOOLS:
        assert f"({name}," in server_src, f"{name} not registered in server.py"


def test_runtime_registration_is_exactly_eighteen():
    import sys

    sys.path.insert(0, os.path.join(PROJECT_ROOT, "src"))
    import server

    config = server.parse_config(["--transport", "stdio"])
    mcp = server.build_server(config)
    import asyncio

    registered = asyncio.run(mcp.list_tools())
    assert {t.name for t in registered} == EXPECTED_TOOLS

import os

PROJECT_ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
SERVER_SRC = os.path.join(PROJECT_ROOT, "src", "server.py")
TOOLS_SRC = os.path.join(PROJECT_ROOT, "src", "tools.py")

EXPECTED_TOOLS = {
    "get_user",
    "create_storage_pool",
    "list_storage_pools",
    "get_storage_pool",
    "rename_storage_pool",
    "delete_storage_pool",
    "create_cluster",
    "list_clusters",
    "get_cluster",
    "add_branch_extensions",
    "delete_cluster",
    "create_branch",
    "list_branches",
    "get_branch",
    "delete_branch",
    "create_endpoint",
    "list_endpoints",
    "get_endpoint",
    "delete_endpoint",
    "resize_endpoint",
    "update_branch_settings",
    "update_endpoint_settings",
    "list_tasks",
    "get_task",
    "test_source_connectivity",
    "create_migration",
    "list_migrations",
    "get_migration",
    "get_migration_assessment",
    "start_migration_assessment",
    "provision_migration_target",
    "start_migration",
    "end_migration",
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


def test_all_tools_present_in_tools_module():
    tools_src = _read(TOOLS_SRC)
    for name in EXPECTED_TOOLS:
        assert f"def {name}(" in tools_src, f"missing tool def: {name}"
    assert len(EXPECTED_TOOLS) == 33


def test_all_tools_registered_in_server():
    server_src = _read(SERVER_SRC)
    for name in EXPECTED_TOOLS:
        assert f"({name}," in server_src, f"{name} not registered in server.py"


def test_runtime_registration_matches_expected():
    import sys

    sys.path.insert(0, os.path.join(PROJECT_ROOT, "src"))
    import server

    config = server.parse_config(["--transport", "stdio"])
    mcp = server.build_server(config)
    import asyncio

    registered = asyncio.run(mcp.list_tools())
    assert {t.name for t in registered} == EXPECTED_TOOLS

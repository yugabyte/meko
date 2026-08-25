from server_instructions import AMP_CONTROLLER_SERVER_INSTRUCTIONS


def test_mentions_resource_ladder():
    text = AMP_CONTROLLER_SERVER_INSTRUCTIONS.lower()
    for word in ("storage pool", "cluster", "branch", "endpoint"):
        assert word in text


def test_mentions_owner_user_id_decision():
    text = AMP_CONTROLLER_SERVER_INSTRUCTIONS
    assert "owner_user_id" in text
    assert "create_storage_pool" in text


def test_mentions_async_task_polling():
    text = AMP_CONTROLLER_SERVER_INSTRUCTIONS
    assert "get_task" in text
    assert "succeeded" in text and "failed" in text
    assert "asynchronous" in text.lower()


def test_mentions_migration_workflow_and_poll_surface():
    text = AMP_CONTROLLER_SERVER_INSTRUCTIONS
    for tool in (
        "test_source_connectivity",
        "create_storage_pool",
        "create_migration",
        "start_migration_assessment",
        "get_migration_assessment",
        "provision_migration_target",
        "start_migration",
        "get_migration",
    ):
        assert tool in text
    # The key guidance: migration progress is tracked via get_migration, not
    # get_task.
    assert "get_migration" in text
    for state in ("assessed", "target_ready", "completed"):
        assert state in text
    assert "migration_id" in text
    assert "pool_id" in text

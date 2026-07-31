from server_instructions import AMP_CONTROLLER_SERVER_INSTRUCTIONS


def test_mentions_resource_ladder():
    text = AMP_CONTROLLER_SERVER_INSTRUCTIONS.lower()
    for word in ("storage pool", "project", "branch", "endpoint"):
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

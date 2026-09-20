import pytest


@pytest.fixture
def anyio_backend():
    return "asyncio"


@pytest.hookimpl(hookwrapper=True)
def pytest_runtest_makereport(item, call):
    """Record each phase's result on the item.

    The hub runs as a subprocess now, so when a test times out waiting for it to
    do something, the explanation is in the hub's log rather than in the
    traceback. Fixtures read this to decide whether to print that log.
    """
    outcome = yield
    setattr(item, "report_" + call.when, outcome.get_result())

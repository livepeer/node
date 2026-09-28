"""Exercise the pinned Python runner's paid single-shot call against real Go HTTP servers."""

import asyncio
import sys

from livepeer_gateway.live_runner import (
    LiveRunnerInstance,
    LiveRunnerPriceInfo,
    call_runner,
)


async def main(orchestrator: str, signer: str) -> None:
    runner = LiveRunnerInstance(
        url=f"{orchestrator}/apps/paid-python/app",
        app="paid-python-app",
        runner_id="paid-python",
        mode="single-shot",
        orchestrator_url=orchestrator,
        raw={},
        price_info=LiveRunnerPriceInfo(price=10, currency="wei", unit="fixed"),
    )
    for unit in ("fixed", "seconds"):
        runner_id = "paid-python" if unit == "fixed" else "paid-python-live"
        priced_runner = LiveRunnerInstance(
            url=f"{orchestrator}/apps/{runner_id}/app",
            app=runner.app,
            runner_id=runner_id,
            mode=runner.mode,
            orchestrator_url=runner.orchestrator_url,
            raw=runner.raw,
            price_info=LiveRunnerPriceInfo(price=10, currency="wei", unit=unit),
        )
        response = await call_runner(
            runner=priced_runner,
            signer_url=signer,
            payload={"prompt": "hello"},
            timeout=5.0,
        )
        assert response.data == {"paid": True, "prompt": "hello"}, response.data
        assert response.session_id
    print("Pinned Python runner paid fixed and live calls passed")


if __name__ == "__main__":
    asyncio.run(main(*sys.argv[1:]))

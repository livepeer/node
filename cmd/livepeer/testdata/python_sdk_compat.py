"""Exercise the pinned, unmodified Live Runner Python SDK against a real server."""

import asyncio
import sys

import aiohttp

from livepeer_gateway.live_runner import register_runner


async def main(orchestrator: str, runner_url: str, bootstrap: str) -> None:
    reserved: asyncio.Queue[str] = asyncio.Queue()
    released: asyncio.Queue[str] = asyncio.Queue()

    async def on_reserve(event: object) -> None:
        await reserved.put(event.session_id)

    async def on_release(event: object) -> None:
        await released.put(event.session_id)

    registration = await register_runner(
        orchestrator,
        secret=bootstrap,
        runner_url=runner_url,
        app="python-sdk-compat",
        auto_detect_gpu=False,
        heartbeat_interval_s=0.2,
        on_session_reserve=on_reserve,
        on_session_release=on_release,
    )
    try:
        assert registration.runner_id
        assert registration.o2r_channel
        async with aiohttp.ClientSession() as client:
            async with client.post(
                f"{orchestrator}/apps/{registration.runner_id}/session"
            ) as response:
                assert response.status == 200, await response.text()
                session = await response.json()
            session_id = session["session_id"]
            assert await asyncio.wait_for(reserved.get(), 5) == session_id
            assert session_id in registration.active_session_ids
            async with client.get(session["app_url"] + "/hello") as response:
                assert response.status == 200, await response.text()
                assert await response.text() == "from-runner"
            async with client.get(session["app_url"] + "/credentials") as response:
                assert response.status == 200, await response.text()
                token = (await response.json())["token"]
            proxy = await registration.create_proxy(
                session_id, runner_url, session_token=token
            )
            async with client.get(proxy.url + "/hello") as response:
                assert response.status == 200, await response.text()
                assert await response.text() == "from-runner"
            channels = await registration.create_trickle_channels(
                session_id,
                [{"name": "events", "mime_type": "application/json"}],
                session_token=token,
            )
            assert len(channels) == 1
            channel = channels[0]
            async with client.post(channel["url"] + "/0", data=b'{"ok":true}') as response:
                assert response.status == 200, await response.text()
            async with client.get(channel["url"] + "/0") as response:
                assert response.status == 200, await response.text()
                assert await response.json() == {"ok": True}
            deleted = await registration.remove_trickle_channels(
                session_id, [channel["channel_name"]], session_token=token
            )
            assert deleted == [channel["channel_name"]]
            async with client.post(session["control_url"] + "/stop") as response:
                assert response.status == 200, await response.text()
            assert await asyncio.wait_for(released.get(), 5) == session_id
            assert session_id not in registration.active_session_ids
    finally:
        await registration.close()
    print("Python SDK registration, heartbeat, reserve/release callbacks and unregister passed")


if __name__ == "__main__":
    asyncio.run(main(*sys.argv[1:]))

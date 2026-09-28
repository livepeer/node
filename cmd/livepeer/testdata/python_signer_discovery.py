"""Check retained remote signer discovery with the pinned Python runner."""

import asyncio
import sys

from livepeer_gateway.discovery import discover_runners


async def main(signer_url: str) -> None:
    entries = await discover_runners(signer_url=signer_url, app="discovery-app", gpu="H100")
    assert len(entries) == 1, entries
    runners = entries[0].get("runners", [])
    assert len(runners) == 1, runners
    assert runners[0]["app"] == "discovery-app", runners[0]
    assert runners[0]["gpu"]["name"] == "H100", runners[0]
    print("Pinned Python signer discovery passed")


if __name__ == "__main__":
    asyncio.run(main(sys.argv[1]))

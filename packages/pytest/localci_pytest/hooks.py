"""The core's test hooks: `POST $LOCALCI_API/hooks/test/begin|end?test=<id>`."""
from __future__ import annotations

import os
import urllib.parse
import urllib.request

# The core does real work inside these calls (resetting and dumping coverage, write sets): a short timeout would let
# the next test start in the middle of it. Same wait as the Playwright package.
TIMEOUT = 30.0

# Never through an environment proxy: the API is on this machine.
_opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))


def call(kind: str, test: str) -> None:
    """Calls the hook and waits for the answer. A missing, slow or failing core never fails a test."""
    api = os.environ.get("LOCALCI_API")
    if not api:
        return
    try:
        url = "%s/hooks/test/%s?%s" % (api.rstrip("/"), kind, urllib.parse.urlencode({"test": test}))
        request = urllib.request.Request(url, data=b"", method="POST")
        token = os.environ.get("LOCALCI_HOOK_TOKEN")
        if token:
            request.add_header("Authorization", "Bearer " + token)
        with _opener.open(request, timeout=TIMEOUT) as response:
            response.read()
    except Exception:
        pass

#!/usr/bin/env python3
"""Verify public resource isolation and Management Key enforcement in a real CPA."""

import json
import urllib.request

from run_integration import CPA, MANAGEMENT_KEY, verify_oauth_boundary


def main() -> None:
    req = urllib.request.Request(
        CPA + "/v0/management/plugins/cpa-provider-nexus/console/oauth/start",
        data=b"{}",
        headers={
            "Authorization": "Bearer " + MANAGEMENT_KEY,
            "Content-Type": "application/json",
        },
        method="POST",
    )
    with urllib.request.urlopen(req, timeout=20) as response:
        login = json.loads(response.read())
    assert login["state"], login
    verify_oauth_boundary(login["state"])
    print("PASS: public resources are inert; OAuth callback requires Management Key")


if __name__ == "__main__":
    main()

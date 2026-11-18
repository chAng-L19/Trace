"""Single-request JSONL endpoint for Trace's generic stdio adapter."""

import hashlib
import json
from pathlib import Path
import sys


def main():
    request = json.loads(sys.stdin.readline())
    path = Path(request["path"]).resolve()
    digest = hashlib.sha256()
    size = 0
    with path.open("rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(chunk)
            size += len(chunk)
    print(json.dumps({"path": str(path), "bytes": size, "sha256": digest.hexdigest()}))


if __name__ == "__main__":
    main()

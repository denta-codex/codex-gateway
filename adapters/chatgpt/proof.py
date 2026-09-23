"""ChatGPT Web proof-of-work helpers."""

from __future__ import annotations

import base64
import hashlib
import json
import random
from copy import deepcopy
from datetime import datetime, timezone
from typing import Any


def decode_proof_config(proof_header: str | None) -> list[Any] | None:
    if not proof_header or "gAAAAAB" not in proof_header:
        return None
    encoded = proof_header.split("gAAAAAB", 1)[1].split("~", 1)[0]
    encoded += "=" * (-len(encoded) % 4)
    try:
        parsed = json.loads(base64.b64decode(encoded.encode()).decode())
    except Exception:
        return None
    return parsed if isinstance(parsed, list) else None


def generate_proof_token(
    required: bool,
    seed: str = "",
    difficulty: str = "",
    user_agent: str | None = None,
    proof_config: list[Any] | None = None,
) -> str | None:
    if not required:
        return None

    proof = deepcopy(proof_config) if proof_config is not None else _default_proof_config(user_agent)
    difficulty_length = len(difficulty)
    for attempt in range(100000):
        proof[3] = attempt
        proof_json = json.dumps(proof, separators=(",", ":"))
        proof_base = base64.b64encode(proof_json.encode()).decode()
        hash_value = hashlib.sha3_512((seed + proof_base).encode()).hexdigest()
        if hash_value[:difficulty_length] <= difficulty:
            return "gAAAAAB" + proof_base

    fallback_base = base64.b64encode(f'"{seed}"'.encode()).decode()
    return "gAAAAABwQ8Lk5FbGpA2NcR9dShT6gYjU7VxZ4D" + fallback_base


def _default_proof_config(user_agent: str | None) -> list[Any]:
    screen = random.choice([3008, 4010, 6000]) * random.choice([1, 2, 4])
    now_utc = datetime.now(timezone.utc)
    parse_time = now_utc.strftime("%a, %d %b %Y %H:%M:%S GMT")
    return [
        screen,
        parse_time,
        None,
        0,
        user_agent,
        "https://tcr9i.chat.openai.com/v2/35536E1E-65B4-4D96-9D97-6ADB7EFF8147/api.js",
        "dpl=1440a687921de39ff5ee56b92807faaadce73f13",
        "en",
        "en-US",
        None,
        "plugins−[object PluginArray]",
        random.choice(["_reactListeningcfilawjnerp", "_reactListening9ne2dfo1i47", "_reactListening410nzwhan2a"]),
        random.choice(["alert", "ontransitionend", "onprogress"]),
    ]

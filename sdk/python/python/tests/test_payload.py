# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Cross-platform determinism tests for the embedded-payload transport."""

from __future__ import annotations

import base64
import hashlib
import json
from typing import Mapping

from tau._notebook_pkg import package
from tau._payload import decode

NOTEBOOK_BYTES = json.dumps(
    {
        "nbformat": 4,
        "nbformat_minor": 5,
        "metadata": {
            "kernelspec": {"language": "python", "name": "python3"},
            "language_info": {"name": "python"},
        },
        "cells": [
            {"cell_type": "code", "id": "c0", "metadata": {}, "outputs": [{"x": 1}], "execution_count": 3, "source": ["print(1)"]},
        ],
    }
).encode()
PINNED_ENCODED = """H4sIAAAAAAAC/wXB3Y6juLoA0Hep69EWP0XP0NK5sF2F+UhCdUhhGx8djbBdMSQ2sDuBlDOadz9r/fOyff2+jfP08jP54+U8uq/by8///edl6v3Xy8+Xv+/9+vc037/UPF//1vN0//q+/+dym6eXP15Mf+9ffr58hcp90fpmeONkkV8k/76JxF11gB88jm4w4SAFHrRnV5HWc/cJcT3CD7jop7wcvj/o4VG/meuBVmPnj6/1mxm7y+FbvrXPgy+uNX1/dE8dH57VWF9qX/vCHT6Lq6RyOHw21/oNooM/ZN2lfe0+YdyTKhierVJUZ+NZ0Inb1Ag/DuEx6qSeJY8HuMyjoUW899lmKBtVkv/e+3pTU+O+yuNmLu/jnlRPlbLQJeysk2E4XNoELvMofRvkW/v6wdvk8HlNu89jUvPuebi8v8rLIZEXmxw+u6TmXVq/sfHjrYsOn+1TfjJXfx5fDxdIuotz9fMQd0/mPyhEXajyl3//+Odl6v3Xy8+Xv+/9+vfvdZq+fv9nCS9/vJj+3r/8fIHwvhlRh31SLWrMnZo6q0sX9TRfD49lUQJvejranscPlVaRom4k3t01zYMheFECb3pqbj2H1YhmUWhZlMCbno5WpYddz+OHSqsIyno2olkUWhYl8Kano9WpexLv7prmwRD8oxfY9/zbES+DSiKraRH19HvpRrwogTc9HW1Li6hHy6lhddE6WTQE5VDepo5ngyrdsxeHtRc4Ux6mPUGTSJvBsDwYnq1SwKrL67QnaBJpMxiWryptXOfzrY/zi0qySIoh2vvlqZLX6XxcoGmLdxHlH5/F1f46oblKirXj35lO3HPv3eOLZ2MVHraK86gT7KwntirPwn7CWRUetorzqBPsrHweSV5tKrmfuyRfDWWvhmT/1Um+VuG6a97dJyuq6uhY27TNmb0XpyZirYjYBxtRDsS0R8bO7XshP99z3BR5e2RV2bDm3Ly7U/VY3k+sbturI4w1RVPk700rMWu/WxE3uL2aghGUA9GbTurQCxxVj+X9xOq2vTrCWFM0RY7baw0nJgsRZfizbe2vE5pE2gyG5Q/t8/9KXkf7aVk00btjxA6fRd6e2rj4jFkr4ro4RvlHUxzsrxNKDm9o115Z/Rk3hYjuRXvNis8i/zi2cQFvka1SnBk6bOpymIhvnBzxveNu3ZGr3X++2p5n0YedLRBke8qCPKEcCjwYamcR577n307Efy37qXI6yW/Gt/PulD06UTk1HXdAkAVysIwODkq8SYLvKmWrKQ9Wlix0vLrJE34amoeOGwe0iIDk1040w0fAkRR1pAMelD9aRfNLxx9WT2zVAT9U4laDZgsEWQi4PTJ2bt6L9sjy9xMDK/n3UwozS1E5KOsNymaWJxwMz1YpwCrPEinAKs+cLA/W0L9sl1auE40Dkl870Qx7O1sgyJokD31AORR4MNTOKj2skmdJL6pNjdkkxXGu4gYzlr8fWYNF1FTtqG9A9CZpEXUnveyOswWCrPLFXZ5QDsQMyhe3L1EvOmSLLt3ajXoHBFmgRZBpZ3+d8PNLHNZOVJNxt+8PF+2AIAvUeSD6vufZZigbVZL/rgJe1IgH7U3yYWcLBFkgyALNBsVb++uEB+1NwpMiyLRbe55dpbBzFaK78nkkebWp5DbtTug3vBWGjMgCwWs3ohzo4LRv7T7gteOxIyOyQPBDe7caMvgqbaa9qGLlM6cvs5VicF3Kop5nE5T3tRuzteOxO5+w1z6/Q3mfpajc+aRvQOXNiHr+xZpgeLuQEVkgONLT9QcZkQWCLBBkex4/VFpFQLOxS/LVeBYMmi0QZIEgC9TdNc2DIXjtvNy0jweDZgsEWSmGixQ4gnd31zQP5p0F7fPwYWcLBFkgyEKJQ8+zaEdMJEMcDM9WKeAH0Gzsknw1ngVD8KrSo+159jS0uCnKrkDdCmUzyxMOhmdRz2MH1N07blwVHlZ6d5Of8fNLHFadNlcpqrAn2KuSPfu3uNUTc7vjbIEgCwRZKCtnShbUiJ5kRBYIusC7XBRlTyibWZ5wkKJ2naguPWUBaD2rtHZQNhuU9dwLZBUtVknwKEW9SNpaQwcHNNsMZaNK8t9Q1huUzdyJ4w4IskAOVnt260SzGN/anscPlVaRDnhQ/mildzd5wkHy4qoDDlLUmyqlg9LMPf92QF0EZRWr6bBC0czyhFeVNq7z+dY/ZgsEWQh4MWXtFO2sTpvhSxxsz1/tPq0v2ruHIXhQ/mh7cbBSDK5LWST50ZrERT1Bm6RF1J3woANeTHmwkrogeR2ptMr2drZAkDVJHvqQ3fukWfQ4PDpROTU1z1+sCYa3N6DstRd1JJL89y/WBMPbhYzIAsFPQ4tJ8qP9dcJKHGcLBNkuye+aFmvP81Uk8SZLdpPiYH+dsBLH2QJBVvo8AHWR5JHt+avVSR4MZdcdHZz27dqLxmnfLHq0y+5z3gFBFgiyQPAiR7wYyu57754ikYui7bw74UH5o+1F49QpWzseO6DZZghe1IhPDauL1smiIXhQ/miVzyOgLpI8WpUv7vKUPQ0tginrtBfNvCN6rcL1BxmRBYIsEGSBIAtlPRvRLIpkF5XiTHp3k6dhMZTd9wSnKq1+A/nL9qJx6pStHY/d7jhbIMgCQRYIskDw09Bikvy4dgI/JM+uO+oiyaNV+eIuT9cdEGSBIAsEWSDI9ryzvWicOmVPw6XvhbU9f7U7olddXqc9QdN+wlkX9PJhZwsEWSDIAkEWCLJAkO2S/K5psfY8X0USb7JkNykOayfwQ/LsuqMukjxayIgsELzIET8NLSbJjz/IiCwQZIEg2/t8UZ5d4S2yVXjYKmT/VYlbd2Udddw4Sa47IMgCQRYIfmjvVkMGX6XNtBdVrHzm9GW2Om0GmbArlPeb5K+zTptBJuy6E5GV3t3kaXjuTnhRU7NBeU9VWv0+f872K1m2nmdOltG0J9irkj37t7jVE3O742yBIAsBYyhrpyi7GMquQONNluwmTzhIUW+qlE4HPEouN+1ba+jgoKxi5TOnR7wogTc9NU8oq+Hr9LA6+ct2JzxL/v2QAnZAkAVysIYOgyF4MWXtFO1sz+OHSqtIBxw6cbVdUqzK5xGU9dzxZjMBRz1trfZsUAQ/Ol7/7rhxeztbIMhKnweg8SZLdpMsf6qUhS5pbc9fbZfkd02Ltef5KpJ4kyW7SXH4QUZkgSALBFnF86vh306k9UPyg/11wosSeNNTc+s5rEY0iyLZU1N2EYkMKonP0rubZPlNJfVgqNvUONxV0sSKsrNO8lj72u2nOpI8ugGNN1mym2T5U6UsdEm7kBFZIMgCQbbnnVU8vxr+7URaPyQ/2F4c7KfPV3nCmx7xXSVNrCg76xS7LmQ3lRRXKcD24mA/fb7Kz3kHBFkgyAJBFgiyXZKvhrrV8HYHBFkgyALBd5U0saKt/XXCixJ401Nz6zmsRjSLItldJU2sKDvLqdoUy5+assuOxpss2U2y/Kkpu+yOswWCLBBkoawzHbK7SppYUfbkSbzJkt0ky58qZaFL2lWnjVMssr9O+K6SJla03QFBFgiyQPBdJU2sKDvrFLsuZDeVFFcpYJVicF2c31XSxIq2s+L51fBvtzvOFgiy2hcpvEVW+cqrtLp34rhqzwZJhrUbH7YT9dl4FnTiNnWJot1xtkCQ7ZLvQacH26ZNkLy49zybGjG4LmWRSmFWvrqoJEukqKL9hIMUOKikdjqtNz0d1kYMrktZJBkOUuCgktrptN70eP1BRmSBIAsEWUmZhxIHldROp/VZcfbUSTHJ0/CU/NvvCb7rRN+A1k7Rxw1o7RT9Pvc8u0phlw87WyDIAkEWCLJA8CJHfNeJXmXCoh0xd52YsyndQ570Am9RDsQ8TVm5jkfTh50tEGSBIAsEWSDIAkG2S/LVULYagnKg8VOGbJLiOFdJvampcWo6TnuC/zyfrjsgyAJBFgiyQJAFgiyUdaQ9G9QJ5VDWmQ7Z09DG6QlszzvbJflqKFsNySYpjnOVZIPi7bQ7ofzXCU06ba5SVKEK2KmydlDWmQ7Z09BmM+K4A4IsEGSBIAsEWSDIQllH2rNBnbJUexfJ03BRSRZJnkV7b5whdjKUvRqib0D0tDtdd0CQBYIsEGSBIAsEWSjrSHs2qFPmVcmePbELGZEFgiwQZIEgC2XlTMmCGvHTCOz0aJf9hINKaqfT+qw4e+qkmORpuOtE34DWTtHHDWjtFP0+9zy7SmEXMiILBF2gwIsacdTT1vYJC8qzG9AqA5oNircr0Gzsknw1ngVD8Gbo4PRkFp20tqf5qlIWdMBRT1urfB5JXm0quU36MVsgyELAm0lebZ+woDy76RS7LjysTv6y3QmvKm1c5/OtD/jZCekkwYNMikVNdQT03SqaXzr+sMa7YEo2KMpWM+JBJ/cnGZEFgi5QNrM84WB4FvU8dkDdvePGAZWbHvEA9O60z5wieDEEX1XCnkCzzRA8d0I6oMUqCfYddzcd8Ci53LRvbcezjIzIAkEXoLVT9GGlGFyXskiKwwpFM8sTXhQvJnnCz566hw446mlrddoMyjeD9kdbpTgzdNjU5TABvTvtM6fI6w4IskDZA94i26ZNkLy49zybGjG4LmWRSmE21N0lz2PzFr817fdBxE312bJfjDXnNmJvn1H23oaH7RMWlGc3kWSD4m3eXln9GTeFiO5Fe82KzyL/OLZxsTvOFgiyUqBVl5XTZbV1CXvqMIRO6BuUt0lxFnW8GQx9nz5G/GeV4sFQO32M+GlKmE2Shz64/Hy67oAgCzSPDUE5lGbT/mb3AU0dzwZVumcvDqsUg+tSFkl+XHuBM+VhIiOyQPDaeblpHw+GZKn2LpKnIXRC34DmsSEPazwLOnGbukTR7jhbIMjqslrUdJzlaCIZ4mB4tkoBP6A0QaWNg/K+GdHkVXhY6b9jndiclVUsT9cdEGShrJwpWVAjehDvPBT5WfniLll+hrcoB2LOIomHnmdnEfQPMiILBIeOu6c84c8vUUeSx8UXddGOxkPPX+fd6bp7+fePf16m3n+9/Hzpp96F23j7z7iESb388WL6e//y8+UrVGvn5aZ9PBgCP2ryGJWvvEqreyeas+JuVSmMH5f2Bj52hhbXTjQDXJY/wd+d9plTZf2Q/DB+TLdR0WKVKRtk0o4fY/X4Es2sktdxT6q147GDyzzq0kU9zddDqPI9qW4dzybDi0myfFFebnBZ/gSfDYq348dYPb5EM6vkdTyL6Aa+dop+P+GyqK9QXST/vom0yTRtx4+xuqikcTA+xp4fx4+xuhwI3MDHztDi2olmgMvy5/n0GFXKIl2ySAf4wePoBp69Sl7HhrpNufyiUrYaAj/UxG6KPEad5LH2tYPLomDCoedZtHt7X8DFuRDR/7z8+3///j/YQhQQ5xUAAA=="""
PINNED_DIGEST = "29ca1ce6127163a9d1a2c0548d204f8acfbffe57b5c594aaed4293adbc80aa26"
PINNED_SHA256 = "12e75396635fc34cde534c65024472927c3fbad1e8257013cb0a5cdd942aaeb7"


def _pseudo_random(length: int) -> bytes:
    state = 20261026
    out = bytearray()
    for _ in range(length):
        state = (1103515245 * state + 12345) & 0x7FFFFFFF
        out.append(state >> 16 & 0xFF)
    return bytes(out)


INCOMPRESSIBLE = _pseudo_random(12000)


def _envelope(files: Mapping[str, bytes]) -> bytes:
    return json.dumps(
        {
            "version": 2,
            "files": [{"name": name, "data": base64.b64encode(files[name]).decode("ascii")} for name in sorted(files)],
        },
        separators=(",", ":"),
        ensure_ascii=False,
    ).encode("utf-8")


def test_encode_matches_the_pinned_cross_platform_bytes():
    staged = package(NOTEBOOK_BYTES)
    assert staged.encoding.encoded == PINNED_ENCODED
    assert hashlib.sha256(staged.encoding.encoded.encode()).hexdigest() == PINNED_SHA256


def test_decode_round_trips_the_pinned_cross_platform_bytes():
    staged = package(NOTEBOOK_BYTES)
    assert decode(PINNED_ENCODED, PINNED_DIGEST) == staged.files


def test_digest_is_sha256_of_the_uncompressed_envelope():
    staged = package(NOTEBOOK_BYTES)
    assert staged.encoding.digest == PINNED_DIGEST
    assert staged.encoding.digest == hashlib.sha256(_envelope(staged.files)).hexdigest()

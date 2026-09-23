# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Bounded, source-isolated loss evidence for status polling."""

from __future__ import annotations

import copy
import json
import math
import os
import re
import threading
import time
from collections import OrderedDict
from datetime import datetime, timezone
from urllib.parse import urlencode, urlsplit

import urllib3

from tau._kube_io import bounded_body, timeout
from tau.jupyter.runs import read_owned_logs

LIMIT_BYTES = 65536
MAX_POINTS = 512
MAX_STEP = 9007199254740991
TAG = re.compile(r"(?<!\S)step=(\d+)\s+loss=([-+]?\d*\.?\d+(?:[eE][-+]?\d+)?)(?=\s|$)")


def parse_loss(data):
    reasons = ["tail-window"]
    clipped = len(data) > LIMIT_BYTES
    if clipped:
        reasons.append("byte-limit")
    records = data[:LIMIT_BYTES].split(b"\n")
    records.pop()
    if clipped and records:
        records.pop(0)
    points = {}
    for record in records:
        if len(record) > 4096:
            if "record-limit" not in reasons:
                reasons.append("record-limit")
            continue
        for match in TAG.finditer(record.decode("utf-8", errors="replace")):
            if len(match[1]) > 16:
                continue
            step, value = int(match[1]), float(match[2])
            if step <= MAX_STEP and math.isfinite(value):
                points[step] = value
    if len(points) > MAX_POINTS:
        reasons.append("point-limit")
    return [{"step": step, "value": points[step]} for step in sorted(points)[-MAX_POINTS:]], reasons


def empty(message, source=None):
    return {"state": "unavailable", "source": source, "samples": [], "checkedAt": None,
            "stale": True, "limitBytes": LIMIT_BYTES, "maxPoints": MAX_POINTS,
            "possiblyTruncated": True, "truncationReasons": ["tail-window"], "message": message}



def read_stdout(client, info, source, deadline):
    return read_owned_logs(client, info, source, deadline)


def portal_source(info):
    if os.environ.get("TAUGRID_METRICS_PORTAL_ENABLED") != "1":
        return None
    base = os.environ.get("TAUGRID_METRICS_PORTAL_URL", "")
    parsed = urlsplit(base)
    if parsed.scheme not in ("http", "https") or not parsed.hostname or parsed.username or parsed.password or parsed.query or parsed.fragment:
        return None
    mapping = json.loads(os.environ.get("TAUGRID_METRICS_PORTAL_RUNS", "{}"))
    selection = mapping.get(f"{info.namespace}/{info.kind}/{info.name}/{info.uid}")
    if not isinstance(selection, dict) or not all(isinstance(selection.get(key), str) and selection[key] for key in ("target", "run_id")):
        return None
    return {"type": "portal", "runUid": info.uid, "runId": selection["run_id"], "target": selection["target"], "url": base.rstrip("/")}


def read_portal(source, deadline):
    deadline = min(deadline, time.monotonic() + 5)
    manager = urllib3.PoolManager()
    try:
        response = manager.request("GET", source["url"] + "/api/stellar/series?" + urlencode(
            {"target": source["target"], "metric": "train/loss", "run_id": source["runId"], "max_points": MAX_POINTS}),
            redirect=False, retries=False, preload_content=False,
            timeout=urllib3.Timeout(connect=timeout(deadline)[0], read=timeout(deadline)[1]))
        if response.status != 200:
            response.close()
            raise ValueError("Portal did not return a series")
        data = bounded_body(response, deadline)
        if len(data) > LIMIT_BYTES:
            raise ValueError("Portal series exceeded the byte limit")
        payload = json.loads(data)
        series = [row for row in payload.get("chart", {}).get("series", []) if row.get("run_id") == source["runId"]]
        if len(series) != 1:
            raise ValueError("Portal run mapping did not resolve exactly one series")
        row = series[0]
        points = {}
        for point in row.get("values", []):
            step, value = point.get("step"), point.get("value")
            if type(step) is int and 0 <= step <= MAX_STEP and type(value) in (int, float) and math.isfinite(value):
                points[step] = value
        coverage = {key: row.get(key) for key in ("point_count", "rendered_points", "decimated", "sampling")}
        reasons = ["portal-coverage"]
        if len(points) > MAX_POINTS:
            reasons.append("point-limit")
        return [{"step": step, "value": points[step]} for step in sorted(points)[-MAX_POINTS:]], reasons, coverage
    finally:
        manager.clear()


class Collector:
    def __init__(self, now=time.monotonic):
        self.now = now
        self.lock = threading.Lock()
        self.slots = threading.BoundedSemaphore(4)
        self.entries = OrderedDict()
        self.read = read_stdout

    def collect(self, client, info, force=False):
        configuration = getattr(getattr(getattr(client, "custom", None), "api_client", None), "configuration", None)
        host = getattr(configuration, "host", "")
        target = (host, info.namespace, info.kind, info.name, info.uid)
        now = self.now()
        with self.lock:
            for key, entry in list(self.entries.items()):
                if not entry["busy"] and now - entry["used"] >= 600:
                    del self.entries[key]
            prior = next((entry for entry in reversed(self.entries.values()) if entry["target"] == target), None)
            source = info.metric_sources[0] if len(info.metric_sources) == 1 and not info.metric_discovery_error else None
            if source is None and not info.metric_sources and not info.metric_discovery_error and not prior:
                try:
                    source = portal_source(info)
                except (ValueError, TypeError, AttributeError):
                    source = None
            if (source is None and not info.metric_sources and not info.metric_discovery_error
                    and prior and prior["result"]["source"] and prior["result"]["source"]["type"] == "portal"):
                try:
                    source = portal_source(info)
                except (ValueError, TypeError, AttributeError):
                    source = None
            if not source or not info.uid:
                if prior:
                    prior["used"] = now
                result = copy.deepcopy(prior["result"]) if prior else empty("No unique verified loss source; no trustworthy portal fallback.")
                result.update(stale=True, state="unavailable", message="Loss source is missing, ambiguous or unverified. Last known samples are not refreshed.")
                return result
            if prior and prior["result"]["source"] != source:
                prior["used"] = now
                result = copy.deepcopy(prior["result"])
                result.update(stale=True, state="unavailable", message="Loss source changed. Retaining original evidence; replacement pods and portal series are not substituted or merged.")
                return result
            key = target + (json.dumps(source, sort_keys=True),)
            entry = self.entries.get(key)
            final_attempt = force and (entry is None or not entry.get("terminal_attempted", False))
            if entry and (entry["busy"] or (now - entry["attempted"] < 10 and not final_attempt)):
                entry["used"] = now
                self.entries.move_to_end(key)
                result = copy.deepcopy(entry["result"])
                if entry["busy"]:
                    result.update(stale=True, message="Metrics collection already in progress; showing last known evidence.")
                return result
            if not self.slots.acquire(blocking=False):
                result = copy.deepcopy(entry["result"]) if entry else empty("Metrics capacity reached; retry on next status poll.", source)
                result.update(stale=True)
                return result
            if entry is None:
                if len(self.entries) >= 128:
                    oldest = next((identity for identity, cached in self.entries.items() if not cached["busy"]), None)
                    if oldest is None:
                        self.slots.release()
                        return empty("Metrics cache busy.", source)
                    del self.entries[oldest]
                entry = {"target": target, "result": empty("Waiting for samples.", source)}
                self.entries[key] = entry
            entry.update(busy=True, used=now, attempted=now)
            if force:
                entry["terminal_attempted"] = True
        result = entry["result"]
        try:
            deadline = time.monotonic() + 10
            if source["type"] == "portal":
                samples, reasons, coverage = read_portal(source, deadline)
            else:
                samples, reasons = parse_loss(self.read(client, info, source, deadline))
                coverage = None
            result = {**empty("", source), "state": "ready" if samples else "empty", "samples": samples,
                      "checkedAt": datetime.now(timezone.utc).isoformat(), "stale": False,
                      "truncationReasons": reasons, "coverage": coverage,
                      "message": "Bounded evidence, not the complete run history." if samples else "No explicit finite step/loss records in this bounded window."}
        except Exception:
            result = {**entry["result"], "state": "error", "stale": True,
                      "message": "Metrics could not be refreshed (permission, identity, transport or deadline). Last known evidence is retained."}
        finally:
            with self.lock:
                entry.update(result=result, busy=False, used=self.now(), attempted=self.now())
                self.slots.release()
        return copy.deepcopy(result)


collector = Collector()

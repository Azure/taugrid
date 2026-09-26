# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import io
import threading
import time
from concurrent.futures import ThreadPoolExecutor

import pytest

from tau.jupyter.metrics import Collector, bounded_body, parse_loss
from tau.jupyter.runs import NativeStatus


class Body(io.BytesIO):
    def set_read_timeout(self, timeout):
        assert 0 < timeout <= 5


def test_explicit_finite_samples_and_limits():
    data = b'loss=9\nstep=2 loss=3\nstep=1 loss=4\nstep=2 loss=2\nstep=3 loss=1e999\nstep=9007199254740992 loss=1\nstep=-1 loss=1\n'
    samples, reasons = parse_loss(data)
    assert samples == [{'step': 1, 'value': 4.0}, {'step': 2, 'value': 2.0}]
    assert 'tail-window' in reasons
    samples, reasons = parse_loss(b''.join(f'step={step} loss=1\n'.encode() for step in range(600)))
    assert len(samples) == 512
    assert samples[0]['step'] == 88
    assert 'point-limit' in reasons
    assert 'record-limit' in parse_loss(b'x' * 4097 + b'\n')[1]
    assert parse_loss(b'step=1 loss=2')[0] == []


def test_stream_bound_and_close():
    body = Body(b'x' * 100000)
    data = bounded_body(body, time.monotonic() + 10)
    assert len(data) == 65537
    assert body.closed


def test_identity_documents_use_bounded_nonpreloaded_reads():
    from tau._kube_io import read_document, DOCUMENT_BYTES

    calls = []
    body = Body(b'{"metadata":{"uid":"run"}}')

    def reader(**kwargs):
        calls.append(kwargs)
        return body

    assert read_document(reader, time.monotonic() + 10)["metadata"]["uid"] == "run"
    assert len(calls) == 1
    assert calls[0]["_preload_content"] is False
    assert 0 < calls[0]["_request_timeout"][0] <= 2
    assert 0 < calls[0]["_request_timeout"][1] <= 5
    assert body.closed
    oversized = Body(b" " * (DOCUMENT_BYTES + 1))
    with pytest.raises(ValueError, match="Identity document exceeded"):
        read_document(lambda **kwargs: oversized, time.monotonic() + 10)
    assert oversized.closed


def test_connect_and_read_timeouts_share_remaining_budget(monkeypatch):
    from tau.jupyter import metrics

    monkeypatch.setattr(metrics.time, "monotonic", lambda: 100)
    connect, read = metrics.timeout(101)
    assert connect + read <= 1
    with pytest.raises(TimeoutError):
        metrics.timeout(100)


def test_cache_singleflight_and_error_staleness():
    clock = [0.0]
    collector = Collector(now=lambda: clock[0])
    info = NativeStatus(name='train', namespace='ray', uid='run')
    source = {'type': 'stdout', 'runUid': 'run', 'podUid': 'pod', 'pod': 'driver', 'container': 'main', 'ownerKind': 'Job', 'ownerUid': 'job'}
    info.metric_sources = [source]
    entered, release = threading.Event(), threading.Event()
    calls = []

    def read(client, status, selected, deadline):
        calls.append(selected)
        entered.set()
        assert release.wait(2)
        return b'step=1 loss=2\n'

    collector.read = read
    with ThreadPoolExecutor() as pool:
        task = pool.submit(collector.collect, None, info)
        assert entered.wait(2)
        assert collector.collect(None, info)['stale']
        release.set()
        first = task.result()
    assert collector.collect(None, info) == first
    assert len(calls) == 1
    clock[0] = 11

    def fail(*args):
        raise OSError('gone')

    collector.read = fail
    failed = collector.collect(None, info)
    assert failed['samples'] == first['samples']
    assert failed['checkedAt'] == first['checkedAt']
    assert failed['stale']
    info.uid = 'replacement'
    info.metric_sources = []
    assert collector.collect(None, info)['samples'] == []


def metric_status(uid='run', pod_uid='pod'):
    info = NativeStatus(name='train', namespace='ray', uid=uid)
    info.metric_sources = [{'type': 'stdout', 'runUid': uid, 'podUid': pod_uid,
                            'pod': 'driver', 'container': 'main', 'ownerKind': 'Job', 'ownerUid': 'job'}]
    return info


def test_terminal_bypasses_ttl_once_and_source_changes_do_not_replace_evidence():
    clock = [0.0]
    collector = Collector(now=lambda: clock[0])
    calls = []

    def read(*args):
        calls.append(1)
        return f'step={len(calls)} loss=2\n'.encode()

    collector.read = read
    info = metric_status()
    collector.collect(None, info)
    final = collector.collect(None, info, force=True)
    assert len(calls) == 2
    assert collector.collect(None, info, force=True) == final
    assert len(calls) == 2
    clock[0] = 11
    collector.collect(None, info, force=True)
    assert len(calls) == 3
    changed = collector.collect(None, metric_status(pod_uid='replacement'))
    assert changed['stale'] and changed['state'] == 'unavailable'
    assert changed['source']['podUid'] == 'pod'
    assert changed['samples'] == [{'step': 3, 'value': 2.0}]
    assert 'changed' in changed['message']
    assert len(calls) == 3


def test_cache_lru_idle_expiry_and_cached_errors():
    clock = [0.0]
    collector = Collector(now=lambda: clock[0])
    collector.read = lambda *args: b'step=1 loss=1\n'
    for index in range(128):
        collector.collect(None, metric_status(uid=str(index)))
    collector.collect(None, metric_status(uid='0'))
    collector.collect(None, metric_status(uid='new'))
    assert len(collector.entries) == 128
    assert any(entry['target'][-1] == '0' for entry in collector.entries.values())
    assert not any(entry['target'][-1] == '1' for entry in collector.entries.values())
    clock[0] = 600
    collector.collect(None, metric_status(uid='after-idle'))
    assert len(collector.entries) == 1
    calls = []

    def fail(*args):
        calls.append(1)
        raise OSError('denied')

    collector.read = fail
    failed = collector.collect(None, metric_status(uid='error'))
    assert failed['state'] == 'error'
    assert collector.collect(None, metric_status(uid='error')) == failed
    assert len(calls) == 1


def test_capacity_is_bounded_without_waiting():
    collector = Collector()
    release = threading.Event()
    entered = threading.Barrier(5)

    def read(*args):
        entered.wait(timeout=5)
        assert release.wait(5)
        return b'step=1 loss=1\n'

    collector.read = read
    with ThreadPoolExecutor(max_workers=4) as pool:
        tasks = [pool.submit(collector.collect, None, metric_status(uid=str(index))) for index in range(4)]
        try:
            entered.wait(timeout=5)
            saturated = collector.collect(None, metric_status(uid='fifth'))
            assert saturated['state'] == 'unavailable' and saturated['stale']
            assert 'capacity' in saturated['message']
            assert len(collector.entries) == 4
        finally:
            release.set()
        assert all(task.result()['state'] == 'ready' for task in tasks)


def test_read_deadline_failure_closes_body_and_clipped_records_are_not_samples():
    body = Body(b'step=1 loss=2\n')
    with pytest.raises(TimeoutError):
        bounded_body(body, time.monotonic() - 1)
    assert body.closed
    data = b'step=0 loss=9\nstep=1 loss=2\n' + b'x' * 65536
    samples, reasons = parse_loss(data)
    assert samples == [{'step': 1, 'value': 2.0}]
    assert 'byte-limit' in reasons and 'tail-window' in reasons


def test_portal_requires_explicit_uid_mapping_and_preserves_source(monkeypatch):
    from tau.jupyter.metrics import portal_source

    info = metric_status()
    monkeypatch.setenv('TAUGRID_PORTAL_URL', 'http://portal.example')
    monkeypatch.delenv('TAUGRID_METRICS_PORTAL_ENABLED', raising=False)
    assert portal_source(info) is None
    monkeypatch.setenv('TAUGRID_METRICS_PORTAL_ENABLED', '1')
    monkeypatch.setenv('TAUGRID_METRICS_PORTAL_URL', 'http://portal.example')
    monkeypatch.setenv('TAUGRID_METRICS_PORTAL_RUNS', '{"ray/RayJob/train/run":{"target":"experiment","run_id":"exact-run"}}')
    info.kind = 'RayJob'
    selected = portal_source(info)
    assert selected['runId'] == 'exact-run'
    info.uid = 'replacement'
    assert portal_source(info) is None


def test_prior_portal_cannot_override_failed_discovery(monkeypatch):
    import tau.jupyter.metrics as metrics

    info = metric_status()
    info.metric_sources = []
    source = {"type": "portal", "runUid": info.uid, "target": "target", "runId": "run", "url": "http://portal"}
    monkeypatch.setattr(metrics, "portal_source", lambda info: source)
    calls = []
    monkeypatch.setattr(metrics, "read_portal", lambda *args: (calls.append(1) or ([{"step": 1, "value": 2}], [], {})))
    clock = [0]
    collector = Collector(now=lambda: clock[0])
    assert collector.collect(None, info)["state"] == "ready"
    clock[0] = 11
    info.metric_discovery_error = True
    result = collector.collect(None, info)
    assert result["state"] == "unavailable" and result["stale"]
    assert len(calls) == 1


def test_metric_cache_is_partitioned_by_cluster():
    from types import SimpleNamespace

    def client(host):
        return SimpleNamespace(custom=SimpleNamespace(api_client=SimpleNamespace(configuration=SimpleNamespace(host=host))))

    collector = Collector()
    calls = []
    collector.read = lambda *args: (calls.append(1) or b"step=1 loss=2\n")
    collector.collect(client("https://cluster-a"), metric_status())
    collector.collect(client("https://cluster-b"), metric_status())
    assert len(calls) == 2

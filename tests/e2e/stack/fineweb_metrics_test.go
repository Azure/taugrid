// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package stack

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseFineWebMetricsAggregatesSampledHosts(t *testing.T) {
	logs := `
(RayTrainWorker pid=1) FINEWEB_PERF_METRICS_JSON {"batch_size_per_rank":1,"block_size":1024,"duration_seconds":2.0,"global_tokens":32768,"steps":2,"tokens_per_second":16384.0,"world_size":16}
(RayTrainWorker pid=1) FINEWEB_IB_METRICS_JSON {"ranks":[{"rank":0,"local_rank":0,"hostname":"h200-a","sampled":true,"counters":{"mlx5_0/1/port_xmit_data_bytes":1000,"mlx5_0/1/port_rcv_data_bytes":900,"mlx5_0/1/port_xmit_packets":10,"mlx5_0/1/port_rcv_packets":9,"mlx5_0/1/link_downed":0}},{"rank":1,"local_rank":1,"hostname":"h200-a","sampled":false,"counters":{}},{"rank":8,"local_rank":0,"hostname":"h200-b","sampled":true,"counters":{"mlx5_0/1/port_xmit_data_bytes":1100,"mlx5_0/1/port_rcv_data_bytes":950,"mlx5_0/1/port_xmit_packets":11,"mlx5_0/1/port_rcv_packets":10,"mlx5_0/1/port_rcv_errors":0}}]}`

	got, err := parseFineWebMetrics(logs)
	require.NoError(t, err)
	require.Equal(t, 2, got.InfiniBand.Hosts)
	require.EqualValues(t, 2100, got.InfiniBand.TransmitBytes)
	require.EqualValues(t, 1850, got.InfiniBand.ReceiveBytes)
	require.EqualValues(t, 21, got.InfiniBand.TransmitPackets)
	require.EqualValues(t, 19, got.InfiniBand.ReceivePackets)
	require.Zero(t, got.InfiniBand.ErrorEvents)
	require.Equal(t, 16384.0, got.Performance.TokensPerSecond)
}

func TestParseFineWebMetricsRequiresSentinels(t *testing.T) {
	_, err := parseFineWebMetrics("NET/IB : Using mlx5")
	require.ErrorContains(t, err, "FINEWEB_PERF_METRICS_JSON")
}

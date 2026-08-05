package e2e

import (
	"os"
	"strings"
	"testing"
)

// readFineWebFixture loads the FineWeb IB RayJob fixture once for the shape tests.
func readFineWebFixture(t *testing.T) string {
	t.Helper()
	path, err := findRepoFile("tests/e2e/stack/fixtures/fineweb-rayjob-16xh200-ib.yaml")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fineweb-rayjob-16xh200-ib.yaml: %v", err)
	}
	return string(data)
}

// TestFineWebFixtureRequestsInfiniBand asserts the worker requests the RDMA shared
// device, adds IPC_LOCK, and provides a real 16Gi /dev/shm — the three pod-level
// requirements for NCCL to use GPUDirect RDMA over InfiniBand.
func TestFineWebFixtureRequestsInfiniBand(t *testing.T) {
	text := readFineWebFixture(t)

	// rdma/rdma_shared_device_a must appear in both requests and limits.
	if got := strings.Count(text, "rdma/rdma_shared_device_a:"); got != 2 {
		t.Fatalf("expected rdma/rdma_shared_device_a in worker requests and limits (2), got %d", got)
	}
	if !strings.Contains(text, "IPC_LOCK") {
		t.Fatal("expected worker securityContext to add the IPC_LOCK capability for RDMA")
	}
	// The worker must run as root so the added IPC_LOCK capability is effective;
	// otherwise mlock is bound by RLIMIT_MEMLOCK and IB's ibv_create_cq fails with
	// "Cannot allocate memory".
	if !strings.Contains(text, "runAsUser: 0") {
		t.Fatal("expected the worker to run as root (runAsUser: 0) so CAP_IPC_LOCK is effective for IB memory registration")
	}
	for _, want := range []string{"medium: Memory", "sizeLimit: 16Gi", "mountPath: /dev/shm"} {
		if !strings.Contains(text, want) {
			t.Fatalf("expected a 16Gi tmpfs /dev/shm emptyDir (%q) to avoid the NCCL SHM crash", want)
		}
	}
}

// TestFineWebFixtureInjectsRDMAUserspace asserts the fixture stages the rdma-core
// userspace stack into the worker. The base Ray image (Azure Linux) ships no
// libibverbs, so without this injection NCCL logs "Failed to open libibverbs.so"
// and falls back to the TCP socket transport — InfiniBand silently never engages.
func TestFineWebFixtureInjectsRDMAUserspace(t *testing.T) {
	text := readFineWebFixture(t)

	for _, want := range []string{
		"name: inject-ib-libs",       // the staging initContainer
		"tdnf install -y libibverbs", // installs the RDMA userspace stack
		"librdmacm",                  // rdmacm dependency
		"libnl3",                     // provider transitive dependency
		"/usr/lib/libibverbs/",       // the mlx5 verbs provider directory
		"/etc/libibverbs.d",          // libibverbs driver config search path
		"name: ib-libs",              // shared volume carrying the staged libs
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("expected the FineWeb fixture to stage rdma-core for NCCL IB (%q missing); without it NCCL falls back to NET/Socket", want)
		}
	}

	// The worker must put the injected libs on LD_LIBRARY_PATH and mount the
	// provider + driver configs at the paths libibverbs searches by default.
	for _, want := range []string{
		"name: LD_LIBRARY_PATH",
		"value: /opt/ib/lib",
		"mountPath: /usr/lib/libibverbs",
		"mountPath: /etc/libibverbs.d",
		"subPath: providers",
		"subPath: drivers",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("expected the worker to expose the injected RDMA stack to NCCL (%q missing)", want)
		}
	}
}

// TestFineWebFixtureSplits8x8 asserts the workers spread evenly across the two
// H200 nodes using a topology spread constraint keyed on a dedicated label.
func TestFineWebFixtureSplits8x8(t *testing.T) {
	text := readFineWebFixture(t)

	for _, want := range []string{
		"topologySpreadConstraints:",
		"maxSkew: 1",
		"topologyKey: kubernetes.io/hostname",
		"whenUnsatisfiable: DoNotSchedule",
		"e2e-test: fineweb-16xh200-ib",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("expected the 8+8 topology spread (%q) on the worker template", want)
		}
	}
	// The dedicated label must appear at least twice: once on the worker template
	// metadata and once in the topology spread labelSelector.
	if got := strings.Count(text, "e2e-test: fineweb-16xh200-ib"); got < 2 {
		t.Fatalf("expected the dedicated worker label on both the template and the spread selector, got %d", got)
	}
}

// TestFineWebFixtureUsesInfiniBandNCCLEnv asserts IB NCCL env is wired and that
// NCCL_IB_DISABLE is NOT set (which would force the socket transport).
func TestFineWebFixtureUsesInfiniBandNCCLEnv(t *testing.T) {
	text := readFineWebFixture(t)

	for _, want := range []string{"NCCL_IB_HCA:", "NCCL_SOCKET_IFNAME:", "NCCL_DEBUG:"} {
		if !strings.Contains(text, want) {
			t.Fatalf("expected IB NCCL env var %q in the runtime env", want)
		}
	}
	if strings.Contains(text, "NCCL_IB_DISABLE") {
		t.Fatal("NCCL_IB_DISABLE must NOT be set on the FineWeb IB fixture; it would force the socket transport")
	}
}

// TestFineWebFixturePinsSubmitterAndHeadToCPUPool asserts the submitter and head
// stay on the CPU node pool and that the GPU workers carry a GPU request.
func TestFineWebFixturePinsSubmitterAndHeadToCPUPool(t *testing.T) {
	text := readFineWebFixture(t)

	if got := strings.Count(text, "{{RAY_SUBMITTER_NODE_SELECTOR_KEY}}: {{RAY_SUBMITTER_NODE_SELECTOR_VALUE}}"); got != 2 {
		t.Fatalf("expected submitter and head to be pinned to the CPU pool selector (2), got %d", got)
	}
	if !strings.Contains(text, "{{GPU_NODE_SELECTOR_KEY}}: {{GPU_NODE_SELECTOR_VALUE}}") {
		t.Fatal("expected workers to target the GPU node selector")
	}
	if got := strings.Count(text, "nvidia.com/gpu:"); got != 2 {
		t.Fatalf("expected nvidia.com/gpu in worker requests and limits (2), got %d", got)
	}
}

// TestFineWebFixtureSetsStartupProbe asserts the head and worker carry the 10/90
// startup probes used by the large-GPU path (slower image pulls than the smoke
// fixtures, which use 5/60).
func TestFineWebFixtureSetsStartupProbe(t *testing.T) {
	text := readFineWebFixture(t)

	if got := strings.Count(text, "startupProbe:"); got != 2 {
		t.Fatalf("expected startupProbe on head and worker Ray containers, got %d", got)
	}
	for _, want := range []string{"periodSeconds: 10", "timeoutSeconds: 2", "failureThreshold: 90"} {
		if got := strings.Count(text, want); got != 2 {
			t.Fatalf("expected %q twice, got %d", want, got)
		}
	}
	for _, want := range []string{"path: /api/version", "port: 8265", "path: /api/healthz", "port: 52365"} {
		if got := strings.Count(text, want); got != 1 {
			t.Fatalf("expected %q once, got %d", want, got)
		}
	}
}

// TestFineWebWorkloadEmitsCheckpointSentinel asserts the Python workload emits the
// parseable first-checkpoint and success sentinels the Go test depends on, and
// shards with FSDP rather than DDP.
func TestFineWebWorkloadEmitsCheckpointSentinel(t *testing.T) {
	path, err := findRepoFile("tests/e2e/stack/fixtures/fineweb_ray_train.py")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fineweb_ray_train.py: %v", err)
	}
	text := string(data)

	for _, want := range []string{
		"FINEWEB_FIRST_CHECKPOINT step=",
		"params=",
		"bytes=",
		"FINEWEB_RAY_TRAIN_SUCCESS",
		"FullyShardedDataParallel",
		"FULL_STATE_DICT",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("expected FineWeb workload to contain %q", want)
		}
	}
	if strings.Contains(text, "prepare_model(") {
		t.Fatal("FineWeb workload should wrap with FSDP manually, not ray.train.torch.prepare_model (which defaults to DDP)")
	}
}

// TestFineWebDefaultsCoverUint16TokenShards guards the default/effective contract
// for the tokenized FineWeb .bin shards. The shards are read as uint16 token IDs,
// so the workload must cover the full uint16 range even when a caller still passes
// the old GPT-2 padded vocab default.
func TestFineWebDefaultsCoverUint16TokenShards(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		want string
	}{
		{
			name: "fixture-renderer",
			path: "tests/e2e/testcontext.go",
			want: `{"{{FINEWEB_VOCAB_SIZE}}", "FINEWEB_VOCAB_SIZE", "65536"}`,
		},
		{
			name: "ray-workload",
			path: "tests/e2e/stack/fixtures/fineweb_ray_train.py",
			want: "DEFAULT_FINEWEB_VOCAB_SIZE = 65536",
		},
		{
			name: "ray-workload-effective-vocab",
			path: "tests/e2e/stack/fixtures/fineweb_ray_train.py",
			want: "return max(configured_vocab_size, DEFAULT_FINEWEB_VOCAB_SIZE)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, err := findRepoFile(tc.path)
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", tc.path, err)
			}
			if !strings.Contains(string(data), tc.want) {
				t.Fatalf("expected %s to default FineWeb vocab to 65536 for uint16 token shards", tc.path)
			}
		})
	}
}

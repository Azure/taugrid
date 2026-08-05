package ray

import (
	"context"
	"errors"
	"testing"
)

// fakeReader returns canned Services JSON (and records the namespace it was
// asked for) so the board can be tested without a Kubernetes API.
type fakeReader struct {
	json     string
	err      error
	lastNS   string
	podsJSON string
	podsErr  error
}

func (f *fakeReader) ListServices(_ context.Context, namespace string) ([]byte, error) {
	f.lastNS = namespace
	if f.err != nil {
		return nil, f.err
	}
	return []byte(f.json), nil
}

// ListPods returns canned Pods JSON. When podsJSON is empty it returns an empty
// list so tests that don't care about readiness degrade cleanly.
func (f *fakeReader) ListPods(_ context.Context, _ string) ([]byte, error) {
	if f.podsErr != nil {
		return nil, f.podsErr
	}
	if f.podsJSON == "" {
		return []byte(`{"items":[]}`), nil
	}
	return []byte(f.podsJSON), nil
}

// servicesJSON is a mixed Services list: three KubeRay head Services (carrying
// ray.io/cluster + ray.io/node-type=head + correct <cluster>-head-svc name), one
// worker Service, one mislabeled Service (head label but wrong name convention),
// and one plain Service. Only the three correctly-named head Services should be
// discovered.
const servicesJSON = `{"items":[
  {"metadata":{"name":"alpha-head-svc","namespace":"ray",
    "labels":{"ray.io/cluster":"alpha","ray.io/node-type":"head"}},
   "spec":{"type":"ClusterIP"}},
  {"metadata":{"name":"bravo-head-svc","namespace":"ray",
    "labels":{"ray.io/cluster":"bravo","ray.io/node-type":"head"}},
   "spec":{"type":"ClusterIP"}},
  {"metadata":{"name":"charlie-head-svc","namespace":"team-b",
    "labels":{"ray.io/cluster":"charlie","ray.io/node-type":"head"}},
   "spec":{"type":"ClusterIP"}},
  {"metadata":{"name":"alpha-worker-svc","namespace":"ray",
    "labels":{"ray.io/cluster":"alpha","ray.io/node-type":"worker"}},
   "spec":{"type":"ClusterIP"}},
  {"metadata":{"name":"mislabeled-svc","namespace":"ray",
    "labels":{"ray.io/cluster":"delta","ray.io/node-type":"head"}},
   "spec":{"type":"ClusterIP"}},
  {"metadata":{"name":"unrelated","namespace":"ray"},
   "spec":{"type":"ClusterIP"}}
]}`

func TestBoardDiscoversHeadServices(t *testing.T) {
	r := &fakeReader{json: servicesJSON}
	snap, err := Board(context.Background(), r, Options{})
	if err != nil {
		t.Fatalf("Board: %v", err)
	}

	// Only the three head Services survive; worker and unrelated drop out.
	if snap.Total != 3 || len(snap.Clusters) != 3 {
		t.Fatalf("total = %d, clusters = %d, want 3/3: %+v", snap.Total, len(snap.Clusters), snap.Clusters)
	}

	byName := map[string]Cluster{}
	for _, c := range snap.Clusters {
		byName[c.Name] = c
	}

	if got := byName["alpha"]; got.ProxyPath != "/api/portal/ray/proxy/ray/alpha/" || got.Namespace != "ray" {
		t.Fatalf("alpha = %+v, want proxyPath /api/portal/ray/proxy/ray/alpha/", got)
	}
	if got := byName["charlie"]; got.ProxyPath != "/api/portal/ray/proxy/team-b/charlie/" || got.Namespace != "team-b" {
		t.Fatalf("charlie = %+v, want proxyPath in team-b", got)
	}
}

func TestBoardSortOrder(t *testing.T) {
	snap, err := Board(context.Background(), &fakeReader{json: servicesJSON}, Options{})
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	want := []string{"alpha", "bravo", "charlie"} // ns-then-name ordering
	for i, w := range want {
		if snap.Clusters[i].Name != w {
			t.Fatalf("clusters[%d] = %q, want %q (order %v)", i, snap.Clusters[i].Name, w, want)
		}
	}
}

func TestBoardEmptyIsNotError(t *testing.T) {
	// No head Services → empty board, no error.
	snap, err := Board(context.Background(), &fakeReader{json: `{"items":[]}`}, Options{})
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	if snap.Total != 0 {
		t.Fatalf("total = %d, want 0", snap.Total)
	}
	if snap.Clusters == nil {
		t.Fatal("clusters is nil, want non-nil so it serializes as []")
	}
}

func TestBoardNamespaceScope(t *testing.T) {
	r := &fakeReader{json: `{"items":[]}`}
	if _, err := Board(context.Background(), r, Options{Namespace: "ray"}); err != nil {
		t.Fatalf("Board: %v", err)
	}
	if r.lastNS != "ray" {
		t.Fatalf("reader namespace = %q, want ray", r.lastNS)
	}
}

func TestBoardPropagatesError(t *testing.T) {
	sentinel := errors.New("api down")
	_, err := Board(context.Background(), &fakeReader{err: sentinel}, Options{})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want it to wrap %v", err, sentinel)
	}
}

func TestBoardRejectsBadJSON(t *testing.T) {
	_, err := Board(context.Background(), &fakeReader{json: `not json`}, Options{})
	if err == nil {
		t.Fatal("want decode error on malformed services JSON")
	}
}

// podsJSON marks alpha's head pod Ready and bravo's head pod NotReady; charlie
// has no pod at all. Only alpha should be reported Available.
const podsJSON = `{"items":[
  {"metadata":{"namespace":"ray",
    "labels":{"ray.io/cluster":"alpha","ray.io/node-type":"head"}},
   "status":{"conditions":[{"type":"Ready","status":"True"}]}},
  {"metadata":{"namespace":"ray",
    "labels":{"ray.io/cluster":"bravo","ray.io/node-type":"head"}},
   "status":{"conditions":[{"type":"Ready","status":"False"}]}},
  {"metadata":{"namespace":"ray",
    "labels":{"ray.io/cluster":"alpha","ray.io/node-type":"worker"}},
   "status":{"conditions":[{"type":"Ready","status":"True"}]}}
]}`

// TestBoardAvailabilityFromHeadPodReadiness verifies each cluster's Available
// flag tracks whether its Ray head pod is Ready.
func TestBoardAvailabilityFromHeadPodReadiness(t *testing.T) {
	snap, err := Board(context.Background(), &fakeReader{json: servicesJSON, podsJSON: podsJSON}, Options{})
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	byName := map[string]Cluster{}
	for _, c := range snap.Clusters {
		byName[c.Name] = c
	}
	if !byName["alpha"].Available {
		t.Error("alpha head pod is Ready, want Available=true")
	}
	if byName["bravo"].Available {
		t.Error("bravo head pod is NotReady, want Available=false")
	}
	if byName["charlie"].Available {
		t.Error("charlie has no head pod, want Available=false")
	}
}

// TestBoardDegradesToAvailableOnPodListError verifies a pod-list failure never
// hides a healthy cluster's link: all clusters default to Available=true.
func TestBoardDegradesToAvailableOnPodListError(t *testing.T) {
	snap, err := Board(context.Background(), &fakeReader{json: servicesJSON, podsErr: errors.New("pods api down")}, Options{})
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	for _, c := range snap.Clusters {
		if !c.Available {
			t.Errorf("%s: Available=false on pod-list error, want degrade to true", c.Name)
		}
	}
}

// TestIsHeadServiceRequiresHeadNodeType verifies the discovery filter: a
// Service must carry both ray.io/cluster and ray.io/node-type=head.
func TestIsHeadServiceRequiresHeadNodeType(t *testing.T) {
	head := serviceObj{}
	head.Metadata.Name = "alpha-head-svc"
	head.Metadata.Labels = map[string]string{clusterLabel: "alpha", nodeTypeLabel: nodeTypeHead}
	if !isHeadService(head) {
		t.Fatal("head Service with ray.io/node-type=head should match")
	}

	// Correct labels but wrong service name (mislabeled).
	mislabeled := serviceObj{}
	mislabeled.Metadata.Name = "wrong-name"
	mislabeled.Metadata.Labels = map[string]string{clusterLabel: "alpha", nodeTypeLabel: nodeTypeHead}
	if isHeadService(mislabeled) {
		t.Fatal("Service with head label but wrong name convention should not match")
	}

	worker := serviceObj{}
	worker.Metadata.Name = "alpha-worker-svc"
	worker.Metadata.Labels = map[string]string{clusterLabel: "alpha", nodeTypeLabel: "worker"}
	if isHeadService(worker) {
		t.Fatal("worker Service should not match")
	}

	plain := serviceObj{}
	plain.Metadata.Name = "alpha-head-svc"
	plain.Metadata.Labels = map[string]string{nodeTypeLabel: nodeTypeHead}
	if isHeadService(plain) {
		t.Fatal("Service without ray.io/cluster should not match")
	}
}

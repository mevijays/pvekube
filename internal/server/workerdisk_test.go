package server

import (
	"net/url"
	"pvekube/internal/jobs"
	"testing"
)

func TestParseWorkerDiskForm(t *testing.T) {
	p, err := parseWorkerDiskForm(url.Values{"storage": {" local-lvm "}, "size_gib": {"150"}})
	if err != nil || p.Storage != "local-lvm" || p.SizeGiB != 150 {
		t.Fatalf("p=%+v err=%v", p, err)
	}
	bad := map[string]url.Values{
		"no storage":       {"size_gib": {"10"}},
		"no size":          {"storage": {"local"}},
		"fractional size":  {"storage": {"local"}, "size_gib": {"1.5"}},
		"text size":        {"storage": {"local"}, "size_gib": {"big"}},
		"zero":             {"storage": {"local"}, "size_gib": {"0"}},
		"negative":         {"storage": {"local"}, "size_gib": {"-4"}},
		"option injection": {"storage": {"local,backup=1"}, "size_gib": {"10"}},
		"absurd size":      {"storage": {"local"}, "size_gib": {"99999999"}},
	}
	for name, form := range bad {
		if _, err := parseWorkerDiskForm(form); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestWorkerDiskPolicyRoundTrip(t *testing.T) {
	s := &Server{db: migratedDB(t)}
	if _, ok := s.getWorkerDiskPolicy("devk8s"); ok {
		t.Fatal("a new cluster must have no policy")
	}
	if err := s.setWorkerDiskPolicy("devk8s", workerDiskPolicy{"local-lvm", 100}); err != nil {
		t.Fatal(err)
	}
	// Setting again must replace, not duplicate or fail.
	if err := s.setWorkerDiskPolicy("devk8s", workerDiskPolicy{"ceph", 200}); err != nil {
		t.Fatal(err)
	}
	p, ok := s.getWorkerDiskPolicy("devk8s")
	if !ok || p.Storage != "ceph" || p.SizeGiB != 200 {
		t.Fatalf("p=%+v ok=%v", p, ok)
	}
	if _, ok := s.getWorkerDiskPolicy("prodk8s"); ok {
		t.Fatal("policies must be per cluster")
	}
}

// attachWorkerDisks must not add a step when the cluster has no policy —
// otherwise every upgrade would grow a pointless extra step.
func TestAttachWorkerDisksWithoutPolicyLeavesSpecAlone(t *testing.T) {
	s := &Server{db: migratedDB(t)}
	spec := newTestSpec()
	before := len(spec.Steps)
	if got := s.attachWorkerDisks("devk8s", spec); len(got.Steps) != before {
		t.Fatalf("steps %d -> %d", before, len(got.Steps))
	}
}

func newTestSpec() *jobs.Spec {
	return jobs.NewSpec("t", "t").Step("one", func(*jobs.Ctx) error { return nil })
}

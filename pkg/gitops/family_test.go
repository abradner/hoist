package gitops

import (
	"reflect"
	"testing"
)

func TestFamilyWorkloads(t *testing.T) {
	f := &Family{Occurrences: []Occurrence{
		{Kind: "Deployment", Name: "web"},
		{Kind: "Deployment", Name: "web"}, // a second container in the same document
		{Kind: "Deployment", Name: "api"},
		{Kind: "CronJob", Name: "purge"},
		{Kind: "Job", Name: "migrate"},
		{Kind: "StatefulSet", Name: "db"},
		{Kind: "Job", Name: "api"}, // same name, different kind: a different workload
	}}
	want := []Workload{
		{"CronJob", "purge"},
		{"Deployment", "api"}, {"Deployment", "web"},
		{"Job", "api"}, {"Job", "migrate"},
		{"StatefulSet", "db"},
	}
	if got := f.Workloads(); !reflect.DeepEqual(got, want) {
		t.Errorf("Workloads() = %v, want %v", got, want)
	}
	if got, want := f.Named("Deployment"), []string{"api", "web"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Named(Deployment) = %v, want %v", got, want)
	}
	if got := f.Named("DaemonSet"); len(got) != 0 {
		t.Errorf("Named(DaemonSet) = %v, want none", got)
	}
}

func TestFamilyWorkloadsNilAndEmpty(t *testing.T) {
	var nilFam *Family
	if got := nilFam.Workloads(); got != nil {
		t.Errorf("nil family Workloads() = %v, want nil", got)
	}
	if got := (&Family{}).Named("Deployment"); len(got) != 0 {
		t.Errorf("empty family Named() = %v, want none", got)
	}
}

func TestFamilyWorkloadsOverTheFixtureRepo(t *testing.T) {
	r, err := Discover("../../testdata/repo", DefaultAppsRoot)
	if err != nil {
		t.Fatal(err)
	}
	var seenDeployment bool
	for _, e := range r.Envs {
		for _, fam := range e.Families {
			for _, w := range fam.Workloads() {
				if w.Kind == "" || w.Name == "" {
					t.Errorf("%s: workload %+v has no kind or name", fam.Dir, w)
				}
				if w.Kind == "Deployment" {
					seenDeployment = true
				}
			}
		}
	}
	if !seenDeployment {
		t.Error("the fixture repo declares Deployments; Workloads() found none (positive control)")
	}
}

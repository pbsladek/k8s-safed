package cmd

import "testing"

func FuzzSliceToSet(f *testing.F) {
	f.Add("", "Deployment/default/api", "Deployment/default/api")
	f.Add("StatefulSet/data/postgres", "Deployment/prod/api", "DaemonSet/kube-system/cni")

	f.Fuzz(func(t *testing.T, a, b, c string) {
		input := []string{a, b, c}
		got := sliceToSet(input)
		for _, s := range input {
			if !got[s] {
				t.Fatalf("sliceToSet(%q) missing %q", input, s)
			}
		}
		if len(got) > len(input) {
			t.Fatalf("sliceToSet(%q) produced too many entries: %v", input, got)
		}
	})
}

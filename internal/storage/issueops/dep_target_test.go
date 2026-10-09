package issueops

import "testing"

func TestResolveExternalDepTarget(t *testing.T) {
	cases := map[string]string{
		"external:applications:applications-1yu": "applications-1yu",
		"external:a:b:c":                         "b:c",
		"task-e3g":                               "task-e3g",
		"external:nostore":                       "external:nostore",
		"external:store:":                        "external:store:",
		"":                                       "",
	}
	for in, want := range cases {
		if got := ResolveExternalDepTarget(in); got != want {
			t.Errorf("ResolveExternalDepTarget(%q) = %q, want %q", in, got, want)
		}
	}
}

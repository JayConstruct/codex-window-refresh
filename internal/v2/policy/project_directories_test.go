package policy

import (
	"strings"
	"testing"
)

func TestClientDirectoryKeysArePlatformIndependent(t *testing.T) {
	cases := []struct{ input, want string }{
		{"/work/project/../project/", "/work/project"},
		{"C:\\Work\\Project\\..\\Repo", "c:/work/repo"},
		{"\\\\Server\\Share\\Repo", "unc:/server/share/repo"},
		{"relative/project", ""}, {"C:relative", ""}, {"~/project", ""},
		{"/work/\nproject", ""}, {" /work/project", ""}, {"\\\\server", ""},
		{strings.Repeat("/a", 2049), ""},
	}
	for _, c := range cases {
		got, valid := ProjectDirectoryKey(c.input)
		if got != c.want || valid != (c.want != "") {
			t.Errorf("%q: got %q, %v; want %q", c.input, got, valid, c.want)
		}
	}
}

func TestSchedulingDirectoryRulesPreserveLegacyLabelsAndRejectAmbiguousMaps(t *testing.T) {
	config := DefaultScheduling()
	config.Projects = []ProjectPreference{{Project: "legacy project", Accounts: []string{"a"}}}
	if err := config.Validate(); err != nil {
		t.Fatal("legacy project rejected", err)
	}
	config.Projects[0].Directories = []string{"/client/project"}
	if err := config.Validate(); err == nil {
		t.Fatal("launcher-incompatible project label accepted")
	}
	config.Projects = []ProjectPreference{
		{Project: "root", Accounts: []string{"a"}, Directories: []string{"/client/project", "C:\\Work\\Repo"}},
		{Project: "nested", Accounts: []string{"b"}, Directories: []string{"/client/project/nested"}},
	}
	if err := config.Validate(); err != nil {
		t.Fatal("nested longest-path map rejected", err)
	}
	for _, duplicate := range []string{"/client/project/./", "c:/work/repo/"} {
		config.Projects[1].Directories = []string{duplicate}
		if err := config.Validate(); err == nil {
			t.Fatalf("duplicate directory accepted: %s", duplicate)
		}
	}
	config.Projects[1].Directories = []string{"/client/project/nested", "/client/project/nested"}
	if err := config.Validate(); err == nil {
		t.Fatal("duplicate within a project accepted")
	}
}

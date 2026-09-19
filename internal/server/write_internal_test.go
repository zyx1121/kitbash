package server

import (
	"testing"

	"github.com/zyx1121/kitbash/internal/problem"
)

// mixedForms is the handler's own answer to a call that is both forms at once.
// The input schema refuses one too, and this is the belt behind it: an
// approved call is replayed from the input the approval stored, which no
// schema has looked at by then, see approve.go.
func TestMixedFormsRefusesACallThatIsBothForms(t *testing.T) {
	content := "one\n"
	list := []writeFile{{Path: "/home/tester/app/two.md", Content: &content}}

	cases := map[string]struct {
		in      writeInput
		refused bool
	}{
		"one file": {in: writeInput{Path: "/home/tester/app/one.md", Content: &content}},
		"a list":   {in: writeInput{Files: list}},
		"both":     {in: writeInput{Path: "/home/tester/app/one.md", Content: &content, Files: list}, refused: true},
		"a path beside a list": {
			in: writeInput{Path: "/home/tester/app/one.md", Files: list}, refused: true},
		"content beside a list": {in: writeInput{Content: &content, Files: list}, refused: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			prob := mixedForms(tc.in)
			if tc.refused && prob == nil {
				t.Fatal("a call carrying both forms was accepted")
			}
			if !tc.refused && prob != nil {
				t.Fatalf("a call carrying one form was refused: %s", prob.Detail)
			}
			if tc.refused && prob.Slug() != problem.SlugBadRequest {
				t.Errorf("problem is %s, want bad-request", prob.Slug())
			}
		})
	}
}

// approvedPaths is what an approved write is confined and permitted by, and
// for a list that is every path in it rather than the first.
func TestApprovedPathsNamesEveryPathOfAList(t *testing.T) {
	content := "x\n"
	got := approvedPaths(writeInput{Files: []writeFile{
		{Path: "/org/handbook/kitbash.yaml", Content: &content},
		{Path: "/org/handbook/policies/writing.md", Content: &content},
	}})
	if len(got) != 2 {
		t.Fatalf("approvedPaths returned %v, want both paths", got)
	}
	if one := approvedPaths(writeInput{Path: "/org/handbook/README.md"}); len(one) != 1 || one[0] != "/org/handbook/README.md" {
		t.Errorf("approvedPaths of the one file form returned %v", one)
	}
}

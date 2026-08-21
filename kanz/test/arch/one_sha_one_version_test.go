package arch

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// A PINNED SHA CARRIES EXACTLY ONE VERSION ANNOTATION (#680).
//
// # What went wrong without it
//
// actions/checkout was pinned to one SHA at TWENTY sites and annotated three
// different ways on that same SHA: "# v4" at eighteen of them, "# v5.0.0" at one,
// "# v7.0.1" at one. The SHA is v7.0.1, so nineteen comments were false.
// actions/setup-go had the same defect on a smaller scale — one SHA, "# v7.0.0"
// at eight sites and "# v6.0.0" at a ninth.
//
// # Why a wrong comment is worse than a missing one
//
// supplychain_test.go already requires the annotation, and states its purpose:
//
//	Dependabot reads that comment to know what tag/version the SHA corresponds
//	to; a bare SHA with no comment is invisible to it and will never receive a
//	security bump
//
// A MISSING comment is invisible. A WRONG one is worse: it is present, it looks
// maintained, and it answers a security question incorrectly. A reviewer asking
// "are we exposed to this CVE in checkout v4?" reads "# v4" and reasons about a
// version the estate left three majors ago. Dependabot reasons from the same
// string.
//
// # Why THIS property, and not "the comment matches the real tag"
//
// Checking the annotation against the tag the SHA actually resolves to would be
// the stronger claim, and it needs the network — an arch guard that calls the
// GitHub API is a guard that fails offline and gets switched off. Internal
// consistency is checkable from the files alone and catches the same defect at
// the moment it appears: the second, disagreeing annotation. One SHA cannot be
// two versions, so a disagreement proves at least one comment is wrong without
// asking anybody which.
func TestOnePinnedSHACarriesOneVersionAnnotation(t *testing.T) {
	repoRoot := filepath.Dir(moduleRoot(t))
	files := workflowFiles(t, repoRoot)
	if len(files) == 0 {
		t.Fatal("no workflow files found under .github/workflows — the layout changed and this guard " +
			"would otherwise pass vacuously")
	}

	// name@<40-hex> followed by a "# <ref>" trailer. The same shape
	// supplychain_test.go enforces, read here for its trailer rather than its SHA.
	pinRe := regexp.MustCompile(`(?m)^\s*(?:-\s*)?uses:\s*([\w./-]+)@([0-9a-fA-F]{40})\s*#\s*(\S+)`)

	// sha -> annotation -> the sites carrying it.
	byS := map[string]map[string][]string{}
	pins := 0

	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		// Normalize CRLF for the same reason supplychain_test.go does: git on
		// Windows hands these over with CRLF and a line-anchored regex would
		// behave differently here than on CI.
		body := strings.ReplaceAll(string(raw), "\r\n", "\n")
		rel, err := filepath.Rel(repoRoot, path)
		if err != nil {
			rel = path
		}
		rel = filepath.ToSlash(rel)

		for _, m := range pinRe.FindAllStringSubmatch(body, -1) {
			action, sha, ver := m[1], strings.ToLower(m[2]), m[3]
			pins++
			if byS[sha] == nil {
				byS[sha] = map[string][]string{}
			}
			byS[sha][ver] = append(byS[sha][ver], rel+" ("+action+")")
		}
	}

	// NON-VACUITY. A regex that matched nothing — because the trailer format
	// changed, or the walk broke — would report perfect agreement across zero
	// pins. The estate carries dozens; a floor well under that still catches a
	// broken walk without breaking every time a workflow is added or removed.
	if pins < 20 {
		t.Fatalf("found only %d SHA-pinned action(s) with a version annotation across %d workflow "+
			"file(s) — the pin regex or the walk is broken, and this guard proves nothing", pins, len(files))
	}

	var problems []string
	for sha, byVer := range byS {
		if len(byVer) < 2 {
			continue
		}
		vers := make([]string, 0, len(byVer))
		for v := range byVer {
			vers = append(vers, v)
		}
		sort.Strings(vers)
		var detail []string
		for _, v := range vers {
			sites := byS[sha][v]
			sort.Strings(sites)
			shown := sites
			if len(shown) > 4 {
				shown = append(append([]string{}, shown[:4]...), "…and more")
			}
			detail = append(detail, "    "+v+" — "+strconv.Itoa(len(sites))+" site(s): "+strings.Join(shown, ", "))
		}
		problems = append(problems, "  "+sha[:12]+"… is annotated "+strconv.Itoa(len(vers))+" different ways:\n"+
			strings.Join(detail, "\n"))
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("%d pinned SHA(s) carry disagreeing version annotations:\n\n%s\n\n"+
			"One commit cannot be two versions, so at least one of these comments is FALSE. That comment "+
			"is what Dependabot reads to decide whether a security bump applies (see supplychain_test.go), "+
			"and what a reviewer reads to decide whether a CVE affects this estate. Resolve the SHA with "+
			"`gh api repos/<owner>/<repo>/tags --paginate --jq '.[]|select(.commit.sha==\"<sha>\")|.name'` "+
			"and make every site say what it actually is.", len(problems), strings.Join(problems, "\n\n"))
	}
}

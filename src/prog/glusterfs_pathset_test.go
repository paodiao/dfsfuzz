package prog

import (
	"math/rand"
	"strings"
	"testing"
)

func TestGlusterfsRenamePolicyCrossParent(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	ft := glusterfsPlanTree()
	r := newRand(target, rand.NewSource(61))
	r.hmcfg = &Hmdfs_config{Profile: glusterfsProfile}
	cross := false
	for i := 0; i < 40; i++ {
		dst := pickRenameDstPolicy(glusterfsProfile, ft, "dirA/a", PathNoRel, r.Rand)
		if dst == "" || dst == "dirA/a" || strings.HasPrefix(dst, "dirA/a/") {
			t.Fatalf("bad glusterfs rename dst %q", dst)
		}
		if GetParentDir(dst) != GetParentDir("dirA/a") {
			cross = true
		}
	}
	if !cross {
		t.Fatal("GlusterFS rename policy never produced a cross-parent destination")
	}

	r2 := newRand(target, rand.NewSource(62))
	for i := 0; i < 20; i++ {
		dst := pickRenameDstPolicy(hmdfsProfile, ft, "dirA/a", PathNoRel, r2.Rand)
		if dst == "" || GetParentDir(dst) != GetParentDir("dirA/a") {
			t.Fatalf("HMDFS rename dst %q must stay same-parent", dst)
		}
	}
}

func TestPathSetPrefixIsProfileAware(t *testing.T) {
	if got := pathSetTopPath("", "dirA"); got != "dirA" {
		t.Fatalf("empty-prefix top path = %q", got)
	}
	if got := pathSetTopPath(MergeViewPrefix, "dirA"); got != "merge_view/dirA" {
		t.Fatalf("hmdfs top path = %q", got)
	}
	if got := mapPathSetPath("", "dirA/x", "", "dirB", []string{"dirA"}); got != "dirB/dirA/x" {
		t.Fatalf("empty-prefix forest map = %q", got)
	}
}

func TestBuildPathMembersLinkSymlinkRoles(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	if _, ok := target.SyscallMap["link"]; !ok {
		t.Skip("target has no link")
	}
	if _, ok := target.SyscallMap["symlink"]; !ok {
		t.Skip("target has no symlink")
	}
	ft := glusterfsPlanTree()
	link := mkLinkCall(t, target, "dirA/a", "dirA/l1")
	sym := mkSymlinkCall(t, target, "a", "dirA/l2")
	read := mkReadlinkCall(t, target, "dirA/l2")
	p := &Prog{Target: target, Calls: []*Call{link, sym, read}}

	members := buildPathMembers([]*Prog{p}, ft)
	roles := map[pathRole]pathSpec{}
	for _, m := range members {
		if len(m.Paths) != 1 {
			t.Fatalf("member %+v has %d paths", m, len(m.Paths))
		}
		roles[m.Paths[0].Role] = m.Paths[0]
	}
	if spec, ok := roles[pathRoleLinkTarget]; !ok || spec.Path != "dirA/a" {
		t.Fatalf("link target member = %+v", spec)
	}
	if spec, ok := roles[pathRoleSymlinkTarget]; !ok || spec.Path != "dirA/a" {
		t.Fatalf("symlink target member = %+v", spec)
	}
	for role, spec := range roles {
		if strings.HasPrefix(spec.Path, "a") {
			t.Fatalf("symlink payload leaked as member path: role=%v spec=%+v", role, spec)
		}
	}
	if len(members) != 2 {
		t.Fatalf("members = %d, want link+symlink only (readlink skipped)", len(members))
	}
}

func TestApplyPathSetSymlinkTargetPayload(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	if _, ok := target.SyscallMap["symlink"]; !ok {
		t.Skip("target has no symlink")
	}
	sym := mkSymlinkCall(t, target, "a", "dirA/l2")
	p := &Prog{Target: target, Calls: []*Call{sym}}
	plan := &pathPlan{Updates: []pathUpdate{{
		Pos: GroupPosition{ProgIdx: 0, CallIdx: 0},
		Specs: []pathSpec{{
			Path: "dirB/c", Kind: pathExisting, Role: pathRoleSymlinkTarget,
			ArgIdx: 0, TypeConstraint: relTargetAny,
		}},
	}}}
	r := newRand(target, rand.NewSource(63))
	if !applyPathSetPlan([]*Prog{p}, plan, r) {
		t.Fatal("applyPathSetPlan failed")
	}
	payload := extractPathFromCallByArgIdx(p.Calls[0], 0)
	if payload != "../dirB/c" {
		t.Fatalf("symlink payload = %q, want ../dirB/c", payload)
	}
	if got := symlinkCanonicalTarget("dirA/l2", payload); got != "dirB/c" {
		t.Fatalf("payload resolves to %q, want dirB/c", got)
	}
}

func TestPathSetOpenCreatRole(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	ocreate := target.GetConst("O_CREAT")
	if ocreate == 0 {
		t.Skip("target has no O_CREAT")
	}
	ft := glusterfsPlanTree()
	call := mkOpenWithFlags(target, "dirA/new", ocreate|2)
	p := &Prog{Target: target, Calls: []*Call{call}}
	members := buildPathMembers([]*Prog{p}, ft)
	if len(members) != 1 || len(members[0].Paths) != 1 {
		t.Fatalf("open(O_CREAT) members = %+v", members)
	}
	spec := members[0].Paths[0]
	if spec.Role != pathRoleCreat || spec.Kind != pathFresh {
		t.Fatalf("open(O_CREAT) spec = %+v, want fresh creat", spec)
	}
}

func TestFreshBoundaryPrefix(t *testing.T) {
	ft := glusterfsPlanTree()
	if !freshParentNodeEligible(ft, "Eris_link_top") {
		t.Fatal("top-level fresh linkpath must be eligible under an empty GlusterFS prefix")
	}
	if freshParentNodeEligible(ft, "dirA/fresh/child") {
		t.Fatal("nested fresh parent must not be eligible")
	}
	hmdfsTree := NewFileTreeWithPrefix(MergeViewPrefix)
	if freshParentNodeEligible(hmdfsTree, "Eris_link_top") {
		t.Fatal("HMDFS top-level path outside merge_view must stay ineligible")
	}
}

func TestLegacyPathSkipsDualPathCalls(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	if _, ok := target.SyscallMap["symlink"]; !ok {
		t.Skip("target has no symlink")
	}
	sym := mkSymlinkCall(t, target, "a", "dirA/l2")
	p := &Prog{Target: target, Calls: []*Call{sym}}
	if got := pathOfCall([]*Prog{p}, GroupPosition{ProgIdx: 0, CallIdx: 0}); got != "" {
		t.Fatalf("legacy pathOfCall(symlink) = %q, want empty", got)
	}
}

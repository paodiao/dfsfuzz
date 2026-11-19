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

func TestPickRenameSrcDirProfilePolicy(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	ft := NewFileTreeWithPrefix("")
	ft.AddNode("dirA", NodeTypeDir, "c1")
	ft.AddNode("dirA/nonempty", NodeTypeDir, "c1")
	ft.AddNode("dirA/nonempty/child", NodeTypeFile, "c1")
	ft.AddNode("dirA/sib", NodeTypeDir, "c1")
	ft.AddNode("dirB", NodeTypeDir, "c1")
	ft.AddNode("dirB/src", NodeTypeDir, "c1")
	r := newRand(target, rand.NewSource(71))

	cross := false
	for i := 0; i < 40; i++ {
		src := pickRenameSrcDir(ft, r, "dirA/nonempty", glusterfsProfile)
		if src == "" {
			t.Fatal("GlusterFS rename source missing")
		}
		if parentDirOrRoot(src) != parentDirOrRoot("dirA/nonempty") {
			cross = true
		}
	}
	if !cross {
		t.Fatal("GlusterFS rename source never crossed parents")
	}

	for i := 0; i < 20; i++ {
		src := pickRenameSrcDir(ft, r, "dirA/nonempty")
		if src == "" || parentDirOrRoot(src) != parentDirOrRoot("dirA/nonempty") {
			t.Fatalf("HMDFS rename source %q must stay same-parent", src)
		}
	}
}

func TestGetPathsForRenameVariantUsesProfilePolicy(t *testing.T) {
	ft := glusterfsPlanTree()
	lcs := &LayeredChoiceStrategy{FileTree: ft, Profile: glusterfsProfile}
	r := rand.New(rand.NewSource(63))
	cross := false
	for i := 0; i < 40; i++ {
		src, dst := lcs.GetPathsForRenameVariant("dirA/a", "", PathNoRel, r, false)
		if src == "" || dst == "" || dst == src {
			t.Fatalf("bad GlusterFS rename variant %q -> %q", src, dst)
		}
		if parentDirOrRoot(dst) != parentDirOrRoot(src) {
			cross = true
		}
	}
	if !cross {
		t.Fatal("GlusterFS rename variant never produced a cross-parent destination")
	}

	hmft := NewFileTree()
	hmft.AddNode("merge_view/dirA", NodeTypeDir, "c1")
	hmft.AddNode("merge_view/dirA/a", NodeTypeFile, "c1")
	hmft.AddNode("merge_view/dirB", NodeTypeDir, "c1")
	hmft.AddNode("merge_view/dirB/b", NodeTypeFile, "c1")
	hm := &LayeredChoiceStrategy{FileTree: hmft, Profile: hmdfsProfile}
	for i := 0; i < 20; i++ {
		src, dst := hm.GetPathsForRenameVariant("merge_view/dirA/a", "", PathNoRel, r, false)
		if src == "" || dst == "" || dst == src {
			t.Fatalf("bad HMDFS rename variant %q -> %q", src, dst)
		}
		if parentDirOrRoot(dst) != parentDirOrRoot(src) {
			t.Fatalf("HMDFS rename dst %q must stay same-parent with %q", dst, src)
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

	members := buildPathMembers([]*Prog{p}, ft, glusterfsProfile)
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
	members := buildPathMembers([]*Prog{p}, ft, glusterfsProfile)
	if len(members) != 1 || len(members[0].Paths) != 1 {
		t.Fatalf("open(O_CREAT) members = %+v", members)
	}
	spec := members[0].Paths[0]
	if spec.Role != pathRoleCreat || spec.Kind != pathFresh {
		t.Fatalf("open(O_CREAT) spec = %+v, want fresh creat", spec)
	}
}

func TestFreshLinkPathsBefore(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	if _, ok := target.SyscallMap["link"]; !ok {
		t.Skip("target has no link")
	}
	if _, ok := target.SyscallMap["symlink"]; !ok {
		t.Skip("target has no symlink")
	}
	ft := glusterfsPlanTree()
	link := mkLinkCall(t, target, "dirA/a", "dirA/l1")
	link.CheckInfo = &FileMetadata{Stime: 100, Etime: 200, Retv: 0}
	sym := mkSymlinkCall(t, target, "a", "dirA/l2")
	sym.CheckInfo = &FileMetadata{Stime: 210, Etime: 260, Retv: 0}
	p := &Prog{Target: target, Calls: []*Call{link, sym}}
	links := freshLinkPathsBefore(p, ft, 2)
	if info := links["dirA/l1"]; info.Kind != linkPathHardlink || info.Target != "dirA/a" {
		t.Fatalf("hardlink info = %+v", info)
	}
	if info := links["dirA/l2"]; info.Kind != linkPathSymlink ||
		info.Target != "dirA/a" || info.FollowIsDir {
		t.Fatalf("symlink info = %+v", info)
	}
}

func TestFreshLinkPathsRenameMovesBinding(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	if _, ok := target.SyscallMap["link"]; !ok {
		t.Skip("target has no link")
	}
	if _, ok := target.SyscallMap["rename"]; !ok {
		t.Skip("target has no rename")
	}
	ft := glusterfsPlanTree()
	link := mkLinkCall(t, target, "dirA/a", "dirA/l1")
	link.CheckInfo = &FileMetadata{Stime: 100, Etime: 200, Retv: 0}
	rn := mkRenameCall(t, target, "dirA/l1", "dirA/l2", 210, 260)
	rn.CheckInfo = &FileMetadata{Stime: 210, Etime: 260, Retv: 0}
	p := &Prog{Target: target, Calls: []*Call{link, rn}}

	links := freshLinkPathsBefore(p, ft, 2)
	if _, ok := links["dirA/l1"]; ok {
		t.Fatal("renamed linkpath still present at the old name")
	}
	info := links["dirA/l2"]
	if info.Kind != linkPathHardlink || info.Target != "dirA/a" {
		t.Fatalf("renamed linkpath = %+v, want hardlink binding moved to l2", info)
	}
}

func TestFreshLinkPathsRenameOverwritesDestination(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	if _, ok := target.SyscallMap["link"]; !ok {
		t.Skip("target has no link")
	}
	if _, ok := target.SyscallMap["rename"]; !ok {
		t.Skip("target has no rename")
	}
	ft := glusterfsPlanTree()
	link := mkLinkCall(t, target, "dirA/a", "dirA/l2")
	link.CheckInfo = &FileMetadata{Stime: 100, Etime: 200, Retv: 0}
	rn := mkRenameCall(t, target, "dirA/b", "dirA/l2", 210, 260)
	rn.CheckInfo = &FileMetadata{Stime: 210, Etime: 260, Retv: 0}
	p := &Prog{Target: target, Calls: []*Call{link, rn}}

	links := freshLinkPathsBefore(p, ft, 2)
	if _, ok := links["dirA/l2"]; ok {
		t.Fatal("rename-overwritten linkpath still recorded as a binding")
	}
	if kind := freshPathKindsBefore(p, ft, 2, glusterfsProfile)["dirA/l2"]; kind != relTargetFile {
		t.Fatalf("rename destination type = %d, want file", kind)
	}
}

func TestBuildPathMembersLinkFreshTarget(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	if _, ok := target.SyscallMap["link"]; !ok {
		t.Skip("target has no link")
	}
	if _, ok := target.SyscallMap["creat"]; !ok {
		t.Skip("target has no creat")
	}
	ft := glusterfsPlanTree()
	creator := mkCreat(target, "dirA/fresh.txt")
	creator.CheckInfo = &FileMetadata{Stime: 100, Etime: 200, Retv: 0}
	link := mkLinkCall(t, target, "dirA/fresh.txt", "dirA/l1")
	link.CheckInfo = &FileMetadata{Stime: 210, Etime: 260, Retv: 0}
	p := &Prog{Target: target, Calls: []*Call{creator, link}}

	members := buildPathMembers([]*Prog{p}, ft, glusterfsProfile)
	var linkSpec *pathSpec
	for i := range members {
		if members[i].Pos == (GroupPosition{ProgIdx: 0, CallIdx: 1}) {
			linkSpec = &members[i].Paths[0]
		}
	}
	if linkSpec == nil || linkSpec.Kind != pathFresh ||
		linkSpec.Role != pathRoleLinkTarget || linkSpec.Path != "dirA/fresh.txt" {
		t.Fatalf("fresh link target spec = %+v", linkSpec)
	}
	if !pathSetSpecValid(nil, GroupPosition{}, *linkSpec, relTargetFile, ft) {
		t.Fatal("fresh link target rejected by pathSetSpecValid")
	}
}

func TestBuildPathMembersSymlinkFreshDirTarget(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	if _, ok := target.SyscallMap["symlink"]; !ok {
		t.Skip("target has no symlink")
	}
	ft := glusterfsPlanTree()
	creator := mkMkdir(target, "dirA/freshDir")
	creator.CheckInfo = &FileMetadata{Stime: 100, Etime: 200, Retv: 0}
	sym := mkSymlinkCall(t, target, "freshDir", "dirA/l2")
	sym.CheckInfo = &FileMetadata{Stime: 210, Etime: 260, Retv: 0}
	p := &Prog{Target: target, Calls: []*Call{creator, sym}}

	members := buildPathMembers([]*Prog{p}, ft, glusterfsProfile)
	var symSpec *pathSpec
	for i := range members {
		if members[i].Pos == (GroupPosition{ProgIdx: 0, CallIdx: 1}) {
			symSpec = &members[i].Paths[0]
		}
	}
	if symSpec == nil || symSpec.Kind != pathFresh ||
		symSpec.Role != pathRoleSymlinkTarget || symSpec.Path != "dirA/freshDir" {
		t.Fatalf("fresh symlink target spec = %+v", symSpec)
	}
}

func TestBuildPathMembersRejectsNestedLinkTarget(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	if _, ok := target.SyscallMap["link"]; !ok {
		t.Skip("target has no link")
	}
	ft := glusterfsPlanTree()
	first := mkLinkCall(t, target, "dirA/a", "dirA/l1")
	first.CheckInfo = &FileMetadata{Stime: 100, Etime: 200, Retv: 0}
	second := mkLinkCall(t, target, "dirA/l1", "dirA/l2")
	second.CheckInfo = &FileMetadata{Stime: 210, Etime: 260, Retv: 0}
	p := &Prog{Target: target, Calls: []*Call{first, second}}

	for _, m := range buildPathMembers([]*Prog{p}, ft, glusterfsProfile) {
		if m.Pos == (GroupPosition{ProgIdx: 0, CallIdx: 1}) {
			t.Fatalf("nested linkpath target produced a member: %+v", m)
		}
	}
}

func TestBuildPathMembersAllowsRegularOverwrittenLinkpath(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	if _, ok := target.SyscallMap["link"]; !ok {
		t.Skip("target has no link")
	}
	if _, ok := target.SyscallMap["rename"]; !ok {
		t.Skip("target has no rename")
	}
	ft := glusterfsPlanTree()
	link := mkLinkCall(t, target, "dirA/a", "dirA/l2")
	link.CheckInfo = &FileMetadata{Stime: 100, Etime: 200, Retv: 0}
	rn := mkRenameCall(t, target, "dirA/b", "dirA/l2", 210, 260)
	rn.CheckInfo = &FileMetadata{Stime: 210, Etime: 260, Retv: 0}
	relink := mkLinkCall(t, target, "dirA/l2", "dirA/l3")
	relink.CheckInfo = &FileMetadata{Stime: 270, Etime: 320, Retv: 0}
	p := &Prog{Target: target, Calls: []*Call{link, rn, relink}}

	var spec *pathSpec
	for _, m := range buildPathMembers([]*Prog{p}, ft, glusterfsProfile) {
		if m.Pos == (GroupPosition{ProgIdx: 0, CallIdx: 2}) {
			spec = &m.Paths[0]
		}
	}
	if spec == nil || spec.Kind != pathFresh || spec.Path != "dirA/l2" {
		t.Fatalf("regular-overwritten linkpath target spec = %+v", spec)
	}
}

func TestApplyPathSetFreshLinkTarget(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	if _, ok := target.SyscallMap["link"]; !ok {
		t.Skip("target has no link")
	}
	if _, ok := target.SyscallMap["creat"]; !ok {
		t.Skip("target has no creat")
	}
	creator := mkCreat(target, "dirA/fresh.txt")
	link := mkLinkCall(t, target, "dirA/fresh.txt", "dirA/l1")
	p := &Prog{Target: target, Calls: []*Call{creator, link}}
	plan := &pathPlan{Updates: []pathUpdate{
		{Pos: GroupPosition{ProgIdx: 0, CallIdx: 0}, Specs: []pathSpec{{
			Path: "dirB/fresh.txt", Kind: pathFresh, Role: pathRoleCreat,
			ArgIdx: 0, TypeConstraint: relTargetFile,
		}}},
		{Pos: GroupPosition{ProgIdx: 0, CallIdx: 1}, Specs: []pathSpec{{
			Path: "dirB/fresh.txt", Kind: pathFresh, Role: pathRoleLinkTarget,
			ArgIdx: 0, TypeConstraint: relTargetFile,
		}}},
	}}
	if !applyPathSetPlan([]*Prog{p}, plan, newRand(target, rand.NewSource(71))) {
		t.Fatal("applyPathSetPlan failed")
	}
	if got := extractPathFromCall(p.Calls[0]); got != "dirB/fresh.txt" {
		t.Fatalf("creator path = %q, want dirB/fresh.txt", got)
	}
	if got := extractPathFromCallByArgIdx(p.Calls[1], 0); got != "dirB/fresh.txt" {
		t.Fatalf("link target = %q, want dirB/fresh.txt", got)
	}
}

func TestPathSetSpecValidFreshLinkTarget(t *testing.T) {
	ft := glusterfsPlanTree()
	spec := pathSpec{
		Path: "dirA/fresh.txt", Kind: pathFresh, Role: pathRoleLinkTarget,
		ArgIdx: 0, TypeConstraint: relTargetFile,
	}
	if !pathSetSpecValid(nil, GroupPosition{}, spec, relTargetFile, ft) {
		t.Fatal("fresh link target with valid parent rejected")
	}
	badParent := spec
	badParent.Path = "missing/fresh.txt"
	if pathSetSpecValid(nil, GroupPosition{}, badParent, relTargetFile, ft) {
		t.Fatal("fresh link target with missing parent accepted")
	}
	existing := spec
	existing.Path = "dirA/a"
	if pathSetSpecValid(nil, GroupPosition{}, existing, relTargetFile, ft) {
		t.Fatal("fresh link target that already exists accepted")
	}
}

func TestPathSetDirectedTargetCall(t *testing.T) {
	for _, name := range []string{"link", "symlink", "readlink"} {
		if pathSetDirectedTargetCall(name) {
			t.Fatalf("%s must not participate in the directed target arm", name)
		}
	}
	for _, name := range []string{"stat", "open", "creat"} {
		if !pathSetDirectedTargetCall(name) {
			t.Fatalf("%s should remain eligible for the directed target arm", name)
		}
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

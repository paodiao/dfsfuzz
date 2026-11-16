package prog

import (
	"math/rand"
	"strings"
	"sync"
)

// Directed coverage (Phase 1): the feedback DAG pair bit is already a hash of
// (Temporal, PathRel, featuresOf(A), featuresOf(B)) — see hashPair — so this
// board re-materializes that structure from each novel pair and keeps a
// structured record of which cells have been discovered. The selection side
// (chooseVariant) can then bias toward combos whose producible cells
// are still uncovered ("directed" picks), instead of learning weights from a
// signal that is too sparse to be learnable at the observed volume.
//
// The board is process-global (not per-Proc LCS): the throwaway LCS used by
// from-scratch generation shares the same discovery state. It starts empty on
// every fresh run and is filled by novel pairs only.

// cellKey is the structured identity of one feedback cell of a DAG pair.
type cellKey struct {
	Root     string
	Variant  string
	Rel      PathRelation
	Temporal TemporalRel
	RetA     RetBucket
	RetB     RetBucket
	OffB     uint64
	PersistB bool
	DepthB   uint64
	TypeB    bool // true: B is a directory
}

// comboKey identifies a DCT (root, variant, relation) combination.
type comboKey struct {
	Root    string
	Variant string
	Rel     PathRelation
}

// shapeDrainKey identifies one (combo, A-side event context) pair for the
// Direction 3 shape-drain memo. The A context is the root's func class plus
// the base path's depth/type/persist margins; the exact path string is
// deliberately not part of the key (fresh names vary, margins do not).
type shapeDrainKey struct {
	Combo    comboKey
	AFunc    uint32
	ADepth   uint64
	AIsDir   bool
	APersist bool
}

// shapeDrainEntry memoizes one shapeDrained outcome at the board's shape
// epoch: shapes only grow, so an outcome stays valid until a new shape
// arrives (epoch change) or the board resets.
type shapeDrainEntry struct {
	Epoch   uint64
	Drained bool
}

// coverageBoard tracks discovered cells and per-combo producible-cell counts.
// producible/found are indexed by temporal form (HB=0, CC=1, matching the
// TemporalRel enum values). All fields are guarded by mu.
type coverageBoard struct {
	mu          sync.RWMutex
	discovered  map[cellKey]int
	producible  map[comboKey][2]int // cells producible per temporal form
	found       map[comboKey][2]int // discovered cells per temporal form
	knownShapes map[uint64]bool     // observed event shapes (call-name agnostic)
	// shapeEpoch increments on every newly observed shape; shapeDrainCache
	// entries are valid only at the epoch they were written, since shapes
	// never disappear within a board generation. The cache is replaced
	// wholesale when the epoch advances (and on reset), so it only ever holds
	// live entries.
	shapeEpoch      uint64
	shapeDrainCache map[shapeDrainKey]shapeDrainEntry
	profile         *DFSProfile // per-run call-name/func classifier
	persistDir      string
	persistBucket   uint64  // depth bucket of the persist dir (persist = path prefix)
	depths          [4]bool // depth buckets present in the initial tree
	ready           bool

	directedPicks    uint64
	directedScoreSum uint64

	// Combo-level unreachable accounting (pick-time, no execution plumbing):
	// directedAttempts/resolveMark/foundSnapshot track a combo's directed
	// picks, the attempt mark of its last resolution window, and its found
	// count at that mark; unreachable drops the combo from directed selection.
	directedAttempts map[comboKey]int
	resolveMark      map[comboKey]int
	foundSnapshot    map[comboKey]int
	unreachable      map[comboKey]bool

	// Cell-level directed accounting (Phase 2b): cellAttempts counts the
	// consecutive planned executions of a concrete cell that did not match;
	// cellUnreachable drops the cell from the 2b enumeration pool once the
	// threshold is reached. A hit, or a fresh discovery of the cell, clears
	// both. cellSuppressed counts the currently cell-suppressed cells per
	// combo, so comboPotentials can answer the 2b-available potential in O(1)
	// and skip fully suppressed combos immediately (the combo-level unreachable
	// ledger only retires them after blind Direction-3 picks). cellPlans/hits/
	// misses/noPlans/insertFails are the directCell diagnostics (cellPlans
	// counts cells, cellBatches counts insertion units); cellCfgErrs counts
	// precondition/config anomalies.
	cellAttempts    map[cellKey]int
	cellUnreachable map[cellKey]bool
	cellSuppressed  map[comboKey]int
	cellPlans       uint64
	cellBatches     uint64
	cellHits        uint64
	cellMisses      uint64
	cellNoPlans     uint64
	cellInsertFails uint64
	cellCfgErrs     uint64
}

var covBoard = &coverageBoard{
	discovered:       make(map[cellKey]int),
	producible:       make(map[comboKey][2]int),
	found:            make(map[comboKey][2]int),
	knownShapes:      make(map[uint64]bool),
	shapeDrainCache:  make(map[shapeDrainKey]shapeDrainEntry),
	directedAttempts: make(map[comboKey]int),
	resolveMark:      make(map[comboKey]int),
	foundSnapshot:    make(map[comboKey]int),
	unreachable:      make(map[comboKey]bool),
	cellAttempts:     make(map[cellKey]int),
	cellUnreachable:  make(map[cellKey]bool),
	cellSuppressed:   make(map[comboKey]int),
}

// directedBiasProb is the probability that a variant selection is driven by
// the coverage board instead of the static-weight sampler; the remaining 25%
// is the stochastic tail (Phase 3). It gates three things: the Direction 3
// potential roulette inside chooseVariant (generation side), the Phase 2b
// exact-plan dispatch in MutateFsopsWithDCT (mutation side, whose inner
// Direction 3 is disabled to avoid a double bias), and the statistical
// directed arms of the path/data group mutations.
const directedBiasProb = 0.75

// unreachableThreshold is the number of consecutive directed picks a combo may
// receive without its found count moving before it is marked unreachable (a
// directed attempt that produced nothing) and drops out of directed selection.
// Pick-time accounting: a discovery re-baselines the window immediately
// (recordPairs), so the effective rule is "threshold consecutive picks since
// the last produced cell"; a combo producing more often than that is never
// suppressed.
//
// Calibrated to 6 (from 8, 2026-10): one unit is one directed variant pick and
// one insertion event carries len(ps)-1 picks, so 6 is about three insertion
// events of exclusive attention; aimed at retiring dead combos during the
// late-run pool shrink instead of only at run end.
const unreachableThreshold = 6

// cellMissThreshold is the number of consecutive directed plans for one
// concrete cell that executed without matching before the cell is marked
// unreachable and drops out of the Phase 2b plan enumeration. A hit (or a
// fresh discovery of the cell) resets the counter.
//
// Calibrated to 4 (from 8, 2026-10): cell misses are diluted by the global
// pickable pool (a batch samples one seed uniformly among all uncovered
// cells), so the expected batches needed to retire one cell scale with the
// pool size; 4 makes retirement reachable within a run once the pool shrinks
// while keeping the wrong-retirement probability low. Recovery stays
// observation-only (or reset()) -- no tree/shape relaxation (V4-8).
const cellMissThreshold = 4

// freshSameCombo reports whether the fresh-fresh construction table has a
// form for (root, variant): both sides operate on the same freshly created
// path (freshSameForms in directed_plan.go). That table is a construction
// channel independent of the root/variant class rules, so those combos stay
// in the grid even when the class predicate below would exclude them.
func freshSameCombo(root, variant string) bool {
	for _, fp := range freshSameForms {
		if fp.root == root && fp.variant == variant {
			return true
		}
	}
	return false
}

// comboStaticallyUnreachable reports whether no construction channel can
// produce any predicted cell of the (root, variant, rel) combo: the 2a/seed
// picker (eligible in distributed_choice.go) and the planner's existing-B and
// fresh-pass templates. The check is tree-independent and conservative:
// combos that are merely blocked by today's picker gates (e.g. mkdir
// Parent/Sibling under a file-typed root) or by the freshSame table stay in
// the grid. Only class-impossible cases are excluded:
//   - Same: the base type must match the variant's type (mkdir is dir-only);
//     a file base cannot host a dir-only target and vice versa,
//   - Parent: the target is always a directory, so file-only variants can
//     never produce their predicted cell,
//   - Child: a file base has no children.
//
// Sibling stays in the grid: it only needs a parent directory and is planned
// today (existing-dir B and the synthesized fresh B under a parent).
func comboStaticallyUnreachable(root, variant string, rel PathRelation) bool {
	return comboStaticallyUnreachableFor(hmdfsProfile, root, variant, rel)
}

func comboStaticallyUnreachableFor(profile *DFSProfile, root, variant string, rel PathRelation) bool {
	if profile == nil {
		profile = hmdfsProfile
	}
	switch rel {
	case PathSame:
		rRoot, rVar := profile.TargetConstraint(root), profile.TargetConstraint(variant)
		if rRoot == relTargetFile {
			return rVar == relTargetDir && !freshSameCombo(root, variant)
		}
		if rRoot == relTargetDir {
			return rVar == relTargetFile && !freshSameCombo(root, variant)
		}
		return false
	case PathParent:
		return profile.TargetConstraint(variant) == relTargetFile
	case PathChild:
		return profile.TargetConstraint(root) == relTargetFile
	}
	return false
}

// coverageBoardCallLists returns the root/variant call lists the coverage grid
// is built from. HMDFS has no explicit RootCalls/VariantCalls split, so it
// keeps using its full SubCalls set; GlusterFS uses the 19/21 v1 lists.
func coverageBoardCallLists(profile *DFSProfile) (roots, variants []string) {
	if profile != nil {
		roots = profile.Calls.RootCalls
		if len(roots) == 0 {
			roots = profile.Calls.SubCalls
		}
		variants = profile.Calls.VariantCalls
		if len(variants) == 0 {
			variants = profile.Calls.SubCalls
		}
	}
	if len(roots) == 0 {
		roots = FsopsSubCalls
	}
	if len(variants) == 0 {
		variants = FsopsSubCalls
	}
	return roots, variants
}

// InitCoverageBoard initializes the global coverage board from the run config
// (persistence dir + depth buckets present in the initial tree) and eagerly
// precomputes every combo's producible-cell counts. Idempotent: the
// first call wins so extra Procs cannot reset a shared board mid-run.
func InitCoverageBoard(hmcfg *Hmdfs_config) {
	if hmcfg == nil {
		return
	}
	b := covBoard
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ready {
		return
	}
	b.persistDir = hmcfg.Persistence_dir
	b.profile = hmcfg.ProfileOrDefault()
	if b.persistDir != "" {
		b.persistBucket = DepthBucketOf(b.persistDir)
	}
	if hmcfg.FileTree != nil {
		hmcfg.FileTree.mu.RLock()
		for _, files := range hmcfg.FileTree.FilesByCid {
			for _, n := range files {
				b.depths[DepthBucketOf(n.FullPath)] = true
			}
		}
		for _, dirs := range hmcfg.FileTree.DirsByCid {
			for _, n := range dirs {
				b.depths[DepthBucketOf(n.FullPath)] = true
			}
		}
		hmcfg.FileTree.mu.RUnlock()
	}
	if b.depths == [4]bool{} {
		b.depths = [4]bool{false, true, true, true}
	}
	// PathNoRel is intentionally absent: unrelated pairs are dropped by
	// ExtractPairs (PathNone), so NoRel cells can never be observed; keeping
	// them in the grid inflates cover= and lets Direction-3 directed sampling
	// aim at unobservable combos. NoRel generation itself stays on the DCT
	// weighted path (lowest initial weight tier via relBaseOf).
	rels := []PathRelation{PathSame, PathParent, PathChild, PathSibling}
	roots, variants := coverageBoardCallLists(hmcfg.ProfileOrDefault())
	for _, root := range roots {
		for _, variant := range variants {
			for _, rel := range rels {
				ck := comboKey{Root: root, Variant: variant, Rel: rel}
				// Class-impossible combos (no construction channel can produce
				// any of their predicted cells) stay out of the grid entirely:
				// they must not inflate cover=/target or stay live for the
				// Directed pickers. This is a new scoring epoch (the target
				// denominator shrinks; cover= is not comparable across it).
				if comboStaticallyUnreachableFor(b.profile, root, variant, rel) {
					continue
				}
				b.producible[ck] = b.computeProducibleLocked(ck)
			}
		}
	}
	b.ready = true
}

// RecordCoveragePairs records the observed DAG pairs into the coverage board:
// every observation is offered and the board's own first-sighting ledgers
// (knownShapes / discovered / found) count each shape and cell once. Gating
// this on the globally deduped DAG novelty bits is wrong on both counts: the
// manager pre-seeds those bits on restart (a restarted process would never
// learn the persisted corpus shapes), and a novelty bit is a lossy 32-bit
// hash of call-name-agnostic features, so twin combos (write/pwrite64,
// open/creat) and plain hash collisions map to different cells but the same
// bit and would silently starve.
func RecordCoveragePairs(observedPairs []DAGPair) {
	covBoard.recordPairs(observedPairs)
}

// CoverageStats returns (found cells, producible cells).
func CoverageStats() (found, target int) {
	return covBoard.coverStats()
}

// DirectedPickStats returns (directed pick count, summed potential score).
func DirectedPickStats() (picks, scoreSum uint64) {
	return covBoard.directedPickStats()
}

// UnreachableComboCount returns how many combos are currently unreachable.
func UnreachableComboCount() int {
	return covBoard.unreachableComboCount()
}

// CoverageBoardReady reports whether the board has been initialized.
func CoverageBoardReady() bool {
	return covBoard.isReady()
}

// CoveragePotential returns the directed-scope uncovered producible-cell count
// of a combo (both temporal forms); 0 when the board is not ready, the combo is
// not in the producible grid, or it is suppressed as unreachable (directed-
// failure accounting).
func CoveragePotential(root, variant string, rel PathRelation) int {
	return covBoard.uncoveredPotential(root, variant, rel)
}

// DirectedCellStats returns the Phase 2b counters: directed cells inserted
// (one per batch variant), pending hits, pending misses, directed attempts
// that produced no plan, and batches that were built but failed to insert
// (they never executed, so never a miss).
func DirectedCellStats() (plans, hits, misses, noPlans, insertFails uint64) {
	return covBoard.cellStats()
}

// DirectedBatchStats returns the number of directed batches that entered
// execution; with DirectedCellStats().plans it gives the average batch fill.
func DirectedBatchStats() uint64 {
	return covBoard.batchStats()
}

// DirectedCfgErrCount returns the directed-pick precondition/config anomalies
// (planner inputs that should never occur, e.g. an empty or mismatched
// hmcfg.Cids). Kept out of the cell ledger.
func DirectedCfgErrCount() uint64 {
	return covBoard.cfgErrCount()
}

// CellUnreachableCount returns how many concrete cells are currently
// suppressed by the 2b miss accounting.
func CellUnreachableCount() int {
	return covBoard.cellUnreachableCount()
}

// NoteDirectedInsertFail records a directed plan that was built but failed to
// insert (generator/timing/recheck): it never executed and must not count as
// a cell miss.
func NoteDirectedInsertFail() {
	covBoard.noteDirectedInsertFail()
}

// NoteDirectedNoPlan records a directed attempt that found nothing to target
// (enumeration space saturated, tree too small, or every combo filtered).
func NoteDirectedNoPlan() {
	covBoard.noteDirectedNoPlan()
}

// NoteDirectedCfgErr records a directed attempt aborted by a precondition or
// config anomaly (see DirectedCfgErrCount).
func NoteDirectedCfgErr() {
	covBoard.noteDirectedCfgErr()
}

// ResetCoverageBoard clears the global board (test helper).
func ResetCoverageBoard() {
	covBoard.reset()
}

// cellInDomain reports whether the cell's depth bucket was present in the
// initial tree's domain. This is a model-consistency constraint, not a target
// margin: found must stay within the producible model's domain
// (computeProducibleLocked builds its depthList from the same b.depths), so
// prod-found can never go negative and the 2b enumerator cannot aim at cells
// the model cannot count. There is no static target subset anymore: every
// producible cell counts towards found/target/potential (full scoring,
// 2026-09-28; the former OR margins excluded only 4-17% of the cell space).
func (b *coverageBoard) cellInDomain(ck cellKey) bool {
	return b.depths[ck.DepthB]
}

// computeProducibleLocked computes the per-temporal producible-cell counts of
// a combo arithmetically (no cell enumeration). The root's return bucket is
// pinned to Success (retA=1): directed cells target B-side margins with a
// working root, and this removes the root's producible-ret multiplicity from
// the potential (a root with 3 reachable rets would otherwise get 3x the
// potential of a single-ret root). There is no target subset: every producible
// cell counts (full scoring).
//
// The modifier gates mirror ExtractPairs reachability:
//   - an HB pair requires the temporally earlier vertex (the pair's root) to
//     be a succeeded modifier, so a non-modifier root can never produce HB
//     cells (previously counted, inflating those combos' potential ~2x);
//   - a CC pair requires at least one side to be a modifier (by type), so a
//     combo whose root and variant are both non-modifiers produces nothing.
//
// Persist is depth-conditional: a persist path is the persist dir itself or a
// descendant (path-prefix match, see cellKeyOf), so its depth bucket is at
// least the dir's bucket (buckets are monotone in slash count). One extra
// depth multiplicity is counted per domain bucket that can hold persist paths.
//
// The caller must hold mu.
func (b *coverageBoard) computeProducibleLocked(ck comboKey) [2]int {
	retACount := 1 // RetSuccess only; see the doc comment above
	retB := producibleRetsFor(b.profile, ck.Variant)
	offCount := 0
	for _, r := range retB {
		offCount += len(producibleOffBucketsForRet(ck.Variant, r))
	}
	var depthList []uint64
	for d := 0; d < 4; d++ {
		if b.depths[d] {
			depthList = append(depthList, uint64(d))
		}
	}
	typeList := producibleTypesFor(b.profile, ck.Variant)

	// Non-persist cells exist at every domain bucket; persist cells only at
	// buckets >= the persist dir's own bucket. The >= also covers shallow
	// persist dirs, whose descendants may cross bucket thresholds.
	depthMult := len(depthList)
	if b.persistDir != "" {
		for _, d := range depthList {
			if d >= b.persistBucket {
				depthMult++
			}
		}
	}
	total := retACount * offCount * depthMult * len(typeList)
	rootMod := isModifierCall(ck.Root)
	variantMod := isModifierCall(ck.Variant)
	var hb, cc int
	switch {
	case rootMod:
		hb, cc = total, total
	case variantMod:
		hb, cc = 0, total
	default:
		// Neither side is a modifier: no pair can form.
	}
	return [2]int{hb, cc}
}

// cellKeyOf maps a pair onto its coverage cell. persistDir is the run's
// persistence directory (hmcfg.Persistence_dir); the pure form is shared by
// the board's recordPairs and the Phase 2b post-execution pending matching.
func cellKeyOf(p *DAGPair, persistDir string) (cellKey, bool) {
	root, variant, temporal, ok := DagPairToVariant(p)
	if !ok {
		return cellKey{}, false
	}
	offB := offsetBucketOf(p.B)
	if p.B.RetBucket == RetENOENT && isOffsetCall(p.B.CallName) {
		// The model folds an offset call's ENOENT cells onto OffB=NA
		// (producibleOffBucketsForRet): the common failure is a lookup miss,
		// and a late setattr/open ENOENT must land in the same cell.
		offB = offsetBucketNA
	}
	ck := cellKey{
		Root:     root,
		Variant:  variant.CallName,
		Rel:      variant.PathRelation,
		Temporal: temporal,
		RetA:     p.A.RetBucket,
		RetB:     p.B.RetBucket,
		OffB:     offB,
		DepthB:   DepthBucketOf(p.B.Path),
		TypeB:    p.B.IsDir,
		PersistB: pathInDir(p.B.Path, persistDir),
	}
	return ck, true
}

// cellKeyOfLocked maps a novel pair onto its coverage cell. The caller must
// hold mu.
func (b *coverageBoard) cellKeyOfLocked(p *DAGPair) (cellKey, bool) {
	return cellKeyOf(p, b.persistDir)
}

func (b *coverageBoard) recordPairs(newPairs []DAGPair) {
	if len(newPairs) == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.ready {
		return
	}
	for i := range newPairs {
		p := &newPairs[i]
		// Event-shape ledger (call-name agnostic): the first sighting of a
		// shape is exactly the first novel feedback bit, so recording here
		// keeps the shape set complete while the fuzzer-side novelty unit
		// stays untouched.
		if p.A != nil && p.B != nil {
			shape := hashShape(p.Temporal, p.PathRel,
				featuresOfDir(p.A, b.persistDir), featuresOfDir(p.B, b.persistDir))
			if !b.knownShapes[shape] {
				b.knownShapes[shape] = true
				b.shapeEpoch++
				// Old-epoch entries are already dead (the lookup requires an
				// epoch match), so drop them wholesale instead of letting them
				// linger until their key is touched again.
				b.shapeDrainCache = make(map[shapeDrainKey]shapeDrainEntry)
			}
		}
		ck, ok := b.cellKeyOfLocked(p)
		if !ok {
			continue
		}
		b.markDiscoveredLocked(ck)
	}
}

// markDiscoveredLocked records a sighting of cell ck: a first sighting clears
// the cell's directed-failure suppression and counts towards the combo's found
// ledger, while out-of-model observations still mark the exact cell discovered
// (without deflating the combo's potential below its undiscovered model
// cells). The caller must hold mu.
func (b *coverageBoard) markDiscoveredLocked(ck cellKey) {
	if b.discovered[ck] == 0 {
		// The cell demonstrably exists (Phase 2b accounting).
		delete(b.cellAttempts, ck)
		if b.cellUnreachable[ck] {
			delete(b.cellUnreachable, ck)
			fk := comboKey{Root: ck.Root, Variant: ck.Variant, Rel: ck.Rel}
			if b.cellSuppressed[fk] > 0 {
				b.cellSuppressed[fk]--
				if b.cellSuppressed[fk] == 0 {
					delete(b.cellSuppressed, fk)
				}
			}
		}
		// Count a cell towards the combo only on first sight, only when the
		// root succeeded (the scored set pins retA=Success; see
		// computeProducibleLocked) and only inside the producible model's
		// dimension tables.
		if ck.RetA == RetSuccess && b.cellInDomain(ck) && b.cellInModel(ck) {
			fk := comboKey{Root: ck.Root, Variant: ck.Variant, Rel: ck.Rel}
			f := b.found[fk]
			f[int(ck.Temporal)]++
			b.found[fk] = f
			// F1: a productive combo lifts its directed-failure suppression
			// and re-baselines its window, so it can re-enter directed
			// selection once new evidence arrives (tree growth, the 2b
			// planner, refreshed model tables).
			delete(b.unreachable, fk)
			b.directedAttempts[fk] = 0
			b.resolveMark[fk] = 0
			b.foundSnapshot[fk] = f[0] + f[1]
		}
	}
	b.discovered[ck]++
}

// cellInModel reports whether the cell lies inside the producible model: the
// combo is in the grid, its temporal form is producible (prod>0, e.g. an
// open(O_CREAT) event carries FuncCreate at the extractor level but the call
// name "open" is not a modifier, so its HB/CC pairs have no model cells), and
// RetB/OffB/TypeB are listed. OffB is checked against the ret-conditional
// table (producibleOffBucketsForRet): an offset-carrying call's ENOENT cells
// exist only at NA, where the failed-lookup event lands. The caller must hold
// b.mu.
func (b *coverageBoard) cellInModel(ck cellKey) bool {
	ckm := comboKey{Root: ck.Root, Variant: ck.Variant, Rel: ck.Rel}
	prod, ok := b.producible[ckm]
	if !ok {
		return false
	}
	if prod[int(ck.Temporal)] == 0 {
		return false
	}
	inRets := false
	for _, r := range producibleRetsFor(b.profile, ck.Variant) {
		if r == ck.RetB {
			inRets = true
			break
		}
	}
	if !inRets {
		return false
	}
	inOffs := false
	for _, o := range producibleOffBucketsForRet(ck.Variant, ck.RetB) {
		if o == ck.OffB {
			inOffs = true
			break
		}
	}
	if !inOffs {
		return false
	}
	for _, t := range producibleTypesFor(b.profile, ck.Variant) {
		if t == ck.TypeB {
			return true
		}
	}
	return false
}

// shapesAllKnown reports whether every candidate shape of a planned cell is
// already known. The directed selection guard uses it to skip cells that can
// only reproduce event shapes the feedback already has; unknown board state
// keeps the plan (false).
func (b *coverageBoard) shapesAllKnown(shapes []uint64) bool {
	if len(shapes) == 0 {
		return false
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	if !b.ready {
		return false
	}
	for _, s := range shapes {
		if !b.knownShapes[s] {
			return false
		}
	}
	return true
}

// cellShapeKnown reports whether every candidate event shape of one uncovered
// cell is already in the shape ledger, given the root call's A-side context
// (func class, base-path depth/type/persist margins) and the event-level path
// relation. Candidate shapes keep the cell's temporal form and enumerate the
// root's possible offset buckets; the root's ret is pinned to Success (the
// cell model counts successful roots only; failure shapes are injected by the
// generator independently). An unpredictable variant func class keeps the
// cell "unknown" (false). The caller must hold b.mu (shapeDrained holds the
// write lock).
func (b *coverageBoard) cellShapeKnown(cell cellKey, aFunc uint32, aDepth uint64, aIsDir, aPersist bool, rel PathRel) bool {
	bFuncs, ok := predictedEventFuncs(b.profile, cell.Variant, cell.TypeB, cell.RetB, cell.Temporal)
	if !ok {
		return false
	}
	for _, bFunc := range bFuncs {
		bf := packFeatures(bFunc, cell.RetB, cell.DepthB, cell.TypeB, cell.PersistB, cell.OffB)
		for _, off := range rootOffsetBuckets(cell.Root, false) {
			af := packFeatures(aFunc, RetSuccess, aDepth, aIsDir, aPersist, off)
			if !b.knownShapes[hashShape(cell.Temporal, rel, af, bf)] {
				return false
			}
		}
	}
	return true
}

// shapeDrained reports whether every uncovered cell of the combo can only
// reproduce event shapes the feedback already has, for the given root A-side
// context. It is the Direction 3 counterpart of the 2b shapeCovered guard:
// generation-side variant selection has the base path at hand, so the A
// context is derived directly instead of from a witness view. Conservative:
// an empty/unknown combo, an unknown relation, or the flags-ambiguous "open"
// variant all return false (the directed pick is kept). Outcomes are memoized
// per (combo, A context) at the current shapeEpoch; shapes only grow, so the
// memo is invalidated by epoch changes (reset() clears it). The mutation is
// done under the write lock: several proc goroutines run Direction 3
// concurrently on the shared board.
func (b *coverageBoard) shapeDrained(ck comboKey, aFunc uint32, aDepth uint64, aIsDir, aPersist bool, rel PathRel) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.shapeDrainedLocked(ck, aFunc, aDepth, aIsDir, aPersist, rel)
}

// shapeDrainedLocked is shapeDrained's body with the A context already
// materialized; the caller must hold b.mu (write lock). Keeping the A-context
// derivation (e.g. the base path's persist prefix) and the lookup in one
// critical section removes the window where a board reset could pair a stale
// margin flag with a new board state.
func (b *coverageBoard) shapeDrainedLocked(ck comboKey, aFunc uint32, aDepth uint64, aIsDir, aPersist bool, rel PathRel) bool {
	if rel == PathNone || ck.Variant == "open" {
		return false
	}
	if !b.ready {
		return false
	}
	prod, ok := b.producible[ck]
	if !ok || (prod[0] == 0 && prod[1] == 0) {
		return false
	}
	key := shapeDrainKey{Combo: ck, AFunc: aFunc, ADepth: aDepth, AIsDir: aIsDir, APersist: aPersist}
	if e, ok := b.shapeDrainCache[key]; ok && e.Epoch == b.shapeEpoch {
		return e.Drained
	}
	drained := true
	n := 0
	b.forEachUncoveredCellKey(ck, func(cell cellKey) {
		n++
		if drained && !b.cellShapeKnown(cell, aFunc, aDepth, aIsDir, aPersist, rel) {
			drained = false
		}
	})
	if n == 0 {
		// Nothing enumerable (combo suppressed or every cell cell-suppressed):
		// "drained" would be vacuous and could go stale without an epoch
		// change, so stay conservative and keep the directed pick.
		return false
	}
	b.shapeDrainCache[key] = shapeDrainEntry{Epoch: b.shapeEpoch, Drained: drained}
	return drained
}

// direction3ShapeDrain derives the Direction 3 A-side context from the
// generation-side base path and reports whether the picked combo is
// shape-drained there, so the caller can fall back to the static sampler.
// Unknown inputs (no path, unpredictable root func class, non-derivable
// relation) keep the directed pick (false).
func (b *coverageBoard) direction3ShapeDrain(rootCall, variant string, rel PathRelation, basePath string, isDir bool) bool {
	if basePath == "" {
		return false
	}
	aFuncs, ok := callFuncClasses(b.profile, rootCall, isDir)
	if !ok || len(aFuncs) != 1 {
		// An ambiguous A class (creat fires both create and open) has no
		// single-function A context for the memo key; keep the directed pick.
		return false
	}
	aFunc := aFuncs[0]
	dagRel := dagRelOfPathRelation(rel)
	if dagRel == PathNone {
		return false
	}
	// One critical section: derive the persist margin from the same board
	// state the lookup runs against.
	b.mu.Lock()
	defer b.mu.Unlock()
	persist := pathInDir(basePath, b.persistDir)
	return b.shapeDrainedLocked(comboKey{Root: rootCall, Variant: variant, Rel: rel},
		aFunc, DepthBucketOf(basePath), isDir, persist, dagRel)
}

// uncoveredPotentials returns the uncovered producible-cell counts of the
// given candidates under one read lock (per-combo lookup of the precomputed
// tables).
func (b *coverageBoard) uncoveredPotentials(root string, cands []CallVariant) []int {
	res := make([]int, len(cands))
	b.mu.RLock()
	defer b.mu.RUnlock()
	if !b.ready {
		return res
	}
	for i := range cands {
		ck := comboKey{Root: root, Variant: cands[i].CallName, Rel: cands[i].PathRelation}
		if b.unreachable[ck] {
			continue
		}
		prod, ok := b.producible[ck]
		if !ok {
			continue
		}
		found := b.found[ck]
		for t := 0; t < 2; t++ {
			if d := prod[t] - found[t]; d > 0 {
				res[i] += d
			}
		}
	}
	return res
}

// pickByCoveragePotential samples a candidate with probability proportional
// to its uncovered producible-cell count (potential roulette). Roulette, not
// argmax, is deliberate: argmax would direct the whole budget onto a single
// combo until its potential drains below the runner-up, whereas roulette
// spreads across combos in proportion to what they still have to offer.
// Returns (nil, 0) when every candidate has zero potential (so the caller
// falls back to the weighted sampler).
func pickByCoveragePotential(cand []CallVariant, rootCall string, r *rand.Rand) (*CallVariant, int) {
	pots := covBoard.uncoveredPotentials(rootCall, cand)
	idx := pickWeightedIndex(pots, r)
	if idx < 0 {
		return nil, 0
	}
	return &cand[idx], pots[idx]
}

// pickWeightedIndex picks an index with probability proportional to pots[i];
// zero/negative entries are never picked. Returns -1 when total <= 0.
func pickWeightedIndex(pots []int, r *rand.Rand) int {
	total := 0
	for _, p := range pots {
		if p > 0 {
			total += p
		}
	}
	if total <= 0 {
		return -1
	}
	x := r.Intn(total)
	acc := 0
	for i, p := range pots {
		if p <= 0 {
			continue
		}
		acc += p
		if x < acc {
			return i
		}
	}
	return -1
}

// uncoveredPotential is the single-combo variant of uncoveredPotentials.
func (b *coverageBoard) uncoveredPotential(root, variant string, rel PathRelation) int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if !b.ready {
		return 0
	}
	ck := comboKey{Root: root, Variant: variant, Rel: rel}
	if b.unreachable[ck] {
		return 0
	}
	prod, ok := b.producible[ck]
	if !ok {
		return 0
	}
	found := b.found[ck]
	pot := 0
	for t := 0; t < 2; t++ {
		if d := prod[t] - found[t]; d > 0 {
			pot += d
		}
	}
	return pot
}

// forEachUncoveredCellKey enumerates the uncovered cells of one combo in the
// producible space: RetA pinned to Success, every producible RetB/OffB/TypeB,
// every depth bucket in the run's domain, both persistence margins and both
// temporal forms (a form is skipped when its precomputed producible count is
// zero, i.e. the ExtractPairs modifier gate). fn receives the full cell.
// The caller must hold b.mu for reading.
func (b *coverageBoard) forEachUncoveredCellKey(ck comboKey, fn func(cell cellKey)) {
	if !b.ready || b.unreachable[ck] {
		return
	}
	prod, ok := b.producible[ck]
	if !ok {
		return
	}
	var depthList [4]uint64
	nd := 0
	for d := 0; d < 4; d++ {
		if b.depths[d] {
			depthList[nd] = uint64(d)
			nd++
		}
	}
	if nd == 0 {
		return
	}
	for _, rb := range producibleRetsFor(b.profile, ck.Variant) {
		for _, off := range producibleOffBucketsForRet(ck.Variant, rb) {
			for _, isDir := range producibleTypesFor(b.profile, ck.Variant) {
				for i := 0; i < nd; i++ {
					depth := depthList[i]
					persists := [2]bool{false}
					np := 1
					if b.persistDir != "" && depth >= b.persistBucket {
						persists[1] = true
						np = 2
					}
					for pi := 0; pi < np; pi++ {
						for t := 0; t < 2; t++ {
							if prod[t] == 0 {
								continue
							}
							cell := cellKey{
								Root: ck.Root, Variant: ck.Variant, Rel: ck.Rel,
								Temporal: TemporalRel(t), RetA: RetSuccess, RetB: rb,
								OffB: off, PersistB: persists[pi], DepthB: depth, TypeB: isDir,
							}
							if b.discovered[cell] != 0 || b.cellUnreachable[cell] {
								continue
							}
							fn(cell)
						}
					}
				}
			}
		}
	}
}

// forEachUncoveredCell is the margin-only view of forEachUncoveredCellKey.
// The caller must hold b.mu for reading.
func (b *coverageBoard) forEachUncoveredCell(ck comboKey, fn func(off, depth uint64, ret RetBucket)) {
	b.forEachUncoveredCellKey(ck, func(cell cellKey) {
		fn(cell.OffB, cell.DepthB, cell.RetB)
	})
}

// uncoveredOffPots aggregates, for every combo whose variant is in variants
// (any root and relation), the uncovered cells per offset bucket. The result
// is indexed by the offset bucket value (offsetBucketNA..offsetBucketBeyond).
// RetENOENT cells are skipped: a failed path lookup never enters the offset
// bucket space (offsetBucketOf requires an offset FuncID), so the model's
// ENOENT x bucket cells are unobservable for this margin.
func (b *coverageBoard) uncoveredOffPots(variants []string) [5]int {
	var pots [5]int
	if len(variants) == 0 {
		return pots
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	if !b.ready {
		return pots
	}
	set := make(map[string]bool, len(variants))
	for _, v := range variants {
		set[v] = true
	}
	for ck := range b.producible {
		if !set[ck.Variant] {
			continue
		}
		b.forEachUncoveredCell(ck, func(off, _ uint64, ret RetBucket) {
			if ret == RetENOENT {
				return
			}
			if off < uint64(len(pots)) {
				pots[off]++
			}
		})
	}
	return pots
}

// uncoveredDepthPots aggregates the uncovered cells of one combo per depth
// bucket (the DepthB margin). ENOENT cells count here: a failed lookup has a
// real path and depth even though its offset bucket is NA.
func (b *coverageBoard) uncoveredDepthPots(root, variant string, rel PathRelation) [4]int {
	var pots [4]int
	b.mu.RLock()
	defer b.mu.RUnlock()
	b.forEachUncoveredCell(comboKey{Root: root, Variant: variant, Rel: rel}, func(_, depth uint64, _ RetBucket) {
		if depth < 4 {
			pots[depth]++
		}
	})
	return pots
}

func (b *coverageBoard) isReady() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.ready
}

// noteDirectedPick records a directed pick of combo ck (score pot) and runs
// the combo-level unreachable accounting: once a combo has been picked
// unreachableThreshold times without its found count moving, it is marked
// unreachable and drops out of directed selection. The found snapshot is
// re-baselined whenever the combo produced a cell inside its window, so
// productive combos are never suppressed.
func (b *coverageBoard) noteDirectedPick(ck comboKey, pot int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.directedPicks++
	b.directedScoreSum += uint64(pot)
	a := b.directedAttempts[ck] + 1
	b.directedAttempts[ck] = a
	if a-b.resolveMark[ck] >= unreachableThreshold {
		f := 0
		if fc, ok := b.found[ck]; ok {
			f = fc[0] + fc[1]
		}
		if f == b.foundSnapshot[ck] {
			b.unreachable[ck] = true
		} else {
			b.resolveMark[ck] = a
			b.foundSnapshot[ck] = f
		}
	}
}

// noteDirectedMiss records one planned execution of cell ck that produced no
// matching pair. Consecutive misses suppress the cell from 2b enumeration. A
// cell already discovered before this pending resolved (a cross-proc race)
// is not charged: the miss would only add noise, and the cell is out of the
// 2b pool anyway.
func (b *coverageBoard) noteDirectedMiss(ck cellKey) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.discovered[ck] != 0 {
		delete(b.cellAttempts, ck)
		return
	}
	b.cellMisses++
	n := b.cellAttempts[ck] + 1
	b.cellAttempts[ck] = n
	if n >= cellMissThreshold && !b.cellUnreachable[ck] {
		b.cellUnreachable[ck] = true
		b.cellSuppressed[comboKey{Root: ck.Root, Variant: ck.Variant, Rel: ck.Rel}]++
	}
}

// noteDirectedHit records a match (or a fresh discovery) of cell ck and
// clears its miss baseline. The hit's exact pair was observed in this
// execution, so the discovery ledger is updated here too: the pair may never
// have reached recordPairs when its bit was already known globally.
func (b *coverageBoard) noteDirectedHit(ck cellKey) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cellHits++
	delete(b.cellAttempts, ck)
	if b.cellUnreachable[ck] {
		// Defensive: a suppressed cell is excluded from plan enumeration, so
		// this only fires if a pending/hit path ever observes one.
		delete(b.cellUnreachable, ck)
		fk := comboKey{Root: ck.Root, Variant: ck.Variant, Rel: ck.Rel}
		if b.cellSuppressed[fk] > 0 {
			b.cellSuppressed[fk]--
			if b.cellSuppressed[fk] == 0 {
				delete(b.cellSuppressed, fk)
			}
		}
	}
	b.markDiscoveredLocked(ck)
}

// noteDirectedPlans records that n directed cells (one batch) entered
// execution. Cells and batches are counted separately so the diagnostics can
// show the average batch fill (how much of the node budget was used).
func (b *coverageBoard) noteDirectedPlans(n int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cellPlans += uint64(n)
	b.cellBatches++
}

// noteDirectedInsertFail records a plan that was built but failed to insert
// (it never executed, so it is not a cell miss).
func (b *coverageBoard) noteDirectedInsertFail() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cellInsertFails++
}

// noteDirectedNoPlan records a directed attempt with no targetable plan.
func (b *coverageBoard) noteDirectedNoPlan() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cellNoPlans++
}

// noteDirectedCfgErr records a directed attempt aborted by a config anomaly.
func (b *coverageBoard) noteDirectedCfgErr() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cellCfgErrs++
}

// cellUncovered reports whether ck is still a valid directed-plan target:
// undiscovered, inside the producible model, success root, and not
// cell-suppressed. Mirrors uncoveredCellFlags (the batch form) exactly.
func (b *coverageBoard) cellUncovered(ck cellKey) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if !b.ready || ck.RetA != RetSuccess || !b.cellInDomain(ck) || !b.cellInModel(ck) {
		return false
	}
	return b.discovered[ck] == 0 && !b.cellUnreachable[ck]
}

// uncoveredCellFlags evaluates the given cell candidates under one read lock
// (batch form used by the plan enumerator).
func (b *coverageBoard) uncoveredCellFlags(cells []cellKey) []bool {
	res := make([]bool, len(cells))
	b.mu.RLock()
	defer b.mu.RUnlock()
	if !b.ready {
		return res
	}
	for i := range cells {
		ck := cells[i]
		res[i] = ck.RetA == RetSuccess && b.cellInDomain(ck) && b.cellInModel(ck) &&
			b.discovered[ck] == 0 && !b.cellUnreachable[ck]
	}
	return res
}

func (b *coverageBoard) cellStats() (plans, hits, misses, noPlans, insertFails uint64) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.cellPlans, b.cellHits, b.cellMisses, b.cellNoPlans, b.cellInsertFails
}

func (b *coverageBoard) batchStats() uint64 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.cellBatches
}

func (b *coverageBoard) cfgErrCount() uint64 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.cellCfgErrs
}

func (b *coverageBoard) cellUnreachableCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.cellUnreachable)
}

// comboSelectableFlags reports which of the given combos are in the producible
// grid and not suppressed at the combo (directed) scope. Batch form used by the
// Phase 2b plan enumerator.
func (b *coverageBoard) comboSelectableFlags(cks []comboKey) map[comboKey]bool {
	res := make(map[comboKey]bool, len(cks))
	b.mu.RLock()
	defer b.mu.RUnlock()
	if !b.ready {
		return res
	}
	for _, ck := range cks {
		if b.unreachable[ck] {
			continue
		}
		if _, ok := b.producible[ck]; ok {
			res[ck] = true
		}
	}
	return res
}

// comboPotentials returns the 2b-available uncovered potential of each combo
// in one batch (0 when the board is not ready, the combo is outside the
// producible grid, or it is suppressed at the combo scope). found[t] counts
// distinct discovered cells and cellSuppressed subtracts the cells the 2b miss
// ledger currently suppresses, so a combo whose remaining uncovered cells are
// all cell-suppressed reads 0 and the planner skips it immediately instead of
// waiting for the combo-level blind-pick retirement. The 2a APIs
// (uncoveredPotentials / CoveragePotential) intentionally keep the raw model
// potential: generation can still observe cells the 2b templates gave up on.
func (b *coverageBoard) comboPotentials(cks []comboKey) []int {
	res := make([]int, len(cks))
	b.mu.RLock()
	defer b.mu.RUnlock()
	if !b.ready {
		return res
	}
	for i, ck := range cks {
		if b.unreachable[ck] {
			continue
		}
		prod, ok := b.producible[ck]
		if !ok {
			continue
		}
		found := b.found[ck]
		pot := 0
		for t := 0; t < 2; t++ {
			if d := prod[t] - found[t]; d > 0 {
				pot += d
			}
		}
		pot -= b.cellSuppressed[ck]
		if pot > 0 {
			res[i] = pot
		}
	}
	return res
}

func (b *coverageBoard) coverStats() (found, target int) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, f := range b.found {
		found += f[0] + f[1]
	}
	for _, p := range b.producible {
		target += p[0] + p[1]
	}
	return found, target
}

func (b *coverageBoard) directedPickStats() (uint64, uint64) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.directedPicks, b.directedScoreSum
}

// unreachableComboCount returns how many combos are currently unreachable.
func (b *coverageBoard) unreachableComboCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.unreachable)
}

func (b *coverageBoard) reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.discovered = make(map[cellKey]int)
	b.producible = make(map[comboKey][2]int)
	b.found = make(map[comboKey][2]int)
	b.knownShapes = make(map[uint64]bool)
	b.shapeEpoch = 0
	b.shapeDrainCache = make(map[shapeDrainKey]shapeDrainEntry)
	b.profile = nil
	b.persistDir = ""
	b.persistBucket = 0
	b.depths = [4]bool{}
	b.ready = false
	b.directedPicks = 0
	b.directedScoreSum = 0
	b.directedAttempts = make(map[comboKey]int)
	b.resolveMark = make(map[comboKey]int)
	b.foundSnapshot = make(map[comboKey]int)
	b.unreachable = make(map[comboKey]bool)
	b.cellAttempts = make(map[cellKey]int)
	b.cellUnreachable = make(map[cellKey]bool)
	b.cellSuppressed = make(map[comboKey]int)
	b.cellPlans = 0
	b.cellBatches = 0
	b.cellHits = 0
	b.cellMisses = 0
	b.cellNoPlans = 0
	b.cellInsertFails = 0
	b.cellCfgErrs = 0
}

// producibleRets is the Phase-1 static reachability table (Doc A.4): which
// RetBuckets a call can realistically produce. Refresh from the next run's
// `dag marg` (RetByFunc/ErrHist) data. Note that within RetFailure the pair
// bit cannot distinguish ENOTEMPTY from EINVAL; errno detail stays offline.
func producibleRets(call string) []RetBucket {
	return producibleRetsFor(hmdfsProfile, call)
}

func producibleRetsFor(profile *DFSProfile, call string) []RetBucket {
	if profile != nil && profile.Name == "glusterfs" {
		switch call {
		case "link", "symlink", "readlink":
			return []RetBucket{RetSuccess}
		case "getxattr":
			return []RetBucket{RetSuccess, RetFailure}
		case "setxattr":
			return []RetBucket{RetSuccess, RetEEXIST}
		case "rmdir", "rename":
			return []RetBucket{RetSuccess, RetFailure}
		case "unlink":
			return []RetBucket{RetSuccess}
		}
	}
	switch call {
	case "mkdir":
		return []RetBucket{RetSuccess, RetEEXIST}
	case "creat":
		return []RetBucket{RetSuccess, RetFailure}
	case "unlink", "chmod", "stat", "truncate", "open":
		return []RetBucket{RetSuccess, RetENOENT}
	case "rmdir", "rename":
		return []RetBucket{RetSuccess, RetENOENT, RetFailure}
	case "link", "symlink":
		return []RetBucket{RetSuccess, RetEEXIST, RetENOENT, RetFailure}
	case "readlink", "getxattr", "setxattr":
		return []RetBucket{RetSuccess, RetENOENT, RetFailure}
	case "write", "pwrite64":
		return []RetBucket{RetSuccess, RetFailure}
	case "getdents64":
		return []RetBucket{RetSuccess, RetENOENT}
	default: // read, pread64, fsync, fdatasync
		return []RetBucket{RetSuccess}
	}
}

// isOffsetCall reports whether the syscall carries offset semantics, at the
// call-name level: the FuncID groups the kernel function, the call name
// refines the semantics (the same layering as funcMatchesCall's write/
// pwrite64). Contains-style matching keeps suffixed variants classified;
// chmod/utime share FuncSetattr with truncate but carry no ia_size and must
// classify as NA.
func isOffsetCall(call string) bool {
	if strings.Contains(call, "readlink") {
		return false
	}
	return strings.Contains(call, "write") ||
		strings.Contains(call, "read") ||
		strings.Contains(call, "truncate")
}

func producibleOffBuckets(call string) []uint64 {
	if !isOffsetCall(call) {
		return []uint64{offsetBucketNA}
	}
	return []uint64{offsetBucketZero, offsetBucketMid, offsetBucketTail, offsetBucketBeyond}
}

// producibleOffBucketsForRet narrows the offset dimension by return bucket: a
// failed path lookup never enters the offset-bucket space (the event is a
// FuncLookup with OffB=NA), so an offset-carrying call's ENOENT cells exist
// only at NA. Every other (ret, off) combination keeps producibleOffBuckets.
// This is the single source for the model's per-cell grid (count, enumeration,
// and found gate), so truncate no longer counts four unobservable
// ENOENT x bucket cells nor rejects the observable ENOENT x NA one.
func producibleOffBucketsForRet(call string, ret RetBucket) []uint64 {
	if ret == RetENOENT && isOffsetCall(call) {
		return []uint64{offsetBucketNA}
	}
	return producibleOffBuckets(call)
}

func producibleTypes(call string) []bool {
	return producibleTypesFor(hmdfsProfile, call)
}

func producibleTypesFor(profile *DFSProfile, call string) []bool {
	if profile != nil && profile.Name == "glusterfs" {
		switch call {
		case "readlink":
			return []bool{false}
		case "symlink":
			return []bool{false, true}
		}
	}
	switch profile.TargetConstraint(call) {
	case relTargetFile:
		return []bool{false}
	case relTargetDir:
		return []bool{true}
	default:
		return []bool{false, true}
	}
}

package conformance

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"
)

// The repository-state suite checks draft-fossil-repo-state-01. Most of its
// cases are a scripted sequence of artifacts and the derived rows the draft
// says loading it produces. Every case is also loaded into fossil(1), whose
// rows must equal the expectation, so an expectation cannot drift from
// fossil unnoticed. Each case runs through manifest.Crosslink, once as one
// sweep and once per artifact; most also run as a ".rebuild" case through
// verify.Rebuild on the same artifacts.

// rsCase is one scripted sequence and the rows it must derive.
type rsCase struct {
	Case
	arts []rsArt
	// tables are the tables compared; a table with no want rows must be
	// empty. tags, when set, limits tagxref to those tag names.
	tables []string
	tags   []string
	// want is every expected row of the compared tables, "table: row".
	want []string
	// ignoring is what the compared rows would be if go-libfossil ignored
	// the requirement the case names: "+table: row" for a row it would
	// add, "-table: row" for one it would lack. TestRepoStateReasons
	// checks that it differs from want, so a case cannot pass by accident.
	ignoring []string
	// each loads one artifact at a time only, for cases where arrival
	// order is the point.
	each bool
	// batch loads all the artifacts in one sweep only, for cases where
	// crosslinking them together is the point.
	batch bool
	// rebuild adds a ".rebuild" case: the same artifacts, stored without
	// crosslinking, then derived by verify.Rebuild.
	rebuild bool
	// doctor edits the tables partway through a per-artifact load.
	doctor *rsDoctor
	// pull, when set, adds a ".pull" case: the first pull artifacts are
	// stored and crosslinked locally, then a sync session pulls the rest
	// from a go-libfossil server holding them all, so the phantom-fill
	// cascade, not a sweep, links what arrives.
	pull int
	// incremental says why fossil's per-artifact crosslink is not the
	// reference for this case; `fossil rebuild` is used instead, and the
	// case fails once the two agree.
	incremental string
	// rebuildWrites says why `fossil rebuild` is not the reference for the
	// ".rebuild" case: it stores artifacts of its own. fossil's
	// per-artifact result is used instead.
	rebuildWrites string
	// fossil is set only when fossil(1) derives other rows than want: the
	// case follows the draft text, and fossil says why and names the
	// erratum proposed. The case fails once fossil agrees with want.
	fossil string
}

// rsVariant is one way of loading a case's artifacts into go-libfossil.
type rsVariant struct {
	Case
	loads []rsLoad
}

// variants lists the crosslink case and, when asked for, the rebuild case.
func (c rsCase) variants() []rsVariant {
	crosslink := rsVariant{Case: c.Case, loads: []rsLoad{loadSweep, loadEach}}
	if c.each || c.doctor != nil {
		crosslink.loads = []rsLoad{loadEach}
	}
	if c.batch {
		if c.each || c.doctor != nil {
			panic("conformance: " + c.ID + " loads both in one batch and per artifact")
		}
		crosslink.loads = []rsLoad{loadSweep}
	}
	crosslink.Gap = repostateGaps[c.ID]
	crosslink.Deviation = repostateDeviations[c.ID]
	all := []rsVariant{crosslink}
	if c.pull > 0 {
		pulled := rsVariant{Case: c.Case, loads: []rsLoad{loadPull}}
		pulled.ID = c.ID + ".pull"
		pulled.Gap = repostateGaps[pulled.ID]
		pulled.Deviation = repostateDeviations[pulled.ID]
		all = append(all, pulled)
	}
	if c.rebuild {
		rebuilt := rsVariant{Case: c.Case, loads: []rsLoad{loadRebuild}}
		rebuilt.ID = c.ID + ".rebuild"
		rebuilt.Gap = repostateGaps[rebuilt.ID]
		rebuilt.Deviation = repostateDeviations[rebuilt.ID]
		all = append(all, rebuilt)
	}
	return all
}

// expected groups want by table.
func (c rsCase) expected() rsState {
	state := rsState{}
	for _, table := range c.tables {
		state[table] = nil
	}
	for _, line := range c.want {
		table, row, ok := strings.Cut(line, ": ")
		if !ok {
			panic("conformance: want row without a table: " + line)
		}
		if _, listed := state[table]; !listed {
			panic("conformance: " + c.ID + " wants a row of unlisted table " + table)
		}
		state[table] = append(state[table], row)
	}
	for table := range state {
		sort.Strings(state[table])
	}
	return state
}

func TestRepoState(t *testing.T) {
	runRepoStateCases(t, repostateCases())
}

// runRepoStateCases checks each case's variants against fossil(1) and
// go-libfossil.
func runRepoStateCases(t *testing.T, cases []rsCase) {
	t.Helper()
	ref := newFossilReference(t)
	loaded := map[string]*rsFossil{}
	for _, c := range cases {
		script := rsBuild(c.arts)
		for _, v := range c.variants() {
			check(t, v.Case, func() error {
				if ref != nil {
					if err := c.fossilAgrees(ref, script, v, loaded); err != nil {
						return err
					}
				}
				return c.goConforms(t, script, v)
			})
		}
	}
}

// goConforms loads the script each way the variant names and compares the
// rows with want.
func (c rsCase) goConforms(t *testing.T, script rsScript, v rsVariant) error {
	var problems []string
	for _, load := range v.loads {
		var path string
		var err error
		if load == loadPull {
			path, err = goLoadPull(t, script, c.pull)
		} else {
			path, err = goLoad(t, script, load, c.doctor)
		}
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", load, err))
			continue
		}
		got, err := readState(path, script.labels, c.tables, c.tags)
		if err != nil {
			return err
		}
		if d := diffState(c.expected(), got, "want", load.String()); d != "" {
			problems = append(problems, d)
		}
		if slices.Contains(c.tables, "leaf") {
			issues, err := verifyLeafIssues(path, script.labels)
			if err != nil {
				return err
			}
			if len(issues) > 0 {
				problems = append(problems, fmt.Sprintf("%s: verify.Verify reports leaf issues on %v",
					load, issues))
			}
		}
	}
	if len(problems) > 0 {
		return expectf("%s", strings.Join(problems, " | "))
	}
	return nil
}

// rsFossil is what fossil(1) derived from one case's script: per artifact,
// and by `fossil rebuild` on a copy, with whether that rebuild stored
// artifacts of its own.
type rsFossil struct {
	incremental rsState
	rebuilt     rsState
	wrote       bool
}

// fossilAgrees checks want against fossil(1): its per-artifact crosslink
// for a crosslink case, its rebuild for a rebuild case, unless the case
// says why the other is the reference. loaded caches fossil's rows by case.
func (c rsCase) fossilAgrees(
	ref *fossilReference, script rsScript, v rsVariant, loaded map[string]*rsFossil,
) error {
	f, err := c.fossilLoad(ref, script, loaded)
	if err != nil {
		return err
	}
	isRebuild := v.loads[0] == loadRebuild
	reference, name := f.incremental, "fossil per-artifact"
	if c.incremental != "" && !isRebuild {
		if diffState(c.expected(), f.incremental, "want", "fossil") == "" {
			return expectf("fossil's per-artifact crosslink now gives want; remove incremental: %q",
				c.incremental)
		}
		reference, name = f.rebuilt, "fossil rebuild"
	}
	if isRebuild {
		if f.wrote != (c.rebuildWrites != "") {
			return expectf("fossil rebuild stored artifacts: %v; the case says %q",
				f.wrote, c.rebuildWrites)
		}
		if !f.wrote {
			reference, name = f.rebuilt, "fossil rebuild"
		}
	}
	d := diffState(c.expected(), reference, "want", name)
	switch {
	case c.fossil != "" && d == "":
		return expectf("%s now gives want; remove fossil: %q", name, c.fossil)
	case c.fossil == "" && d != "":
		return expectf("fossil(1) disagrees with the case: %s", d)
	}
	return nil
}

// fossilLoad loads the script into fossil(1) once per case and reads both
// its per-artifact and its rebuilt rows.
func (c rsCase) fossilLoad(
	ref *fossilReference, script rsScript, loaded map[string]*rsFossil,
) (*rsFossil, error) {
	if f, ok := loaded[c.ID]; ok {
		return f, nil
	}
	repo, err := ref.loadScript(script, c.doctor)
	if err != nil {
		return nil, err
	}
	f := &rsFossil{}
	if f.incremental, err = readState(repo.path, script.labels, c.tables, c.tags); err != nil {
		return nil, err
	}
	if c.doctor == nil {
		path, wrote, err := ref.rebuildCopy(repo)
		if err != nil {
			return nil, err
		}
		if f.rebuilt, err = readState(path, script.labels, c.tables, c.tags); err != nil {
			return nil, err
		}
		f.wrote = wrote
	}
	loaded[c.ID] = f
	return f, nil
}

// TestRepoStateReasons checks that every case says what ignoring its
// requirement would change, and that the change is visible in the rows it
// compares: a row it would add is not wanted, one it would lack is.
func TestRepoStateReasons(t *testing.T) {
	checkRepoStateReasons(t, repostateCases())
}

func checkRepoStateReasons(t *testing.T, cases []rsCase) {
	t.Helper()
	for _, c := range cases {
		if len(c.ignoring) == 0 {
			t.Errorf("%s: no ignoring rows", c.ID)
		}
		want := c.expected()
		for _, line := range c.ignoring {
			sign, rest := line[:1], line[1:]
			table, row, ok := strings.Cut(rest, ": ")
			rows, listed := want[table]
			switch {
			case !ok || (sign != "+" && sign != "-"):
				t.Errorf("%s: ignoring %q is not \"+table: row\" or \"-table: row\"", c.ID, line)
			case !listed:
				t.Errorf("%s: ignoring %q names a table the case does not compare", c.ID, line)
			case sign == "+" && slices.Contains(rows, row):
				t.Errorf("%s: ignoring adds %q, which the case already wants", c.ID, row)
			case sign == "-" && !slices.Contains(rows, row):
				t.Errorf("%s: ignoring drops %q, which the case does not want", c.ID, row)
			}
		}
	}
}

// repostateSuiteCases is what the coverage test audits for this suite.
func repostateSuiteCases() []Case {
	var all []Case
	for _, c := range repostateCases() {
		for _, v := range c.variants() {
			all = append(all, v.Case)
		}
	}
	for _, c := range rsAPICases() {
		all = append(all, c.Case)
	}
	return all
}

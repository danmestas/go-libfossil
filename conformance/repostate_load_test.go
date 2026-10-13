package conformance

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/sync"
	"github.com/danmestas/go-libfossil/internal/verify"
	"github.com/danmestas/go-libfossil/internal/xfer"
	"github.com/danmestas/go-libfossil/simio"
)

// The repository-state suite loads a scripted sequence of artifacts into
// go-libfossil and into fossil(1), then compares the tables derived from
// them. The two repositories number artifacts differently, so every rid is
// replaced by its hash, and every hash by the label the script gave it.

// rsArt is one artifact of a scripted sequence. Its body may name an earlier
// artifact as {label}; a label the script never defines stands for an
// artifact that is never loaded, which makes a phantom.
type rsArt struct {
	label string
	body  string
	// file marks file content, which is loaded as written, with no Z card.
	file bool
}

// rsBuilt is an artifact ready to load: its bytes, with the Z card, and the
// hash that names it.
type rsBuilt struct {
	label string
	uuid  string
	data  []byte
}

// rsScript is a built sequence and the label of every hash it mentions.
type rsScript struct {
	arts   []rsBuilt
	labels map[string]string // hash to label
	hashes map[string]string // label to hash
}

// rsBuild replaces each {label} with the hash of the artifact it names and
// appends the Z card. Bodies are written with their cards already in order.
// A body may name an artifact the script loads later, such as a tag that
// arrives before its target, so artifacts are hashed in passes: each pass
// builds those whose labels are all known.
func rsBuild(arts []rsArt) rsScript {
	script := rsScript{labels: map[string]string{}, hashes: map[string]string{}}
	defined := map[string]bool{}
	for _, a := range arts {
		if defined[a.label] {
			panic("conformance: label " + a.label + " defined twice")
		}
		defined[a.label] = true
	}
	built := make([]rsBuilt, len(arts))
	for pass, done := 0, 0; done < len(arts); pass++ {
		if pass == len(arts) {
			panic("conformance: artifacts name each other in a cycle")
		}
		for i, a := range arts {
			if built[i].data != nil || !script.ready(a.body, defined) {
				continue
			}
			data := []byte(a.body)
			if !a.file {
				data = withZ(script.resolve(a.body))
			}
			uuid := hash.SHA3(data)
			script.hashes[a.label] = uuid
			script.labels[uuid] = a.label
			built[i] = rsBuilt{label: a.label, uuid: uuid, data: data}
			done++
		}
	}
	script.arts = built
	return script
}

// ready reports whether every label body names is hashed, or is one the
// script never defines.
func (s rsScript) ready(body string, defined map[string]bool) bool {
	for _, part := range strings.Split(body, "{")[1:] {
		label, _, _ := strings.Cut(part, "}")
		if _, ok := s.hashes[label]; defined[label] && !ok {
			return false
		}
	}
	return true
}

// resolve substitutes every {label} in text. A label the script never
// defines is given the hash of "absent <label>", an artifact no script
// loads.
func (s rsScript) resolve(text string) string {
	for {
		start := strings.IndexByte(text, '{')
		if start < 0 {
			return text
		}
		end := strings.IndexByte(text[start:], '}')
		if end < 0 {
			panic("conformance: unterminated label in " + text)
		}
		label := text[start+1 : start+end]
		uuid, ok := s.hashes[label]
		if !ok {
			uuid = hash.SHA3([]byte("absent " + label))
			s.hashes[label] = uuid
			s.labels[uuid] = label
		}
		text = strings.ReplaceAll(text, "{"+label+"}", uuid)
	}
}

// rsLoad is how artifacts reach the derived tables.
type rsLoad int

const (
	// loadSweep stores every artifact, then crosslinks them in one call.
	loadSweep rsLoad = iota
	// loadEach crosslinks after storing each artifact, in script order.
	loadEach
	// loadRebuild stores every artifact without crosslinking, then
	// derives everything with verify.Rebuild.
	loadRebuild
	// loadPull pulls the artifacts from a server holding them all, into a
	// repository already holding and crosslinking the first few (goLoadPull).
	loadPull
)

func (l rsLoad) String() string {
	return [...]string{"sweep", "per-artifact", "verify.Rebuild", "pull"}[l]
}

// rsDoctor edits a repository's tables by SQL after the first at artifacts
// are loaded, to reach a state no known artifact sequence reaches.
type rsDoctor struct {
	at  int
	sql string
}

// goLoad loads a script into a new go-libfossil repository and returns its
// path. The repository is closed again, so a check can open it its own way.
func goLoad(t *testing.T, script rsScript, load rsLoad, doctor *rsDoctor) (string, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "go.fossil")
	r, err := repo.Create(path, "u", simio.CryptoRand{}, "")
	if err != nil {
		return "", err
	}
	loadErr := goStoreAll(r, script, load, doctor)
	if err := r.Close(); err != nil && loadErr == nil {
		loadErr = err
	}
	return path, loadErr
}

// rsProjectCode names both ends of a pull: a sync session is refused
// between repositories with different project codes.
const rsProjectCode = "5cd1ee2f2fa7a1b9e1a2b6f3c4d5e6f708192a3b"

// goLoadPull stores and crosslinks script.arts[:held] in a new repository,
// then pulls the rest with a sync session from a go-libfossil server, in
// process, that holds every artifact. It returns the puller's path, closed.
func goLoadPull(t *testing.T, script rsScript, held int) (string, error) {
	t.Helper()
	if held <= 0 || held >= len(script.arts) {
		panic("conformance: goLoadPull needs 0 < held < len(arts)")
	}
	dir := t.TempDir()
	src, err := goStored(filepath.Join(dir, "server.fossil"), script.arts)
	if err != nil {
		return "", err
	}
	defer src.Close()
	path := filepath.Join(dir, "go.fossil")
	dst, err := goStored(path, script.arts[:held])
	if err != nil {
		return "", err
	}
	transport := &sync.MockTransport{Handler: func(req *xfer.Message) *xfer.Message {
		resp, err := sync.HandleSync(context.Background(), src, req)
		if err != nil {
			return &xfer.Message{Cards: []xfer.Card{&xfer.ErrorCard{Message: err.Error()}}}
		}
		return resp
	}}
	ctx, cancel := context.WithTimeout(context.Background(), clientTimeout)
	defer cancel()
	_, pullErr := sync.Sync(ctx, dst, transport,
		sync.SyncOpts{Pull: true, ProjectCode: rsProjectCode})
	if err := dst.Close(); err != nil && pullErr == nil {
		pullErr = err
	}
	if pullErr != nil {
		return "", fmt.Errorf("pull: %w", pullErr)
	}
	return path, nil
}

// goStored creates a repository at path holding arts, crosslinked in one
// sweep, with the project code both ends of a pull share.
func goStored(path string, arts []rsBuilt) (*repo.Repo, error) {
	r, err := repo.Create(path, "u", simio.CryptoRand{}, rsProjectCode)
	if err != nil {
		return nil, err
	}
	for _, a := range arts {
		if _, _, err := blob.Store(r.DB(), a.data); err != nil {
			r.Close()
			return nil, fmt.Errorf("blob.Store %s: %w", a.label, err)
		}
	}
	if _, err := manifest.Crosslink(r); err != nil {
		r.Close()
		return nil, fmt.Errorf("Crosslink: %w", err)
	}
	return r, nil
}

func goStoreAll(r *repo.Repo, script rsScript, load rsLoad, doctor *rsDoctor) error {
	for i, a := range script.arts {
		if doctor != nil && doctor.at == i {
			if _, err := r.DB().Exec(script.resolve(doctor.sql)); err != nil {
				return fmt.Errorf("doctoring: %w", err)
			}
		}
		_, uuid, err := blob.Store(r.DB(), a.data)
		if err != nil {
			return fmt.Errorf("blob.Store %s: %w", a.label, err)
		}
		if uuid != a.uuid {
			return fmt.Errorf("blob.Store named %s %s, want %s", a.label, uuid, a.uuid)
		}
		if load == loadEach {
			if _, err := manifest.Crosslink(r); err != nil {
				return fmt.Errorf("Crosslink after %s: %w", a.label, err)
			}
		}
	}
	switch load {
	case loadSweep:
		if _, err := manifest.Crosslink(r); err != nil {
			return fmt.Errorf("Crosslink: %w", err)
		}
	case loadRebuild:
		if _, err := verify.Rebuild(r); err != nil {
			return fmt.Errorf("verify.Rebuild: %w", err)
		}
	}
	return nil
}

// rsState is a repository's derived rows, normalized, per table.
type rsState map[string][]string

// rsBase is 2026-01-01T00:00:00Z in milliseconds since the epoch. Scripts
// date every artifact within the minute after it, so times print as "@S",
// the second.
const rsBase = 1767225600000

// rsMillis turns a julian-day column into integer milliseconds since the
// epoch, so equal times compare equal whatever float each side stored.
func rsMillis(column string) string {
	return "CAST(round((" + column + " - 2440587.5) * 86400000) AS INTEGER)"
}

// rsUUID replaces a rid column by its blob hash. Qualify the column: a bare
// name would bind to the subquery's own blob table.
func rsUUID(column string) string {
	return "coalesce((SELECT b.uuid FROM blob b WHERE b.rid = " + column + "), '#' || " +
		column + ")"
}

// rsQueries reads each comparable table. Every rid is a hash, every tagid a
// name and every time milliseconds. The cluster tag's mtime is left out:
// fossil(1) writes the wall clock there.
var rsQueries = map[string]string{
	"plink": "SELECT " + rsUUID("plink.pid") + " || '>' || " + rsUUID("plink.cid") +
		", isprim, " + rsMillis("plink.mtime") + ", coalesce(" + rsUUID("plink.baseid") +
		", '-') FROM plink",
	"tagxref": "SELECT tag.tagname, " + rsUUID("tagxref.rid") + ", tagtype, " +
		"CASE WHEN srcid = 0 THEN '0' ELSE " + rsUUID("tagxref.srcid") + " END, " +
		rsUUID("tagxref.origid") + ", quote(value), " +
		"CASE WHEN tag.tagname = 'cluster' THEN '*' ELSE " + rsMillis("tagxref.mtime") +
		" END FROM tagxref JOIN tag USING (tagid)",
	"leaf": "SELECT " + rsUUID("leaf.rid") + " FROM leaf",
	// orphan reads the delta manifests refused for a baseline, each under
	// that baseline. It is outside rsAllTables: only the baseline cases
	// compare it.
	"orphan": "SELECT " + rsUUID("orphan.rid") + " || '>' || " + rsUUID("orphan.baseline") +
		" FROM orphan",
	"phantom": "SELECT uuid FROM blob WHERE size < 0",
	"event": "SELECT " + rsUUID("event.objid") + ", type, " + rsMillis("event.mtime") +
		" FROM event",
	// eventcols reads the columns the bgcolor, user and comment tags set on a
	// check-in's event row. It is outside rsAllTables: only the cases about
	// those tags compare it.
	"eventcols": "SELECT " + rsUUID("event.objid") + ", quote(bgcolor), quote(euser), " +
		"quote(ecomment) FROM event WHERE type = 'ci'",
	// omtime reads the D-card time a check-in keeps beside its event time.
	// It is outside rsAllTables: only the date-tag cases compare it.
	"omtime": "SELECT " + rsUUID("event.objid") + ", " + rsMillis("event.omtime") +
		" FROM event WHERE type = 'ci'",
	"mlink": "SELECT " + rsUUID("mlink.mid") + ", " + rsUUID("mlink.fid") + ", " +
		rsUUID("mlink.pid") + ", " + rsUUID("mlink.pmid") +
		", (SELECT name FROM filename WHERE fnid = mlink.fnid), isaux FROM mlink",
}

// rsAllTables is every table rsQueries reads.
var rsAllTables = []string{"event", "leaf", "mlink", "phantom", "plink", "tagxref"}

// readState reads tables from the repository at path. When tags is not
// empty, tagxref rows are kept only for those tag names.
func readState(path string, labels map[string]string, tables, tags []string) (rsState, error) {
	conn, err := db.OpenSQL(path, db.OpenConfig{}, nil)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	state := rsState{}
	for _, table := range tables {
		query, ok := rsQueries[table]
		if !ok {
			panic("conformance: no query for table " + table)
		}
		rows, err := queryNormalized(conn, query, labels)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", table, err)
		}
		if table == "tagxref" && len(tags) > 0 {
			rows = keepTags(rows, tags)
		}
		state[table] = rows
	}
	return state, nil
}

func keepTags(rows, tags []string) []string {
	var kept []string
	for _, row := range rows {
		name, _, _ := strings.Cut(row, " ")
		for _, tag := range tags {
			if name == tag {
				kept = append(kept, row)
				break
			}
		}
	}
	return kept
}

// queryNormalized runs query and writes each row as its columns joined by
// spaces: a hash as its label, a time as "@S".
func queryNormalized(conn db.Querier, query string, labels map[string]string) ([]string, error) {
	table, err := queryColumns(conn, query, labels)
	if err != nil {
		return nil, err
	}
	result := make([]string, len(table))
	for i, columns := range table {
		result[i] = strings.Join(columns, " ")
	}
	sort.Strings(result)
	return result, nil
}

// queryColumns runs query and normalizes each column of each row.
func queryColumns(conn db.Querier, query string, labels map[string]string) ([][]string, error) {
	rows, err := conn.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var result [][]string
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			return nil, err
		}
		parts := make([]string, len(values))
		for i, value := range values {
			parts[i] = normalizeValue(value, labels)
		}
		result = append(result, parts)
	}
	return result, rows.Err()
}

func normalizeValue(value any, labels map[string]string) string {
	// A driver may read a column declared BOOLEAN, such as plink.isprim, as
	// a bool; the other drivers read the integer stored.
	if b, ok := value.(bool); ok {
		if b {
			return "1"
		}
		return "0"
	}
	if millis, ok := value.(int64); ok && millis >= rsBase && millis < rsBase+3600000 {
		if offset := millis - rsBase; offset%1000 == 0 {
			return "@" + strconv.FormatInt(offset/1000, 10)
		}
	}
	text := fmt.Sprint(value)
	if b, ok := value.([]byte); ok {
		text = string(b)
	}
	// plink rows join two hashes with ">".
	if left, right, ok := strings.Cut(text, ">"); ok && !strings.HasPrefix(text, "'") {
		return labelOf(left, labels) + ">" + labelOf(right, labels)
	}
	return labelOf(text, labels)
}

// labelOf names a hash, or a hash inside a quoted value, by its label.
func labelOf(text string, labels map[string]string) string {
	if label, ok := labels[text]; ok {
		return label
	}
	if quoted := strings.Trim(text, "'"); len(quoted)+2 == len(text) {
		if label, ok := labels[quoted]; ok {
			return "'{" + label + "}'"
		}
	}
	return text
}

// diffState reports, per table, the rows only one side has; empty when the
// two agree.
func diffState(want, got rsState, wantName, gotName string) string {
	tables := make([]string, 0, len(want))
	for table := range want {
		tables = append(tables, table)
	}
	sort.Strings(tables)
	var parts []string
	for _, table := range tables {
		missing, extra := rowsOnlyIn(want[table], got[table]), rowsOnlyIn(got[table], want[table])
		for _, row := range missing {
			parts = append(parts, fmt.Sprintf("%s: %s lacks [%s]", table, gotName, row))
		}
		for _, row := range extra {
			parts = append(parts, fmt.Sprintf("%s: %s adds [%s]", table, gotName, row))
		}
	}
	if len(parts) > 0 {
		parts = append([]string{"vs " + wantName}, parts...)
	}
	return strings.Join(parts, "; ")
}

// rowsOnlyIn is the rows of a, as a multiset, that b lacks.
func rowsOnlyIn(a, b []string) []string {
	count := map[string]int{}
	for _, row := range b {
		count[row]++
	}
	var only []string
	for _, row := range a {
		if count[row] > 0 {
			count[row]--
			continue
		}
		only = append(only, row)
	}
	return only
}

// copyFile copies a repository so a destructive step runs on the copy.
func copyFile(from, to string) error {
	source, err := os.Open(from)
	if err != nil {
		return err
	}
	defer source.Close()
	target, err := os.Create(to)
	if err != nil {
		return err
	}
	if _, err := io.Copy(target, source); err != nil {
		target.Close()
		return err
	}
	return target.Close()
}

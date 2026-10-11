package conformance

import (
	"compress/zlib"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	libfossil "github.com/danmestas/go-libfossil"
	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/delta"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/simio"
)

// The sync cases all start from one repository, built here by go-libfossil
// so the suite needs no fossil(1) to run. Server cases serve a copy of it,
// to go-libfossil and to fossil(1) alike; client cases sync a copy of it
// against a scripted peer. Every name a case uses is a placeholder the
// fixture fills in, so a case reads as the exchange it is.

const (
	syncProjectCode = "c1b2a3d4e5f60718293a4b5c6d7e8f9012345678"
	syncEpoch       = 1767225600 // 2026-01-01T00:00:00Z, the UV file's mtime
)

// syncFixture is the repository F the cases start from, and the values its
// placeholders stand for.
type syncFixture struct {
	path string
	vars map[string]string
}

// syncArtifacts are F's artifacts beyond its two check-ins: name, content,
// and how each is stored.
var syncArtifacts = []struct {
	name, content string
	sha1, private bool
	deltaOf       string // stored as a delta against this artifact
	fast          bool   // compressed at zlib.BestSpeed
}{
	// B1 is a SHA1-named artifact.
	{name: "B1", content: "an artifact named by SHA1\n", sha1: true},
	// P is a private artifact, and Q a public one stored as a delta
	// against it.
	{name: "P", content: "a private artifact, alpha beta gamma delta\n", private: true},
	{name: "Q", content: "a private artifact, alpha beta gamma delta, made public\n",
		deltaOf: "P"},
	// V is stored compressed at zlib's fastest level, as another
	// implementation may have stored it: recompressing it at the default
	// level gives other bytes, so a cfile of it shows whether the stored
	// bytes went out verbatim.
	{name: "V", content: strings.Repeat("stored at zlib level 1; ", 64) + "\n", fast: true},
}

// syncUsers are F's users beyond nobody, all with the password "secret".
// anonymous is left with no capabilities: fossil(1) gives every logged-in
// user anonymous's capabilities, which go-libfossil does not, and no case
// is about that.
var syncUsers = []struct{ login, caps string }{
	{"alice", "gio"},
	{"admin", "a"},
	{"priv", "giox"},
	{"uvw", "gioy"},
	{"ro", "o"},
}

// newSyncFixture builds F in dir.
func newSyncFixture(t *testing.T, dir string) *syncFixture {
	t.Helper()
	path := filepath.Join(dir, "fixture.fossil")
	r, err := libfossil.Create(path, libfossil.CreateOpts{
		User: "setup", ProjectCode: syncProjectCode, Rand: simio.NewSeededRand(262),
	})
	if err != nil {
		t.Fatal(err)
	}
	f := &syncFixture{path: path, vars: map[string]string{}}
	err = f.populate(r)
	if closeErr := r.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	f.constants()
	return f
}

// populate adds F's check-ins, artifacts, users and unversioned file.
func (f *syncFixture) populate(r *libfossil.Repo) error {
	when := time.Unix(syncEpoch, 0).UTC()
	files := []string{"alpha\n", "alpha\nbeta\n"}
	var parent int64
	for i, text := range files {
		rid, uuid, err := r.Commit(libfossil.CommitOpts{
			Files:    []libfossil.FileToCommit{{Name: "a.txt", Content: []byte(text)}},
			Comment:  fmt.Sprintf("check-in %d", i+1),
			User:     "setup",
			Time:     when.Add(time.Duration(i) * time.Minute),
			ParentID: parent,
		})
		if err != nil {
			return err
		}
		parent = rid
		f.vars[fmt.Sprintf("M%d", i+1)] = uuid
		f.setArtifact(fmt.Sprintf("A%d", i+1), hash.SHA3([]byte(text)), text)
	}
	if err := f.storeArtifacts(r.DB()); err != nil {
		return err
	}
	if err := f.addUsers(r); err != nil {
		return err
	}
	if err := r.UVWrite("u.txt", []byte("hello world"), time.Unix(syncEpoch, 0)); err != nil {
		return err
	}
	_, mtime, uvHash, err := r.UVRead("u.txt")
	if err != nil {
		return err
	}
	f.vars["UVH"], f.vars["UVT"] = uvHash, fmt.Sprint(mtime)
	f.vars["SC"], err = r.Config("server-code")
	return err
}

// storeArtifacts stores syncArtifacts. A SHA1 artifact in a SHA3
// repository cannot be made through blob.Store, so each row is written
// directly, as a peer that predates SHA3 would have left it.
func (f *syncFixture) storeArtifacts(d *db.DB) error {
	rids := map[string]int64{}
	for _, a := range syncArtifacts {
		name := hash.SHA3([]byte(a.content))
		if a.sha1 {
			name = hash.SHA1([]byte(a.content))
		}
		stored := []byte(a.content)
		if a.deltaOf != "" {
			stored = delta.Create([]byte(f.vars[a.deltaOf+".data"]), stored)
		}
		packed, err := blob.Compress(stored)
		if a.fast {
			packed, err = compress(stored, zlib.BestSpeed), nil
		}
		if err != nil {
			return err
		}
		res, err := d.Exec("INSERT INTO blob(uuid, size, content, rcvid) VALUES(?, ?, ?, 1)",
			name, len(a.content), packed)
		if err != nil {
			return err
		}
		if rids[a.name], err = res.LastInsertId(); err != nil {
			return err
		}
		if err := f.classify(d, rids[a.name], rids[a.deltaOf], a.private); err != nil {
			return err
		}
		f.setArtifact(a.name, name, a.content)
	}
	return nil
}

// classify records an artifact stored by storeArtifacts as private or
// unclustered, and as a delta when it is one.
func (f *syncFixture) classify(d *db.DB, rid, src int64, private bool) error {
	if src != 0 {
		if _, err := d.Exec("INSERT INTO delta(rid, srcid) VALUES(?, ?)", rid, src); err != nil {
			return err
		}
	}
	if private {
		return content.MakePrivate(d, rid)
	}
	_, err := d.Exec("INSERT INTO unclustered(rid) VALUES(?)", rid)
	return err
}

func (f *syncFixture) addUsers(r *libfossil.Repo) error {
	for _, u := range syncUsers {
		if err := r.CreateUser(libfossil.UserOpts{
			Login: u.login, Password: "secret", Caps: u.caps,
		}); err != nil {
			return err
		}
	}
	if err := r.SetCaps("anonymous", ""); err != nil {
		return err
	}
	return r.SetCaps("nobody", "gio")
}

// setArtifact records an artifact's name, content and size placeholders,
// and its compressed form for cfile cards.
func (f *syncFixture) setArtifact(key, name, text string) {
	f.vars[key] = name
	f.setPayload(key, text)
}

// setPayload records a payload's placeholders: KEY.data and KEY.size; the
// compressed form a cfile card carries, KEY.z and KEY.zsize (fossil's blob
// format, a 4-byte length then zlib), and KEY.pad, the LF a cfile sender
// puts after it unless it ends in one; and KEY.size1, one more than the
// size.
func (f *syncFixture) setPayload(key, text string) {
	f.vars[key+".data"] = text
	f.vars[key+".size"] = fmt.Sprint(len(text))
	f.vars[key+".size1"] = fmt.Sprint(len(text) + 1)
	packed, err := blob.Compress([]byte(text))
	if err != nil {
		panic("conformance: compress " + key + ": " + err.Error())
	}
	f.vars[key+".z"] = string(packed)
	f.vars[key+".zsize"] = fmt.Sprint(len(packed))
	f.vars[key+".pad"] = "\n"
	if packed[len(packed)-1] == '\n' {
		f.vars[key+".pad"] = ""
	}
}

// constants fills the placeholders that do not depend on F's contents.
func (f *syncFixture) constants() {
	set := map[string]string{
		"PC":  syncProjectCode,
		"CV":  "pragma client-version 22800 20260311 113146",
		"CV0": "pragma client-version 19999 20200101 000000",
		"SV":  "pragma server-version 22800 20260311 113146",
		"SV0": "pragma server-version 19999 20200101 000000",
		"TS":  "# timestamp 2026-01-01T00:00:00 errors 0",
		"R":   "# 0123456789ABCDEF0123456789ABCDEF01234567",
		// Names no repository holds.
		"U":  sha1Hex("held by nobody 1"),
		"U2": sha1Hex("held by nobody 2"),
		"Z1": strings.Repeat("1", 40),
	}
	for k, v := range set {
		f.vars[k] = v
	}
	f.absentArtifacts()
	f.vars["UVT1"] = fmt.Sprint(syncEpoch + 60)
	f.vars["LONG"] = strings.Repeat("x", 200000)
	f.vars["UVCAT"] = sha1Hex("u.txt 2026-01-01 00:00:00 " + f.vars["UVH"] + "\n")
}

// absentArtifacts are artifacts F does not hold, for replies and pushes to
// deliver: N1 and N3 named by SHA1 and SHA3, E the empty artifact, BIN one
// with an LF and a NUL in it, and S and T a delta's basis and target. Each
// delta is "D" plus its target and basis: TS is T against S.
func (f *syncFixture) absentArtifacts() {
	f.setArtifact("N1", sha1Hex("a new artifact\n"), "a new artifact\n")
	f.setArtifact("N3", hash.SHA3([]byte("a new SHA3 artifact\n")), "a new SHA3 artifact\n")
	// N1J.z is N1's compressed form with bytes after the zlib stream.
	f.vars["N1J.z"] = f.vars["N1.z"] + "JUNK"
	f.vars["N1J.zsize"] = fmt.Sprint(len(f.vars["N1J.z"]))
	f.setArtifact("E", sha1Hex(""), "")
	// CFGX.data is a three-byte config payload followed by an X where its
	// trailing LF belongs.
	f.vars["CFGX.data"] = "a\nbX"
	cardLike := "igot " + f.vars["U"] + "\n"
	f.setArtifact("CL", sha1Hex(cardLike), cardLike)
	f.setArtifact("BIN", sha1Hex("a\nfile x\n\x00"), "a\nfile x\n\x00")
	basis := "the basis of a delta: alpha beta gamma delta epsilon zeta eta theta\n"
	target := "the target of a delta: alpha beta gamma delta epsilon zeta eta theta iota\n"
	f.setArtifact("S", sha1Hex(basis), basis)
	f.setArtifact("T", hash.SHA3([]byte(target)), target)
	f.setDelta("DTS", basis, target)
	f.setDelta("DTA1", f.vars["A1.data"], target)
	// DA2U makes A2, which F holds, from a basis nobody holds.
	f.setDelta("DA2U", "a basis nobody holds\n", f.vars["A2.data"])
}

// setDelta records the delta making target from basis as a payload.
func (f *syncFixture) setDelta(key, basis, target string) {
	f.setPayload(key, string(delta.Create([]byte(basis), []byte(target))))
}

// expand replaces every {NAME} in text with its value. An unknown name is
// a mistake in a case, not a value.
func (f *syncFixture) expand(text string) string {
	var out strings.Builder
	for {
		open := strings.IndexByte(text, '{')
		if open < 0 {
			out.WriteString(text)
			return out.String()
		}
		end := strings.IndexByte(text[open:], '}')
		if end < 0 {
			panic("conformance: unclosed placeholder in " + text)
		}
		key := text[open+1 : open+end]
		value, ok := f.vars[key]
		if !ok && !strings.HasPrefix(key, "LOGIN ") {
			panic("conformance: unknown placeholder {" + key + "}")
		}
		if !ok {
			value = "{" + key + "}"
		}
		out.WriteString(text[:open])
		out.WriteString(value)
		text = text[open+end+1:]
	}
}

// copyTo copies F to a new file in dir.
func (f *syncFixture) copyTo(dir, name string) (string, error) {
	path := filepath.Join(dir, name)
	return path, copyFile(f.path, path)
}

// body joins card lines into a body, each ending in LF, then expands its
// placeholders and signs its login lines. An element that is a payload
// placeholder, "{NAME.data}", "{NAME.z}" or "{NAME.pad}", is the bytes
// after a card line and gets no LF of its own.
func (f *syncFixture) body(cardLines ...string) []byte {
	var out strings.Builder
	for _, line := range cardLines {
		out.WriteString(f.expand(line))
		if !isPayload(line) {
			out.WriteByte('\n')
		}
	}
	return []byte(f.sign(out.String()))
}

// isPayload reports whether a body element is a payload placeholder,
// KEY.data, KEY.z or KEY.pad.
func isPayload(line string) bool {
	return strings.HasPrefix(line, "{") && strings.Count(line, "{") == 1 &&
		(strings.HasSuffix(line, ".data}") || strings.HasSuffix(line, ".z}") ||
			strings.HasSuffix(line, ".pad}"))
}

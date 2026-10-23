//go:build !js

// Command engine writes the docs site's hero figure data: one check-in as
// an artifact, as the rows storing it derives, and on the wire as a clone
// pulls it. Everything it records comes from a real run of the engine, and
// every input is fixed, so two runs write the same bytes.
//
//	go run ./tools/docsgen/engine -out docs/site/data/engine.json
//
// It registers the modernc SQLite driver, which has no js/wasm build.
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/danmestas/go-libfossil/db"
	_ "github.com/danmestas/go-libfossil/db/driver/modernc"
	"github.com/danmestas/go-libfossil/internal/auth"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/sync"
	"github.com/danmestas/go-libfossil/internal/xfer"
	"github.com/danmestas/go-libfossil/simio"
)

const (
	originSeed  = 1
	cloneSeed   = 2
	projectCode = "0ab0c192687b3e1f5d84c6a7e2b9f0d3c5a71e48"
	user        = "ana"
	readmeV1    = "# notes\n"
	readmeV2    = "# notes\n\nShip on Friday.\n"
	mainGo      = "package main\n\nfunc main() {}\n"
)

var firstTime = time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)

// Figure is the JSON the landing page renders.
type Figure struct {
	Manifest Manifest `json:"manifest"`
	Rows     Rows     `json:"rows"`
	Pull     []Round  `json:"pull"`
}

type Manifest struct {
	Text   string `json:"text"`
	Name   string `json:"name"`
	Bytes  int    `json:"bytes"`
	Parent Parent `json:"parent"`
}

// Parent is the check-in the clone already holds when it pulls.
type Parent struct {
	RID  int64  `json:"rid"`
	UUID string `json:"uuid"`
}

type Rows struct {
	Blob    []BlobRow    `json:"blob"`
	Event   []EventRow   `json:"event"`
	Plink   []PlinkRow   `json:"plink"`
	Mlink   []MlinkRow   `json:"mlink"`
	Tagxref []TagxrefRow `json:"tagxref"`
	Leaf    []int64      `json:"leaf"`
}

type BlobRow struct {
	RID  int64  `json:"rid"`
	UUID string `json:"uuid"`
	Size int64  `json:"size"`
}

type EventRow struct {
	Type    string `json:"type"`
	ObjID   int64  `json:"objid"`
	User    string `json:"user"`
	Comment string `json:"comment"`
}

type PlinkRow struct {
	PID     int64 `json:"pid"`
	CID     int64 `json:"cid"`
	Primary int64 `json:"isprim"`
}

type MlinkRow struct {
	MID  int64  `json:"mid"`
	FID  int64  `json:"fid"`
	PID  int64  `json:"pid"`
	File string `json:"file"`
}

type TagxrefRow struct {
	Tag   string `json:"tag"`
	Value string `json:"value"`
	RID   int64  `json:"rid"`
}

// Round is one request and its reply, one line per card.
type Round struct {
	Request []string `json:"request"`
	Reply   []string `json:"reply"`
}

func main() {
	out := flag.String("out", "docs/site/data/engine.json", "file to write")
	flag.Parse()
	fig, err := run()
	if err != nil {
		fmt.Fprintln(os.Stderr, "engine:", err)
		os.Exit(1)
	}
	data, err := json.MarshalIndent(fig, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "engine:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, append(data, '\n'), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "engine:", err)
		os.Exit(1)
	}
}

func run() (*Figure, error) {
	dir, err := os.MkdirTemp("", "docsgen-engine-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

	origin, err := repo.Create(filepath.Join(dir, "origin.fossil"), user, simio.NewSeededRand(originSeed), projectCode)
	if err != nil {
		return nil, err
	}
	defer origin.Close()
	if err := auth.UpdateCaps(origin.DB(), "nobody", "gio"); err != nil {
		return nil, err
	}

	first, firstName, err := manifest.Checkin(origin, manifest.CheckinOpts{
		Files: []manifest.File{
			{Name: "README.md", Content: []byte(readmeV1)},
			{Name: "src/main.go", Content: []byte(mainGo)},
		},
		Comment: "first notes",
		User:    user,
		Time:    firstTime,
	})
	if err != nil {
		return nil, fmt.Errorf("first check-in: %w", err)
	}

	ctx := context.Background()
	env := &simio.Env{
		Clock:   simio.NewSimClockAt(firstTime),
		Rand:    simio.NewSeededRand(cloneSeed),
		Storage: simio.OSStorage{},
	}
	clone, _, err := sync.Clone(ctx, filepath.Join(dir, "clone.fossil"), &recorder{origin: origin}, sync.CloneOpts{Env: env})
	if err != nil {
		return nil, fmt.Errorf("clone: %w", err)
	}
	defer clone.Close()

	second, name, err := manifest.Checkin(origin, manifest.CheckinOpts{
		Files: []manifest.File{
			{Name: "README.md", Content: []byte(readmeV2)},
			{Name: "src/main.go", Content: []byte(mainGo)},
		},
		Comment: "plan the release",
		User:    user,
		Parent:  first,
		Time:    firstTime.Add(90 * time.Minute),
	})
	if err != nil {
		return nil, fmt.Errorf("second check-in: %w", err)
	}

	serverCode, err := clone.Config("server-code")
	if err != nil {
		return nil, err
	}
	rec := &recorder{origin: origin, record: true}
	if _, err := sync.Sync(ctx, clone, rec, sync.SyncOpts{
		Pull:        true,
		ProjectCode: projectCode,
		ServerCode:  serverCode,
		Env:         env,
	}); err != nil {
		return nil, fmt.Errorf("pull: %w", err)
	}

	text, err := content.Expand(origin.DB(), second)
	if err != nil {
		return nil, err
	}
	rows, err := readRows(origin, second)
	if err != nil {
		return nil, err
	}
	return &Figure{
		Manifest: Manifest{
			Text:   string(text),
			Name:   name,
			Bytes:  len(text),
			Parent: Parent{RID: int64(first), UUID: firstName},
		},
		Rows: *rows,
		Pull: rec.rounds,
	}, nil
}

// recorder is an in-process transport: each request goes straight to the
// origin's sync handler. When record is set it keeps every card it carries.
type recorder struct {
	origin *repo.Repo
	record bool
	rounds []Round
}

func (t *recorder) Exchange(ctx context.Context, req *xfer.Message) (*xfer.Message, error) {
	reply, err := sync.HandleSync(ctx, t.origin, req)
	if err != nil {
		return nil, err
	}
	if t.record {
		reqLines, err := cardLines(req)
		if err != nil {
			return nil, err
		}
		replyLines, err := cardLines(reply)
		if err != nil {
			return nil, err
		}
		t.rounds = append(t.rounds, Round{Request: reqLines, Reply: replyLines})
	}
	return reply, nil
}

// cardLines encodes each card and keeps its first line, leaving out
// comments (the checksum and timestamp) and pragmas, as the figure does.
func cardLines(msg *xfer.Message) ([]string, error) {
	lines := []string{}
	for _, c := range msg.Cards {
		var buf bytes.Buffer
		if err := xfer.EncodeCard(&buf, c); err != nil {
			return nil, err
		}
		line, _, _ := strings.Cut(buf.String(), "\n")
		if strings.HasPrefix(line, "#") || strings.HasPrefix(line, "pragma ") {
			continue
		}
		lines = append(lines, line)
	}
	return lines, nil
}

func readRows(r *repo.Repo, rid fsltype.FslID) (*Rows, error) {
	d := r.DB()
	var rows Rows
	var err error
	if rows.Blob, err = query(d, func(q *sql.Rows, b *BlobRow) error {
		return q.Scan(&b.RID, &b.UUID, &b.Size)
	}, `SELECT rid, uuid, size FROM blob
		WHERE rid = ?1 OR rid IN (SELECT fid FROM mlink WHERE mid = ?1)
		ORDER BY rid DESC`, rid); err != nil {
		return nil, err
	}
	if rows.Event, err = query(d, func(q *sql.Rows, e *EventRow) error {
		return q.Scan(&e.Type, &e.ObjID, &e.User, &e.Comment)
	}, `SELECT type, objid, user, comment FROM event WHERE objid = ?`, rid); err != nil {
		return nil, err
	}
	if rows.Plink, err = query(d, func(q *sql.Rows, p *PlinkRow) error {
		return q.Scan(&p.PID, &p.CID, &p.Primary)
	}, `SELECT pid, cid, isprim FROM plink WHERE cid = ? ORDER BY pid`, rid); err != nil {
		return nil, err
	}
	if rows.Mlink, err = query(d, func(q *sql.Rows, m *MlinkRow) error {
		return q.Scan(&m.MID, &m.FID, &m.PID, &m.File)
	}, `SELECT mlink.mid, mlink.fid, mlink.pid, filename.name
		FROM mlink JOIN filename ON filename.fnid = mlink.fnid
		WHERE mlink.mid = ? ORDER BY filename.name`, rid); err != nil {
		return nil, err
	}
	if rows.Tagxref, err = query(d, func(q *sql.Rows, t *TagxrefRow) error {
		return q.Scan(&t.Tag, &t.Value, &t.RID)
	}, `SELECT tag.tagname, tagxref.value, tagxref.rid
		FROM tagxref JOIN tag ON tag.tagid = tagxref.tagid
		WHERE tagxref.rid = ? AND tag.tagname = 'branch'`, rid); err != nil {
		return nil, err
	}
	if rows.Leaf, err = query(d, func(q *sql.Rows, l *int64) error {
		return q.Scan(l)
	}, `SELECT rid FROM leaf ORDER BY rid`); err != nil {
		return nil, err
	}
	return &rows, nil
}

// query runs one SELECT and scans every row it returns into a T.
func query[T any](d *db.DB, scan func(*sql.Rows, *T) error, stmt string, args ...any) ([]T, error) {
	q, err := d.Query(stmt, args...)
	if err != nil {
		return nil, err
	}
	defer q.Close()
	var out []T
	for q.Next() {
		var v T
		if err := scan(q, &v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, q.Err()
}

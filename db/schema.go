package db

import (
	"encoding/hex"
	"fmt"

	"github.com/danmestas/libfossil/simio"
)

const schemaRepo1 = `
CREATE TABLE blob(
  rid INTEGER PRIMARY KEY,
  rcvid INTEGER,
  size INTEGER,
  uuid TEXT UNIQUE NOT NULL,
  content BLOB,
  CHECK( length(uuid)>=40 AND rid>0 )
);
CREATE TABLE delta(
  rid INTEGER PRIMARY KEY,
  srcid INTEGER NOT NULL REFERENCES blob
);
CREATE INDEX delta_i1 ON delta(srcid);
CREATE TABLE rcvfrom(
  rcvid INTEGER PRIMARY KEY,
  uid INTEGER REFERENCES user,
  mtime DATETIME,
  nonce TEXT UNIQUE,
  ipaddr TEXT
);
CREATE TABLE user(
  uid INTEGER PRIMARY KEY,
  login TEXT UNIQUE,
  pw TEXT,
  cap TEXT,
  cookie TEXT,
  ipaddr TEXT,
  cexpire DATETIME,
  info TEXT,
  mtime DATE,
  photo BLOB
);
CREATE TABLE config(
  name TEXT PRIMARY KEY NOT NULL,
  value CLOB,
  mtime DATE,
  CHECK( typeof(name)='text' AND length(name)>=1 )
) WITHOUT ROWID;
CREATE TABLE shun(
  uuid TEXT PRIMARY KEY,
  mtime DATE,
  scom TEXT
) WITHOUT ROWID;
CREATE TABLE private(rid INTEGER PRIMARY KEY);
CREATE TABLE reportfmt(
   rn INTEGER PRIMARY KEY,
   owner TEXT,
   title TEXT UNIQUE,
   mtime DATE,
   cols TEXT,
   sqlcode TEXT
);
CREATE TABLE concealed(
  hash TEXT PRIMARY KEY,
  mtime DATE,
  content TEXT
) WITHOUT ROWID;
PRAGMA application_id=252006673;
`

const schemaRepo2 = `
CREATE TABLE filename(
  fnid INTEGER PRIMARY KEY,
  name TEXT UNIQUE
);
CREATE TABLE mlink(
  mid INTEGER,
  fid INTEGER,
  pmid INTEGER,
  pid INTEGER,
  fnid INTEGER REFERENCES filename,
  pfnid INTEGER,
  mperm INTEGER,
  isaux BOOLEAN DEFAULT 0
);
CREATE INDEX mlink_i1 ON mlink(mid);
CREATE INDEX mlink_i2 ON mlink(fnid);
CREATE INDEX mlink_i3 ON mlink(fid);
CREATE INDEX mlink_i4 ON mlink(pid);
CREATE TABLE plink(
  pid INTEGER REFERENCES blob,
  cid INTEGER REFERENCES blob,
  isprim BOOLEAN,
  mtime DATETIME,
  baseid INTEGER REFERENCES blob,
  UNIQUE(pid, cid)
);
CREATE INDEX plink_i2 ON plink(cid,pid);
CREATE TABLE leaf(rid INTEGER PRIMARY KEY);
CREATE TABLE event(
  type TEXT,
  mtime DATETIME,
  objid INTEGER PRIMARY KEY,
  tagid INTEGER,
  uid INTEGER REFERENCES user,
  bgcolor TEXT,
  euser TEXT,
  user TEXT,
  ecomment TEXT,
  comment TEXT,
  brief TEXT,
  omtime DATETIME
);
CREATE INDEX event_i1 ON event(mtime);
CREATE TABLE phantom(
  rid INTEGER PRIMARY KEY
);
CREATE TABLE orphan(
  rid INTEGER PRIMARY KEY,
  baseline INTEGER
);
CREATE INDEX orphan_baseline ON orphan(baseline);
CREATE TABLE unclustered(
  rid INTEGER PRIMARY KEY
);
CREATE TABLE unsent(
  rid INTEGER PRIMARY KEY
);
CREATE TABLE tag(
  tagid INTEGER PRIMARY KEY,
  tagname TEXT UNIQUE
);
INSERT INTO tag VALUES(1, 'bgcolor');
INSERT INTO tag VALUES(2, 'comment');
INSERT INTO tag VALUES(3, 'user');
INSERT INTO tag VALUES(4, 'date');
INSERT INTO tag VALUES(5, 'hidden');
INSERT INTO tag VALUES(6, 'private');
INSERT INTO tag VALUES(7, 'cluster');
INSERT INTO tag VALUES(8, 'branch');
INSERT INTO tag VALUES(9, 'closed');
INSERT INTO tag VALUES(10,'parent');
INSERT INTO tag VALUES(11,'note');
CREATE TABLE tagxref(
  tagid INTEGER REFERENCES tag,
  tagtype INTEGER,
  srcid INTEGER REFERENCES blob,
  origid INTEGER REFERENCES blob,
  value TEXT,
  mtime TIMESTAMP,
  rid INTEGER REFERENCE blob,
  UNIQUE(rid, tagid)
);
CREATE INDEX tagxref_i1 ON tagxref(tagid, mtime);
CREATE TABLE backlink(
  target TEXT,
  srctype INT,
  srcid INT,
  mtime TIMESTAMP,
  UNIQUE(target, srctype, srcid)
);
CREATE INDEX backlink_src ON backlink(srcid, srctype);
CREATE TABLE attachment(
  attachid INTEGER PRIMARY KEY,
  isLatest BOOLEAN DEFAULT 0,
  mtime TIMESTAMP,
  src TEXT,
  target TEXT,
  filename TEXT,
  comment TEXT,
  user TEXT
);
CREATE INDEX attachment_idx1 ON attachment(target, filename, mtime);
CREATE INDEX attachment_idx2 ON attachment(src);
CREATE TABLE cherrypick(
  parentid INT,
  childid INT,
  isExclude BOOLEAN DEFAULT false,
  PRIMARY KEY(parentid, childid)
) WITHOUT ROWID;
CREATE INDEX cherrypick_cid ON cherrypick(childid);
CREATE TABLE forumpost(
  fpid INTEGER PRIMARY KEY,
  froot INT,
  fprev INT,
  firt INT,
  fmtime REAL
);
CREATE INDEX forumpost_froot ON forumpost(froot);
INSERT INTO rcvfrom(rcvid, uid, mtime, nonce, ipaddr) VALUES(1, 0, 0, NULL, NULL);
`

func CreateRepoSchema(d *DB) error {
	if d == nil {
		panic("db.CreateRepoSchema: d must not be nil")
	}
	_, err := d.Exec(schemaRepo1)
	if err != nil {
		return fmt.Errorf("schema repo1: %w", err)
	}
	_, err = d.Exec(schemaRepo2)
	if err != nil {
		return fmt.Errorf("schema repo2: %w", err)
	}
	return nil
}

func SeedUser(d *DB, login string) error {
	if d == nil {
		panic("db.SeedUser: d must not be nil")
	}
	if login == "" {
		panic("db.SeedUser: login must not be empty")
	}
	_, err := d.Exec(
		"INSERT OR IGNORE INTO user(uid, login, pw, cap, info) VALUES(1, ?, '', 's', '')",
		login,
	)
	return err
}

// SeedNobody inserts a "nobody" user with the given capabilities.
// This controls anonymous access policy for the repo.
func SeedNobody(d *DB, caps string) error {
	if d == nil {
		panic("db.SeedNobody: d must not be nil")
	}
	_, err := d.Exec(
		"INSERT OR IGNORE INTO user(login, pw, cap, info) VALUES('nobody', '', ?, '')",
		caps,
	)
	return err
}

// SeedConfig seeds the config table with project-code, server-code, and
// schema markers. If projectCode is empty, a fresh 40-char lowercase-hex
// project-code is generated from rng. If non-empty, it must match
// ^[0-9a-f]{40}$; otherwise an error is returned and no rows are written.
// Server-code is always generated.
func SeedConfig(d *DB, rng simio.Rand, projectCode string) error {
	if d == nil {
		panic("db.SeedConfig: d must not be nil")
	}
	if rng == nil {
		panic("db.SeedConfig: rng must not be nil")
	}
	var projCode string
	if projectCode != "" {
		if !isProjectCodeHex(projectCode) {
			return fmt.Errorf("db.SeedConfig: invalid project-code %q: must be 40-char lowercase hex", projectCode)
		}
		projCode = projectCode
	} else {
		var err error
		projCode, err = randomHex(20, rng)
		if err != nil {
			return fmt.Errorf("generating project-code: %w", err)
		}
	}
	serverCode, err := randomHex(20, rng)
	if err != nil {
		return fmt.Errorf("generating server-code: %w", err)
	}
	stmts := []struct {
		name, value string
	}{
		{"project-code", projCode},
		{"server-code", serverCode},
		{"aux-schema", "2015-01-24"},
		{"content-schema", "2"},
	}
	for _, s := range stmts {
		_, err := d.Exec(
			"INSERT INTO config(name, value, mtime) VALUES(?, ?, strftime('%s','now'))",
			s.name, s.value,
		)
		if err != nil {
			return fmt.Errorf("seed config %q: %w", s.name, err)
		}
	}
	_, err = d.Exec("INSERT OR REPLACE INTO rcvfrom(rcvid, uid, mtime, nonce, ipaddr) VALUES(1, 1, julianday('now'), NULL, NULL)")
	if err != nil {
		return fmt.Errorf("seed rcvfrom: %w", err)
	}
	return nil
}

func randomHex(nBytes int, rng simio.Rand) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rng.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// isProjectCodeHex reports whether s matches ^[0-9a-f]{40}$ — the format
// produced by randomHex(20, ...). Kept private; callers that need this
// invariant should validate at their boundary.
func isProjectCodeHex(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') {
			continue
		}
		return false
	}
	return true
}

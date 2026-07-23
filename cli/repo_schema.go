package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/danmestas/go-libfossil/internal/repo"
)

// RepoSchemaCmd groups synced table schema operations.
type RepoSchemaCmd struct {
	Add    RepoSchemaAddCmd    `cmd:"" help:"Register a new synced table"`
	List   RepoSchemaListCmd   `cmd:"" help:"List registered synced tables"`
	Show   RepoSchemaShowCmd   `cmd:"" help:"Show table schema details"`
	Remove RepoSchemaRemoveCmd `cmd:"" help:"Remove a synced table registration"`
}

// RepoSchemaAddCmd registers a new synced table.
type RepoSchemaAddCmd struct {
	Name     string `arg:"" help:"Table name (without x_ prefix)"`
	Columns  string `required:"" help:"Column defs: name:type[:pk],... (e.g. peer_id:text:pk,addr:text)"`
	Conflict string `default:"mtime-wins" enum:"self-write,mtime-wins,owner-write" help:"Conflict strategy"`
}

func (c *RepoSchemaAddCmd) Run(g *Globals) error {
	r, err := g.OpenRepo()
	if err != nil {
		return err
	}
	defer r.Close()

	inner := r.Inner()
	if err := repo.EnsureSyncSchema(inner.DB()); err != nil {
		return err
	}

	colDefs, err := parseColumnDefs(c.Columns)
	if err != nil {
		return err
	}

	def := repo.TableDef{
		Columns:  colDefs,
		Conflict: c.Conflict,
	}

	mtime := time.Now().Unix()
	if err := repo.RegisterSyncedTable(inner.DB(), c.Name, def, mtime); err != nil {
		return err
	}

	fmt.Printf("Registered table %q with %d columns (%s)\n", c.Name, len(colDefs), c.Conflict)
	return nil
}

// RepoSchemaListCmd lists registered synced tables.
type RepoSchemaListCmd struct{}

func (c *RepoSchemaListCmd) Run(g *Globals) error {
	r, err := g.OpenRepo()
	if err != nil {
		return err
	}
	defer r.Close()

	inner := r.Inner()
	if err := repo.EnsureSyncSchema(inner.DB()); err != nil {
		return err
	}

	tables, err := repo.ListSyncedTables(inner.DB())
	if err != nil {
		return err
	}

	if len(tables) == 0 {
		fmt.Println("(no synced tables registered)")
		return nil
	}

	fmt.Printf("%-20s %-15s %s\n", "TABLE", "CONFLICT", "COLUMNS")
	fmt.Printf("%-20s %-15s %s\n", strings.Repeat("-", 20), strings.Repeat("-", 15), strings.Repeat("-", 7))
	for _, t := range tables {
		fmt.Printf("%-20s %-15s %d\n", t.Name, t.Def.Conflict, len(t.Def.Columns))
	}
	return nil
}

// RepoSchemaShowCmd shows table schema details.
type RepoSchemaShowCmd struct {
	Name string `arg:"" help:"Table name (without x_ prefix)"`
}

func (c *RepoSchemaShowCmd) Run(g *Globals) error {
	r, err := g.OpenRepo()
	if err != nil {
		return err
	}
	defer r.Close()

	inner := r.Inner()
	if err := repo.EnsureSyncSchema(inner.DB()); err != nil {
		return err
	}

	tables, err := repo.ListSyncedTables(inner.DB())
	if err != nil {
		return err
	}

	var found *repo.TableInfo
	for _, t := range tables {
		if t.Name == c.Name {
			t := t
			found = &t
			break
		}
	}

	if found == nil {
		return fmt.Errorf("table %q not found", c.Name)
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(found)
}

// RepoSchemaRemoveCmd removes a synced table registration.
type RepoSchemaRemoveCmd struct {
	Name string `arg:"" help:"Table name (without x_ prefix)"`
}

func (c *RepoSchemaRemoveCmd) Run(g *Globals) error {
	r, err := g.OpenRepo()
	if err != nil {
		return err
	}
	defer r.Close()

	inner := r.Inner()
	if err := repo.EnsureSyncSchema(inner.DB()); err != nil {
		return err
	}

	if err := repo.ValidateTableName(c.Name); err != nil {
		return err
	}

	tables, err := repo.ListSyncedTables(inner.DB())
	if err != nil {
		return err
	}
	found := false
	for _, t := range tables {
		if t.Name == c.Name {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("table %q not registered", c.Name)
	}

	if _, err := inner.DB().Exec("DELETE FROM _sync_schema WHERE name=?", c.Name); err != nil {
		return fmt.Errorf("delete from _sync_schema: %w", err)
	}

	dropSQL := fmt.Sprintf("DROP TABLE IF EXISTS x_%s", c.Name)
	if _, err := inner.DB().Exec(dropSQL); err != nil {
		return fmt.Errorf("drop table: %w", err)
	}

	fmt.Printf("Removed table %q and dropped x_%s\n", c.Name, c.Name)
	return nil
}

func parseColumnDefs(s string) ([]repo.ColumnDef, error) {
	if s == "" {
		return nil, fmt.Errorf("column definitions must not be empty")
	}

	parts := strings.Split(s, ",")
	var cols []repo.ColumnDef

	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		fields := strings.Split(part, ":")
		if len(fields) < 2 {
			return nil, fmt.Errorf("column definition %q must have at least name:type", part)
		}

		col := repo.ColumnDef{
			Name: strings.TrimSpace(fields[0]),
			Type: strings.TrimSpace(fields[1]),
			PK:   false,
		}

		if len(fields) >= 3 && strings.TrimSpace(fields[2]) == "pk" {
			col.PK = true
		}

		cols = append(cols, col)
	}

	if len(cols) == 0 {
		return nil, fmt.Errorf("no valid columns found in definition")
	}

	return cols, nil
}

package conformance

import (
	"fmt"
	"slices"

	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/verify"
)

// verify.Verify against the leaf table. Section 7.3 lets the table depend on
// the order artifacts arrived in, so Verify must accept every table a load
// derives -- goConforms runs it on every case that compares leaf, including
// repostate.7.3.recheck-untagged-child -- and must still report what the
// Section 7.2 test decides whatever the order.

// verifyLeafIssues runs verify.Verify on the repository at path and returns
// the check-ins its leaf issues name, by label.
func verifyLeafIssues(path string, labels map[string]string) ([]string, error) {
	r, err := repo.Open(path)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	report, err := verify.Verify(r)
	if err != nil {
		return nil, fmt.Errorf("verify.Verify: %w", err)
	}
	var named []string
	for _, iss := range report.Issues {
		if iss.Kind != verify.IssueLeafIncorrect {
			continue
		}
		var uuid string
		if err := r.DB().QueryRow("SELECT uuid FROM blob WHERE rid=?", iss.RID).Scan(&uuid); err != nil {
			return nil, fmt.Errorf("leaf issue rid=%d: %w", iss.RID, err)
		}
		label, ok := labels[uuid]
		if !ok {
			label = uuid
		}
		named = append(named, label)
	}
	slices.Sort(named)
	return named, nil
}

func verifyLeafCases() []rsAPICase {
	return []rsAPICase{{
		// b1, merged into trunk by m, and m are the leaves of merged(); b1's
		// only child is on another branch. Verify accepts that table, then
		// reports b1 once its row is dropped and c1 once one is planted: c1's
		// child c2 is on trunk with it, by a branch row propagated along the
		// link, so the recheck that row scheduled removed c1.
		Case: Case{ID: "repostate.7.2.verify-leaf-table", Req: "RS-7.2-1"},
		arts: merged(),
		run:  verifyLeafTable,
	}}
}

func verifyLeafTable(e *rsEnv) error {
	steps := []struct {
		sql  string
		want []string
	}{
		{"", nil},
		{"DELETE FROM leaf WHERE rid=(SELECT rid FROM blob WHERE uuid='%[1]s')", []string{"b1"}},
		{"INSERT INTO leaf(rid) SELECT rid FROM blob WHERE uuid='%[2]s'", []string{"b1", "c1"}},
	}
	for _, s := range steps {
		if s.sql != "" {
			if err := e.goExec(fmt.Sprintf(s.sql, e.script.hashes["b1"], e.script.hashes["c1"])); err != nil {
				return err
			}
		}
		got, err := verifyLeafIssues(e.goPath, e.script.labels)
		if err != nil {
			return err
		}
		if !slices.Equal(got, s.want) {
			return expectf("after %q: Verify leaf issues %v, want %v", s.sql, got, s.want)
		}
	}
	return nil
}

// goExec runs one statement against the go-libfossil repository.
func (e *rsEnv) goExec(query string) error {
	r, err := repo.Open(e.goPath)
	if err != nil {
		return err
	}
	defer r.Close()
	if _, err := r.DB().Exec(query); err != nil {
		return fmt.Errorf("%s: %w", query, err)
	}
	return nil
}

package merge

import (
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// DetectForks finds divergent branches by querying the leaf table
// (maintained by Fossil/manifest.Checkin) for checkins with no children.
func DetectForks(r *repo.Repo) ([]Fork, error) {
	if r == nil {
		panic("merge.DetectForks: r must not be nil")
	}
	rows, err := r.DB().Query(`
		SELECT l.rid FROM leaf l
		JOIN event e ON e.objid=l.rid
		WHERE e.type='ci'
		ORDER BY e.mtime DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var leaves []libfossil.FslID
	for rows.Next() {
		var rid int64
		if err := rows.Scan(&rid); err != nil {
			continue
		}
		leaves = append(leaves, libfossil.FslID(rid))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if len(leaves) <= 1 {
		return nil, nil
	}

	var forks []Fork
	for i := 0; i < len(leaves); i++ {
		for j := i + 1; j < len(leaves); j++ {
			ancestor, err := FindCommonAncestor(r, leaves[i], leaves[j])
			if err != nil {
				continue
			}
			forks = append(forks, Fork{
				Ancestor:  ancestor,
				LocalTip:  leaves[i],
				RemoteTip: leaves[j],
			})
		}
	}
	return forks, nil
}

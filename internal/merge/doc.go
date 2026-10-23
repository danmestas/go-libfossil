// Package merge provides three-way merge with pluggable strategies.
//
// Register strategies at init time via [Register], then look them up by
// name with [StrategyByName]. The built-in strategies are:
//
//   - ThreeWayText: fossil's line-level merge (merge3.c), byte for byte,
//     with its conflict markers
//   - Binary: treats any difference as a conflict
//   - LastWriterWins: always picks the remote side
//   - ConflictFork: creates a fork instead of merging
//
// [FindPivot] is fossil's pivot_find: it walks the plink DAG, newest
// check-in first, to the merge base fossil's merge chooses, and
// [FindCommonAncestor] asks it for two check-ins. [DetectForks]
// identifies open forks in a repository.
package merge

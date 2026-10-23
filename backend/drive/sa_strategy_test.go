// Regress cross-folder grouping, fair turns and cursor lifecycle independently of I/O.
package drive

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func strategyPool(t *testing.T, strategy string, names ...string) (*SaInfo, []string) {
	t.Helper()
	root := t.TempDir()
	paths := make([]string, len(names))
	for i, name := range names {
		paths[i] = filepath.Join(root, filepath.FromSlash(name))
	}
	pool, err := newSAInfo(strategy, root)
	require.NoError(t, err)
	pool.updateSas(paths, paths[0])
	return pool, paths
}

func TestSAStrategyUnequalGroups(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files []string
		want  []int
	}{
		{"three-one", []string{"p1/a.json", "p1/b.json", "p1/c.json", "p2/a.json"}, []int{3, 1, 3, 2, 3, 0, 3, 1}},
		{"three-groups", []string{"p1/a.json", "p1/b.json", "p2/a.json", "p3/a.json", "p3/b.json", "p3/c.json"}, []int{2, 3, 1, 2, 4, 0, 2, 5, 1}},
		{"single-group", []string{"p1/a.json", "p1/b.json", "p1/c.json"}, []int{1, 2, 0, 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, paths := strategyPool(t, "cross-folder", tc.files...)
			for _, index := range tc.want {
				candidate := pool.rollup()
				require.Equal(t, paths[index], candidate)
				require.Equal(t, candidate, pool.rollup(), "selection does not consume a turn")
				pool.activeSa(candidate)
			}
		})
	}
}

func TestSAStrategyGrouping(t *testing.T) {
	pool, paths := strategyPool(t, "cross-folder", "root.json", "p1/a.json", "p1/nested/b.json", "p2/a.json")
	for _, index := range []int{1, 3, 0, 2, 3, 0, 1} {
		require.Equal(t, paths[index], pool.rollup())
		pool.activeSa(paths[index])
	}
	outside := filepath.Join(t.TempDir(), "outside.json")
	pool.updateSas(append(paths, outside), outside)
	names := []string{}
	for _, group := range pool.folders.groups {
		names = append(names, group.name)
	}
	require.Contains(t, names, filepath.Dir(outside))
	require.Len(t, names, 4)
}

func TestSAStrategyMissingGroupUsesSuccessor(t *testing.T) {
	pool, paths := strategyPool(t, "cross-folder", "p1/a.json", "p3/a.json")
	root := filepath.Dir(filepath.Dir(paths[0]))
	require.Equal(t, paths[1], pool.rollingReplacement(filepath.Join(root, "p2", "removed.json")))
	require.Equal(t, paths[0], pool.rollingReplacement(filepath.Join(root, "p4", "removed.json")))
}

func TestSAStrategyUnavailableGroupsAndQuota(t *testing.T) {
	pool, paths := strategyPool(t, "cross-folder", "p1/a.json", "p1/b.json", "p2/a.json", "p2/b.json", "p3/a.json")
	pool.rejectFile(paths[2])
	require.Equal(t, paths[3], pool.rollup())
	pool.rejectFile(paths[3])
	require.Equal(t, paths[4], pool.rollup())
	pool.markStaleSa(paths[4])
	require.Equal(t, paths[1], pool.rollup(), "fallback to current group")
	pool.markStaleSa(paths[1])
	require.Empty(t, pool.rollup(), "do not switch to the only active account")
	pool.revertStaleSa(paths[1])
	exhausted, candidate := pool.staleSa(paths[0])
	require.False(t, exhausted)
	require.Equal(t, paths[1], candidate)
	require.Equal(t, 0, pool.activeIdx, "quota selection cannot publish a failed candidate")
	pool.activeSa(candidate)
	exhausted, candidate = pool.staleSa(candidate)
	require.True(t, exhausted)
	require.Empty(t, candidate)
}

func TestSAStrategyRebuildPreservesTurns(t *testing.T) {
	pool, paths := strategyPool(t, "cross-folder", "p1/a.json", "p1/b.json", "p2/a.json", "p2/b.json")
	pool.activeSa(paths[2])
	pool.activeSa(paths[1])
	pool.updateSas(paths, paths[1])
	require.Equal(t, paths[3], pool.rollup())
	pool.updateSas([]string{paths[0], paths[1], paths[3]}, paths[1])
	require.Equal(t, paths[3], pool.rollup(), "removed last-used file retains lexical cursor")
	folders := pool.folders
	require.Error(t, pool.loadInfoFromDir(filepath.Join(t.TempDir(), "missing"), paths[1]))
	require.Same(t, folders, pool.folders, "failed scan leaves strategy state intact")
}

func TestSAStrategyDefaultUnchanged(t *testing.T) {
	for _, mode := range []string{"", "default"} {
		pool, paths := strategyPool(t, mode, "p1/a.json", "p1/b.json", "p2/a.json")
		require.Nil(t, pool.folders)
		for _, index := range []int{1, 2, 0} {
			require.Equal(t, paths[index], pool.rollup())
			pool.activeSa(paths[index])
		}
		for i := 0; i < 10; i++ {
			pool.updateSas(paths, paths[0])
			exhausted, candidate := pool.staleSa(paths[0])
			require.False(t, exhausted)
			require.Contains(t, paths[1:], candidate)
			require.Equal(t, candidate, pool.sas[pool.activeIdx].saPath)
		}
	}
}

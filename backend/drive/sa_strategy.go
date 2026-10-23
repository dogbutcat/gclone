// Select SA candidates by policy without advancing cursors before activation.
package drive

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

const (
	saStrategyDefault     = "default"
	saStrategyCrossFolder = "cross-folder"
)

// A folder is a first-level subtree of the configured root. Direct root files
// form their own group; an explicit outside account is grouped by its parent.
type saFolder struct {
	name  string
	files []string
	last  string
}

type saFolderRotation struct {
	groups []saFolder
}

func newSAInfo(strategy, root string) (*SaInfo, error) {
	if strategy == "" {
		strategy = saStrategyDefault
	}
	switch strategy {
	case saStrategyDefault:
	case saStrategyCrossFolder:
		if root == "" {
			return nil, fmt.Errorf("drive: cross-folder strategy requires service_account_file_path")
		}
	default:
		return nil, fmt.Errorf("drive: invalid service_account_file_strategy %q (want default or cross-folder)", strategy)
	}
	return &SaInfo{strategy: strategy, folderRoot: root}, nil
}

func serviceAccountFolder(root, name string) string {
	rel, err := filepath.Rel(root, name)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return filepath.Dir(name)
	}
	if first, _, nested := strings.Cut(rel, string(filepath.Separator)); nested {
		return filepath.Join(root, first)
	}
	return root
}

// Rebuild only after a successful pool scan. Retain per-folder positions across
// rescans, including when the previously used file no longer exists.
func (sa *SaInfo) rebuildFolderRotation() {
	if sa.strategy != saStrategyCrossFolder {
		return
	}
	last := make(map[string]string)
	if sa.folders != nil {
		for _, group := range sa.folders.groups {
			last[group.name] = group.last
		}
	}
	byFolder := make(map[string][]string)
	for _, entry := range sa.sas {
		group := serviceAccountFolder(sa.folderRoot, entry.saPath)
		byFolder[group] = append(byFolder[group], entry.saPath)
	}
	names := make([]string, 0, len(byFolder))
	for name := range byFolder {
		names = append(names, name)
	}
	sort.Strings(names)
	rotation := &saFolderRotation{}
	for _, name := range names {
		files := byFolder[name]
		sort.Strings(files)
		rotation.groups = append(rotation.groups, saFolder{name: name, files: files, last: last[name]})
	}
	sa.folders = rotation
	if active, ok := sa.sas[sa.activeIdx]; ok {
		sa.recordFolderActivation(active.saPath)
	}
}

func (sa *SaInfo) recordFolderActivation(name string) {
	if sa.folders == nil || name == "" {
		return
	}
	// A committed client can outlive its credential file. Preserve its position
	// even when a successful rescan has excluded that file from the pool.
	folder := serviceAccountFolder(sa.folderRoot, name)
	group := sort.Search(len(sa.folders.groups), func(i int) bool { return sa.folders.groups[i].name >= folder })
	if group < len(sa.folders.groups) && sa.folders.groups[group].name == folder {
		sa.folders.groups[group].last = name
	}
}

// nextCrossFolder tries every other group first, then the current group. A
// single remaining group keeps rotating; the active account itself is skipped.
// Selection is read-only: failed/cancelled construction must not consume a turn.
func (sa *SaInfo) nextCrossFolder() string {
	return sa.nextCrossFolderFrom(sa.sas[sa.activeIdx].saPath)
}

func (sa *SaInfo) nextCrossFolderFrom(active string) string {
	if sa.folders == nil || len(sa.folders.groups) == 0 {
		return ""
	}
	current := -1
	if active != "" {
		folder := serviceAccountFolder(sa.folderRoot, active)
		current = sort.Search(len(sa.folders.groups), func(i int) bool { return sa.folders.groups[i].name >= folder })
		if current == len(sa.folders.groups) || sa.folders.groups[current].name != folder {
			current-- // If the whole group vanished, start at its lexical successor.
		}
	}
	for offset := 1; offset <= len(sa.folders.groups); offset++ {
		group := &sa.folders.groups[(current+offset)%len(sa.folders.groups)]
		start := sort.Search(len(group.files), func(i int) bool { return group.files[i] > group.last })
		for i := 0; i < len(group.files); i++ {
			name := group.files[(start+i)%len(group.files)]
			if _, usable := sa.saPool[name]; usable && name != active {
				return name
			}
		}
	}
	return ""
}

func (sa *SaInfo) rollingReplacement(active string) string {
	if sa.strategy == saStrategyCrossFolder {
		return sa.nextCrossFolderFrom(active)
	}
	return sa.rollup()
}

func (sa *SaInfo) quotaReplacement(active string) string {
	if sa.strategy == saStrategyCrossFolder {
		return sa.nextCrossFolderFrom(active)
	}
	if idx := sa.randomPick(); idx >= 0 {
		return sa.sas[idx].saPath
	}
	return ""
}

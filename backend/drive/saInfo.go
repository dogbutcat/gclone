package drive

import (
	"fmt"
	"math/rand"
	"path/filepath"
	"time"

	"github.com/rclone/rclone/fs"
)

type Sa struct {
	saPath  string
	isStale bool
}

type SaInfo struct {
	sas       map[int]Sa
	activeIdx int
	saPool    map[string]int
	invalid   map[string]bool
}

func (sa *SaInfo) updateSas(data []string, activeSa string) {
	convSas := make(map[int]Sa)
	convData := make(map[string]int)

	for i, v := range data {
		convSas[i] = Sa{saPath: v, isStale: false}
		convData[v] = i
	}
	sa.sas = convSas
	sa.saPool = convData
	sa.invalid = make(map[string]bool)
	sa.activeIdx = -1
	if len(data) == 0 || activeSa == "" {
		return
	}

	if result := sa.findIdxByStrInPool(activeSa); result != -1 {
		sa.activeIdx = result
	} else {
		existLen := len(sa.sas)
		sa.sas[existLen] = Sa{saPath: activeSa, isStale: false}
		sa.saPool[activeSa] = existLen
		sa.activeIdx = existLen
	}
}

func (sa *SaInfo) findIdxByStrInPool(str string) int {
	result := -1
	for k, v := range sa.saPool {
		if k == str {
			result = v
		}
	}
	return result
}

func (sa *SaInfo) findIdxByStr(str string) int {
	result := -1
	for k, v := range sa.sas {
		if v.saPath == str {
			result = k
		}
	}
	return result
}

func (sa *SaInfo) rollup() string {
	existLen := len(sa.sas)
	nextIdx := -1
	for i := sa.activeIdx + 1; i < existLen; i++ {
		if !sa.sas[i].isStale {
			nextIdx = i
			break
		}
	}
	if nextIdx == -1 {
		for i := 0; i < sa.activeIdx; i++ {
			if !sa.sas[i].isStale {
				nextIdx = i
				break
			}
		}
	}
	if nextIdx == -1 {
		return ""
	} else {
		return sa.sas[nextIdx].saPath
	}

}

func (sa *SaInfo) activeSa(saPath string) {
	if entry, ok := sa.saPool[saPath]; ok {
		sa.activeIdx = entry
	}
}

// markStaleSa removes only the named account without changing the active account.
func (sa *SaInfo) markStaleSa(target string) {
	if oldIdx, ok := sa.saPool[target]; ok {
		entry := sa.sas[oldIdx]
		entry.isStale = true
		sa.sas[oldIdx] = entry
		delete(sa.saPool, target)
	}
}

func (sa *SaInfo) staleSa(target string) (bool, string) {
	if target == "" {
		target = sa.sas[sa.activeIdx].saPath
	}
	sa.markStaleSa(target)
	if sa.isPoolEmpty() {
		sa.activeIdx = -1
		return true, ""
	}
	if ret := sa.randomPick(); ret != -1 {
		sa.activeIdx = ret
		return false, sa.sas[ret].saPath
	} else {
		return true, ""
	}

}

func (sa *SaInfo) randomPick() int {
	existLen := len(sa.saPool)
	if existLen == 0 {
		return -1
	}

	seed := time.Now().UnixNano() + int64(rand.Intn(1000000))
	rand_source := rand.NewSource(seed)
	rand_instance := rand.New(rand_source)
	r := rand_instance.Intn(existLen)

	// fmt.Println(time.Now().UnixNano())
	// fmt.Println("seed: ", seed)
	// fmt.Println("random val:", r)
	// fmt.Println(time.Now().UnixNano())

	var nextIdx int
	for _, v := range sa.saPool {
		if r == 0 {
			nextIdx = v
			break
		}
		r--
	}
	return nextIdx
}

func (sa *SaInfo) isPoolEmpty() bool {
	if len(sa.saPool) == 0 {
		return true
	} else {
		return false
	}
}

func (sa *SaInfo) revertStaleSa(target string) {
	if sa.invalid[target] {
		return
	}
	if target == "" {
		return
	}
	if oldIdx := sa.findIdxByStr(target); oldIdx != -1 {
		if entry, ok := sa.sas[oldIdx]; ok {
			entry.isStale = false
			sa.saPool[target] = oldIdx
			sa.sas[oldIdx] = entry
		}
	}

}

// rejectFile is called under the same rotation lock as other pool mutations.
func (sa *SaInfo) rejectFile(name string) {
	if sa.invalid == nil {
		sa.invalid = make(map[string]bool)
	}
	sa.invalid[name] = true
	sa.markStaleSa(name)
}

func (sa *SaInfo) loadInfoFromDir(dirPath string, activeSa string) error {
	files, err := discoverServiceAccountFiles(dirPath)
	if err != nil {
		return err
	}
	if activeSa != "" {
		activeSa, err = filepath.Abs(filepath.Clean(activeSa))
		if err != nil {
			return err
		}
	}
	valid := make([]string, 0, len(files))
	seen := make(map[string]bool, len(files))
	for _, name := range files {
		seen[name] = true
		if _, err := readServiceAccountFile(name); err != nil {
			fs.Errorf(nil, "Skipping %v", err)
			continue
		}
		valid = append(valid, name)
	}
	// Keep a valid explicitly selected account outside the directory, as before.
	if activeSa != "" && !seen[activeSa] {
		if _, err := readServiceAccountFile(activeSa); err == nil {
			valid = append(valid, activeSa)
		} else {
			fs.Errorf(nil, "Skipping %v", err)
			activeSa = ""
		}
	}
	if len(valid) == 0 {
		return fmt.Errorf("service account directory %q contains no usable credentials", dirPath)
	}
	activePresent := false
	for _, name := range valid {
		if name == activeSa {
			activePresent = true
			break
		}
	}
	if !activePresent {
		activeSa = ""
	}
	sa.updateSas(valid, activeSa)
	return nil
}

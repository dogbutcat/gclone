package drive

import (
	"fmt"
	"math/rand"
	"os"
	"path"
	"strings"
	"time"
)

type Sa struct {
	saPath  string
	isStale bool
}

type SaInfo struct {
	sas       map[int]Sa
	activeIdx int
	saPool    map[string]int
}

func (sa *SaInfo) updateSas(data []string, activeSa string) {
	if len(data) == 0 || activeSa == "" {
		return
	}
	convSas := make(map[int]Sa)
	convData := make(map[string]int)

	for i, v := range data {
		convSas[i] = Sa{saPath: v, isStale: false}
		convData[v] = i
	}
	sa.sas = convSas
	sa.saPool = convData

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

func (sa *SaInfo) staleSa(target string) (bool, string) {
	if target == "" {
		target = sa.sas[sa.activeIdx].saPath
	}
	oldIdx := sa.saPool[target]
	if entry, ok := sa.sas[oldIdx]; ok {
		entry.isStale = true
		sa.sas[oldIdx] = entry
	}
	delete(sa.saPool, target)
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

	rand_source := rand.NewSource(time.Now().UnixNano())
	rand_instance := rand.New(rand_source)
	r := rand_instance.Intn(existLen)

	var nextIdx int
	for _, v := range sa.saPool {
		if r == 0 {
			nextIdx = v
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

func (sa *SaInfo) loadInfoFromDir(dirPath string, activeSa string) {
	var fileNames []string
	pathSeparator := string(os.PathSeparator)
	if !strings.HasSuffix(dirPath, pathSeparator) {
		dirPath += pathSeparator
	}

	dir, err := os.Open(dirPath)
	if err != nil {
		fmt.Println("read ServiceAccount Folder error")
	}

	defer dir.Close()

	dir_list, err := dir.ReadDir(-1)

	if err != nil {
		fmt.Println("read ServiceAccountFilePath Files error")
	}
	for _, v := range dir_list {
		filePath := fmt.Sprintf("%s%s", dirPath, v.Name())
		if path.Ext(filePath) == ".json" {
			//fmt.Println(filePath)
			fileNames = append(fileNames, filePath)
		}
	}
	sa.updateSas(fileNames, activeSa)
}

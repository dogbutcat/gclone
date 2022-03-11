package drive

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestUpdate(t *testing.T) {
	a := &SaInfo{}
	b := []string{"a", "b", "c", "d"}

	a.updateSas(b, "a")
	assert.Equal(t, 0, a.activeIdx)
	a.updateSas(b, "d")
	assert.Equal(t, 3, a.activeIdx)
	a.updateSas(b, "e")
	assert.Equal(t, 4, a.activeIdx)
}

func TestActive(t *testing.T) {
	a := &SaInfo{}
	b := []string{"a", "b", "c", "d"}
	a.updateSas(b, "a")

	a.activeSa("c")
	assert.Equal(t, 2, a.activeIdx)
	a.activeSa("f")
	assert.Equal(t, 2, a.activeIdx)
}

func TestStale(t *testing.T) {
	a := &SaInfo{}
	b := []string{"a", "b", "c", "d"}
	a.updateSas(b, "a")

	err, newOne := a.staleSa("")
	assert.Equal(t, false, err)
	fmt.Println(newOne)
	assert.NotEqual(t, "a", newOne)
	assert.Equal(t, 3, len(a.saPool))
	assert.Equal(t, 4, len(a.sas))

	a.activeSa(newOne)
	assert.NotEqual(t, 0, a.activeIdx)

	err, newOne = a.staleSa("")
	fmt.Println(err)
	assert.Equal(t, false, err)
	assert.Equal(t, 2, len(a.saPool))
	a.activeSa(newOne)
	fmt.Println(a.activeIdx)
}

func TestStaleEnd(t *testing.T) {
	a := &SaInfo{}
	b := []string{"a", "b"}
	a.updateSas(b, "a")

	err, newOne := a.staleSa("")
	assert.Equal(t, false, err)
	assert.NotEqual(t, "a", newOne)
	assert.Equal(t, 1, len(a.saPool))
	assert.Equal(t, true, a.sas[0].isStale)
	a.activeSa(newOne)

	err, newOne = a.staleSa("")
	assert.Equal(t, true, err)
	assert.Equal(t, "", newOne)
}

func TestRollingDirect(t *testing.T) {
	a := &SaInfo{}
	b := []string{"a", "b", "c"}
	a.updateSas(b, "a")

	nextSa := a.rollup()
	assert.NotEqual(t, "a", nextSa)
	assert.Equal(t, "b", nextSa)
	a.activeSa(nextSa)
	assert.NotEqual(t, 0, a.activeIdx)
	assert.NotEqual(t, true, a.sas[0].isStale)
	assert.Equal(t, 1, a.activeIdx)

	nextSa = a.rollup()
	assert.NotEqual(t, "b", nextSa)
	assert.Equal(t, "c", nextSa)
	a.activeSa(nextSa)
	assert.NotEqual(t, 1, a.activeIdx)
	assert.NotEqual(t, true, a.sas[1].isStale)
	assert.Equal(t, 2, a.activeIdx)

	nextSa = a.rollup()
	assert.NotEqual(t, "c", nextSa)
	assert.Equal(t, "a", nextSa)
	a.activeSa(nextSa)
	assert.NotEqual(t, 2, a.activeIdx)
	assert.NotEqual(t, true, a.sas[2].isStale)
	assert.Equal(t, 0, a.activeIdx)
}

func TestRollingWithStale(t *testing.T) {
	a := &SaInfo{}
	b := []string{"a", "b", "c", "d"}
	a.updateSas(b, "a")

	err, newOne := a.staleSa("")
	assert.Equal(t, false, err)
	fmt.Println(newOne)
	a.activeSa(newOne)
	assert.NotEqual(t, "a", newOne)

	nextSa := a.rollup()
	fmt.Println("nextSa: ", nextSa)
	a.activeSa(nextSa)
	assert.NotEqual(t, 0, a.activeIdx)

	nextSa = a.rollup()
	fmt.Println("nextSa: ", nextSa)
	idx := a.saPool[nextSa]
	a.activeSa(nextSa)
	assert.NotEqual(t, 0, a.activeIdx)

	err, newOne = a.staleSa("")
	assert.Equal(t, false, err)
	fmt.Println(newOne)
	a.activeSa(newOne)
	assert.NotEqual(t, "a", newOne)

	nextSa = a.rollup()
	fmt.Println("nextSa: ", nextSa)
	assert.NotEqual(t, 0, a.activeIdx)
	assert.NotEqual(t, idx, a.activeIdx)
	a.activeSa(nextSa)

	nextSa = a.rollup()
	fmt.Println("nextSa: ", nextSa)
	a.activeSa(nextSa)
	assert.NotEqual(t, 0, a.activeIdx)
	assert.NotEqual(t, idx, a.activeIdx)
	idx = a.saPool[nextSa]

	err, newOne = a.staleSa("")
	assert.Equal(t, false, err)
	fmt.Println(newOne)
	a.activeSa(newOne)
	assert.NotEqual(t, "a", newOne)

	nextSa = a.rollup()
	fmt.Println("nextSa: ", nextSa)
	a.activeSa(nextSa)
	assert.NotEqual(t, 0, a.activeIdx)
	assert.NotEqual(t, idx, a.activeIdx)
}

func TestEmptyInit(t *testing.T) {
	a := &SaInfo{}
	b := []string{}
	a.updateSas(b, "")

	assert.Equal(t, true, a.isPoolEmpty())
}

func TestRevertStaleSa(t *testing.T) {
	a := &SaInfo{}
	b := []string{"a", "b", "c", "d"}
	a.updateSas(b, "a")

	_, step2Sa := a.staleSa("")
	a.activeSa(step2Sa)
	step2Idx := a.activeIdx

	assert.NotEqual(t, 0, a.activeIdx)
	assert.Equal(t, step2Sa, a.sas[a.activeIdx].saPath)

	_, step3Sa := a.staleSa("")
	a.activeSa(step3Sa)
	assert.NotEqual(t, step2Idx, a.activeIdx)
	assert.Equal(t, step3Sa, a.sas[a.activeIdx].saPath)
	assert.Equal(t, true, a.sas[0].isStale)
	assert.Equal(t, true, a.sas[step2Idx].isStale)

	a.revertStaleSa("a")
	assert.Equal(t, false, a.sas[0].isStale)
	assert.Equal(t, true, a.sas[step2Idx].isStale)

	a.revertStaleSa("f")
	assert.Equal(t, false, a.sas[0].isStale)
	assert.Equal(t, true, a.sas[step2Idx].isStale)

}

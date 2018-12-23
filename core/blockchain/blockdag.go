package blockchain

import (
	"sync"
	"sort"
	"container/list"
	"time"
	"github.com/noxproject/nox/common/hash"
	"fmt"
	"github.com/noxproject/nox/common/anticone"
)

//The abstract inferface is used to dag block
type IBlock interface {
	// Get hash of block
	GetHash() *hash.Hash

	// Get all parents set,the dag block has more than one parent
	GetParents() *BlockSet

	GetChildren() *BlockSet
	GetTimestamp() int64
	SetPastSetNum(num uint64)
	GetPastSetNum() uint64

	// The height is global order
	SetHeight(h uint64)
	GetHeight() uint64
}

//The abstract inferface is used to build and manager DAG
type IBlockDAG interface {
	HasBlock(h *hash.Hash) bool
	GetBlock(h *hash.Hash) IBlock
	GetFutureSet(fs *BlockSet, b IBlock)
	GetTips() *BlockSet
	GetBlockCount() uint
	GetGenesis() IBlock
}

type BlockDAG struct {
	// The outer layer block chain,it can use to interaction with the block dag.
	bc *BlockChain

	// The genesis of block dag
	genesis hash.Hash

	mtx   sync.Mutex

	// The total number blocks that this dag currently owned
	totalBlocks      uint

	// This is a set that only include honest block and it is the common part of each
	// tips in the block dag, so it is a blue set too.
	commonBlueSet    *BlockSet

	// This is a set that only include honest block exclude from "commonBlueSet",
	// but it's not very stable.
	tempBlueSet      *BlockSet

	lastCommonBlocks *BlockSet

	// Well understood,this orderly array is the sorting of common set.
	commonOrder      []*hash.Hash

	// This orderly array is the sorting of the end of dag set.
	tempOrder        []*hash.Hash

	// The terminal block is in block dag,this block have not any connecting at present.
	tips             *BlockSet

	// If it happens that during two propagation delays only one block is created, this block is called hourglass block.
	// This means it reference all the tips and is reference by all following blocks.
	// Hourglass block is a strong signal of finality because its blue set is stable.
	hourglassBlocks *BlockSet

	// This is time when the last block have added
	lastTime time.Time

	// The block anticone size is all in the DAG which did not reference it and
	// were not referenced by it.
	anticoneSize int

	// The Spectre algorithm
	s *Spectre
}

// Initialization block dag,for example, calculation anticone size.
func (bd *BlockDAG) Init(bch *BlockChain){
	bd.bc=bch
	bd.totalBlocks=0
	//bd.genesis=&bd.Genesis().hash
	bd.lastTime=time.Unix(time.Now().Unix(), 0)

	bd.anticoneSize = anticone.GetSize(bd.bc.params.BlockDelay,bd.bc.params.BlockRate,
		bd.bc.params.SecurityLevel)

	//spectre
	bd.s = NewSpectre(bd)

	log.Info(fmt.Sprintf("anticone size:%d",bd.anticoneSize))
}

func (bd *BlockDAG) GetBlock(h *hash.Hash) IBlock {
	return bd.bc.index.LookupNode(h)
}

// return the genesis block node
func (bd *BlockDAG) GetGenesis() IBlock {
	if bd.bc.params!=nil {
		return bd.GetBlock(bd.bc.params.GenesisHash)
	}
	return nil
}

// return the terminal blocks, because there maybe more than one, so this is a set.
func (bd *BlockDAG) GetTips() *BlockSet {
	return bd.tips
}

func (bd *BlockDAG) setTips(bs *BlockSet){
	bd.tips=bs
}

func (bd *BlockDAG) GetTipsList() []IBlock {
	result:=[]IBlock{}
	for k,_:=range bd.tips.GetMap(){
		result=append(result,bd.GetBlock(&k))
	}
	return result
}

// This is an entry for update the block dag,you need pass in a block parameter,
// If add block have failure,it will return false.
func (bd *BlockDAG) AddBlock(b IBlock) *list.List {
	if b == nil {
		return nil
	}
	bd.mtx.Lock()
	defer bd.mtx.Unlock()

	bd.bc.index.AddNode(b)
	bd.totalBlocks++
	bd.tempBlueSet=nil

	log.Trace(fmt.Sprintf("Add block:%v",b.GetHash().String()))

	t:=time.Unix(b.GetTimestamp(), 0)
	if bd.lastTime.Before(t) {
		bd.lastTime=t
	}

	bd.updateTips(b)
	bd.calculatePastBlockSetNum(b)
	//
	//obs:=NewBlockSet()
	bd.updateCommonBlueSet(b.GetHash())
	bd.updateHourglass()

	return	bd.updateOrder(b)
}

// Refresh the dag tip whith new block,it will cause changes in tips set.
func (bd *BlockDAG) updateTips(b IBlock) {
	if bd.tips == nil {
		bd.tips = NewBlockSet()
		bd.tips.Add(b.GetHash())
		bd.genesis=*b.GetHash()
		return
	}
	isBelong:=bd.tips.Has(b.GetHash())

	for k, _ := range bd.tips.GetMap() {
		node:=bd.GetBlock(&k)
		if node==nil {
			continue
		}
		children:=node.GetChildren()
		if children!=nil&&!children.IsEmpty() {
			bd.tips.Remove(&k)
		}
	}
	if !isBelong {
		bd.tips.Add(b.GetHash())
	}
}

// The past set is all its its ancestors.Because the past cannot be
// changed, so its number is fixed.
func (bd *BlockDAG) addPastSetNum(b IBlock, num uint64) {
	b.SetPastSetNum(num)
}

func (bd *BlockDAG) GetPastSetNum(b IBlock) uint64 {
	return b.GetPastSetNum()
}

func isVirtualTip(b IBlock, futureSet *BlockSet, anticone *BlockSet, children *BlockSet) bool {
	for k, _ := range children.GetMap() {
		if k.IsEqual(b.GetHash()) {
			return false
		}
		if !futureSet.Has(&k) && !anticone.Has(&k) {
			return false
		}
	}
	return true
}

// This function is used to GetAnticone recursion
func (bd *BlockDAG) recAnticone(b IBlock, futureSet *BlockSet, anticone *BlockSet, h *hash.Hash) {
	if h.IsEqual(b.GetHash()) {
		return
	}
	node:=bd.GetBlock(h)
	children := node.GetChildren()
	needRecursion := false
	if children == nil || children.Len() == 0 {
		needRecursion = true
	} else {
		needRecursion = isVirtualTip(b, futureSet, anticone, children)
	}
	if needRecursion {
		if !futureSet.Has(h) {
			anticone.Add(h)
		}
		parents := node.GetParents()

		//Because parents can not be empty, so there is no need to judge.
		for k, _ := range parents.GetMap() {
			bd.recAnticone(b, futureSet, anticone, &k)
		}
	}
}

// This function can get anticone set for an block that you offered in the block dag,If
// the exclude set is not empty,the final result will exclude set that you passed in.
func (bd *BlockDAG) GetAnticone(b IBlock, exclude *BlockSet) *BlockSet {
	futureSet := NewBlockSet()
	bd.GetFutureSet(futureSet, b)
	anticone := NewBlockSet()
	for k, _ := range bd.tips.GetMap() {
		bd.recAnticone(b, futureSet, anticone, &k)
	}
	if exclude != nil {
		anticone.Exclude(exclude)
	}
	return anticone
}

func (bd *BlockDAG) GetFutureSet(fs *BlockSet, b IBlock) {
	children := b.GetChildren()
	if children == nil || children.IsEmpty() {
		return
	}
	for k, _ := range children.GetMap() {
		if !fs.Has(&k) {
			fs.Add(&k)
			bd.GetFutureSet(fs, bd.GetBlock(&k))
		}
	}
}

// Calculate the size of the past block set.Because the past block set of block
// is stable,we can calculate and save.
func (bd *BlockDAG) calculatePastBlockSetNum(b IBlock) {

	if b.GetHash().IsEqual(&bd.genesis) {
		bd.addPastSetNum(b, 0)
		return
	}
	parents:=b.GetParents()
	if parents == nil || parents.IsEmpty() {
		return
	}
	parentsList:=[]IBlock{}
	for k,_:=range parents.GetMap(){
		parentsList=append(parentsList,bd.GetBlock(&k))
	}

	if len(parentsList) == 1 {
		bd.addPastSetNum(b, bd.GetPastSetNum(parentsList[0])+1)
		return
	}
	anticone := bd.GetAnticone(b, nil)

	anOther := bd.GetAnticone(parentsList[0], anticone)

	bd.addPastSetNum(b, bd.GetPastSetNum(parentsList[0])+uint64(anOther.Len())+1)
}

func (bd *BlockDAG) sortBlockSet(set *BlockSet, bs *BlockSet) SortBlocks {
	sb0 := SortBlocks{}
	sb1 := SortBlocks{}

	for k, _ := range set.GetMap() {
		node:=bd.GetBlock(&k)
		kv:=k
		if bs != nil && bs.Has(&k) {
			sb0 = append(sb0, SortBlock{&kv, bd.GetPastSetNum(node)})
		} else {
			sb1 = append(sb1, SortBlock{&kv, bd.GetPastSetNum(node)})
		}

	}
	sort.Sort(sb0)
	sort.Sort(sb1)
	sb0 = append(sb0, sb1...)
	return sb0
}

func (bd *BlockDAG) getPastSetByOrder(pastSet *BlockSet, exclude *BlockSet, h *hash.Hash) {
	if exclude.Has(h) || pastSet.Has(h) {
		return
	}

	if h.IsEqual(&bd.genesis) {
		return
	}

	parents := bd.GetBlock(h).GetParents()
	parentsList := parents.List()
	if parents == nil || len(parentsList) == 0 {
		return
	}
	for _, v := range parentsList {

		pastSet.Add(v)
		bd.getPastSetByOrder(pastSet, exclude, v)
	}
}

func (bd *BlockDAG) GetTempOrder(tempOrder *[]*hash.Hash, tempOrderM *BlockSet, bs *BlockSet, h *hash.Hash, exclude *BlockSet) {

	//1.If h that has already appeared must be excluded.
	if exclude != nil && exclude.Has(h) {
		return
	}
	node:=bd.GetBlock(h)
	parents := node.GetParents()

	//2.If its father hasn't sorted,the function must return.
	if parents != nil && parents.Len() > 0 {
		for k, _ := range parents.GetMap() {
			if exclude != nil && exclude.Has(&k) {
				continue
			}
			if !tempOrderM.Has(&k) {
				return
			}
		}
	}
	var anticone *BlockSet

	//3.Search some uncle block that it is in front of me, then
	//make sure they are sorted.
	if !tempOrderM.Has(h) {
		if !bd.genesis.IsEqual(h) && !bd.lastCommonBlocks.Has(h) {
			anticone = bd.GetAnticone(node, exclude)
			//
			if !anticone.IsEmpty() {
				ansb := bd.sortBlockSet(anticone, bs)
				if bs.Has(h) {
					for _, av := range ansb {
						avNode:=bd.GetBlock(av.h)
						if bs.Has(av.h) && bd.GetPastSetNum(avNode) < bd.GetPastSetNum(node) && !tempOrderM.Has(av.h) {
							bd.GetTempOrder(tempOrder, tempOrderM, bs, av.h, exclude)
						}
					}
				} else {
					for _, av := range ansb {
						if bs.Has(av.h) && !tempOrderM.Has(av.h) {
							bd.GetTempOrder(tempOrder, tempOrderM, bs, av.h, exclude)
						}
					}
				}

			}
		}

	}

	//4.Add myself to the array
	if !tempOrderM.Has(h) {
		(*tempOrder) = append(*tempOrder, h)
		tempOrderM.Add(h)
	}

	//5.Sort all my children
	childrenSrc := node.GetChildren()
	children := childrenSrc.Clone()
	if exclude != nil {
		children.Exclude(exclude)
	}
	if children == nil || children.Len() == 0 {
		return
	}
	pastSet := NewBlockSet()
	redSet := NewBlockSet()
	sb := bd.sortBlockSet(children, bs)

	for _, v := range sb {

		if bs.Has(v.h) {
			if !tempOrderM.Has(v.h) {
				pastSet.Clear()
				redSet.Clear()
				var excludeT *BlockSet
				if exclude != nil {
					excludeT = tempOrderM.Clone()
					excludeT.AddSet(exclude)
				} else {
					excludeT = tempOrderM
				}

				bd.getPastSetByOrder(pastSet, excludeT, v.h)

				inbs := pastSet.Intersection(anticone)
				if inbs != nil && inbs.Len() > 0 {
					insb := bd.sortBlockSet(inbs, bs)

					for _, v0 := range insb {
						if bs.Has(v0.h) {
							if !tempOrderM.Has(v0.h) {
								bd.GetTempOrder(tempOrder, tempOrderM, bs, v0.h, exclude)
							}
						} else {
							redSet.Add(v0.h)
						}
					}
					if !redSet.IsEmpty() {
						pastSet.Exclude(redSet)
						isAllOrder := true
						for k, _ := range pastSet.GetMap() {
							if !tempOrderM.Has(&k) {
								isAllOrder = false
								break
							}
						}
						if isAllOrder {
							redsb := bd.sortBlockSet(redSet, bs)
							for _, v1 := range redsb {
								bd.GetTempOrder(tempOrder, tempOrderM, bs, v1.h, exclude)
							}
						}
					}

				}
			}
			bd.GetTempOrder(tempOrder, tempOrderM, bs, v.h, exclude)
		}
	}
	for _, v := range sb {
		if !bs.Has(v.h) {
			bd.GetTempOrder(tempOrder, tempOrderM, bs, v.h, exclude)
		}
	}
}

func (bd *BlockDAG) updateCommonOrder(tip *hash.Hash, blueSet *BlockSet, isRollBack bool, exclude *BlockSet, curLastCommonBS *BlockSet, startIndex int) {

	if tip.IsEqual(&bd.genesis) {
		bd.commonOrder = []*hash.Hash{}
		return
	}
	node:=bd.GetBlock(tip)
	parents := node.GetParents()

	if parents.HasOnly(&bd.genesis) {
		if len(bd.commonOrder) == 0 {
			bd.commonOrder = append(bd.commonOrder, &bd.genesis)
		}
	}

	if !isRollBack {
		if blueSet == nil {
			return
		}
		tempOrder := []*hash.Hash{}
		tempOrderM := NewBlockSet()

		lpsb := bd.sortBlockSet(bd.lastCommonBlocks, blueSet)

		for _, v := range lpsb {
			bd.GetTempOrder(&tempOrder, tempOrderM, blueSet, v.h, exclude)
		}
		toLen := len(tempOrder)
		var poLen int = 0
		for i := 0; i < toLen; i++ {
			if bd.lastCommonBlocks.Has(tempOrder[i]) {
				continue
			}
			index := startIndex + i
			poLen = len(bd.commonOrder)
			if index < poLen {
				bd.commonOrder[index] = tempOrder[i]
			} else {
				bd.commonOrder = append(bd.commonOrder, tempOrder[i])
			}
		}
		poLen = len(bd.commonOrder)
		for i := poLen - 1; i >= 0; i-- {
			if bd.commonOrder[i]!=nil {
				if !curLastCommonBS.Has(bd.commonOrder[i]) {
					log.Error("order errer:end block is not new common block")
				}
				break
			}

		}

	} else {
		poLen := len(bd.commonOrder)
		rNum := 0
		for i := poLen - 1; i >= 0; i-- {
			if curLastCommonBS.Has(bd.commonOrder[i]) {
				break
			}
			bd.commonOrder[i] = nil
			rNum++
		}
		if (poLen - rNum) != startIndex {
			log.Error("order errer:number")
		}
	}
}

func (bd *BlockDAG) recPastBlockSet(genealogy *BlockSet, tipsAncestors *map[hash.Hash]*BlockSet, tipsGenealogy *map[hash.Hash]*BlockSet) {

	var maxPastHash *hash.Hash = nil
	var maxPastNum uint64 = 0
	var tipsHash *hash.Hash = nil

	for tk, v := range *tipsAncestors {
		tkv:=tk
		if v.Len() == 1 && v.Has(&bd.genesis) {
			continue
		}

		for k, _ := range v.GetMap() {
			kv:=k
			node:=bd.GetBlock(&kv)
			pastNum := bd.GetPastSetNum(node)
			if maxPastHash == nil || maxPastNum < pastNum {
				maxPastHash = &kv
				maxPastNum = pastNum
				tipsHash = &tkv
			}
		}

	}
	if maxPastHash == nil {
		return
	}
	parents := bd.GetBlock(maxPastHash).GetParents()
	if parents == nil || parents.Len() == 0 {
		return
	}
	(*tipsAncestors)[*tipsHash].Remove(maxPastHash)
	for k, _ := range parents.GetMap() {

		if !(*tipsGenealogy)[*tipsHash].Has(&k) {
			(*tipsAncestors)[*tipsHash].Add(&k)
			(*tipsGenealogy)[*tipsHash].Add(&k)
			if genealogy != nil {
				genealogy.Add(&k)
			}
		}

	}
}

func (bd *BlockDAG) calLastCommonBlocks(tip *hash.Hash) *BlockSet {
	tips := bd.GetTips()
	if tips == nil {
		return nil
	}
	tipsList := tips.List()
	if len(tipsList) <= 1 {
		return nil
	}
	tipsGenealogy:=make(map[hash.Hash]*BlockSet)
	tipsAncestors := make(map[hash.Hash]*BlockSet)
	for _, v := range tipsList {
		tipsAncestors[*v] = NewBlockSet()
		tipsAncestors[*v].Add(v)

		tipsGenealogy[*v]=NewBlockSet()
		tipsGenealogy[*v].Add(v)
	}

	//
	for {
		hasDifferent := false
		for k, v := range tipsAncestors {
			if k.IsEqual(tip) {
				continue
			}
			if !tipsAncestors[*tip].IsEqual(v) {
				hasDifferent = true
				break
			}
		}
		if !hasDifferent {
			break
		}
		bd.recPastBlockSet(nil, &tipsAncestors, &tipsGenealogy)
	}
	return tipsAncestors[*tip]
}

func (bd *BlockDAG) calLastCommonBlocksPBS(pastBlueSet *map[hash.Hash]*BlockSet) {
	/////
	lastPFuture := NewBlockSet()
	for k, _ := range bd.lastCommonBlocks.GetMap() {
		bd.GetFutureSet(lastPFuture, bd.GetBlock(&k))
	}

	if bd.lastCommonBlocks.Len() == 1 {
		lpbHash := bd.lastCommonBlocks.List()[0]
		if pastBlueSet != nil {
			(*pastBlueSet)[*lpbHash] = NewBlockSet()
		}

		//pastBlueSet[lpbHash].Add(lpbHash)

	} else {
		lastTempBlueSet := NewBlockSet()
		lpbAnti := make(map[hash.Hash]*BlockSet)

		for k, _ := range bd.lastCommonBlocks.GetMap() {
			lpbAnti[k] = bd.GetAnticone(bd.GetBlock(&k), lastPFuture)
			lastTempBlueSet.AddSet(lpbAnti[k])
		}
		if pastBlueSet != nil {
			for k, _ := range lastTempBlueSet.GetMap() {
				if !bd.commonBlueSet.Has(&k) {
					lastTempBlueSet.Remove(&k)
				}
			}
			for k, _ := range bd.lastCommonBlocks.GetMap() {
				(*pastBlueSet)[k] = lastTempBlueSet.Clone()
				(*pastBlueSet)[k].Exclude(lpbAnti[k])
				(*pastBlueSet)[k].Remove(&k)
			}
		}

	}
}

func (bd *BlockDAG) calculateBlueSet(parents *BlockSet, exclude *BlockSet, pastBlueSet *map[hash.Hash]*BlockSet, useCommon bool) *BlockSet {

	parentsPBSS := make(map[hash.Hash]*BlockSet)
	for k, _ := range parents.GetMap() {
		if _, ok := (*pastBlueSet)[k]; ok {
			parentsPBSS[k] = (*pastBlueSet)[k]
		} else {
			parentsPBSS[k] = NewBlockSet()
		}

	}

	maxBluePBSHash := GetMaxLenBlockSet(parentsPBSS)
	if maxBluePBSHash == nil {
		return nil
	}
	//
	result := NewBlockSet()
	result.AddSet(parentsPBSS[*maxBluePBSHash])
	result.Add(maxBluePBSHash)

	if parents.Len() == 1 {
		return result
	}

	maxBlueAnBS := bd.GetAnticone(bd.GetBlock(maxBluePBSHash), exclude)

	//

	if maxBlueAnBS != nil && maxBlueAnBS.Len() > 0 {

		for k, _ := range maxBlueAnBS.GetMap() {
			bAnBS := bd.GetAnticone(bd.GetBlock(&k), exclude)
			if bAnBS == nil || bAnBS.Len() == 0 {
				continue
			}
			inBS := result.Intersection(bAnBS)
			if useCommon {
				inPBS := bd.commonBlueSet.Intersection(bAnBS)
				inBS.AddSet(inPBS)
			}

			if inBS == nil || inBS.Len() <= bd.anticoneSize {
				result.Add(&k)
			}
		}
	}
	return result
}

func (bd *BlockDAG) calculatePastBlueSet(h *hash.Hash, pastBlueSet *map[hash.Hash]*BlockSet, useCommon bool) {

	_, ok := (*pastBlueSet)[*h]
	if ok {
		return
	}
	if h.IsEqual(&bd.genesis) {
		(*pastBlueSet)[*h] = NewBlockSet()
		return
	}
	//
	parents := bd.GetBlock(h).GetParents()
	if parents == nil || parents.IsEmpty() {
		return
	} else if parents.HasOnly(&bd.genesis) {
		(*pastBlueSet)[*h] = NewBlockSet()
		(*pastBlueSet)[*h].Add(&bd.genesis)
		return
	}

	for k, _ := range parents.GetMap() {
		bd.calculatePastBlueSet(&k, pastBlueSet, useCommon)
	}
	//
	anticone := bd.GetAnticone(bd.GetBlock(h), nil)
	(*pastBlueSet)[*h] = bd.calculateBlueSet(parents, anticone, pastBlueSet, useCommon)
}

func (bd *BlockDAG) updateCommonBlueSet(tip *hash.Hash){

	if tip.IsEqual(&bd.genesis) {
		//needOrderBS.Add(tip)
		bd.commonBlueSet = NewBlockSet()
		bd.lastCommonBlocks = NewBlockSet()
		bd.updateCommonOrder(tip, nil, false, nil, nil, 0)

		return
	}
	parents := bd.GetBlock(tip).GetParents()

	if parents.HasOnly(&bd.genesis) {
		//needOrderBS.AddList(bd.tempOrder)
		bd.commonBlueSet.Clear()
		bd.commonBlueSet.Add(&bd.genesis)
		bd.lastCommonBlocks.Clear()
		bd.lastCommonBlocks.Add(&bd.genesis)
		bd.updateCommonOrder(tip, nil, false, nil, nil, 0)

	} else {
		tips := bd.GetTips()
		if tips.Len() <= 1 {
			//needOrderBS.Add(tip)
			return
		}
		curLastCommonBS := bd.calLastCommonBlocks(tip)
		if curLastCommonBS.IsEqual(bd.lastCommonBlocks) {
			return
		}
		curLPFuture := NewBlockSet()
		for k, _ := range curLastCommonBS.GetMap() {
			bd.GetFutureSet(curLPFuture, bd.GetBlock(&k))
		}

		lastPFuture := NewBlockSet()
		for k, _ := range bd.lastCommonBlocks.GetMap() {
			bd.GetFutureSet(lastPFuture, bd.GetBlock(&k))
		}
		//
		pastBlueSet := make(map[hash.Hash]*BlockSet)

		if lastPFuture.Contain(curLPFuture) {
			//needOrderBS.AddSet(lastPFuture)
			//
			oExclude := NewBlockSet()
			oExclude.AddSet(curLPFuture)
			for k, _ := range bd.lastCommonBlocks.GetMap() {
				oExclude.AddSet(bd.GetBlock(&k).GetParents())
			}

			bd.calLastCommonBlocksPBS(&pastBlueSet)

			for k, _ := range curLastCommonBS.GetMap() {
				bd.calculatePastBlueSet(&k, &pastBlueSet, false)
			}
			commonBlueSet := bd.calculateBlueSet(curLastCommonBS, curLPFuture, &pastBlueSet, false)
			//
			bd.updateCommonOrder(tip, commonBlueSet, false, oExclude, curLastCommonBS, int(bd.totalBlocks)-lastPFuture.Len())
			//
			bd.commonBlueSet.AddSet(commonBlueSet)
			bd.lastCommonBlocks = curLastCommonBS
		} else if curLPFuture.Contain(lastPFuture) {
			//needOrderBS.AddSet(curLPFuture)

			bd.updateCommonOrder(tip, nil, true, nil, curLastCommonBS, int(bd.totalBlocks)-curLPFuture.Len())
			bd.commonBlueSet.Exclude(curLPFuture)
			bd.lastCommonBlocks = curLastCommonBS
		} else {
			log.Error("error:common set")
		}

	}

}

func (bd *BlockDAG) GetTempBlueSet() *BlockSet {
	//
	tips := bd.GetTips()
	//

	result := NewBlockSet()
	if tips.HasOnly(&bd.genesis) {
		result = NewBlockSet()
		result.Add(&bd.genesis)
	} else {
		pastBlueSet := make(map[hash.Hash]*BlockSet)

		bd.calLastCommonBlocksPBS(&pastBlueSet)

		for k, _ := range tips.GetMap() {
			bd.calculatePastBlueSet(&k, &pastBlueSet, false)
		}

		result = bd.calculateBlueSet(tips, nil, &pastBlueSet, false)
	}
	return result
}

func (bd *BlockDAG) getTempBS() *BlockSet{
	if bd.tempBlueSet==nil {
		bd.tempBlueSet=bd.GetTempBlueSet()
	}
	return bd.tempBlueSet
}

func (bd *BlockDAG) recCalHourglass(genealogy *BlockSet, ancestors *BlockSet) {

	var maxPastHash *hash.Hash = nil
	var maxPastNum uint64 = 0

	for k, _ := range ancestors.GetMap() {
		pastNum := bd.GetPastSetNum(bd.GetBlock(&k))
		if maxPastHash == nil || maxPastNum < pastNum {
			maxPastHash = &k
			maxPastNum = pastNum
		}
	}

	if maxPastHash == nil {
		return
	}
	parents := bd.GetBlock(maxPastHash).GetParents()
	if parents == nil || parents.Len() == 0 {
		return
	}
	ancestors.Remove(maxPastHash)
	for k, _ := range parents.GetMap() {
		if !genealogy.Has(&k) {
			ancestors.Add(&k)
			genealogy.Add(&k)
		}
	}

}

func (bd *BlockDAG) updateHourglass(){
	tips := bd.GetTips()
	if tips == nil||tips.Len()==0 {
		return
	}
	if bd.hourglassBlocks==nil {
		bd.hourglassBlocks=NewBlockSet()
	}
	if tips.HasOnly(&bd.genesis){

		bd.hourglassBlocks.Add(&bd.genesis)
		return
	}
	tempNum:=0
	for k,_:=range tips.GetMap(){
		parents:=bd.GetBlock(&k).GetParents()
		if parents!=nil&&parents.HasOnly(&bd.genesis) {
			tempNum++
		}
	}
	if tempNum==tips.Len() {
		return
	}
	//
	genealogy:=NewBlockSet()
	ancestors:=NewBlockSet()

	for k,_:=range tips.GetMap(){
		genealogy.Add(&k)
		ancestors.Add(&k)
	}
	tempBs:=bd.getTempBS()

	for  {
		bd.recCalHourglass(genealogy,ancestors)

		ne0:=tempBs.Intersection(ancestors)
		ne1:=bd.commonBlueSet.Intersection(ancestors)
		ne0.AddSet(ne1)

		ancestors=ne0


		//
		if ancestors.IsEmpty()||ancestors.HasOnly(&bd.genesis) {
			bd.hourglassBlocks.Clear()
			bd.hourglassBlocks.Add(&bd.genesis)
			return
		}

		sb := bd.sortBlockSet(ancestors,nil)
		for _,v:=range sb{
			anti:=bd.GetAnticone(bd.GetBlock(v.h),nil)
			if anti.Len()==0 {
				bd.hourglassBlocks.Exclude(genealogy)
				bd.hourglassBlocks.Add(v.h)
				return
			}else{
				banti0:=tempBs.Intersection(anti)
				banti1:=bd.commonBlueSet.Intersection(anti)
				banti0.AddSet(banti1)

				if banti0.Len()==0 {
					bd.hourglassBlocks.Exclude(genealogy)
					bd.hourglassBlocks.Add(v.h)
					return
				}
			}
		}
	}
}

func (bd *BlockDAG) updateOrder(b IBlock) *list.List{
	bd.tempOrder=[]*hash.Hash{}
	refNodes:=list.New()
	if bd.totalBlocks == 1 {
		bd.tempOrder=append(bd.tempOrder, &bd.genesis)
		refNodes.PushBack(bd.genesis)
		b.SetHeight(0)
		return refNodes
	}
	tempOrder := []*hash.Hash{}
	tempOrderM := NewBlockSet()
	//
	blueSet := bd.getTempBS()
	lpsb := bd.sortBlockSet(bd.lastCommonBlocks, nil)
	exclude := NewBlockSet()
	for k, _ := range bd.lastCommonBlocks.GetMap() {
		exclude.AddSet(bd.GetBlock(&k).GetParents())
	}
	for _, v := range lpsb {
		bd.GetTempOrder(&tempOrder, tempOrderM, blueSet, v.h, exclude)
	}
	tLen := len(tempOrder)
	//
	pNum:=bd.GetCommonOrderNum()
	tIndex:=0
	for i := 0; i < tLen; i++ {
		if !bd.lastCommonBlocks.Has(tempOrder[i]) {
			bd.tempOrder = append(bd.tempOrder, tempOrder[i])
			//
			node:=bd.GetBlock(tempOrder[i])

			node.SetHeight(uint64(pNum+tIndex))
			tIndex++
			if node.GetHeight()==0 {
				log.Error(fmt.Sprintf("Order error:%v",*node.GetHash()))
			}
		}
	}
	checkOrder:=bd.GetCommonOrderNum()+len(bd.tempOrder)
	if uint(checkOrder)!=bd.totalBlocks {
		log.Error(fmt.Sprintf("Order error:The number is a problem"))
	}
	//////
	tips:=bd.GetTips()
	if tips.HasOnly(b.GetHash())||bd.tempOrder[len(bd.tempOrder)-1].IsEqual(b.GetHash()) {
		b.SetHeight(uint64(bd.totalBlocks-1))
		refNodes.PushBack(b.GetHash())
		return refNodes
	}
	////
	tLen = len(bd.tempOrder)
	for i:=tLen-1;i>=0;i-- {
		refNodes.PushFront(bd.tempOrder[i])
		if bd.tempOrder[i].IsEqual(b.GetHash()) {
			break
		}
	}
	return refNodes
}

func (bd *BlockDAG) GetLastBlock() IBlock{
	if bd.tempOrder==nil {
		return nil
	}
	tLen:=len(bd.tempOrder)
	if tLen>0 {
		return bd.GetBlock(bd.tempOrder[tLen-1])
	}
	pLen:=len(bd.commonOrder)
	if pLen>0 {
		for i:=pLen-1;i>=0 ;i--  {
			if bd.commonOrder[i]!=nil {
				return bd.GetBlock(bd.commonOrder[i])
			}
		}
	}
	return nil
}

func (bd *BlockDAG) GetCommonOrderNum() int{
	pLen:=len(bd.commonOrder)

	if pLen>0 {
		var i int
		for i=pLen-1;i>=0 ;i--  {
			if bd.commonOrder[i]!=nil {
				break
			}
		}
		return i+1
	}
	return 0
}

func (bd *BlockDAG) GetBlockOrder(h *hash.Hash) int32{
	var result int32=-1
	if bd.tempOrder==nil {
		return result
	}
	result=int32(bd.totalBlocks)
	tLen:=len(bd.tempOrder)
	if tLen>0 {
		for i:=tLen-1;i>=0 ;i--  {
			if bd.tempOrder[i]!=nil {
				result--
				if h.IsEqual(bd.tempOrder[i]) {
					return result
				}
			}
		}
	}
	pLen:=len(bd.commonOrder)
	if pLen>0 {
		for i:=pLen-1;i>=0 ;i--  {
			if bd.commonOrder[i]!=nil {
				result--
				if h.IsEqual(bd.commonOrder[i]) {
					return result
				}
			}
		}
	}

	return -1
}

// This function need a stable sequence,so call it before sorting the DAG.
// If the h is invalid,the function will become a little inefficient.
func (bd *BlockDAG) GetPrevious(h *hash.Hash) *hash.Hash{
	if bd.tempOrder==nil {
		return nil
	}
	isEnd:=false
	tLen:=len(bd.tempOrder)
	if tLen>0 {
		for i:=tLen-1;i>=0 ;i--  {
			if bd.tempOrder[i]!=nil {
				if h.IsEqual(bd.tempOrder[i]) {
					if i>0 {
						return bd.tempOrder[i-1]
					}else{
						isEnd=true
					}
				}
			}
		}
	}
	pLen:=len(bd.commonOrder)
	if pLen>0 {
		for i:=pLen-1;i>=0 ;i--  {
			if bd.commonOrder[i]!=nil {
				if isEnd {
					return bd.commonOrder[i]
				}
				if h.IsEqual(bd.commonOrder[i]) {
					if i>0 {
						return bd.commonOrder[i-1]
					}
				}
			}
		}
	}

	return nil
}

func (bd *BlockDAG) GetBlockByOrder(order int) *hash.Hash{
	if bd.tempOrder==nil||order<0 {
		return nil
	}
	pNum:=bd.GetCommonOrderNum()
	if order<pNum {
		return bd.commonOrder[order]
	}
	rIndex:=order-pNum
	tLen:=len(bd.tempOrder)
	if rIndex<tLen {
		return bd.tempOrder[rIndex]
	}
	return nil
}

func (bd *BlockDAG) GetLastTime() *time.Time{
	return &bd.lastTime
}

func (bd *BlockDAG) HasBlock(h *hash.Hash) bool {
	return bd.GetBlock(h)!=nil
}

func (bd *BlockDAG) GetBlockCount() uint {
	return bd.totalBlocks
}
///////
type SortBlock struct {
	h          *hash.Hash
	pastSetNum uint64
}

type SortBlocks []SortBlock

func (a SortBlocks) Len() int {
	return len(a)
}

func (a SortBlocks) Less(i, j int) bool {
	if a[i].pastSetNum == a[j].pastSetNum {
		return a[i].h.String() < a[j].h.String()
	}
	return a[i].pastSetNum < a[j].pastSetNum
}

func (a SortBlocks) Swap(i, j int) {
	a[i], a[j] = a[j], a[i]
}
/////////

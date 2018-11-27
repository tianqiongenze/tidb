// Copyright 2017 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package core

import (
	"math"

	"github.com/pingcap/parser/ast"
	"github.com/pingcap/parser/model"
	"github.com/pingcap/parser/mysql"
	"github.com/pingcap/tidb/expression"
	"github.com/pingcap/tidb/expression/aggregation"
	"github.com/pingcap/tidb/planner/property"
	"github.com/pingcap/tidb/sessionctx"
	"github.com/pingcap/tidb/types"
	"github.com/pingcap/tidb/util/chunk"
	"github.com/pingcap/tidb/util/ranger"
	log "github.com/sirupsen/logrus"
)

func (p *LogicalUnionScan) exhaustPhysicalPlans(prop *property.PhysicalProperty) []PhysicalPlan {
	us := PhysicalUnionScan{Conditions: p.conditions}.Init(p.ctx, p.stats, prop)
	return []PhysicalPlan{us}
}

func getMaxSortPrefix(sortCols, allCols []*expression.Column) []int {
	tmpSchema := expression.NewSchema(allCols...)
	sortColOffsets := make([]int, 0, len(sortCols))
	for _, sortCol := range sortCols {
		offset := tmpSchema.ColumnIndex(sortCol)
		if offset == -1 {
			return sortColOffsets
		}
		sortColOffsets = append(sortColOffsets, offset)
	}
	return sortColOffsets
}

func findMaxPrefixLen(candidates [][]*expression.Column, keys []*expression.Column) int {
	maxLen := 0
	for _, candidateKeys := range candidates {
		matchedLen := 0
		for i := range keys {
			if i < len(candidateKeys) && keys[i].Equal(nil, candidateKeys[i]) {
				matchedLen++
			} else {
				break
			}
		}
		if matchedLen > maxLen {
			maxLen = matchedLen
		}
	}
	return maxLen
}

func (p *LogicalJoin) moveEqualToOtherConditions(offsets []int) []expression.Expression {
	otherConds := make([]expression.Expression, len(p.OtherConditions))
	copy(otherConds, p.OtherConditions)
	for i, eqCond := range p.EqualConditions {
		match := false
		for _, offset := range offsets {
			if i == offset {
				match = true
				break
			}
		}
		if !match {
			otherConds = append(otherConds, eqCond)
		}
	}
	return otherConds
}

// Only if the input required prop is the prefix fo join keys, we can pass through this property.
func (p *PhysicalMergeJoin) tryToGetChildReqProp(prop *property.PhysicalProperty) ([]*property.PhysicalProperty, bool) {
	lProp := &property.PhysicalProperty{TaskTp: property.RootTaskType, Cols: p.LeftKeys, ExpectedCnt: math.MaxFloat64}
	rProp := &property.PhysicalProperty{TaskTp: property.RootTaskType, Cols: p.RightKeys, ExpectedCnt: math.MaxFloat64}
	if !prop.IsEmpty() {
		// sort merge join fits the cases of massive ordered data, so desc scan is always expensive.
		if prop.Desc {
			return nil, false
		}
		if !prop.IsPrefix(lProp) && !prop.IsPrefix(rProp) {
			return nil, false
		}
		if prop.IsPrefix(rProp) && p.JoinType == LeftOuterJoin {
			return nil, false
		}
		if prop.IsPrefix(lProp) && p.JoinType == RightOuterJoin {
			return nil, false
		}
	}

	return []*property.PhysicalProperty{lProp, rProp}, true
}

func (p *LogicalJoin) getMergeJoin(prop *property.PhysicalProperty) []PhysicalPlan {
	joins := make([]PhysicalPlan, 0, len(p.leftProperties))
	// The leftProperties caches all the possible properties that are provided by its children.
	for _, lhsChildProperty := range p.leftProperties {
		offsets := getMaxSortPrefix(lhsChildProperty, p.LeftJoinKeys)
		if len(offsets) == 0 {
			continue
		}

		leftKeys := lhsChildProperty[:len(offsets)]
		rightKeys := expression.NewSchema(p.RightJoinKeys...).ColumnsByIndices(offsets)

		prefixLen := findMaxPrefixLen(p.rightProperties, rightKeys)
		if prefixLen == 0 {
			continue
		}

		leftKeys = leftKeys[:prefixLen]
		rightKeys = rightKeys[:prefixLen]
		offsets = offsets[:prefixLen]
		mergeJoin := PhysicalMergeJoin{
			JoinType:        p.JoinType,
			LeftConditions:  p.LeftConditions,
			RightConditions: p.RightConditions,
			DefaultValues:   p.DefaultValues,
			LeftKeys:        leftKeys,
			RightKeys:       rightKeys,
		}.Init(p.ctx, p.stats.ScaleByExpectCnt(prop.ExpectedCnt))
		mergeJoin.SetSchema(p.schema)
		mergeJoin.OtherConditions = p.moveEqualToOtherConditions(offsets)
		if reqProps, ok := mergeJoin.tryToGetChildReqProp(prop); ok {
			mergeJoin.childrenReqProps = reqProps
			joins = append(joins, mergeJoin)
		}
	}
	// If TiDB_SMJ hint is existed && no join keys in children property,
	// it should to enforce merge join.
	if len(joins) == 0 && (p.preferJoinType&preferMergeJoin) > 0 {
		return p.getEnforcedMergeJoin(prop)
	}

	return joins
}

// Change JoinKeys order, by offsets array
// offsets array is generate by prop check
func getNewJoinKeysByOffsets(oldJoinKeys []*expression.Column, offsets []int) []*expression.Column {
	newKeys := make([]*expression.Column, 0, len(oldJoinKeys))
	for _, offset := range offsets {
		newKeys = append(newKeys, oldJoinKeys[offset])
	}
	for pos, key := range oldJoinKeys {
		isExist := false
		for _, p := range offsets {
			if p == pos {
				isExist = true
				break
			}
		}
		if !isExist {
			newKeys = append(newKeys, key)
		}
	}
	return newKeys
}

// Change EqualConditions order, by offsets array
// offsets array is generate by prop check
func getNewEqualConditionsByOffsets(oldEqualCond []*expression.ScalarFunction, offsets []int) []*expression.ScalarFunction {
	newEqualCond := make([]*expression.ScalarFunction, 0, len(oldEqualCond))
	for _, offset := range offsets {
		newEqualCond = append(newEqualCond, oldEqualCond[offset])
	}
	for pos, condition := range oldEqualCond {
		isExist := false
		for _, p := range offsets {
			if p == pos {
				isExist = true
				break
			}
		}
		if !isExist {
			newEqualCond = append(newEqualCond, condition)
		}
	}
	return newEqualCond
}

func (p *LogicalJoin) getEnforcedMergeJoin(prop *property.PhysicalProperty) []PhysicalPlan {
	// Check whether SMJ can satisfy the required property
	offsets := make([]int, 0, len(p.LeftJoinKeys))
	for _, col := range prop.Cols {
		isExist := false
		for joinKeyPos := 0; joinKeyPos < len(p.LeftJoinKeys); joinKeyPos++ {
			var key *expression.Column
			if col.Equal(p.ctx, p.LeftJoinKeys[joinKeyPos]) {
				key = p.LeftJoinKeys[joinKeyPos]
			}
			if col.Equal(p.ctx, p.RightJoinKeys[joinKeyPos]) {
				key = p.RightJoinKeys[joinKeyPos]
			}
			if key == nil {
				continue
			}
			for i := 0; i < len(offsets); i++ {
				if offsets[i] == joinKeyPos {
					isExist = true
					break
				}
			}
			if !isExist {
				offsets = append(offsets, joinKeyPos)
			}
			isExist = true
			break
		}
		if !isExist {
			return nil
		}
	}
	// Generate the enforced sort merge join
	leftKeys := getNewJoinKeysByOffsets(p.LeftJoinKeys, offsets)
	rightKeys := getNewJoinKeysByOffsets(p.RightJoinKeys, offsets)
	lProp := &property.PhysicalProperty{TaskTp: property.RootTaskType, Cols: leftKeys, ExpectedCnt: math.MaxFloat64, Enforced: true, Desc: prop.Desc}
	rProp := &property.PhysicalProperty{TaskTp: property.RootTaskType, Cols: rightKeys, ExpectedCnt: math.MaxFloat64, Enforced: true, Desc: prop.Desc}
	enforcedPhysicalMergeJoin := PhysicalMergeJoin{
		JoinType:        p.JoinType,
		LeftConditions:  p.LeftConditions,
		RightConditions: p.RightConditions,
		DefaultValues:   p.DefaultValues,
		LeftKeys:        leftKeys,
		RightKeys:       rightKeys,
		OtherConditions: p.OtherConditions,
	}.Init(p.ctx, p.stats.ScaleByExpectCnt(prop.ExpectedCnt))
	enforcedPhysicalMergeJoin.SetSchema(p.schema)
	enforcedPhysicalMergeJoin.childrenReqProps = []*property.PhysicalProperty{lProp, rProp}
	return []PhysicalPlan{enforcedPhysicalMergeJoin}
}

func (p *LogicalJoin) getHashJoins(prop *property.PhysicalProperty) []PhysicalPlan {
	if !prop.IsEmpty() { // hash join doesn't promise any orders
		return nil
	}
	joins := make([]PhysicalPlan, 0, 2)
	switch p.JoinType {
	case SemiJoin, AntiSemiJoin, LeftOuterSemiJoin, AntiLeftOuterSemiJoin, LeftOuterJoin:
		joins = append(joins, p.getHashJoin(prop, 1))
	case RightOuterJoin:
		joins = append(joins, p.getHashJoin(prop, 0))
	case InnerJoin:
		joins = append(joins, p.getHashJoin(prop, 1))
		joins = append(joins, p.getHashJoin(prop, 0))
	}
	return joins
}

func (p *LogicalJoin) getHashJoin(prop *property.PhysicalProperty, innerIdx int) *PhysicalHashJoin {
	chReqProps := make([]*property.PhysicalProperty, 2)
	chReqProps[innerIdx] = &property.PhysicalProperty{ExpectedCnt: math.MaxFloat64}
	chReqProps[1-innerIdx] = &property.PhysicalProperty{ExpectedCnt: prop.ExpectedCnt}
	hashJoin := PhysicalHashJoin{
		EqualConditions: p.EqualConditions,
		LeftConditions:  p.LeftConditions,
		RightConditions: p.RightConditions,
		OtherConditions: p.OtherConditions,
		JoinType:        p.JoinType,
		Concurrency:     uint(p.ctx.GetSessionVars().HashJoinConcurrency),
		DefaultValues:   p.DefaultValues,
		InnerChildIdx:   innerIdx,
	}.Init(p.ctx, p.stats.ScaleByExpectCnt(prop.ExpectedCnt), chReqProps...)
	hashJoin.SetSchema(p.schema)
	return hashJoin
}

// joinKeysMatchIndex checks whether the join key is in the index.
// It returns a slice a[] what a[i] means keys[i] is related with indexCols[a[i]], -1 for no matching column.
// It will return nil if there's no column that matches index.
func joinKeysMatchIndex(keys, indexCols []*expression.Column, colLengths []int) []int {
	keyOff2IdxOff := make([]int, len(keys))
	for i := range keyOff2IdxOff {
		keyOff2IdxOff[i] = -1
	}
	// There should be at least one column in join keys which can match the index's column.
	matched := false
	tmpSchema := expression.NewSchema(keys...)
	for i, idxCol := range indexCols {
		if colLengths[i] != types.UnspecifiedLength {
			continue
		}
		keyOff := tmpSchema.ColumnIndex(idxCol)
		if keyOff == -1 {
			continue
		}
		matched = true
		keyOff2IdxOff[keyOff] = i
	}
	if !matched {
		return nil
	}
	return keyOff2IdxOff
}

// When inner plan is TableReader, the parameter `ranges` will be nil. Because pk only have one column. So all of its range
// is generated during execution time.
func (p *LogicalJoin) constructIndexJoin(prop *property.PhysicalProperty, innerJoinKeys, outerJoinKeys []*expression.Column, outerIdx int,
	innerPlan PhysicalPlan, ranges []*ranger.Range, keyOff2IdxOff []int, compareFilters *ColWithCompareOps) []PhysicalPlan {
	joinType := p.JoinType
	outerSchema := p.children[outerIdx].Schema()
	// If the order by columns are not all from outer child, index join cannot promise the order.
	if !prop.AllColsFromSchema(outerSchema) {
		return nil
	}
	chReqProps := make([]*property.PhysicalProperty, 2)
	chReqProps[outerIdx] = &property.PhysicalProperty{TaskTp: property.RootTaskType, ExpectedCnt: prop.ExpectedCnt, Cols: prop.Cols, Desc: prop.Desc}
	newInnerKeys := make([]*expression.Column, 0, len(innerJoinKeys))
	newOuterKeys := make([]*expression.Column, 0, len(outerJoinKeys))
	newKeyOff := make([]int, 0, len(keyOff2IdxOff))
	newOtherConds := make([]expression.Expression, len(p.OtherConditions), len(p.OtherConditions)+len(p.EqualConditions))
	copy(newOtherConds, p.OtherConditions)
	for keyOff, idxOff := range keyOff2IdxOff {
		if keyOff2IdxOff[keyOff] < 0 {
			newOtherConds = append(newOtherConds, p.EqualConditions[keyOff])
			continue
		}
		newInnerKeys = append(newInnerKeys, innerJoinKeys[keyOff])
		newOuterKeys = append(newOuterKeys, outerJoinKeys[keyOff])
		newKeyOff = append(newKeyOff, idxOff)
	}
	join := PhysicalIndexJoin{
		OuterIndex:      outerIdx,
		LeftConditions:  p.LeftConditions,
		RightConditions: p.RightConditions,
		OtherConditions: newOtherConds,
		JoinType:        joinType,
		OuterJoinKeys:   newOuterKeys,
		InnerJoinKeys:   newInnerKeys,
		DefaultValues:   p.DefaultValues,
		innerPlan:       innerPlan,
		KeyOff2IdxOff:   newKeyOff,
		Ranges:          ranges,
		compareFilters:  compareFilters,
	}.Init(p.ctx, p.stats.ScaleByExpectCnt(prop.ExpectedCnt), chReqProps...)
	join.SetSchema(p.schema)
	return []PhysicalPlan{join}
}

// getIndexJoinByOuterIdx will generate index join by outerIndex. OuterIdx points out the outer child.
// First of all, we'll check whether the inner child is DataSource.
// Then, we will extract the join keys of p's equal conditions. Then check whether all of them are just the primary key
// or match some part of on index. If so we will choose the best one and construct a index join.
func (p *LogicalJoin) getIndexJoinByOuterIdx(prop *property.PhysicalProperty, outerIdx int) []PhysicalPlan {
	innerChild := p.children[1-outerIdx]
	var (
		innerJoinKeys []*expression.Column
		outerJoinKeys []*expression.Column
	)
	if outerIdx == 0 {
		outerJoinKeys = p.LeftJoinKeys
		innerJoinKeys = p.RightJoinKeys
	} else {
		innerJoinKeys = p.LeftJoinKeys
		outerJoinKeys = p.RightJoinKeys
	}
	ds, isDataSource := innerChild.(*DataSource)
	us, isUnionScan := innerChild.(*LogicalUnionScan)
	if !isDataSource && !isUnionScan {
		return nil
	}
	if isUnionScan {
		ds = us.Children()[0].(*DataSource)
	}
	var tblPath *accessPath
	for _, path := range ds.possibleAccessPaths {
		if path.isTablePath {
			tblPath = path
			break
		}
	}
	if pkCol := ds.getPKIsHandleCol(); pkCol != nil && tblPath != nil {
		keyOff2IdxOff := make([]int, len(innerJoinKeys))
		pkMatched := false
		for i, key := range innerJoinKeys {
			if !key.Equal(nil, pkCol) {
				keyOff2IdxOff[i] = -1
				continue
			}
			pkMatched = true
			keyOff2IdxOff[i] = 0
		}
		if pkMatched {
			innerPlan := p.constructInnerTableScan(ds, pkCol, outerJoinKeys, us)
			// Since the primary key means one value corresponding to exact one row, this will always be a no worse one
			// comparing to other index.
			return p.constructIndexJoin(prop, innerJoinKeys, outerJoinKeys, outerIdx, innerPlan, nil, keyOff2IdxOff, nil)
		}
	}
	var (
		bestIndexInfo  *model.IndexInfo
		rangesOfBest   []*ranger.Range
		maxUsedCols    int
		remainedOfBest []expression.Expression
		idxOff2KeyOff  []int
		comparesOfBest *ColWithCompareOps
	)
	for _, path := range ds.possibleAccessPaths {
		if path.isTablePath {
			continue
		}
		indexInfo := path.index
		ranges, tmpIdxOff2KeyOff, remained, compareFilters, err := p.analyzeLookUpFilters(indexInfo, ds, innerJoinKeys)
		if err != nil {
			log.Warnf("[planner]: error happened when build index join: %v", err)
			continue
		}
		// We choose the index by the number of used columns of the range, the much the better.
		// Notice that there may be the cases like `t1.a=t2.a and b > 2 and b < 1`. So ranges can be nil though the conditions are valid.
		// But obviously when the range is nil, we don't need index join.
		if len(ranges) > 0 && len(ranges[0].LowVal) > maxUsedCols {
			bestIndexInfo = indexInfo
			maxUsedCols = len(ranges[0].LowVal)
			rangesOfBest = ranges
			remainedOfBest = remained
			idxOff2KeyOff = tmpIdxOff2KeyOff
			comparesOfBest = compareFilters
		}
	}
	if bestIndexInfo != nil {
		keyOff2IdxOff := make([]int, len(innerJoinKeys))
		for i := range keyOff2IdxOff {
			keyOff2IdxOff[i] = -1
		}
		for idxOff, keyOff := range idxOff2KeyOff {
			if keyOff != -1 {
				keyOff2IdxOff[keyOff] = idxOff
			}
		}
		innerPlan := p.constructInnerIndexScan(ds, bestIndexInfo, remainedOfBest, outerJoinKeys, us)
		return p.constructIndexJoin(prop, innerJoinKeys, outerJoinKeys, outerIdx, innerPlan, rangesOfBest, keyOff2IdxOff, comparesOfBest)
	}
	return nil
}

// constructInnerTableScan is specially used to construct the inner plan for PhysicalIndexJoin.
func (p *LogicalJoin) constructInnerTableScan(ds *DataSource, pk *expression.Column, outerJoinKeys []*expression.Column, us *LogicalUnionScan) PhysicalPlan {
	ranges := ranger.FullIntRange(mysql.HasUnsignedFlag(pk.RetType.Flag))
	ts := PhysicalTableScan{
		Table:           ds.tableInfo,
		Columns:         ds.Columns,
		TableAsName:     ds.TableAsName,
		DBName:          ds.DBName,
		filterCondition: ds.pushedDownConds,
		Ranges:          ranges,
		rangeDecidedBy:  outerJoinKeys,
	}.Init(ds.ctx)
	ts.SetSchema(ds.schema)

	var rowCount float64
	pkHist, ok := ds.statisticTable.Columns[pk.ID]
	if ok && !ds.statisticTable.Pseudo {
		rowCount = pkHist.AvgCountPerValue(ds.statisticTable.Count)
	} else {
		rowCount = ds.statisticTable.PseudoAvgCountPerValue()
	}

	ts.stats = property.NewSimpleStats(rowCount)
	ts.stats.UsePseudoStats = ds.statisticTable.Pseudo

	copTask := &copTask{
		tablePlan:         ts,
		indexPlanFinished: true,
	}
	ts.addPushedDownSelection(copTask, ds.stats)
	t := finishCopTask(ds.ctx, copTask)
	reader := t.plan()
	return p.constructInnerUnionScan(us, reader)
}

func (p *LogicalJoin) constructInnerUnionScan(us *LogicalUnionScan, reader PhysicalPlan) PhysicalPlan {
	if us == nil {
		return reader
	}
	// Use `reader.stats` instead of `us.stats` because it should be more accurate. No need to specify
	// childrenReqProps now since we have got reader already.
	physicalUnionScan := PhysicalUnionScan{Conditions: us.conditions}.Init(us.ctx, reader.statsInfo(), nil)
	physicalUnionScan.SetChildren(reader)
	return physicalUnionScan
}

// constructInnerIndexScan is specially used to construct the inner plan for PhysicalIndexJoin.
func (p *LogicalJoin) constructInnerIndexScan(ds *DataSource, idx *model.IndexInfo, remainedConds []expression.Expression, outerJoinKeys []*expression.Column, us *LogicalUnionScan) PhysicalPlan {
	is := PhysicalIndexScan{
		Table:            ds.tableInfo,
		TableAsName:      ds.TableAsName,
		DBName:           ds.DBName,
		Columns:          ds.Columns,
		Index:            idx,
		dataSourceSchema: ds.schema,
		KeepOrder:        false,
		Ranges:           ranger.FullRange(),
		rangeDecidedBy:   outerJoinKeys,
	}.Init(ds.ctx)
	is.filterCondition = remainedConds

	var rowCount float64
	idxHist, ok := ds.statisticTable.Indices[idx.ID]
	if ok && !ds.statisticTable.Pseudo {
		rowCount = idxHist.AvgCountPerValue(ds.statisticTable.Count)
	} else {
		rowCount = ds.statisticTable.PseudoAvgCountPerValue()
	}
	is.stats = property.NewSimpleStats(rowCount)
	is.stats.UsePseudoStats = ds.statisticTable.Pseudo

	cop := &copTask{
		indexPlan: is,
	}
	if !isCoveringIndex(ds.schema.Columns, is.Index.Columns, is.Table.PKIsHandle) {
		// On this way, it's double read case.
		ts := PhysicalTableScan{Columns: ds.Columns, Table: is.Table}.Init(ds.ctx)
		ts.SetSchema(is.dataSourceSchema)
		cop.tablePlan = ts
	}

	is.initSchema(ds.id, idx, cop.tablePlan != nil)
	indexConds, tblConds := splitIndexFilterConditions(remainedConds, idx.Columns, ds.tableInfo)
	path := &accessPath{indexFilters: indexConds, tableFilters: tblConds, countAfterIndex: math.MaxFloat64}
	is.addPushedDownSelection(cop, ds, math.MaxFloat64, path)
	t := finishCopTask(ds.ctx, cop)
	reader := t.plan()
	return p.constructInnerUnionScan(us, reader)
}

var symmetricOp = map[string]string{
	ast.LT: ast.GT,
	ast.GE: ast.LE,
	ast.GT: ast.LT,
	ast.LE: ast.GE,
}

type ColWithCompareOps struct {
	targetCol         *expression.Column
	OpType            []string
	opArg             []expression.Expression
	tmpConstant       []*expression.Constant
	affectedColSchema *expression.Schema
	compareFuncs      []chunk.CompareFunc
}

func (cwc *ColWithCompareOps) appendNewExpr(opName string, arg expression.Expression, affectedCols []*expression.Column) {
	cwc.OpType = append(cwc.OpType, opName)
	cwc.opArg = append(cwc.opArg, arg)
	cwc.tmpConstant = append(cwc.tmpConstant, &expression.Constant{RetType: cwc.targetCol.RetType})
	for _, col := range affectedCols {
		if cwc.affectedColSchema.Contains(col) {
			continue
		}
		cwc.compareFuncs = append(cwc.compareFuncs, chunk.GetCompareFunc(col.RetType))
		cwc.affectedColSchema.Append(col)
	}
}

func (cwc *ColWithCompareOps) CompareRow(lhs, rhs chunk.Row) int {
	for i, col := range cwc.affectedColSchema.Columns {
		ret := cwc.compareFuncs[i](lhs, col.Index, rhs, col.Index)
		if ret != 0 {
			return ret
		}
	}
	return 0
}

func (cwc *ColWithCompareOps) BuildRangesByRow(ctx sessionctx.Context, row chunk.Row) ([]*ranger.Range, error) {
	exprs := make([]expression.Expression, len(cwc.OpType))
	for i, opType := range cwc.OpType {
		constantArg, err := cwc.opArg[i].Eval(row)
		if err != nil {
			return nil, err
		}
		cwc.tmpConstant[i].Value = constantArg
		newExpr, err := expression.NewFunction(ctx, opType, types.NewFieldType(mysql.TypeTiny), cwc.targetCol, cwc.tmpConstant[i])
		if err != nil {
			return nil, err
		}
		exprs = append(exprs, newExpr)
	}
	ranges, err := ranger.BuildColumnRange(exprs, ctx.GetSessionVars().StmtCtx, cwc.targetCol.RetType)
	if err != nil {
		return nil, err
	}
	return ranges, nil
}

func (cwc *ColWithCompareOps) resolveIndices(schema *expression.Schema) {
	for i := range cwc.opArg {
		cwc.opArg[i] = cwc.opArg[i].ResolveIndices(schema)
	}
}

func (p *LogicalJoin) analyzeLookUpFilters(indexInfo *model.IndexInfo, innerPlan *DataSource, innerJoinKeys []*expression.Column) ([]*ranger.Range, []int, []expression.Expression, *ColWithCompareOps, error) {
	idxCols, colLengths := expression.IndexInfo2Cols(innerPlan.schema.Columns, indexInfo)
	if len(idxCols) == 0 {
		return nil, nil, nil, nil, nil
	}
	tmpSchema := expression.NewSchema(innerJoinKeys...)
	idxOff2keyOff := make([]int, len(idxCols))
	possibleUsedKeys := make([]*expression.Column, 0, len(idxCols))
	notKeyIdxCols := make([]*expression.Column, 0, len(idxCols))
	notKeyIdxColsLen := make([]int, 0, len(idxCols))
	matchedKeyCnt := 0
	for i, idxCol := range idxCols {
		idxOff2keyOff[i] = tmpSchema.ColumnIndex(idxCol)
		if idxOff2keyOff[i] >= 0 {
			matchedKeyCnt++
			possibleUsedKeys = append(possibleUsedKeys, idxCol)
			continue
		}
		notKeyIdxCols = append(notKeyIdxCols, idxCol)
		notKeyIdxColsLen = append(notKeyIdxColsLen, colLengths[i])
	}
	if matchedKeyCnt <= 0 {
		return nil, nil, nil, nil, nil
	}
	keyMatchedLen := len(idxCols) - 1
	for ; keyMatchedLen > 0; keyMatchedLen-- {
		if idxOff2keyOff[keyMatchedLen] == -1 {
			continue
		}
	}
	remained := make([]expression.Expression, 0, len(innerPlan.pushedDownConds))
	rangeFilterCandidates := make([]expression.Expression, 0, len(innerPlan.pushedDownConds))
	for _, innerFilter := range innerPlan.pushedDownConds {
		affectedCols := expression.ExtractColumns(innerFilter)
		if expression.ColumnSliceIsIntersect(affectedCols, possibleUsedKeys) {
			remained = append(remained, innerFilter)
			continue
		}
		rangeFilterCandidates = append(rangeFilterCandidates, innerFilter)
	}
	notKeyEqAndIn, remainedEqAndIn, rangeFilterCandidates, _ := ranger.ExtractEqAndInCondition(p.ctx, rangeFilterCandidates, notKeyIdxCols, notKeyIdxColsLen)
	// We hope that the index cols appeared in the join keys can all be used to build range. If it cannot be satisfied,
	// we'll mark this index as cannot be used for index join.
	if len(notKeyEqAndIn) < keyMatchedLen-matchedKeyCnt {
		return nil, nil, nil, nil, nil
	}
	remained = append(remained, remainedEqAndIn...)
	nextColPos := matchedKeyCnt + len(notKeyEqAndIn)
	// If all cols have been considered, we can return the current result.
	if nextColPos == len(idxCols) {
		ranges, err := p.buildTemplateRange(idxOff2keyOff, matchedKeyCnt, notKeyEqAndIn, nil, false)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		return ranges, idxOff2keyOff, remained, nil, nil
	}
	nextCol := idxCols[nextColPos]
	nextColCmpFilterManager := &ColWithCompareOps{
		targetCol:         nextCol,
		affectedColSchema: expression.NewSchema(),
	}
loopCandidates:
	for _, filter := range rangeFilterCandidates {
		sf, ok := filter.(*expression.ScalarFunction)
		if !ok || !(sf.FuncName.L == ast.LE || sf.FuncName.L == ast.LT || sf.FuncName.L == ast.GE || sf.FuncName.L == ast.GT) {
			continue
		}
		if lCol, ok := sf.GetArgs()[0].(*expression.Column); ok && lCol.Equal(nil, nextCol) {
			affectedCols := expression.ExtractColumns(sf.GetArgs()[1])
			if len(affectedCols) == 0 {
				continue
			}
			for _, col := range affectedCols {
				if innerPlan.schema.Contains(col) {
					continue loopCandidates
				}
			}
			nextColCmpFilterManager.appendNewExpr(sf.FuncName.L, sf.GetArgs()[1], affectedCols)
		} else if rCol, ok := sf.GetArgs()[1].(*expression.Column); ok && rCol.Equal(nil, nextCol) {
			affectedCols := expression.ExtractColumns(sf.GetArgs()[0])
			if len(affectedCols) == 0 {
				continue
			}
			for _, col := range affectedCols {
				if innerPlan.schema.Contains(col) {
					continue loopCandidates
				}
			}
			nextColCmpFilterManager.appendNewExpr(symmetricOp[sf.FuncName.L], sf.GetArgs()[0], affectedCols)
		}
	}
	if len(nextColCmpFilterManager.OpType) == 0 {
		colAccesses, colRemained := ranger.DetachCondsForTableRange(p.ctx, rangeFilterCandidates, nextCol)
		remained = append(remained, colRemained...)
		if colLengths[nextColPos] != types.UnspecifiedLength {
			remained = append(remained, colAccesses...)
		}
		nextColRange, err := ranger.BuildColumnRange(colAccesses, p.ctx.GetSessionVars().StmtCtx, nextCol.RetType)
		ranges, err := p.buildTemplateRange(idxOff2keyOff, matchedKeyCnt, notKeyEqAndIn, nextColRange, false)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		return ranges, idxOff2keyOff, remained, nil, nil
	}
	ranges, err := p.buildTemplateRange(idxOff2keyOff, matchedKeyCnt, notKeyEqAndIn, nil, true)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	return ranges, idxOff2keyOff, remained, nextColCmpFilterManager, nil
}

func (p *LogicalJoin) buildTemplateRange(idxOff2KeyOff []int, matchedKeyCnt int, eqAndInFuncs []expression.Expression, nextColRange []*ranger.Range, haveExtraCol bool) (ranges []*ranger.Range, err error) {
	pointLength := matchedKeyCnt + len(eqAndInFuncs)
	if nextColRange != nil {
		for _, colRan := range nextColRange {
			// The range's exclude status is the same with last col's.
			ran := &ranger.Range{
				LowVal:      make([]types.Datum, pointLength, pointLength+1),
				HighVal:     make([]types.Datum, pointLength, pointLength+1),
				LowExclude:  colRan.LowExclude,
				HighExclude: colRan.HighExclude,
			}
			ran.LowVal = append(ran.LowVal, colRan.LowVal[0])
			ran.HighVal = append(ran.HighVal, colRan.HighVal[0])
			ranges = append(ranges, ran)
		}
	} else if haveExtraCol {
		ranges = append(ranges, &ranger.Range{
			LowVal:  make([]types.Datum, pointLength+1, pointLength+1),
			HighVal: make([]types.Datum, pointLength+1, pointLength+1),
		})
	} else {
		ranges = append(ranges, &ranger.Range{
			LowVal:  make([]types.Datum, pointLength, pointLength),
			HighVal: make([]types.Datum, pointLength, pointLength),
		})
	}
	emptyRow := chunk.Row{}
	for i, j := 0, 0; j < len(eqAndInFuncs); i++ {
		// This position is occupied by join key.
		if idxOff2KeyOff[i] != -1 {
			continue
		}
		sf := eqAndInFuncs[j].(*expression.ScalarFunction)
		// Deal with the first two args.
		if _, ok := sf.GetArgs()[0].(*expression.Column); ok {
			for _, ran := range ranges {
				ran.LowVal[i], err = sf.GetArgs()[1].Eval(emptyRow)
				if err != nil {
					return nil, err
				}
				ran.HighVal[i] = ran.LowVal[i]
			}
		} else {
			for _, ran := range ranges {
				ran.LowVal[i], err = sf.GetArgs()[0].Eval(emptyRow)
				if err != nil {
					return nil, err
				}
				ran.HighVal[i] = ran.LowVal[i]
			}
		}
		// If the length of in function's constant list is more than one, we will expand ranges.
		curRangeLen := len(ranges)
		for argIdx := 2; argIdx < len(sf.GetArgs()); argIdx++ {
			newRanges := make([]*ranger.Range, 0, curRangeLen)
			for oldRangeIdx := 0; oldRangeIdx < curRangeLen; oldRangeIdx++ {
				newRange := ranges[oldRangeIdx].Clone()
				newRange.LowVal[i], err = sf.GetArgs()[argIdx].Eval(emptyRow)
				if err != nil {
					return nil, err
				}
				newRanges = append(newRanges, newRange)
			}
			ranges = append(ranges, newRanges...)
		}
		j++
	}
	return ranges, nil
}

// tryToGetIndexJoin will get index join by hints. If we can generate a valid index join by hint, the second return value
// will be true, which means we force to choose this index join. Otherwise we will select a join algorithm with min-cost.
func (p *LogicalJoin) tryToGetIndexJoin(prop *property.PhysicalProperty) ([]PhysicalPlan, bool) {
	if len(p.EqualConditions) == 0 {
		return nil, false
	}
	plans := make([]PhysicalPlan, 0, 2)
	rightOuter := (p.preferJoinType & preferLeftAsIndexInner) > 0
	leftOuter := (p.preferJoinType & preferRightAsIndexInner) > 0
	switch p.JoinType {
	case SemiJoin, AntiSemiJoin, LeftOuterSemiJoin, AntiLeftOuterSemiJoin, LeftOuterJoin:
		join := p.getIndexJoinByOuterIdx(prop, 0)
		if join != nil {
			// If the plan is not nil and matches the hint, return it directly.
			if leftOuter {
				return join, true
			}
			plans = append(plans, join...)
		}
	case RightOuterJoin:
		join := p.getIndexJoinByOuterIdx(prop, 1)
		if join != nil {
			// If the plan is not nil and matches the hint, return it directly.
			if rightOuter {
				return join, true
			}
			plans = append(plans, join...)
		}
	case InnerJoin:
		lhsCardinality := p.Children()[0].statsInfo().Count()
		rhsCardinality := p.Children()[1].statsInfo().Count()

		leftJoins := p.getIndexJoinByOuterIdx(prop, 0)
		if leftOuter && leftJoins != nil {
			return leftJoins, true
		}

		rightJoins := p.getIndexJoinByOuterIdx(prop, 1)
		if rightOuter && rightJoins != nil {
			return rightJoins, true
		}

		if leftJoins != nil && lhsCardinality < rhsCardinality {
			return leftJoins, leftOuter
		}

		if rightJoins != nil && rhsCardinality < lhsCardinality {
			return rightJoins, rightOuter
		}

		plans = append(plans, leftJoins...)
		plans = append(plans, rightJoins...)
	}
	return plans, false
}

// LogicalJoin can generates hash join, index join and sort merge join.
// Firstly we check the hint, if hint is figured by user, we force to choose the corresponding physical plan.
// If the hint is not matched, it will get other candidates.
// If the hint is not figured, we will pick all candidates.
func (p *LogicalJoin) exhaustPhysicalPlans(prop *property.PhysicalProperty) []PhysicalPlan {
	mergeJoins := p.getMergeJoin(prop)
	if (p.preferJoinType & preferMergeJoin) > 0 {
		return mergeJoins
	}
	joins := make([]PhysicalPlan, 0, 5)
	joins = append(joins, mergeJoins...)

	indexJoins, forced := p.tryToGetIndexJoin(prop)
	if forced {
		return indexJoins
	}
	joins = append(joins, indexJoins...)

	hashJoins := p.getHashJoins(prop)
	if (p.preferJoinType & preferHashJoin) > 0 {
		return hashJoins
	}
	joins = append(joins, hashJoins...)
	return joins
}

// tryToGetChildProp will check if this sort property can be pushed or not.
// When a sort column will be replaced by scalar function, we refuse it.
// When a sort column will be replaced by a constant, we just remove it.
func (p *LogicalProjection) tryToGetChildProp(prop *property.PhysicalProperty) (*property.PhysicalProperty, bool) {
	newProp := &property.PhysicalProperty{TaskTp: property.RootTaskType, ExpectedCnt: prop.ExpectedCnt}
	newCols := make([]*expression.Column, 0, len(prop.Cols))
	for _, col := range prop.Cols {
		idx := p.schema.ColumnIndex(col)
		switch expr := p.Exprs[idx].(type) {
		case *expression.Column:
			newCols = append(newCols, expr)
		case *expression.ScalarFunction:
			return nil, false
		}
	}
	newProp.Cols = newCols
	newProp.Desc = prop.Desc
	return newProp, true
}

func (p *LogicalProjection) exhaustPhysicalPlans(prop *property.PhysicalProperty) []PhysicalPlan {
	newProp, ok := p.tryToGetChildProp(prop)
	if !ok {
		return nil
	}
	proj := PhysicalProjection{
		Exprs:                p.Exprs,
		CalculateNoDelay:     p.calculateNoDelay,
		AvoidColumnEvaluator: p.avoidColumnEvaluator,
	}.Init(p.ctx, p.stats.ScaleByExpectCnt(prop.ExpectedCnt), newProp)
	proj.SetSchema(p.schema)
	return []PhysicalPlan{proj}
}

func (lt *LogicalTopN) getPhysTopN() []PhysicalPlan {
	ret := make([]PhysicalPlan, 0, 3)
	for _, tp := range wholeTaskTypes {
		resultProp := &property.PhysicalProperty{TaskTp: tp, ExpectedCnt: math.MaxFloat64}
		topN := PhysicalTopN{
			ByItems: lt.ByItems,
			Count:   lt.Count,
			Offset:  lt.Offset,
		}.Init(lt.ctx, lt.stats, resultProp)
		ret = append(ret, topN)
	}
	return ret
}

func (lt *LogicalTopN) getPhysLimits() []PhysicalPlan {
	prop, canPass := getPropByOrderByItems(lt.ByItems)
	if !canPass {
		return nil
	}
	ret := make([]PhysicalPlan, 0, 3)
	for _, tp := range wholeTaskTypes {
		resultProp := &property.PhysicalProperty{TaskTp: tp, ExpectedCnt: float64(lt.Count + lt.Offset), Cols: prop.Cols, Desc: prop.Desc}
		limit := PhysicalLimit{
			Count:  lt.Count,
			Offset: lt.Offset,
		}.Init(lt.ctx, lt.stats, resultProp)
		ret = append(ret, limit)
	}
	return ret
}

// Check if this prop's columns can match by items totally.
func matchItems(p *property.PhysicalProperty, items []*ByItems) bool {
	if len(items) < len(p.Cols) {
		return false
	}
	for i, col := range p.Cols {
		sortItem := items[i]
		if sortItem.Desc != p.Desc || !sortItem.Expr.Equal(nil, col) {
			return false
		}
	}
	return true
}

func (lt *LogicalTopN) exhaustPhysicalPlans(prop *property.PhysicalProperty) []PhysicalPlan {
	if matchItems(prop, lt.ByItems) {
		return append(lt.getPhysTopN(), lt.getPhysLimits()...)
	}
	return nil
}

func (la *LogicalApply) exhaustPhysicalPlans(prop *property.PhysicalProperty) []PhysicalPlan {
	if !prop.AllColsFromSchema(la.children[0].Schema()) { // for convenient, we don't pass through any prop
		return nil
	}
	apply := PhysicalApply{
		PhysicalJoin:  la.getHashJoin(prop, 1),
		OuterSchema:   la.corCols,
		rightChOffset: la.children[0].Schema().Len(),
	}.Init(la.ctx,
		la.stats.ScaleByExpectCnt(prop.ExpectedCnt),
		&property.PhysicalProperty{ExpectedCnt: math.MaxFloat64, Cols: prop.Cols, Desc: prop.Desc},
		&property.PhysicalProperty{ExpectedCnt: math.MaxFloat64})
	apply.SetSchema(la.schema)
	return []PhysicalPlan{apply}
}

// exhaustPhysicalPlans is only for implementing interface. DataSource and Dual generate task in `findBestTask` directly.
func (p *baseLogicalPlan) exhaustPhysicalPlans(_ *property.PhysicalProperty) []PhysicalPlan {
	panic("baseLogicalPlan.exhaustPhysicalPlans() should never be called.")
}

func (la *LogicalAggregation) getStreamAggs(prop *property.PhysicalProperty) []PhysicalPlan {
	if len(la.possibleProperties) == 0 {
		return nil
	}
	for _, aggFunc := range la.AggFuncs {
		if aggFunc.Mode == aggregation.FinalMode {
			return nil
		}
	}
	// group by a + b is not interested in any order.
	if len(la.groupByCols) != len(la.GroupByItems) {
		return nil
	}

	streamAggs := make([]PhysicalPlan, 0, len(la.possibleProperties)*(len(wholeTaskTypes)-1))
	childProp := &property.PhysicalProperty{
		Desc:        prop.Desc,
		ExpectedCnt: math.Max(prop.ExpectedCnt*la.inputCount/la.stats.RowCount, prop.ExpectedCnt),
	}

	for _, possibleChildProperty := range la.possibleProperties {
		sortColOffsets := getMaxSortPrefix(possibleChildProperty, la.groupByCols)
		if len(sortColOffsets) != len(la.groupByCols) {
			continue
		}

		childProp.Cols = possibleChildProperty[:len(sortColOffsets)]
		if !prop.IsPrefix(childProp) {
			continue
		}

		// The table read of "CopDoubleReadTaskType" can't promises the sort
		// property that the stream aggregation required, no need to consider.
		for _, taskTp := range []property.TaskType{property.CopSingleReadTaskType, property.RootTaskType} {
			copiedChildProperty := new(property.PhysicalProperty)
			*copiedChildProperty = *childProp // It's ok to not deep copy the "cols" field.
			copiedChildProperty.TaskTp = taskTp

			agg := basePhysicalAgg{
				GroupByItems: la.GroupByItems,
				AggFuncs:     la.AggFuncs,
			}.initForStream(la.ctx, la.stats.ScaleByExpectCnt(prop.ExpectedCnt), copiedChildProperty)
			agg.SetSchema(la.schema.Clone())
			streamAggs = append(streamAggs, agg)
		}
	}
	return streamAggs
}

func (la *LogicalAggregation) getHashAggs(prop *property.PhysicalProperty) []PhysicalPlan {
	if !prop.IsEmpty() {
		return nil
	}
	hashAggs := make([]PhysicalPlan, 0, len(wholeTaskTypes))
	for _, taskTp := range wholeTaskTypes {
		agg := basePhysicalAgg{
			GroupByItems: la.GroupByItems,
			AggFuncs:     la.AggFuncs,
		}.initForHash(la.ctx, la.stats.ScaleByExpectCnt(prop.ExpectedCnt), &property.PhysicalProperty{ExpectedCnt: math.MaxFloat64, TaskTp: taskTp})
		agg.SetSchema(la.schema.Clone())
		hashAggs = append(hashAggs, agg)
	}
	return hashAggs
}

func (la *LogicalAggregation) exhaustPhysicalPlans(prop *property.PhysicalProperty) []PhysicalPlan {
	aggs := make([]PhysicalPlan, 0, len(la.possibleProperties)+1)
	aggs = append(aggs, la.getHashAggs(prop)...)
	aggs = append(aggs, la.getStreamAggs(prop)...)
	return aggs
}

func (p *LogicalSelection) exhaustPhysicalPlans(prop *property.PhysicalProperty) []PhysicalPlan {
	sel := PhysicalSelection{
		Conditions: p.Conditions,
	}.Init(p.ctx, p.stats.ScaleByExpectCnt(prop.ExpectedCnt), prop)
	return []PhysicalPlan{sel}
}

func (p *LogicalLimit) exhaustPhysicalPlans(prop *property.PhysicalProperty) []PhysicalPlan {
	if !prop.IsEmpty() {
		return nil
	}
	ret := make([]PhysicalPlan, 0, len(wholeTaskTypes))
	for _, tp := range wholeTaskTypes {
		resultProp := &property.PhysicalProperty{TaskTp: tp, ExpectedCnt: float64(p.Count + p.Offset)}
		limit := PhysicalLimit{
			Offset: p.Offset,
			Count:  p.Count,
		}.Init(p.ctx, p.stats, resultProp)
		ret = append(ret, limit)
	}
	return ret
}

func (p *LogicalLock) exhaustPhysicalPlans(prop *property.PhysicalProperty) []PhysicalPlan {
	lock := PhysicalLock{
		Lock: p.Lock,
	}.Init(p.ctx, p.stats.ScaleByExpectCnt(prop.ExpectedCnt), prop)
	return []PhysicalPlan{lock}
}

func (p *LogicalUnionAll) exhaustPhysicalPlans(prop *property.PhysicalProperty) []PhysicalPlan {
	// TODO: UnionAll can not pass any order, but we can change it to sort merge to keep order.
	if !prop.IsEmpty() {
		return nil
	}
	chReqProps := make([]*property.PhysicalProperty, 0, len(p.children))
	for range p.children {
		chReqProps = append(chReqProps, &property.PhysicalProperty{ExpectedCnt: prop.ExpectedCnt})
	}
	ua := PhysicalUnionAll{}.Init(p.ctx, p.stats.ScaleByExpectCnt(prop.ExpectedCnt), chReqProps...)
	ua.SetSchema(p.Schema())
	return []PhysicalPlan{ua}
}

func (ls *LogicalSort) getPhysicalSort(prop *property.PhysicalProperty) *PhysicalSort {
	ps := PhysicalSort{ByItems: ls.ByItems}.Init(ls.ctx, ls.stats.ScaleByExpectCnt(prop.ExpectedCnt), &property.PhysicalProperty{ExpectedCnt: math.MaxFloat64})
	return ps
}

func (ls *LogicalSort) getNominalSort(reqProp *property.PhysicalProperty) *NominalSort {
	prop, canPass := getPropByOrderByItems(ls.ByItems)
	if !canPass {
		return nil
	}
	prop.ExpectedCnt = reqProp.ExpectedCnt
	ps := NominalSort{}.Init(ls.ctx, prop)
	return ps
}

func (ls *LogicalSort) exhaustPhysicalPlans(prop *property.PhysicalProperty) []PhysicalPlan {
	if matchItems(prop, ls.ByItems) {
		ret := make([]PhysicalPlan, 0, 2)
		ret = append(ret, ls.getPhysicalSort(prop))
		ns := ls.getNominalSort(prop)
		if ns != nil {
			ret = append(ret, ns)
		}
		return ret
	}
	return nil
}

func (p *LogicalMaxOneRow) exhaustPhysicalPlans(prop *property.PhysicalProperty) []PhysicalPlan {
	if !prop.IsEmpty() {
		return nil
	}
	mor := PhysicalMaxOneRow{}.Init(p.ctx, p.stats, &property.PhysicalProperty{ExpectedCnt: 2})
	return []PhysicalPlan{mor}
}

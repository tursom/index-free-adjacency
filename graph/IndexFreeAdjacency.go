package graph

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"unsafe"

	"github.com/tursom/index-free-adjacency/wal"
)

const (
	pageSize = 16
)

type (
	Graph struct {
		nodes         []slice[Node]
		relations     []slice[Relation]
		properties    []slice[property]
		freeNode      Index
		freeRelation  *Relation
		freeProperty  *property
		usedNodes     BitSet
		usedRelations BitSet
		nodeCount     int
		relationCount int
	}

	Index = int

	Node struct {
		// reused to next free node
		index         Index
		label         string
		firstProperty *property
		firstRelation *Relation
		g             *Graph
	}

	Relation struct {
		index    Index
		g        *Graph
		from, to *Node
		// s, start = from
		// e, end = to
		// sn as next when it's free
		sp, ep, sn, en *Relation
		firstProperty  *property
	}

	property struct {
		next  *property
		key   string
		value any
	}

	nodeIterator struct {
		node *Node
	}

	relationIterator struct {
		node     *Node
		relation *Relation
	}

	// slice 切片的另类实现，主要为了方便扩展数组的大小
	// 每个 slice[T] 都是一片连续内存，在复制或者移动 slice[T] 本身时能保证指向的内存地址不变
	// 指向的地址不变，使得在扩展时所有对本切片元素的引用都不需要变动。
	slice[T any] struct {
		arr *[pageSize]T
		len uint32
	}
)

var (
	ErrDeletedNode = fmt.Errorf("node alrady deleted")
	ErrRelation    = fmt.Errorf("node have relations")

	ErrDeletedRelation = fmt.Errorf("relation alrady deleted")
)

func (g *Graph) Nodes() Iterator[*Node] {
	firstUsed := g.usedNodes.NextUp(-1)
	var node *Node = nil
	if firstUsed >= 0 {
		node = g.getNodeUnsafe(firstUsed)
	}
	return &nodeIterator{node}
}

func (g *Graph) NodeCount() int {
	return g.nodeCount
}

func (g *Graph) GetNode(index Index) *Node {
	if index >= len(g.nodes)*pageSize || !g.usedNodes.Get(index) {
		return nil
	}

	return g.getNodeUnsafe(index)
}

func (g *Graph) RelationCount() int {
	return g.relationCount
}

// getNodeUnsafe 通过索引获取节点的引用
func (g *Graph) getNodeUnsafe(index Index) *Node {
	return &g.nodes[index/pageSize].arr[index%pageSize]
}

func (g *Graph) GetRelation(index Index) *Relation {
	if index >= len(g.relations)*pageSize || !g.usedRelations.Get(index) {
		return nil
	}

	return g.getRelationUnsafe(index)
}

// getNodeUnsafe 通过索引获取关系的引用
func (g *Graph) getRelationUnsafe(index Index) *Relation {
	return &g.relations[index/pageSize].arr[index%pageSize]
}

func (g *Graph) AddNode(label string) (index Index) {
	var log wal.WAL
	defer func() {
		log.RollBackWhenPanic(recover())
	}()

	if g.freeNode != 0 {
		freeNodeIndex := g.freeNode - 1
		n := g.getNodeUnsafe(freeNodeIndex)
		wal.SetValue(&log, &g.freeNode, n.index)
		wal.SetValue(&log, &n.index, freeNodeIndex)
		index = freeNodeIndex
	} else {
		lastNodes := lastPage(&g.nodes)

		index = (len(g.nodes)-1)*pageSize + int(lastNodes.len)
		wal.SetValue(&log, &lastNodes.arr[lastNodes.len], Node{
			index: index,
			g:     g,
		})
		wal.IncUInt32(&log, &lastNodes.len)
	}

	n := g.getNodeUnsafe(index)
	wal.SetValue(&log, &n.label, label)

	if g.usedNodes.BitLength() < len(g.nodes)*pageSize {
		g.usedNodes = append(g.usedNodes, 0)
	}
	g.usedNodes.SetBitWAL(&log, index, true)

	wal.IncInt(&log, &g.nodeCount)

	return index
}

func (g *Graph) AddRelation(from, to Index) Index {
	var log wal.WAL
	defer func() {
		log.RollBackWhenPanic(recover())
	}()

	f := g.GetNode(from)
	if f == nil {
		return -1
	}
	t := g.GetNode(to)
	if t == nil {
		return -1
	}

	var rla *Relation
	if g.freeRelation != nil {
		// reuse free Relation slot
		rla = g.freeRelation
		wal.SetValue(&log, &g.freeRelation, rla.sn)

		wal.SetValue(&log, &rla.from, f)
		wal.SetValue(&log, &rla.to, t)
		wal.SetValue(&log, &rla.sp, nil)
		wal.SetValue(&log, &rla.ep, nil)
	} else {
		lastRelations := lastPage(&g.relations)
		wal.SetValue(&log, &lastRelations.arr[lastRelations.len], Relation{
			index: (len(g.relations)-1)*pageSize + int(lastRelations.len),
			from:  f,
			to:    t,
		})

		rla = &lastRelations.arr[lastRelations.len]
		wal.SetValue(&log, &rla.g, g)

		wal.IncUInt32(&log, &lastRelations.len)
	}

	wal.SetValue(&log, &rla.sn, f.firstRelation)
	if f.firstRelation != nil {
		wal.SetValue(&log, &f.firstRelation.sp, rla)
	}
	wal.SetValue(&log, &f.firstRelation, rla)

	wal.SetValue(&log, &rla.en, t.firstRelation)
	if t.firstRelation != nil {
		wal.SetValue(&log, &t.firstRelation.ep, rla)
	}
	wal.SetValue(&log, &t.firstRelation, rla)

	if g.usedRelations.BitLength() < len(g.relations)*pageSize {
		g.usedRelations = append(g.usedRelations, 0)
	}
	g.usedRelations.SetBitWAL(&log, rla.index, true)

	wal.IncInt(&log, &g.relationCount)

	return rla.index
}

func (g *Graph) DeleteNode(node Index) error {
	if node < 0 || !g.usedNodes.Get(node) {
		return ErrDeletedNode
	}

	n := g.getNodeUnsafe(node)
	if n.firstRelation != nil {
		return ErrRelation
	}

	var log wal.WAL
	defer func() {
		log.RollBackWhenPanic(recover())
	}()

	log.AddRollBack(func() {
		g.usedNodes.SetBitWAL(&log, node, true)
	})
	g.usedNodes.SetBitWAL(&log, node, false)

	// 节点有属性，一次性回收所有属性
	if n.firstProperty != nil {
		lastPpt := n.firstProperty
		for lastPpt.next != nil {
			lastPpt = lastPpt.next
		}

		wal.SetValue(&log, &lastPpt.next, g.freeProperty)
		wal.SetValue(&log, &g.freeProperty, n.firstProperty)
	}

	index := n.index
	wal.SetValue(&log, &n.index, g.freeNode)
	wal.SetValue(&log, &g.freeNode, index+1)

	g.nodeCount--

	return nil
}

func (g *Graph) DeleteRelation(relation Index) error {
	if relation < 0 || !g.usedRelations.Get(relation) {
		return ErrDeletedRelation
	}

	var log wal.WAL
	defer func() {
		log.RollBackWhenPanic(recover())
	}()

	log.AddRollBack(func() {
		g.usedRelations.SetBitWAL(&log, relation, true)
	})
	g.usedRelations.SetBitWAL(&log, relation, false)

	r := g.getRelationUnsafe(relation)

	if r.sp == nil {
		wal.SetValue(&log, &r.from.firstRelation, r.sn)
	} else if r.sp.from == r.from {
		wal.SetValue(&log, &r.sp.sn, r.sn)
	} else {
		wal.SetValue(&log, &r.sp.en, r.sn)
	}

	if r.ep == nil {
		wal.SetValue(&log, &r.to.firstRelation, r.en)
	} else if r.ep.to == r.to {
		wal.SetValue(&log, &r.ep.en, r.en)
	} else {
		wal.SetValue(&log, &r.ep.sn, r.en)
	}

	if r.sn == nil { // safe check, do nothing
	} else if r.sn.from == r.from {
		wal.SetValue(&log, &r.sn.sp, r.sp)
	} else {
		wal.SetValue(&log, &r.sn.ep, r.sp)
	}

	if r.en == nil { // safe check, do nothing
	} else if r.en.to == r.to {
		wal.SetValue(&log, &r.en.ep, r.ep)
	} else {
		wal.SetValue(&log, &r.en.sp, r.ep)
	}

	wal.SetValue(&log, &r.sn, g.freeRelation)
	wal.SetValue(&log, &g.freeRelation, r)

	wal.DecInt(&log, &g.relationCount)

	return nil
}

func (g *Graph) CheckNodes() Index {
	for i := 0; i < g.nodeCount; i++ {
		node := g.getNodeUnsafe(i)
		if g.usedNodes.Get(i) {
			if node.index != i {
				return i
			}
		} else {
			if i == node.index-1 {
				return i
			}
		}
	}
	return -1
}

func (g *Graph) CheckRelations() Index {
	for relation := g.freeRelation; relation != nil; relation = relation.sn {
		if g.usedRelations.Get(relation.index) {
			return relation.index
		}
	}

	//TODO

	return -1
}

func (n *Node) String() string {
	var sb strings.Builder
	sb.WriteString("Node{label:\"")
	sb.WriteString(n.label)
	sb.WriteString("\", properties: ")
	bytes, _ := json.Marshal(n.GetProperties())
	sb.WriteString(string(bytes))
	sb.WriteString("}")
	return sb.String()
}

func (n *Node) Graph() *Graph {
	return n.g
}

func (n *Node) ID() int {
	return n.index
}

func (n *Node) Lable() string {
	return n.label
}

func (n *Node) GetProperties() map[string]any {
	return n.firstProperty.toMap()
}

func (n *Node) SetProperty(key string, value any) {
	setProperty(n.g, &n.firstProperty, key, value)
}

func (n *Node) DelProperty(key string) bool {
	return delProperty(n.g, &n.firstProperty, key)
}

func (n *Node) Relations() Iterator[*Relation] {
	return &relationIterator{n, n.firstRelation}
}

func (r *Relation) Index() Index {
	return r.index
}

func (r *Relation) Graph() *Graph {
	return r.g
}

func (r *Relation) From() *Node {
	return r.from
}

func (r *Relation) To() *Node {
	return r.to
}

func (r *Relation) Sp() *Relation {
	return r.sp
}

func (r *Relation) Ep() *Relation {
	return r.ep
}

func (r *Relation) Sn() *Relation {
	return r.sn
}

func (r *Relation) En() *Relation {
	return r.en
}

func (r *Relation) GetProperties() map[string]any {
	return r.firstProperty.toMap()
}

func (r *Relation) SetProperty(key string, value any) {
	setProperty(r.g, &r.firstProperty, key, value)
}

func (r *Relation) DelProperty(key string) bool {
	return delProperty(r.g, &r.firstProperty, key)
}

func (r *Relation) String() string {
	var sb strings.Builder
	sb.WriteString("Relation(")
	sb.WriteString(fmt.Sprintf("%d-->%d", r.from.ID(), r.to.ID()))
	bytes, _ := json.Marshal(r.GetProperties())
	sb.Write(bytes)
	sb.WriteString(")")
	return sb.String()
}

func (p *property) toMap() map[string]any {
	m := make(map[string]any)
	for p != nil {
		m[p.key] = p.value
		//goland:noinspection GoAssignmentToReceiver
		p = p.next
	}
	return m
}

func setProperty(g *Graph, p **property, key string, value any) {
	var log wal.WAL
	defer func() {
		log.RollBackWhenPanic(recover())
	}()

	ppt := *p
	for ppt != nil {
		if ppt.key == key {
			wal.SetValue(&log, &ppt.value, value)
			return
		}
		ppt = ppt.next
	}

	if g.freeProperty != nil {
		ppt = g.freeProperty
		wal.SetValue(&log, &g.freeProperty, ppt.next)
	} else {
		propertiesPage := lastPage(&g.properties)
		ppt = &propertiesPage.arr[propertiesPage.len]
		wal.IncUInt32(&log, &propertiesPage.len)
	}

	wal.SetValue(&log, &ppt.next, *p)
	wal.SetValue(&log, p, ppt)

	wal.SetValue(&log, &ppt.key, key)
	wal.SetValue(&log, &ppt.value, value)
}

func delProperty(g *Graph, p **property, key string) bool {
	var log wal.WAL
	defer func() {
		log.RollBackWhenPanic(recover())
	}()

	prev := p
	ppt := *prev
	for ppt != nil {
		if ppt.key == key {
			wal.SetValue(&log, prev, ppt.next)

			wal.SetValue(&log, &ppt.next, g.freeProperty)
			wal.SetValue(&log, &g.freeProperty, ppt)

			return true
		}
		prev = &ppt.next
		ppt = *prev
	}
	return false
}

func (n *nodeIterator) HasNext() bool {
	return n.node != nil
}

func (n *nodeIterator) Next() *Node {
	node := n.node
	next := n.node.g.usedNodes.NextUp(n.node.index)
	if next < 0 {
		n.node = nil
	} else {
		n.node = n.node.g.getNodeUnsafe(next)
	}
	return node
}

func (r *relationIterator) HasNext() bool {
	return r.relation != nil
}

func (r *relationIterator) Next() *Relation {
	relation := r.relation
	if r.relation.from == r.node {
		r.relation = relation.sn
	} else {
		r.relation = relation.en
	}
	return relation
}

func indexOf[T any](s []T, v *T) int {
	begin := *(*uintptr)(unsafe.Pointer(&s))
	addr := uintptr(unsafe.Pointer(v))
	return int((addr - begin) / reflect.TypeOf(*v).Size())
}

func lastPage[T any](s *[]slice[T]) *slice[T] {
	if len(*s) == 0 {
		*s = append(*s, slice[T]{arr: new([pageSize]T)})
		return &(*s)[0]
	}
	if (*s)[len(*s)-1].len == pageSize {
		*s = append(*s, slice[T]{arr: new([pageSize]T)})
	}
	return &(*s)[len(*s)-1]
}

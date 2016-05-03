/*
 * Copyright 2015 DGraph Labs, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 * 		http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package query

import (
	"container/heap"
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/dgraph-io/dgraph/gql"
	"github.com/dgraph-io/dgraph/posting"
	"github.com/dgraph-io/dgraph/query/pb"
	"github.com/dgraph-io/dgraph/task"
	"github.com/dgraph-io/dgraph/worker"
	"github.com/dgraph-io/dgraph/x"
	"github.com/google/flatbuffers/go"
)

/*
 * QUERY:
 * Let's take this query from GraphQL as example:
 * {
 *   me {
 *     id
 *     firstName
 *     lastName
 *     birthday {
 *       month
 *       day
 *     }
 *     friends {
 *       name
 *     }
 *   }
 * }
 *
 * REPRESENTATION:
 * This would be represented in SubGraph format internally, as such:
 * SubGraph [result uid = me]
 *    |
 *  Children
 *    |
 *    --> SubGraph [Attr = "xid"]
 *    --> SubGraph [Attr = "firstName"]
 *    --> SubGraph [Attr = "lastName"]
 *    --> SubGraph [Attr = "birthday"]
 *           |
 *         Children
 *           |
 *           --> SubGraph [Attr = "month"]
 *           --> SubGraph [Attr = "day"]
 *    --> SubGraph [Attr = "friends"]
 *           |
 *         Children
 *           |
 *           --> SubGraph [Attr = "name"]
 *
 * ALGORITHM:
 * This is a rough and simple algorithm of how to process this SubGraph query
 * and populate the results:
 *
 * For a given entity, a new SubGraph can be started off with NewGraph(id).
 * Given a SubGraph, is the Query field empty? [Step a]
 *   - If no, run (or send it to server serving the attribute) query
 *     and populate result.
 * Iterate over children and copy Result Uids to child Query Uids.
 *     Set Attr. Then for each child, use goroutine to run Step:a.
 * Wait for goroutines to finish.
 * Return errors, if any.
 */

var glog = x.Log("query")

type Latency struct {
	Start      time.Time     `json:"-"`
	Parsing    time.Duration `json:"query_parsing"`
	Processing time.Duration `json:"processing"`
	Json       time.Duration `json:"json_conversion"`
}

func (l *Latency) ToMap() map[string]string {
	m := make(map[string]string)
	j := time.Since(l.Start) - l.Processing - l.Parsing
	m["parsing"] = l.Parsing.String()
	m["processing"] = l.Processing.String()
	m["json"] = j.String()
	m["total"] = time.Since(l.Start).String()
	return m
}

// SubGraph is the way to represent data internally. It contains both the
// query and the response. Once generated, this can then be encoded to other
// client convenient formats, like GraphQL / JSON.
type SubGraph struct {
	Attr     string
	Count    int
	Offset   int
	Children []*SubGraph

	query  []byte
	result []byte
}

func mergeInterfaces(i1 interface{}, i2 interface{}) interface{} {
	switch i1.(type) {
	case map[string]interface{}:
		m1 := i1.(map[string]interface{})
		if m2, ok := i2.(map[string]interface{}); ok {
			for k1, v1 := range m1 {
				m2[k1] = v1
			}
			return m2
		}
		break
	}
	glog.Debugf("Got type: %v %v", reflect.TypeOf(i1), reflect.TypeOf(i2))
	glog.Debugf("Got values: %v %v", i1, i2)

	return []interface{}{i1, i2}
}

func postTraverse(g *SubGraph) (result map[uint64]interface{}, rerr error) {
	if len(g.query) == 0 {
		return result, nil
	}

	result = make(map[uint64]interface{})
	// Get results from all children first.
	cResult := make(map[uint64]interface{})

	for _, child := range g.Children {
		m, err := postTraverse(child)
		if err != nil {
			x.Err(glog, err).Error("Error while traversal")
			return result, err
		}
		// Merge results from all children, one by one.
		for k, v := range m {
			if val, present := cResult[k]; !present {
				cResult[k] = v
			} else {
				cResult[k] = mergeInterfaces(val, v)
			}
		}
	}

	// Now read the query and results at current node.
	uo := flatbuffers.GetUOffsetT(g.query)
	q := new(task.Query)
	q.Init(g.query, uo)

	ro := flatbuffers.GetUOffsetT(g.result)
	r := new(task.Result)
	r.Init(g.result, ro)

	if q.UidsLength() != r.UidmatrixLength() {
		glog.Fatalf("Result uidmatrixlength: %v. Query uidslength: %v",
			r.UidmatrixLength(), q.UidsLength())
	}
	if q.UidsLength() != r.ValuesLength() {
		glog.Fatalf("Result valuelength: %v. Query uidslength: %v",
			r.ValuesLength(), q.UidsLength())
	}

	var ul task.UidList
	for i := 0; i < r.UidmatrixLength(); i++ {
		if ok := r.Uidmatrix(&ul, i); !ok {
			return result, fmt.Errorf("While parsing UidList")
		}
		l := make([]interface{}, ul.UidsLength())
		for j := 0; j < ul.UidsLength(); j++ {
			uid := ul.Uids(j)
			m := make(map[string]interface{})
			m["_uid_"] = fmt.Sprintf("%#x", uid)
			if ival, present := cResult[uid]; !present {
				l[j] = m
			} else {
				l[j] = mergeInterfaces(m, ival)
			}
		}
		if len(l) > 0 {
			m := make(map[string]interface{})
			m[g.Attr] = l
			result[q.Uids(i)] = m
		}
	}

	var tv task.Value
	for i := 0; i < r.ValuesLength(); i++ {
		if ok := r.Values(&tv, i); !ok {
			return result, fmt.Errorf("While parsing value")
		}
		var ival interface{}
		if err := posting.ParseValue(&ival, tv.ValBytes()); err != nil {
			return result, err
		}
		if ival == nil {
			continue
		}

		if pval, present := result[q.Uids(i)]; present {
			glog.WithField("prev", pval).
				WithField("_uid_", q.Uids(i)).
				WithField("new", ival).
				Fatal("Previous value detected.")
		}
		m := make(map[string]interface{})
		m["_uid_"] = fmt.Sprintf("%#x", q.Uids(i))
		glog.WithFields(logrus.Fields{
			"_uid_": q.Uids(i),
			"val":   ival,
		}).Debug("Got value")
		m[g.Attr] = ival
		result[q.Uids(i)] = m
	}
	return result, nil
}

func (g *SubGraph) ToJson(l *Latency) (js []byte, rerr error) {
	r, err := postTraverse(g)
	if err != nil {
		x.Err(glog, err).Error("While doing traversal")
		return js, err
	}
	l.Json = time.Since(l.Start) - l.Parsing - l.Processing
	if len(r) == 1 {
		for _, ival := range r {
			var m map[string]interface{}
			if ival != nil {
				m = ival.(map[string]interface{})
			}
			m["server_latency"] = l.ToMap()
			return json.Marshal(m)
		}
	} else {
		glog.Fatal("We don't currently support more than 1 uid at root.")
	}

	glog.Fatal("Shouldn't reach here.")
	return json.Marshal(r)
}

// This method take in a flatbuffer result, extracts values and uids from it
// and converts it to a protocol buffer result
func extract(r *task.Result) (*pb.Result, error) {
	var result = &pb.Result{}
	var ul task.UidList
	for i := 0; i < r.UidmatrixLength(); i++ {
		if ok := r.Uidmatrix(&ul, i); !ok {
			return result, fmt.Errorf("While parsing UidList")
		}

		uidList := &pb.UidList{}
		for j := 0; j < ul.UidsLength(); j++ {
			uid := ul.Uids(j)
			uidList.Uids = append(uidList.Uids, uid)
		}
		result.Uidmatrix = append(result.Uidmatrix, uidList)
	}

	var tv task.Value
	for i := 0; i < r.ValuesLength(); i++ {
		if ok := r.Values(&tv, i); !ok {
			return result, fmt.Errorf("While parsing value")
		}

		var ival interface{}
		if err := posting.ParseValue(&ival, tv.ValBytes()); err != nil {
			return result, err
		}

		if ival == nil {
			ival = ""
		}
		result.Values = append(result.Values, []byte(ival.(string)))
	}
	return result, nil
}

// This method performs a pre traversal on a subgraph and converts it to a
// protocol buffer Graph Response.
func (g *SubGraph) PreTraverse() (gr *pb.GraphResponse, rerr error) {
	gr = &pb.GraphResponse{}
	if len(g.query) == 0 {
		return gr, nil
	}

	gr.Attribute = g.Attr
	ro := flatbuffers.GetUOffsetT(g.result)
	r := new(task.Result)
	r.Init(g.result, ro)

	result, err := extract(r)
	if err != nil {
		return gr, err
	}

	gr.Result = result

	for _, child := range g.Children {
		childPb, err := child.PreTraverse()
		if err != nil {
			x.Err(glog, err).Error("Error while traversal")
			return gr, err
		}

		gr.Children = append(gr.Children, childPb)
	}
	return gr, nil
}

func treeCopy(gq *gql.GraphQuery, sg *SubGraph) {
	// Typically you act on the current node, and leave recursion to deal with
	// children. But, in this case, we don't want to muck with the current
	// node, because of the way we're dealing with the root node.
	// So, we work on the children, and then recurse for grand children.
	for _, gchild := range gq.Children {
		dst := new(SubGraph)
		dst.Attr = gchild.Attr
		dst.Count = gchild.First
		sg.Children = append(sg.Children, dst)
		treeCopy(gchild, dst)
	}
}

func ToSubGraph(gq *gql.GraphQuery) (*SubGraph, error) {
	sg, err := newGraph(gq.UID, gq.XID)
	if err != nil {
		return nil, err
	}
	treeCopy(gq, sg)
	return sg, nil
}

func newGraph(euid uint64, exid string) (*SubGraph, error) {
	// This would set the Result field in SubGraph,
	// and populate the children for attributes.
	if len(exid) > 0 {
		xidToUid := make(map[string]uint64)
		xidToUid[exid] = 0
		if err := worker.GetOrAssignUidsOverNetwork(&xidToUid); err != nil {
			glog.WithError(err).Error("While getting uids over network")
			return nil, err
		}

		euid = xidToUid[exid]
		glog.WithField("xid", exid).WithField("uid", euid).Debug("GetOrAssign")
	}

	if euid == 0 {
		err := fmt.Errorf("Query internal id is zero")
		x.Err(glog, err).Error("Invalid query")
		return nil, err
	}

	// Encode uid into result flatbuffer.
	b := flatbuffers.NewBuilder(0)
	omatrix := x.UidlistOffset(b, []uint64{euid})

	// Also need to add nil value to keep this consistent.
	var voffset flatbuffers.UOffsetT
	{
		bvo := b.CreateByteVector(x.Nilbyte)
		task.ValueStart(b)
		task.ValueAddVal(b, bvo)
		voffset = task.ValueEnd(b)
	}

	task.ResultStartUidmatrixVector(b, 1)
	b.PrependUOffsetT(omatrix)
	mend := b.EndVector(1)

	task.ResultStartValuesVector(b, 1)
	b.PrependUOffsetT(voffset)
	vend := b.EndVector(1)

	task.ResultStart(b)
	task.ResultAddUidmatrix(b, mend)
	task.ResultAddValues(b, vend)
	rend := task.ResultEnd(b)
	b.Finish(rend)

	sg := new(SubGraph)
	sg.Attr = "_root_"
	sg.result = b.Bytes[b.Head():]
	// Also add query for consistency and to allow for ToJson() later.
	sg.query = createTaskQuery(sg, []uint64{euid})
	return sg, nil
}

// createTaskQuery generates the query buffer.
func createTaskQuery(sg *SubGraph, sorted []uint64) []byte {
	b := flatbuffers.NewBuilder(0)
	ao := b.CreateString(sg.Attr)

	task.QueryStartUidsVector(b, len(sorted))
	for i := len(sorted) - 1; i >= 0; i-- {
		b.PrependUint64(sorted[i])
	}
	vend := b.EndVector(len(sorted))

	task.QueryStart(b)
	task.QueryAddAttr(b, ao)
	task.QueryAddUids(b, vend)
	task.QueryAddCount(b, int32(sg.Count))

	qend := task.QueryEnd(b)
	b.Finish(qend)
	return b.Bytes[b.Head():]
}

type ListChannel struct {
	TList *task.UidList
	Idx   int
}

func sortedUniqueUids(r *task.Result) (sorted []uint64, rerr error) {
	// Let's serialize the matrix of uids in result to a
	// sorted unique list of uids.
	h := &x.Uint64Heap{}
	heap.Init(h)

	channels := make([]*ListChannel, r.UidmatrixLength())
	for i := 0; i < r.UidmatrixLength(); i++ {
		tlist := new(task.UidList)
		if ok := r.Uidmatrix(tlist, i); !ok {
			return sorted, fmt.Errorf("While parsing Uidmatrix")
		}
		if tlist.UidsLength() > 0 {
			e := x.Elem{
				Uid: tlist.Uids(0),
				Idx: i,
			}
			heap.Push(h, e)
		}
		channels[i] = &ListChannel{TList: tlist, Idx: 1}
	}

	// The resulting list of uids will be stored here.
	sorted = make([]uint64, 100)
	sorted = sorted[:0]

	var last uint64
	last = 0
	// Itearate over the heap.
	for h.Len() > 0 {
		me := (*h)[0] // Peek at the top element in heap.
		if me.Uid != last {
			sorted = append(sorted, me.Uid) // Add if unique.
			last = me.Uid
		}
		lc := channels[me.Idx]
		if lc.Idx >= lc.TList.UidsLength() {
			heap.Pop(h)

		} else {
			uid := lc.TList.Uids(lc.Idx)
			lc.Idx += 1

			me.Uid = uid
			(*h)[0] = me
			heap.Fix(h, 0) // Faster than Pop() followed by Push().
		}
	}
	return sorted, nil
}

func ProcessGraph(sg *SubGraph, rch chan error, td time.Duration) {
	timeout := time.Now().Add(td)

	var err error
	if len(sg.query) > 0 && sg.Attr != "_root_" {
		sg.result, err = worker.ProcessTaskOverNetwork(sg.query)
		if err != nil {
			x.Err(glog, err).Error("While processing task.")
			rch <- err
			return
		}
	}

	uo := flatbuffers.GetUOffsetT(sg.result)
	r := new(task.Result)
	r.Init(sg.result, uo)

	if r.ValuesLength() > 0 {
		var v task.Value
		if r.Values(&v, 0) {
			glog.WithField("attr", sg.Attr).WithField("val", string(v.ValBytes())).
				Info("Sample value")
		}
	}

	sorted, err := sortedUniqueUids(r)
	if err != nil {
		x.Err(glog, err).Error("While processing task.")
		rch <- err
		return
	}

	if len(sorted) == 0 {
		// Looks like we're done here.
		if len(sg.Children) > 0 {
			glog.Debugf("Have some children but no results. Life got cut short early."+
				"Current attribute: %q", sg.Attr)
		} else {
			glog.Debugf("No more things to process for Attr: %v", sg.Attr)
		}
		rch <- nil
		return
	}

	timeleft := timeout.Sub(time.Now())
	if timeleft < 0 {
		glog.WithField("attr", sg.Attr).Error("Query timeout before children")
		rch <- fmt.Errorf("Query timeout before children")
		return
	}

	// Let's execute it in a tree fashion. Each SubGraph would break off
	// as many goroutines as it's children; which would then recursively
	// do the same thing.
	// Buffered channel to ensure no-blockage.
	childchan := make(chan error, len(sg.Children))
	for i := 0; i < len(sg.Children); i++ {
		child := sg.Children[i]
		child.query = createTaskQuery(child, sorted)
		go ProcessGraph(child, childchan, timeleft)
	}

	tchan := time.After(timeleft)
	// Now get all the results back.
	for i := 0; i < len(sg.Children); i++ {
		select {
		case err = <-childchan:
			glog.WithFields(logrus.Fields{
				"num_children": len(sg.Children),
				"index":        i,
				"attr":         sg.Children[i].Attr,
				"err":          err,
			}).Debug("Reply from child")
			if err != nil {
				x.Err(glog, err).Error("While processing child task.")
				rch <- err
				return
			}
		case <-tchan:
			glog.WithField("attr", sg.Attr).Error("Query timeout after children")
			rch <- fmt.Errorf("Query timeout after children")
			return
		}
	}
	rch <- nil
}

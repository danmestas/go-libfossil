package dst

import (
	"container/heap"
	"time"
)

// NodeID identifies a node in the simulation.
type NodeID string

// EventType classifies simulation events.
type EventType int

const (
	EvTimer    EventType = iota // leaf poll timer fired
	EvSyncNow                   // manual sync trigger for a leaf
	EvUVWrite                   // write a UV file to a node's repo
	EvUVDelete                  // delete a UV file from a node's repo
)

// Event is a scheduled occurrence in the simulation.
type Event struct {
	Time    time.Time
	Type    EventType
	NodeID  NodeID
	UVName  string // for EvUVWrite/EvUVDelete: file name
	UVData  []byte // for EvUVWrite: file content
	UVMTime int64  // for EvUVWrite/EvUVDelete: mtime
}

// EventQueue is a min-heap of events ordered by time.
type EventQueue []*Event

func (q EventQueue) Len() int           { return len(q) }
func (q EventQueue) Less(i, j int) bool { return q[i].Time.Before(q[j].Time) }
func (q EventQueue) Swap(i, j int)      { q[i], q[j] = q[j], q[i] }
func (q *EventQueue) Push(x any)        { *q = append(*q, x.(*Event)) }
func (q *EventQueue) Pop() any {
	old := *q
	n := len(old)
	item := old[n-1]
	old[n-1] = nil
	*q = old[:n-1]
	return item
}

// PushEvent adds an event to the queue.
func (q *EventQueue) PushEvent(e *Event) {
	heap.Push(q, e)
}

// PopEvent removes and returns the earliest event.
func (q *EventQueue) PopEvent() *Event {
	return heap.Pop(q).(*Event)
}
